-- One row a minute in session from riskmon: the book's live M2M and Greeks, so the
-- day's P&L line, its peak and drawdown survive a restart and can be replayed.
-- worst: the worst liquidation P&L on the scenario grid within risk.limits.worst_move.
-- util: margin used / (used + free). Rupees throughout; dn in NIFTY-future lots.
CREATE TABLE IF NOT EXISTS risk_snapshots (
    ts         timestamptz NOT NULL,
    trade_date date NOT NULL,
    m2m        double precision NOT NULL,
    pnl        double precision NOT NULL,
    ref_px     double precision,
    d          double precision,
    dn         double precision,
    g          double precision,
    v          double precision,
    t          double precision,
    worst      double precision,
    util       double precision,
    legs       int NOT NULL DEFAULT 0,
    flags      int NOT NULL DEFAULT 0,
    PRIMARY KEY (ts)
);
SELECT create_hypertable('risk_snapshots', by_range('ts', INTERVAL '30 days'), if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS risk_snapshots_day ON risk_snapshots (trade_date, ts);
