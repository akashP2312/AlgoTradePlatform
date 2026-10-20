-- Operational bookkeeping: gaps, harvest progress, job runs, alerts.

-- kind: snapshot (missing ws minutes per underlying/expiry) | ticks | harvest.
-- filled_by: backup | sweep | harvest, set when another source covers the gap.
CREATE TABLE IF NOT EXISTS recorder_gaps (
    id             bigserial PRIMARY KEY,
    trade_date     date NOT NULL,
    kind           text NOT NULL,
    underlying     text NOT NULL DEFAULT '',
    expiry         date,
    start_ts       timestamptz NOT NULL,
    end_ts         timestamptz NOT NULL,
    minutes        int NOT NULL,
    filled_minutes int NOT NULL DEFAULT 0,
    filled_by      text,
    detail         text,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS recorder_gaps_uniq
    ON recorder_gaps (trade_date, kind, underlying, COALESCE(expiry, '1970-01-01'::date), start_ts);

-- One row per contract per trade date the harvester attempted.
-- status: ok | empty | error.
CREATE TABLE IF NOT EXISTS harvest_log (
    trade_date date NOT NULL,
    contract   text NOT NULL,
    token      bigint NOT NULL,
    status     text NOT NULL,
    rows       int  NOT NULL DEFAULT 0,
    fetched_at timestamptz NOT NULL,
    error      text,
    PRIMARY KEY (trade_date, contract)
);

CREATE TABLE IF NOT EXISTS job_runs (
    id          bigserial PRIMARY KEY,
    job         text NOT NULL,
    args        text NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    status      text NOT NULL,          -- running | ok | error
    detail      text
);
CREATE INDEX IF NOT EXISTS job_runs_job ON job_runs (job, started_at DESC);

CREATE TABLE IF NOT EXISTS alerts (
    id           bigserial PRIMARY KEY,
    rule         text NOT NULL,
    level        text NOT NULL,         -- info | warn | crit
    triggered_at timestamptz NOT NULL,
    message      text NOT NULL,
    delivered    boolean NOT NULL DEFAULT false,
    delivery_err text
);
CREATE INDEX IF NOT EXISTS alerts_time ON alerts (triggered_at DESC);
