package main

import (
	"context"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otellogrus"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// hyde/lab: the third OTel signal. Bridges logrus -> the OTel logs SDK ->
// OTLP/gRPC push to the collector (-> Loki -> Grafana). logrus still writes to
// stdout as before; the hook additionally emits each record to the OTel
// pipeline. Records logged with a span context (logDefault(...).WithContext)
// carry trace_id/span_id, so you can pivot trace <-> logs in Grafana.
func startLogging() func() {
	endpoint := defaultOtlpEndpoint
	if e, ok := os.LookupEnv(envVarOtlpEndpoint); ok && e != "" {
		endpoint = e
	}
	ctx := context.Background()
	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		log.Errorf("logging: OTLP log exporter init failed: %v", err)
		return func() {}
	}
	res, _ := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(traceServiceName)))
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)
	// Send all logrus records to the OTel logs pipeline (stdout unaffected).
	log.AddHook(otellogrus.NewHook(traceServiceName, otellogrus.WithLoggerProvider(lp)))
	log.Infof("logging: pushing OTLP logs to %s", endpoint)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = lp.Shutdown(ctx)
	}
}
