-- Implied vol history from iv_eod and index_eod, written by pmt-ivhist.
-- Vols are annualized and in decimals: 0.15 is 15%. Tenors and horizons are in trading days.

-- Constant-maturity ATM IV, rr25 and bf25 per index, interpolated between the two
-- expiries around the tenor (ATM in total variance, the skew linearly in dte); no row
-- when no two good fits bracket the tenor. Rank is (x - min) / (max - min) and pct the
-- share of days below x, over up to 252 earlier days with a value; n_hist is how many.
CREATE TABLE IF NOT EXISTS ivcm_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    tenor         smallint         NOT NULL,
    atm_iv        double precision NOT NULL,
    rr25          double precision,
    bf25          double precision,
    atm_rank      double precision,
    atm_pct       double precision,
    rr25_pct      double precision,
    bf25_pct      double precision,
    n_hist        integer          NOT NULL,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, tenor)
);

-- The vol premium over the next `horizon` days, made at trade_date's close: the 21-day
-- constant-maturity ATM IV (iv30) and India VIX / 100 (NIFTY only), each against the
-- HAR forecast of realized vol (ex ante, what was expected then) and against the vol
-- that was then realized (ex post, null until the horizon has passed). The premia are
-- in vol points (iv minus rv); var_* are the same in variance (iv^2 minus rv^2).
CREATE TABLE IF NOT EXISTS vrp_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    horizon       smallint         NOT NULL,
    iv30          double precision,
    vix           double precision,
    har_fc        double precision,
    rv21          double precision,
    rv_fwd        double precision,
    iv_ante       double precision,
    iv_post       double precision,
    vix_ante      double precision,
    vix_post      double precision,
    var_vix_ante  double precision,
    var_vix_post  double precision,
    iv_rank       double precision,
    iv_pct        double precision,
    vix_rank      double precision,
    vix_pct       double precision,
    n_iv          integer          NOT NULL DEFAULT 0,
    n_vix         integer          NOT NULL DEFAULT 0,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, horizon)
);
