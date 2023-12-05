package telemetry

import (
	"context"
	"io"

	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace"
)

func newTelemetryExporter() (trace.SpanExporter, error) {
	return stdouttrace.New(stdouttrace.WithPrettyPrint(), stdouttrace.WithWriter(io.Discard))
}

func newMetricExporter() (sdkmetric.Exporter, error) {
	return discardExporter{}, nil
}

type discardExporter struct{}

func (discardExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	return nil
}

func (discardExporter) Export(context.Context, metricdata.ResourceMetrics) error {
	return nil
}

func (discardExporter) ForceFlush(context.Context) error {
	return nil
}

func (discardExporter) Shutdown(ctx context.Context) error {
	return nil
}
