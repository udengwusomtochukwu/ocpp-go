-- hyde/lab: schema for raw per-sample MeterValues persistence — a working
-- preview of hydecharge-ocpi ADR-0003 (raw metering lives in a time-series
-- store; billing/dashboard shapes stay downstream projections).
--
-- Runs once via docker-entrypoint-initdb.d on first volume init. tsdb.go in
-- the central system applies the same idempotent DDL at boot, so a volume
-- that predates this file still converges.
CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE TABLE IF NOT EXISTS meter_samples (
    ts             timestamptz      NOT NULL,
    charge_point   text             NOT NULL,
    connector_id   int              NOT NULL,
    transaction_id int,             -- NULL = sample outside any transaction
    measurand      text             NOT NULL,
    phase          text             NOT NULL DEFAULT '',
    location       text             NOT NULL DEFAULT '',
    unit           text             NOT NULL DEFAULT '',
    value          double precision NOT NULL,
    is_sim         boolean          -- live/sim separation; NULL only on rows
                                    -- predating the column (backfilled at CS
                                    -- boot against LIVE_CHARGER_IDS)
);

-- Pre-existing volumes converge to the same shape (tsdb.go re-runs this).
ALTER TABLE meter_samples ADD COLUMN IF NOT EXISTS is_sim boolean;

-- Durable fault log (low volume — plain table). The CS re-seeds its in-memory
-- REST fault ring from here at boot, so restarts no longer blank fault history.
CREATE TABLE IF NOT EXISTS charger_faults (
    ts                timestamptz NOT NULL,
    charge_point      text        NOT NULL,
    connector_id      int         NOT NULL DEFAULT 0,
    status            text        NOT NULL DEFAULT '',
    error_code        text        NOT NULL,
    vendor_error_code text        NOT NULL DEFAULT '',
    info              text        NOT NULL DEFAULT '',
    severity          text        NOT NULL DEFAULT 'warning',
    is_sim            boolean     NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS charger_faults_cp_ts_idx
    ON charger_faults (charge_point, ts DESC);

SELECT create_hypertable('meter_samples', 'ts', if_not_exists => TRUE);

-- Session-centric access path: "samples of tx N on charger X, in time order".
CREATE INDEX IF NOT EXISTS meter_samples_cp_tx_ts_idx
    ON meter_samples (charge_point, transaction_id, ts DESC);

-- Lifecycle policies, lab-sized (ADR-0003 previews the knobs; prod keeps
-- multi-year data and archives instead of dropping): compress chunks once
-- they're a week old, drop raw samples after 90 days.
ALTER TABLE meter_samples SET (
    timescaledb.compress,
    timescaledb.compress_orderby = 'ts DESC',
    timescaledb.compress_segmentby = 'charge_point, transaction_id'
);
SELECT add_compression_policy('meter_samples', compress_after => INTERVAL '7 days', if_not_exists => TRUE);
SELECT add_retention_policy('meter_samples', drop_after => INTERVAL '90 days', if_not_exists => TRUE);
