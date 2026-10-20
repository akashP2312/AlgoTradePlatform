// Package recorder drains the TICKS, CHAIN and ORDERS streams into Postgres.
//
// Each stream has a durable pull consumer. Rows are buffered and written in one
// transaction together with the stream sequence they cover (recorder_offsets),
// and the JetStream ack follows the commit. A crash between commit and ack only
// causes a redelivery that the stored offset filters out, so nothing is written
// twice and nothing acked is lost.
package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/config"
	"pmt/internal/store"
)

// Consumer names. They are also the recorder_offsets keys.
const (
	ConsumerTicks  = "recorder-ticks"
	ConsumerChain  = "recorder-chain"
	ConsumerOrders = "recorder-orders"
)

const (
	minBackoff   = time.Second
	maxBackoff   = 30 * time.Second
	ackWait      = 2 * time.Minute
	finalTimeout = 5 * time.Second
)

// Counters are per-stream totals since start, safe to read while running.
type Counters struct {
	Msgs      atomic.Int64 // messages fetched, including skipped ones
	Rows      atomic.Int64 // rows written (ticks and bars, chain rows, orders)
	Bad       atomic.Int64 // payloads that failed to decode or validate
	Skipped   atomic.Int64 // redeliveries at or below the committed offset
	Flushes   atomic.Int64
	Errors    atomic.Int64 // failed flushes, retried
	Dropped   atomic.Int64 // buffered rows dropped after a data error
	LastFlush atomic.Int64 // unix ms of the last successful commit
	LastSeq   atomic.Int64 // last committed stream sequence
}

// Detail renders the counters for a health message.
func (c *Counters) Detail(prefix string) map[string]string {
	d := map[string]string{}
	put := func(k string, v int64) { d[prefix+k] = strconv.FormatInt(v, 10) }
	put("msgs", c.Msgs.Load())
	put("rows", c.Rows.Load())
	put("bad", c.Bad.Load())
	put("skipped", c.Skipped.Load())
	put("flushes", c.Flushes.Load())
	put("errors", c.Errors.Load())
	put("dropped", c.Dropped.Load())
	put("last_flush", c.LastFlush.Load())
	put("last_seq", c.LastSeq.Load())
	return d
}

// sink buffers decoded messages for one table family and writes them in a
// caller-owned transaction.
type sink interface {
	add(data []byte, md *jetstream.MsgMetadata) error
	size() int     // buffered rows
	fetchMax() int // messages per fetch
	flush(ctx context.Context, tx pgx.Tx) (int, error)
	reset() // after a commit, or after the buffer is dropped
}

// Recorder runs the three stream consumers.
type Recorder struct {
	Pool *pgxpool.Pool
	JS   jetstream.JetStream
	Cfg  config.Recorder
	// Open returns the session open for a trading day; nil means 09:15 IST.
	// The live bar builder uses it to decide whether a contract's first tick
	// carries the whole day's volume.
	Open func(day time.Time) time.Time

	Ticks, Chain, Orders Counters
}

// Detail merges all counters for a health message.
func (r *Recorder) Detail() map[string]string {
	d := r.Ticks.Detail("ticks_")
	for k, v := range r.Chain.Detail("chain_") {
		d[k] = v
	}
	for k, v := range r.Orders.Detail("orders_") {
		d[k] = v
	}
	return d
}

type stream struct {
	name     string
	consumer string
	sink     sink
	c        *Counters
}

// Run consumes until ctx ends, then makes a last flush of each buffer. It
// returns early only when a consumer cannot be set up.
func (r *Recorder) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	streams := []*stream{
		{bus.StreamTicks, ConsumerTicks, newTicksSink(r.batch(), r.Open), &r.Ticks},
		{bus.StreamChain, ConsumerChain, &chainSink{}, &r.Chain},
		{bus.StreamOrders, ConsumerOrders, &ordersSink{}, &r.Orders},
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for _, s := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.drain(ctx, s); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}
	wg.Wait()
	return first
}

func (r *Recorder) batch() int {
	if r.Cfg.BatchRows > 0 {
		return r.Cfg.BatchRows
	}
	return 5000
}

func (r *Recorder) every() time.Duration {
	if r.Cfg.FlushEvery > 0 {
		return r.Cfg.FlushEvery
	}
	return time.Second
}

// state is one consumer's position. committed is what Postgres holds; hi is
// the highest sequence buffered or skipped past since then.
type state struct {
	committed uint64
	hi        uint64
	last      jetstream.Msg // message carrying hi, acked (AckAll) after the commit
	pending   int           // messages past committed, buffered or not
	deadline  time.Time
	backoff   time.Duration
}

func (r *Recorder) drain(ctx context.Context, s *stream) error {
	cons, err := r.JS.CreateOrUpdateConsumer(ctx, s.name, jetstream.ConsumerConfig{
		Durable:       s.consumer,
		AckPolicy:     jetstream.AckAllPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MaxAckPending: 4 * r.batch(),
		AckWait:       ackWait,
	})
	if err != nil {
		return fmt.Errorf("recorder: consumer %s: %w", s.consumer, err)
	}
	committed, err := LoadOffset(ctx, r.Pool, s.consumer)
	if err != nil {
		return fmt.Errorf("recorder: offset %s: %w", s.consumer, err)
	}
	if info, err := r.JS.Stream(ctx, s.name); err == nil {
		st := info.CachedInfo().State
		switch {
		case st.LastSeq < committed:
			slog.Warn("recorder: stream is behind the stored offset; it was recreated, starting over",
				"stream", s.name, "offset", committed, "last_seq", st.LastSeq)
			committed = 0
		case st.FirstSeq > committed+1 && committed > 0:
			slog.Warn("recorder: messages expired from the stream before they were recorded",
				"stream", s.name, "offset", committed, "first_seq", st.FirstSeq, "lost", st.FirstSeq-committed-1)
		}
	}
	s.c.LastSeq.Store(int64(committed))
	slog.Info("recorder: consuming", "stream", s.name, "consumer", s.consumer, "offset", committed)

	st := &state{committed: committed, hi: committed}
	for {
		if ctx.Err() != nil {
			fctx, cancel := context.WithTimeout(context.Background(), finalTimeout)
			err := r.flush(fctx, s, st)
			cancel()
			if err != nil {
				slog.Warn("recorder: final flush failed; the stream will redeliver", "stream", s.name, "err", err)
			}
			return nil
		}
		if st.pending > 0 && (s.sink.size() >= r.batch() || !time.Now().Before(st.deadline)) {
			if err := r.flush(ctx, s, st); err != nil {
				// Keep the buffer and stop fetching until Postgres is back.
				select {
				case <-ctx.Done():
				case <-time.After(st.backoff):
				}
				continue
			}
		}
		room := r.batch() - s.sink.size()
		if room <= 0 {
			continue
		}
		wait := time.Second
		if st.pending > 0 {
			wait = max(time.Until(st.deadline), 100*time.Millisecond)
		}
		msgs, err := cons.Fetch(min(room, s.sink.fetchMax()), jetstream.FetchMaxWait(wait))
		if err != nil {
			if ctx.Err() == nil && !quiet(err) {
				slog.Warn("recorder: fetch", "stream", s.name, "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
			continue
		}
		for m := range msgs.Messages() {
			r.handle(s, st, m)
		}
		if err := msgs.Error(); err != nil && ctx.Err() == nil && !quiet(err) {
			slog.Warn("recorder: fetch batch", "stream", s.name, "err", err)
		}
	}
}

func quiet(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, jetstream.ErrNoMessages) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (r *Recorder) handle(s *stream, st *state, m jetstream.Msg) {
	s.c.Msgs.Add(1)
	md, err := m.Metadata()
	if err != nil {
		s.c.Bad.Add(1)
		slog.Warn("recorder: message without metadata", "stream", s.name, "err", err)
		return
	}
	seq := md.Sequence.Stream
	if seq <= st.hi {
		s.c.Skipped.Add(1)
		switch {
		case seq <= st.committed:
			_ = m.Ack()
		case seq == st.hi:
			st.last = m // a redelivery of the newest buffered message; ack this copy
		}
		return
	}
	if err := s.sink.add(m.Data(), md); err != nil {
		if n := s.c.Bad.Add(1); n <= 10 || n%1000 == 0 {
			slog.Warn("recorder: bad payload skipped", "stream", s.name, "seq", seq, "bad_total", n, "err", err)
		}
	}
	// Bad payloads still move the offset, so they are not fetched again.
	st.hi, st.last = seq, m
	if st.pending == 0 {
		st.deadline = time.Now().Add(r.every())
	}
	st.pending++
}

// flush writes the buffer and the new offset in one transaction, then acks.
func (r *Recorder) flush(ctx context.Context, s *stream, st *state) error {
	if st.pending == 0 {
		return nil
	}
	var rows int
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		n, err := s.sink.flush(ctx, tx)
		if err != nil {
			return err
		}
		rows = n
		return SaveOffset(ctx, tx, s.consumer, st.hi)
	})
	if err != nil && dataError(err) {
		// The batch holds a row Postgres will never accept. Retrying would
		// wedge the stream, so drop it, keep the offset moving and say so
		// loudly; the gap report shows the minutes that went missing.
		n := s.sink.size()
		slog.Error("recorder: batch rejected by Postgres; dropped", "stream", s.name,
			"rows", n, "through_seq", st.hi, "err", err)
		if oerr := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
			return SaveOffset(ctx, tx, s.consumer, st.hi)
		}); oerr != nil {
			err = oerr
		} else {
			s.c.Dropped.Add(int64(n))
			s.sink.reset()
			r.committed(s, st, 0)
			return nil
		}
	}
	if err != nil {
		s.c.Errors.Add(1)
		st.backoff = min(max(2*st.backoff, minBackoff), maxBackoff)
		slog.Warn("recorder: flush failed; keeping the buffer", "stream", s.name,
			"rows", s.sink.size(), "retry_in", st.backoff, "err", err)
		return err
	}
	s.sink.reset()
	r.committed(s, st, rows)
	return nil
}

func (r *Recorder) committed(s *stream, st *state, rows int) {
	st.committed = st.hi
	if st.last != nil {
		// AckAll covers every earlier message. A lost ack only means a
		// redelivery that the stored offset skips.
		if err := st.last.Ack(); err != nil {
			slog.Debug("recorder: ack", "stream", s.name, "err", err)
		}
	}
	st.last, st.pending, st.backoff = nil, 0, 0
	s.c.Rows.Add(int64(rows))
	s.c.Flushes.Add(1)
	s.c.LastFlush.Store(time.Now().UnixMilli())
	s.c.LastSeq.Store(int64(st.committed))
}

// dataError reports Postgres errors caused by the rows themselves (class 22
// data exception, class 23 integrity violation), which no retry can fix.
func dataError(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return false
	}
	return strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23")
}

// LoadOffset returns the last committed stream sequence for a consumer, or 0.
func LoadOffset(ctx context.Context, q store.Querier, consumer string) (uint64, error) {
	var seq int64
	err := q.QueryRow(ctx, `SELECT stream_seq FROM recorder_offsets WHERE consumer = $1`, consumer).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return uint64(seq), err
}

// SaveOffset stores a consumer's committed stream sequence.
func SaveOffset(ctx context.Context, q store.Querier, consumer string, seq uint64) error {
	_, err := q.Exec(ctx, `INSERT INTO recorder_offsets (consumer, stream_seq, updated_at) VALUES ($1, $2, now())
		ON CONFLICT (consumer) DO UPDATE SET stream_seq = EXCLUDED.stream_seq, updated_at = now()`,
		consumer, int64(seq))
	return err
}
