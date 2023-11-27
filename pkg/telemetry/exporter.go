package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/sdk/trace"
)

func newTelemetryExporter() (trace.SpanExporter, error) {
	return discardExporter{}, nil
}

type discardExporter struct{}

func (discardExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	return nil
}

func (discardExporter) Shutdown(ctx context.Context) error {
	return nil
}
