-- Vol regime labels per index and trade date, written by pmt-regime from vrp_eod,
-- ivcm_eod and rv_eod. Each row keeps its readings, their buckets and the rule that
-- gave the label, so a label can be checked by hand. Labels describe, they are not signals.
--   level_pct   252-day percentile of VIX (level_src 'vix') or of iv30 ('iv30'), 0 to 1
--   term_slope  63-day (else 42-day) minus 21-day constant-maturity ATM IV, decimals
--   rv_ratio    10-day over 63-day Yang-Zhang realized vol
--   premium     iv30 (or VIX) minus the HAR forecast, ex ante, decimals
--   label       null when level or term is missing that day
CREATE TABLE IF NOT EXISTS regime_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    level_src     text,
    level_pct     double precision,
    term_slope    double precision,
    rv_ratio      double precision,
    premium       double precision,
    level         text,
    term          text,
    trend         text,
    prem          text,
    label         text,
    rule          text,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying)
);
