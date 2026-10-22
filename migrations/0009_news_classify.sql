-- newsmon: each headline's classification. impact, tags and rules are what the
-- rules file said when the headline arrived; llm is the optional model rescore
-- ({"impact","why","model"}). confirmed is when the headline began to count
-- toward the risk state: when a trusted source published it, or when enough
-- distinct sources carried it. backfill marks a source's first poll.
ALTER TABLE news_items
    ADD COLUMN IF NOT EXISTS impact    smallint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS tags      text[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS rules     text[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS backfill  boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS confirmed timestamptz,
    ADD COLUMN IF NOT EXISTS llm       jsonb;
CREATE INDEX IF NOT EXISTS news_items_impact ON news_items (impact DESC, seen DESC) WHERE impact > 0;
