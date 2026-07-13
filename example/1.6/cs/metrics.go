package main

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
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
	// charging-telemetry gauges (set-style), fed by MeterValues samples in
	// recordMeterValues. Same cardinality discipline: charge-point id stays
	// OFF the label set, so each gauge is fleet-wide "most recent report" —
	// with several chargers charging at once the newest sample wins.
	mPower     metric.Float64Gauge // ocpp_power_w        <- Power.Active.Import
	mEnergyReg metric.Float64Gauge // ocpp_energy_register_wh{direction} <- Energy.Active.*.Register
	mSoC       metric.Float64Gauge // ocpp_soc_percent    <- SoC
	powerSeen  atomic.Bool         // true once any Power.Active.Import sample arrived
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
	mPower, _ = meter.Float64Gauge("ocpp.power.w",
		metric.WithDescription("Instantaneous charging power (W) from Power.Active.Import meter samples"))
	mEnergyReg, _ = meter.Float64Gauge("ocpp.energy.register.wh",
		metric.WithDescription("Energy register reading (Wh) from Energy.Active.{Import,Export}.Register meter samples, by direction"))
	mSoC, _ = meter.Float64Gauge("ocpp.soc.percent",
		metric.WithDescription("EV state of charge (percent) from SoC meter samples"))

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
		// Chargers stop streaming MeterValues after StopTransaction, which
		// would freeze the power gauge at its last in-charge value. Zero it —
		// but only if power was ever reported, so a charger that never sends
		// Power.Active.Import keeps the gauge honestly empty (no series)
		// instead of a fabricated 0.
		if powerSeen.Load() {
			mPower.Record(ctx, 0)
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

// recordMeterValues extracts charging telemetry from a MeterValues.req and
// sets the matching gauges — the sampled-data counterpart to recordEvent.
// Called from OnMeterValues. Per OCPP 1.6, a SampledValue with no Measurand
// means Energy.Active.Import.Register. Phase-qualified samples (L1/L2/…) are
// skipped so per-phase readings don't overwrite the aggregate value.
func recordMeterValues(req *core.MeterValuesRequest) {
	if mPower == nil {
		return
	}
	ctx := context.Background()
	for _, mv := range req.MeterValue {
		for _, sv := range mv.SampledValue {
			if sv.Phase != "" {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(sv.Value), 64)
			if err != nil {
				continue // SignedData blobs etc. — not chartable
			}
			switch sv.Measurand {
			case types.MeasurandPowerActiveImport:
				powerSeen.Store(true)
				mPower.Record(ctx, scaleToBase(v, sv.Unit))
			case types.MeasurandEnergyActiveImportRegister, "":
				mEnergyReg.Record(ctx, scaleToBase(v, sv.Unit),
					metric.WithAttributes(attribute.String("direction", "import")))
			case types.MeasurandEnergyActiveExportRegister:
				mEnergyReg.Record(ctx, scaleToBase(v, sv.Unit),
					metric.WithAttributes(attribute.String("direction", "export")))
			case types.MeasurandSoC:
				mSoC.Record(ctx, v)
			}
		}
	}
}

// scaleToBase normalizes kW→W / kWh→Wh. An omitted unit already is the base
// unit per OCPP 1.6 (Wh for energy registers, W for power).
func scaleToBase(v float64, unit types.UnitOfMeasure) float64 {
	switch unit {
	case types.UnitOfMeasureKW, types.UnitOfMeasureKWh:
		return v * 1000
	}
	return v
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
