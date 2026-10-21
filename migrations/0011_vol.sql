-- Realized vol and the HAR-RV forecast from index_eod, written by pmt-vol.
-- Vols are annualized (252 trading days) and in decimals: 0.15 is 15%.

-- Trailing realized vol per index over the `win` trading days ending at trade_date:
-- close-to-close, Parkinson, Garman-Klass and Yang-Zhang. Parkinson and Garman-Klass
-- see the session only (high, low, open to close); close-to-close and Yang-Zhang
-- include the overnight gap.
CREATE TABLE IF NOT EXISTS rv_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    win           smallint         NOT NULL,
    cc            double precision,
    pk            double precision,
    gk            double precision,
    yz            double precision,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, win)
);

-- The HAR-RV forecast, made at trade_date's close, of close-to-close vol over the
-- next `horizon` trading days. lo and hi are the 10% and 90% bands from the fit's own
-- out-of-sample residuals. rv_fwd is what that vol turned out to be, null until the
-- horizon has passed. The coefficients are those of the log-HAR fit used that day,
-- trained only on targets already realized by then.
CREATE TABLE IF NOT EXISTS har_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    horizon       smallint         NOT NULL,
    fc            double precision NOT NULL,
    lo            double precision,
    hi            double precision,
    rv_fwd        double precision,
    rv21          double precision,          -- the naive benchmark: trailing 21-day close-to-close
    vix           double precision,          -- India VIX / 100, the implied benchmark (NIFTY only)
    b0            double precision,
    b_d           double precision,
    b_w           double precision,
    b_m           double precision,
    n_train       integer          NOT NULL,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, horizon)
);

-- The out-of-sample scorecard, recomputed on each run: QLIKE and MSE (on variance) of
-- each forecast against rv_fwd over every day with both, for model har, rv21 and vix.
CREATE TABLE IF NOT EXISTS vol_score (
    asof          date             NOT NULL,
    underlying    text             NOT NULL,
    horizon       smallint         NOT NULL,
    model         text             NOT NULL,
    n             integer          NOT NULL,
    frm           date             NOT NULL,
    "to"          date             NOT NULL,
    qlike         double precision NOT NULL,
    mse           double precision NOT NULL,
    bias          double precision NOT NULL,  -- mean forecast vol minus mean realized vol
    hit           double precision,           -- share of rv_fwd inside [lo, hi] (har only)
    model_version text             NOT NULL,
    PRIMARY KEY (asof, underlying, horizon, model)
);
