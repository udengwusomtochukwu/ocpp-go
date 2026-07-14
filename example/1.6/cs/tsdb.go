package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

// hyde/lab: raw MeterValues persistence into TimescaleDB — the product-data
// plane, distinct from the Prometheus ops gauges in metrics.go. This previews
// hydecharge-ocpi ADR-0003: raw metering samples land verbatim in a
// time-series store (every SampledValue, original unit and all); anything
// billing- or dashboard-shaped stays a downstream projection of it. The
// Grafana "OCPP Sessions (lab)" dashboard reads this table through the
// `timescale` datasource.
//
// TSDB_DSN unset → disabled: tsdbCh stays nil and every enqueue is a no-op.
// The OCPP path never blocks on the database — OnMeterValues enqueues onto a
// buffered channel, a single writer goroutine batch-inserts, and a full
// buffer drops the sample with a warning.

const (
	envVarTsdbDSN  = "TSDB_DSN"
	tsdbBufferSize = 1024
	tsdbBatchMax   = 128
	tsdbFlushEvery = 2 * time.Second
)

// meterSample is one SampledValue flattened to a meter_samples row.
type meterSample struct {
	ts            time.Time
	chargePoint   string
	connectorID   int
	transactionID int // < 0 = outside any transaction (stored as NULL)
	measurand     string
	phase         string
	location      string
	unit          string
	value         float64
}

var tsdbCh chan meterSample // nil = writer disabled

// tsdbSchema mirrors deploy/ocpp-lab/timescale-init.sql. Applied idempotently
// at writer start so a database volume that predates the init script (or a
// Dokploy redeploy that skipped it) still converges.
var tsdbSchema = []string{
	`CREATE EXTENSION IF NOT EXISTS timescaledb`,
	`CREATE TABLE IF NOT EXISTS meter_samples (
		ts             timestamptz      NOT NULL,
		charge_point   text             NOT NULL,
		connector_id   int              NOT NULL,
		transaction_id int,
		measurand      text             NOT NULL,
		phase          text             NOT NULL DEFAULT '',
		location       text             NOT NULL DEFAULT '',
		unit           text             NOT NULL DEFAULT '',
		value          double precision NOT NULL
	)`,
	`SELECT create_hypertable('meter_samples', 'ts', if_not_exists => TRUE)`,
	`CREATE INDEX IF NOT EXISTS meter_samples_cp_tx_ts_idx
		ON meter_samples (charge_point, transaction_id, ts DESC)`,
}

// startTimescale launches the meter-sample writer when TSDB_DSN is set.
func startTimescale() {
	dsn, ok := os.LookupEnv(envVarTsdbDSN)
	if !ok || dsn == "" {
		log.Infof("tsdb: %v not set — meter-sample persistence disabled", envVarTsdbDSN)
		return
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Errorf("tsdb: invalid %v, persistence disabled: %v", envVarTsdbDSN, err)
		return
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Errorf("tsdb: pool init failed, persistence disabled: %v", err)
		return
	}
	tsdbCh = make(chan meterSample, tsdbBufferSize)
	go tsdbWriter(pool)
}

// tsdbEnqueue flattens one MeterValues.req into rows on the writer channel.
// txID is the connector's transaction resolved under handler.mu; an explicit
// transactionId on the request wins. Never blocks: drops on a full buffer.
func tsdbEnqueue(chargePointID string, txID int, req *core.MeterValuesRequest) {
	if tsdbCh == nil {
		return
	}
	if req.TransactionId != nil {
		txID = *req.TransactionId
	}
	for _, mv := range req.MeterValue {
		ts := time.Now()
		if mv.Timestamp != nil {
			ts = mv.Timestamp.Time
		}
		for _, sv := range mv.SampledValue {
			v, err := strconv.ParseFloat(strings.TrimSpace(sv.Value), 64)
			if err != nil {
				continue // SignedData blobs etc. — not a numeric sample
			}
			measurand := string(sv.Measurand)
			if measurand == "" {
				// Per OCPP 1.6, an omitted measurand means the import register.
				measurand = string(types.MeasurandEnergyActiveImportRegister)
			}
			sample := meterSample{
				ts: ts, chargePoint: chargePointID, connectorID: req.ConnectorId,
				transactionID: txID, measurand: measurand, phase: string(sv.Phase),
				location: string(sv.Location), unit: string(sv.Unit), value: v,
			}
			select {
			case tsdbCh <- sample:
			default:
				log.Warnf("tsdb: buffer full — dropped %v sample from %v", measurand, chargePointID)
			}
		}
	}
}

// tsdbWriter is the single consumer: ensures the schema (retrying while the
// database container is still starting), then batch-inserts forever.
func tsdbWriter(pool *pgxpool.Pool) {
	for {
		if err := tsdbEnsureSchema(pool); err != nil {
			log.Warnf("tsdb: schema not ready (database starting?): %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	log.Info("tsdb: timescale meter-sample writer started")
	batch := make([]meterSample, 0, tsdbBatchMax)
	ticker := time.NewTicker(tsdbFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case s := <-tsdbCh:
			batch = append(batch, s)
			if len(batch) >= tsdbBatchMax {
				tsdbFlush(pool, batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				tsdbFlush(pool, batch)
				batch = batch[:0]
			}
		}
	}
}

func tsdbEnsureSchema(pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, stmt := range tsdbSchema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

// tsdbFlush inserts one batch; on error the batch is dropped (lab-grade —
// this store is a telemetry preview, not a ledger) and the writer carries on.
func tsdbFlush(pool *pgxpool.Pool, rows []meterSample) {
	b := &pgx.Batch{}
	for _, s := range rows {
		var tx any
		if s.transactionID >= 0 {
			tx = s.transactionID
		}
		b.Queue(`INSERT INTO meter_samples
			(ts, charge_point, connector_id, transaction_id, measurand, phase, location, unit, value)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			s.ts, s.chargePoint, s.connectorID, tx, s.measurand, s.phase, s.location, s.unit, s.value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.SendBatch(ctx, b).Close(); err != nil {
		log.Warnf("tsdb: dropped batch of %d samples: %v", len(rows), err)
	}
}
