-- Reference data: instrument archive, calendars, rules.
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Every raw /instruments dump we archived, with its checksum.
CREATE TABLE IF NOT EXISTS instrument_dumps (
    dump_date  date        NOT NULL,
    fetched_at timestamptz NOT NULL,
    sha256     text        NOT NULL,
    rows       int         NOT NULL,
    path       text        NOT NULL,
    PRIMARY KEY (dump_date, sha256)
);

-- Slowly changing token -> contract map. valid_to is exclusive; NULL means current.
-- Kite reuses tokens after expiry, so nothing long-lived joins on token alone.
CREATE TABLE IF NOT EXISTS instrument_history (
    token          bigint  NOT NULL,
    valid_from     date    NOT NULL,
    valid_to       date,
    contract       text    NOT NULL,
    exchange       text    NOT NULL,
    segment        text    NOT NULL,
    tradingsymbol  text    NOT NULL,
    name           text    NOT NULL,
    underlying     text    NOT NULL,
    expiry         date,
    strike         double precision,
    kind           text    NOT NULL,
    lot_size       int     NOT NULL,
    tick_size      double precision NOT NULL,
    exchange_token bigint  NOT NULL,
    PRIMARY KEY (token, valid_from)
);
CREATE UNIQUE INDEX IF NOT EXISTS instrument_history_open
    ON instrument_history (token) WHERE valid_to IS NULL;
CREATE INDEX IF NOT EXISTS instrument_history_contract
    ON instrument_history (contract, valid_from);
CREATE INDEX IF NOT EXISTS instrument_history_chain
    ON instrument_history (underlying, expiry) WHERE valid_to IS NULL;

-- One row per date and segment group. kind: normal | holiday | weekend | special.
CREATE TABLE IF NOT EXISTS sessions (
    trade_date date NOT NULL,
    segment    text NOT NULL,
    kind       text NOT NULL,
    open_at    time,
    close_at   time,
    note       text,
    PRIMARY KEY (trade_date, segment)
);

-- Dated expiry rules; listed expiries in the instrument dump always win.
CREATE TABLE IF NOT EXISTS expiry_rules (
    underlying text NOT NULL,
    cycle      text NOT NULL,      -- weekly | monthly
    weekday    int  NOT NULL,      -- 0 = Sunday ... 6 = Saturday
    valid_from date NOT NULL,
    valid_to   date,
    note       text,
    PRIMARY KEY (underlying, cycle, valid_from)
);

-- Statutory and broker charges by effective date (seeded in a later phase).
CREATE TABLE IF NOT EXISTS cost_rules (
    effective_from date NOT NULL,
    segment        text NOT NULL,
    item           text NOT NULL,
    side           text NOT NULL DEFAULT 'both',
    rate           double precision NOT NULL,
    basis          text NOT NULL,
    note           text,
    PRIMARY KEY (effective_from, segment, item, side)
);
