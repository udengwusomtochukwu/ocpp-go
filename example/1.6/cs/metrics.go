package main

import (
	"context"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// hyde/lab: registry-health instrumentation for the OCPP connection lifecycle
// (epic #68 / plan 26). Uses the OpenTelemetry metrics SDK with a Prometheus
// exporter, so the same instrument is idiomatic OTel *and* scrapeable by
// Prometheus. Charge-point id is deliberately NOT a label — it is
// high-cardinality and belongs on traces/logs, not metric series.
const (
	envVarMetricsPort  = "METRICS_PORT"
	defaultMetricsPort = "2112"
)

var (
	mConnected metric.Int64UpDownCounter // ocpp_chargers_connected (gauge)
	mOpened    metric.Int64Counter       // ocpp_connections_opened_total
	mClosed    metric.Int64Counter       // ocpp_connections_closed_total
)

// startMetrics wires an OTel meter to a Prometheus exporter and serves
// /metrics on METRICS_PORT (default 2112). Safe no-op if init fails.
func startMetrics() {
	exporter, err := otelprom.New()
	if err != nil {
		log.Errorf("metrics: prometheus exporter init failed: %v", err)
		return
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	meter := provider.Meter("github.com/hydecharge/ocpp-lab/cs")

	mConnected, _ = meter.Int64UpDownCounter("ocpp.chargers.connected",
		metric.WithDescription("Charge points currently connected to the central system"))
	mOpened, _ = meter.Int64Counter("ocpp.connections.opened",
		metric.WithDescription("Total charge-point WebSocket connections opened"))
	mClosed, _ = meter.Int64Counter("ocpp.connections.closed",
		metric.WithDescription("Total charge-point WebSocket connections closed"))

	port := defaultMetricsPort
	if p, ok := os.LookupEnv(envVarMetricsPort); ok && p != "" {
		port = p
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Infof("serving OTel/Prometheus metrics on :%s/metrics", port)
		if err := http.ListenAndServe(":"+port, mux); err != nil {
			log.Errorf("metrics server stopped: %v", err)
		}
	}()
}

// onConnect / onDisconnect are called from the CS connection-lifecycle
// handlers. They hide the context so the callsites stay one-liners.
func onConnect() {
	if mConnected == nil {
		return
	}
	ctx := context.Background()
	mConnected.Add(ctx, 1)
	mOpened.Add(ctx, 1)
}

func onDisconnect() {
	if mConnected == nil {
		return
	}
	ctx := context.Background()
	mConnected.Add(ctx, -1)
	mClosed.Add(ctx, 1)
}
