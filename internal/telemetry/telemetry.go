package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type TelemetrySystem struct {
	TracerProvider trace.TracerProvider
	exporter       sdktrace.SpanExporter
}

func NewTelemetrySystem() TelemetrySystem {
	tp := trace.NewNoopTracerProvider()
	exp, err := newTelemetryExporter()

	if err != nil {
		panic(err)
	}

	otel.SetTracerProvider(tp)

	return TelemetrySystem{
		TracerProvider: tp,
		exporter:       exp,
	}
}

func (ts TelemetrySystem) Shutdown(ctx context.Context) {
	_ = ts.exporter.Shutdown(ctx) // TODO: Best effort?
}

func CallWithTelemetry[TResult any](tracer trace.Tracer, spanName string, parentCtx context.Context, fn func(ctx context.Context) (TResult, error)) (TResult, error) {
	spanCtx, span := tracer.Start(parentCtx, spanName)
	defer span.End()

	result, err := fn(spanCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return result, err
}

func CallWithTelemetryNoResult(tracer trace.Tracer, spanName string, parentCtx context.Context, fn func(ctx context.Context) error) error {
	spanCtx, span := tracer.Start(parentCtx, spanName)
	defer span.End()

	err := fn(spanCtx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

type TelemetryAttribute interface {
	int | int64 | bool | float64 | string | []int | []int64 | []bool | []float64 | []string
}

func SetAttribute[T TelemetryAttribute](ctx context.Context, key string, value T) {
	span := trace.SpanFromContext(ctx)

	switch v := (any)(value).(type) {
	case int:
		span.SetAttributes(attribute.Int(key, v))
	case int64:
		span.SetAttributes(attribute.Int64(key, v))
	case bool:
		span.SetAttributes(attribute.Bool(key, v))
	case float64:
		span.SetAttributes(attribute.Float64(key, v))
	case string:
		span.SetAttributes(attribute.String(key, v))
	case []int:
		span.SetAttributes(attribute.IntSlice(key, v))
	case []int64:
		span.SetAttributes(attribute.Int64Slice(key, v))
	case []bool:
		span.SetAttributes(attribute.BoolSlice(key, v))
	case []float64:
		span.SetAttributes(attribute.Float64Slice(key, v))
	case []string:
		span.SetAttributes(attribute.StringSlice(key, v))
	default:
		// This should never happen
		fmt.Printf("unknown telemetry type for key %s", key)
	}
}
