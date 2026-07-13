package main

import (
	"context"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/attribute"
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
	// event-derived instruments (recorded from the bus — one hook, all signals).
	// Labels stay LOW-cardinality: event type / command status enums only.
	mEvents   metric.Int64Counter // ocpp_events_total{type}
	mTxStart  metric.Int64Counter // ocpp_transactions_started_total
	mTxStop   metric.Int64Counter // ocpp_transactions_stopped_total
	mEnergyWh metric.Int64Counter // ocpp_energy_wh_total
	mFaults   metric.Int64Counter // ocpp_faults_total{error_code}
	mCommands metric.Int64Counter // ocpp_command_results_total{status}
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
	mEvents, _ = meter.Int64Counter("ocpp.events",
		metric.WithDescription("Registry events by type"))
	mTxStart, _ = meter.Int64Counter("ocpp.transactions.started",
		metric.WithDescription("Transactions started"))
	mTxStop, _ = meter.Int64Counter("ocpp.transactions.stopped",
		metric.WithDescription("Transactions stopped"))
	mEnergyWh, _ = meter.Int64Counter("ocpp.energy.wh",
		metric.WithDescription("Energy delivered across stopped transactions (Wh)"))
	mFaults, _ = meter.Int64Counter("ocpp.faults",
		metric.WithDescription("StatusNotifications carrying an errorCode"))
	mCommands, _ = meter.Int64Counter("ocpp.command.results",
		metric.WithDescription("Remote command confirmations by status"))

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

// recordEvent derives counters from bus events — a single instrumentation
// point for everything the registry observes. Called by eventBus.Publish.
func recordEvent(ev Event) {
	if mEvents == nil {
		return
	}
	ctx := context.Background()
	mEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("type", ev.Type)))
	switch ev.Type {
	case "tx.started":
		mTxStart.Add(ctx, 1)
	case "tx.stopped":
		mTxStop.Add(ctx, 1)
		if wh, ok := ev.Data["energyWh"].(int); ok && wh > 0 {
			mEnergyWh.Add(ctx, int64(wh))
		}
	case "status":
		if code, _ := ev.Data["errorCode"].(string); code != "" && code != "NoError" {
			mFaults.Add(ctx, 1, metric.WithAttributes(attribute.String("error_code", code)))
		}
	case "command.result":
		status, _ := ev.Data["status"].(string)
		if status == "" {
			status = "unknown"
		}
		mCommands.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
	}
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
