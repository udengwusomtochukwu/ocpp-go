package main

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// hyde/lab: distributed tracing for the OCPP central system, exported over
// OTLP/gRPC (push) to an OTel Collector -> Jaeger, visible in Grafana. Note
// the deliberate asymmetry with metrics: charge-point id is put on the SPAN
// (attribute) — spans tolerate high cardinality — but is NEVER a metric label.
const (
	envVarOtlpEndpoint  = "OTLP_ENDPOINT"
	defaultOtlpEndpoint = "otel-collector:4317"
	traceServiceName    = "ocpp-central-system"
)

var tracer = otel.Tracer("github.com/hydecharge/ocpp-lab/cs")

// startTracing installs a global tracer provider that pushes spans over
// OTLP/gRPC to the collector. Safe no-op (returns an empty shutdown) on error.
func startTracing() func() {
	endpoint := defaultOtlpEndpoint
	if e, ok := os.LookupEnv(envVarOtlpEndpoint); ok && e != "" {
		endpoint = e
	}
	ctx := context.Background()
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		log.Errorf("tracing: OTLP exporter init failed: %v", err)
		return func() {}
	}
	res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(traceServiceName)))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("github.com/hydecharge/ocpp-lab/cs")
	log.Infof("tracing: pushing OTLP spans to %s", endpoint)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	}
}

// startCPSpan opens a server span for an inbound OCPP message. The
// charge-point id is a span attribute (safe — spans are cardinality-tolerant).
func startCPSpan(chargePointID, action string) (context.Context, trace.Span) {
	return tracer.Start(context.Background(), "ocpp."+action,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("ocpp.charge_point_id", chargePointID),
			attribute.String("ocpp.action", action),
		),
	)
}
