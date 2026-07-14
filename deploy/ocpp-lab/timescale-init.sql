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
    value          double precision NOT NULL
);

SELECT create_hypertable('meter_samples', 'ts', if_not_exists => TRUE);

-- Session-centric access path: "samples of tx N on charger X, in time order".
CREATE INDEX IF NOT EXISTS meter_samples_cp_tx_ts_idx
    ON meter_samples (charge_point, transaction_id, ts DESC);
