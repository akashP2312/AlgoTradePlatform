-- newsmon: every headline it ingested, deduplicated, for backtests and for the
-- feed and deduper to survive a restart. also lists the other sources that
-- carried the same story. lag_ms is seen − published, for headlines that
-- arrived while newsmon ran.
CREATE TABLE IF NOT EXISTS news_items (
    id        text PRIMARY KEY,
    source    text NOT NULL,
    trust     text NOT NULL,
    region    text NOT NULL DEFAULT '',
    title     text NOT NULL,
    summary   text NOT NULL DEFAULT '',
    url       text NOT NULL DEFAULT '',
    guid      text NOT NULL DEFAULT '',
    cats      text[] NOT NULL DEFAULT '{}',
    published timestamptz,
    date_only boolean NOT NULL DEFAULT false,
    seen      timestamptz NOT NULL,
    ts        timestamptz NOT NULL,
    lag_ms    bigint,
    also      text[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS news_items_seen ON news_items (seen DESC);
