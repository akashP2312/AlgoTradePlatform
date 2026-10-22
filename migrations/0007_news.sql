-- newsmon: every change of the news risk state, for backtests (what the filter
-- said, when, and why). reasons is the RiskState reasons array.
CREATE TABLE IF NOT EXISTS news_risk_log (
    ts         timestamptz NOT NULL,
    state      text NOT NULL,
    prev       text,
    reasons    jsonb NOT NULL DEFAULT '[]',
    expires_at timestamptz,
    PRIMARY KEY (ts)
);
