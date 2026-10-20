-- Compression and retention. Timescale 2.18+ calls compression "columnstore";
-- the compress_* names used here are still accepted as aliases.
ALTER TABLE ticks SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract',
    timescaledb.compress_orderby = 'recv_ts'
);
SELECT add_compression_policy('ticks', INTERVAL '1 day', if_not_exists => TRUE);
SELECT add_retention_policy('ticks', INTERVAL '60 days', if_not_exists => TRUE);

ALTER TABLE bars_1m SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'contract',
    timescaledb.compress_orderby = 'ts, source'
);
SELECT add_compression_policy('bars_1m', INTERVAL '3 days', if_not_exists => TRUE);

ALTER TABLE chain_snapshots SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'underlying, expiry',
    timescaledb.compress_orderby = 'ts, contract, source'
);
SELECT add_compression_policy('chain_snapshots', INTERVAL '3 days', if_not_exists => TRUE);
