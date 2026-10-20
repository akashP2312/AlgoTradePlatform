-- Recorder consumer offsets. Each recorder commits the last JetStream stream
-- sequence it wrote in the same transaction as the rows, so a crash between the
-- commit and the ack replays nothing twice.
CREATE TABLE IF NOT EXISTS recorder_offsets (
    consumer   text PRIMARY KEY,
    stream_seq bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
