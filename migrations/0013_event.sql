-- The move the option market prices to each expiry, written by pmt-event. Moves are
-- fractions of the forward: 0.012 is 1.2%. Days are trading days.

-- Per index, trade date and expiry (from the bhavcopy fits in iv_eod):
--   implied_move  the straddle-equivalent expected absolute move to expiry,
--                 atm_iv * sqrt(tte) * sqrt(2/pi);
--   seg_vol       the forward vol between the previous expiry and this one;
--   event_var     the total variance of that segment above the curve's typical
--                 forward rate (its median across expiries): the term-structure kink;
--                 event_move is sqrt(event_var) * sqrt(2/pi), 0 when there is none;
--   hist_move     the mean absolute close-to-close move over dte days in the 504 days
--                 before trade_date, and hist_n the windows behind it;
--   real_move     the absolute log move from trade_date's close to the close on the
--                 expiry date, null until it is known;
--   events        the calendar events (config/calendar/events.csv) in the segment.
CREATE TABLE IF NOT EXISTS event_eod (
    trade_date    date             NOT NULL,
    underlying    text             NOT NULL,
    expiry        date             NOT NULL,
    dte           double precision NOT NULL,
    atm_iv        double precision NOT NULL,
    implied_move  double precision NOT NULL,
    seg_vol       double precision,
    event_var     double precision,
    event_move    double precision,
    hist_move     double precision,
    hist_n        integer          NOT NULL DEFAULT 0,
    real_move     double precision,
    events        text,
    model_version text             NOT NULL,
    PRIMARY KEY (trade_date, underlying, expiry)
);
