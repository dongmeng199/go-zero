package redis

import (
	"context"
	"io"
	"net"
	"strings"
	"time"

	red "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/errorx"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/mapping"
	"github.com/zeromicro/go-zero/core/timex"
	"github.com/zeromicro/go-zero/core/trace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// spanName is the span name of the redis calls.
const spanName = "redis"

var (
	startTimeKey          = contextKey("startTime")
	durationHook          = hook{}
	redisCmdsAttributeKey = attribute.Key("redis.cmds")
)

type (
	contextKey string
	hook       struct{}
)

func (h hook) DialHook(next red.DialHook) red.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (h hook) ProcessHook(next red.ProcessHook) red.ProcessHook {
	return func(ctx context.Context, cmd red.Cmder) error {
		ctx = h.BeforeProcess(context.WithValue(ctx, startTimeKey, timex.Now()), cmd)

		err := next(ctx, cmd)
		if err != nil {
			return err
		}

		h.AfterProcess(ctx, cmd)
		return nil
	}
}

func (h hook) BeforeProcess(ctx context.Context, cmd red.Cmder) context.Context {
	return h.startSpan(context.WithValue(ctx, startTimeKey, timex.Now()), cmd)
}

func (h hook) AfterProcess(ctx context.Context, cmd red.Cmder) {
	err := cmd.Err()
	h.endSpan(ctx, err)

	val := ctx.Value(startTimeKey)
	if val == nil {
		return
	}

	start, ok := val.(time.Duration)
	if !ok {
		return
	}

	duration := timex.Since(start)
	if duration > slowThreshold.Load() {
		logDuration(ctx, []red.Cmder{cmd}, duration)
		metricSlowCount.Inc(cmd.Name())
	}

	metricReqDur.ObserveFloat(float64(duration)/float64(time.Millisecond), cmd.Name())
	if msg := formatError(err); len(msg) > 0 {
		metricReqErr.Inc(cmd.Name(), msg)
	}
}

func (h hook) ProcessPipelineHook(next red.ProcessPipelineHook) red.ProcessPipelineHook {
	return func(ctx context.Context, cmds []red.Cmder) error {
		ctx = h.BeforeProcessPipeline(ctx, cmds)

		err := next(ctx, cmds)
		if err != nil {
			return err
		}

		h.AfterProcessPipeline(ctx, cmds)
		return nil
	}
}

func (h hook) BeforeProcessPipeline(ctx context.Context, cmds []red.Cmder) context.Context {
	if len(cmds) == 0 {
		return ctx
	}

	return h.startSpan(context.WithValue(ctx, startTimeKey, timex.Now()), cmds...)
}

func (h hook) AfterProcessPipeline(ctx context.Context, cmds []red.Cmder) {
	if len(cmds) == 0 {
		return
	}

	batchError := errorx.BatchError{}
	for _, cmd := range cmds {
		err := cmd.Err()
		if err == nil {
			continue
		}

		batchError.Add(err)
	}
	h.endSpan(ctx, batchError.Err())

	val := ctx.Value(startTimeKey)
	if val == nil {
		return
	}

	start, ok := val.(time.Duration)
	if !ok {
		return
	}

	duration := timex.Since(start)
	if duration > slowThreshold.Load()*time.Duration(len(cmds)) {
		logDuration(ctx, cmds, duration)
	}

	metricReqDur.Observe(duration.Milliseconds(), "Pipeline")
	if msg := formatError(batchError.Err()); len(msg) > 0 {
		metricReqErr.Inc("Pipeline", msg)
	}
}

func formatError(err error) string {
	if err == nil || err == red.Nil {
		return ""
	}

	opErr, ok := err.(*net.OpError)
	if ok && opErr.Timeout() {
		return "timeout"
	}

	switch err {
	case io.EOF:
		return "eof"
	case context.DeadlineExceeded:
		return "context deadline"
	case breaker.ErrServiceUnavailable:
		return "breaker"
	default:
		return "unexpected error"
	}
}

func logDuration(ctx context.Context, cmds []red.Cmder, duration time.Duration) {
	var buf strings.Builder
	for k, cmd := range cmds {
		if k > 0 {
			buf.WriteByte('\n')
		}
		var build strings.Builder
		for i, arg := range cmd.Args() {
			if i > 0 {
				build.WriteByte(' ')
			}
			build.WriteString(mapping.Repr(arg))
		}
		buf.WriteString(build.String())
	}
	logx.WithContext(ctx).WithDuration(duration).Slowf("[REDIS] slowcall on executing: %s", buf.String())
}

func (h hook) startSpan(ctx context.Context, cmds ...red.Cmder) context.Context {
	tracer := trace.TracerFromContext(ctx)

	ctx, span := tracer.Start(ctx,
		spanName,
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
	)

	cmdStrs := make([]string, 0, len(cmds))
	for _, cmd := range cmds {
		cmdStrs = append(cmdStrs, cmd.Name())
	}
	span.SetAttributes(redisCmdsAttributeKey.StringSlice(cmdStrs))

	return ctx
}

func (h hook) endSpan(ctx context.Context, err error) {
	span := oteltrace.SpanFromContext(ctx)
	defer span.End()

	if err == nil || err == red.Nil {
		span.SetStatus(codes.Ok, "")
		return
	}

	span.SetStatus(codes.Error, err.Error())
	span.RecordError(err)
}
