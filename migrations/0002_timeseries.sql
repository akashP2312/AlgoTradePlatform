-- Time-series tables. Compression settings live in 0003 so they can be tuned alone.

CREATE TABLE IF NOT EXISTS ticks (
    recv_ts   timestamptz NOT NULL,
    exch_ts   timestamptz,
    ltt       timestamptz,
    contract  text   NOT NULL,
    token     bigint NOT NULL,
    ltp       double precision NOT NULL,
    ltq       bigint,
    volume    bigint,
    oi        bigint,
    bid       double precision,
    ask       double precision,
    bid_qty   bigint,
    ask_qty   bigint,
    tbq       bigint,
    tsq       bigint,
    avg_price double precision,
    open      double precision,
    high      double precision,
    low       double precision,
    close     double precision,
    phase     char(1) NOT NULL
);
SELECT create_hypertable('ticks', by_range('recv_ts', INTERVAL '1 day'), if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS ticks_contract_ts ON ticks (contract, recv_ts DESC);

-- source: live (built from ticks) | kite (harvested minute candles, replaces live).
CREATE TABLE IF NOT EXISTS bars_1m (
    ts       timestamptz NOT NULL,
    contract text NOT NULL,
    source   text NOT NULL,
    open     double precision NOT NULL,
    high     double precision NOT NULL,
    low      double precision NOT NULL,
    close    double precision NOT NULL,
    volume   bigint NOT NULL DEFAULT 0,
    oi       bigint,
    PRIMARY KEY (contract, ts, source)
);
SELECT create_hypertable('bars_1m', by_range('ts', INTERVAL '7 days'), if_not_exists => TRUE);

-- source: ws (minute snapshot from the stream) | sweep (15:25 REST) | backup (second host).
-- IV and Greeks are filled by the analytics phase; the inputs used are kept alongside.
CREATE TABLE IF NOT EXISTS chain_snapshots (
    ts            timestamptz NOT NULL,
    contract      text NOT NULL,
    source        text NOT NULL,
    underlying    text NOT NULL,
    expiry        date NOT NULL,
    strike        double precision NOT NULL,
    opt_type      char(2) NOT NULL,
    token         bigint,
    bid           double precision,
    ask           double precision,
    bid_qty       bigint,
    ask_qty       bigint,
    ltp           double precision,
    ltq           bigint,
    ltt           timestamptz,
    exch_ts       timestamptz,
    quote_age_ms  int,
    trade_age_ms  int,
    oi            bigint,
    volume        bigint,
    avg_price     double precision,
    tbq           bigint,
    tsq           bigint,
    spot          double precision,
    fut           double precision,
    forward       double precision,
    tte_years     double precision,
    iv            double precision,
    delta         double precision,
    gamma         double precision,
    vega          double precision,
    theta         double precision,
    model_version text,
    PRIMARY KEY (ts, contract, source)
);
SELECT create_hypertable('chain_snapshots', by_range('ts', INTERVAL '7 days'), if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS chain_snapshots_chain ON chain_snapshots (underlying, expiry, ts DESC);

-- Daily bhavcopy, one row per contract. turnover is in rupees for both formats.
-- volume_unit: contracts (legacy CONTRACTS) | udiff (TtlTradgVol; unit to verify).
CREATE TABLE IF NOT EXISTS eod_contracts (
    trade_date       date NOT NULL,
    contract         text NOT NULL,
    exchange         text NOT NULL,
    underlying       text NOT NULL,
    expiry           date,
    strike           double precision,
    kind             text NOT NULL,
    open             double precision,
    high             double precision,
    low              double precision,
    close            double precision,
    last             double precision,
    prev_close       double precision,
    settle           double precision,
    underlying_price double precision,
    oi               bigint,
    oi_change        bigint,
    volume           bigint,
    volume_unit      text NOT NULL,
    turnover         double precision,
    trades           bigint,
    lot_size         int,
    source           text NOT NULL,
    PRIMARY KEY (trade_date, contract)
);
CREATE INDEX IF NOT EXISTS eod_contracts_chain ON eod_contracts (underlying, expiry, trade_date);

-- Daily index closes (India VIX and the underlyings) from Kite day candles or NSE CSV.
CREATE TABLE IF NOT EXISTS index_eod (
    trade_date date NOT NULL,
    contract   text NOT NULL,
    open       double precision,
    high       double precision,
    low        double precision,
    close      double precision NOT NULL,
    source     text NOT NULL,
    PRIMARY KEY (trade_date, contract)
);

-- NSE participant-wise OI, long form. instrument: fut_idx, fut_stk, opt_idx_ce, ...
CREATE TABLE IF NOT EXISTS participant_oi (
    trade_date  date NOT NULL,
    client_type text NOT NULL,
    instrument  text NOT NULL,
    long        bigint NOT NULL,
    short       bigint NOT NULL,
    PRIMARY KEY (trade_date, client_type, instrument)
);

-- Every order update seen on the ticker connection, as an append-only log.
CREATE TABLE IF NOT EXISTS orders (
    id                 bigserial PRIMARY KEY,
    received_at        timestamptz NOT NULL,
    order_id           text NOT NULL,
    exchange_order_id  text,
    status             text,
    status_message     text,
    exchange           text,
    tradingsymbol      text,
    contract           text,
    transaction_type   text,
    order_type         text,
    product            text,
    variety            text,
    quantity           int,
    filled_quantity    int,
    pending_quantity   int,
    price              double precision,
    trigger_price      double precision,
    average_price      double precision,
    tag                text,
    order_timestamp    timestamptz,
    exchange_timestamp timestamptz,
    raw                jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS orders_order_id ON orders (order_id, received_at);
