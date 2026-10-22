-- Daily implied vol per underlying and expiry, the history Phase 4 reads.
-- source 'bhav' rows come from the NSE bhavcopy settle prices (pmt-iv-eod);
-- source 'live' rows are the IV engine's own close-of-day snapshot. The two are
-- kept apart rather than merged: settle prices and live mids differ.
CREATE TABLE IF NOT EXISTS iv_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    expiry        date             NOT NULL,
    source        text             NOT NULL CHECK (source IN ('bhav', 'live')),
    ts            timestamptz      NOT NULL,          -- the instant the IV is measured at
    tte           double precision NOT NULL,          -- trading years
    dte           double precision NOT NULL,          -- trading days
    fwd           double precision NOT NULL,
    fsrc          text             NOT NULL,          -- parity | future | spot
    spot          double precision,
    atm_iv        double precision,
    rr25          double precision,
    bf25          double precision,
    fit_a         double precision,
    fit_b         double precision,
    fit_rho       double precision,
    fit_m         double precision,
    fit_s         double precision,
    rmse          double precision,
    ok            boolean          NOT NULL DEFAULT false,
    n_strikes     integer          NOT NULL DEFAULT 0,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, expiry, source)
);

CREATE INDEX IF NOT EXISTS iv_eod_und ON iv_eod (underlying, source, trade_date);
