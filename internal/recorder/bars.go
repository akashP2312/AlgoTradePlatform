package recorder

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

// Bars folds ticks into live 1-minute bars (bars_1m source 'live'), bucketed by
// the minute the tick was received in.
//
// Bar volume is the rise in Kite's cumulative day volume. A contract's first
// tick after a restart only sets the baseline (except in the opening minute,
// where the whole day volume so far belongs to the 09:15 bar), so a restarted
// recorder under-counts that minute. The nightly 'kite' bars stay the reference;
// live bars exist so that today's charts work before the harvest.
type Bars struct {
	Open func(day time.Time) time.Time // session open; nil or zero means 09:15

	day, open time.Time
	last      map[string]*series
	bars      map[barKey]*bar
	dirty     map[barKey]struct{}
	maxTS     int64
}

type barKey struct {
	c  string
	ts int64 // minute, epoch ms
}

type bar struct {
	o, h, l, c float64
	v          int64
	oi         int64
	hasOI      bool
}

type series struct {
	vol uint64 // last cumulative volume
	ltt int64  // last trade time seen, epoch ms
}

// keepBars is how far behind the newest minute a bar stays in memory, so that
// a late tick still lands in the bar already written.
const keepBars = 5 * time.Minute

func NewBars(open func(day time.Time) time.Time) *Bars {
	return &Bars{
		Open:  open,
		last:  map[string]*series{},
		bars:  map[barKey]*bar{},
		dirty: map[barKey]struct{}{},
	}
}

func (b *Bars) Add(t *bus.Tick) {
	recv := bus.FromMs(t.Recv)
	if day := clock.Date(recv); !day.Equal(b.day) {
		b.day = day
		b.open = time.Time{}
		if b.Open != nil {
			b.open = b.Open(day)
		}
		if b.open.IsZero() {
			b.open = clock.At(day, 9, 15, 0)
		}
		clear(b.last)
	}
	s := b.last[t.C]
	switch t.Phase {
	case "p":
		// Seen in pre-open: the first continuous tick's whole volume, pre-open
		// matching included, goes to the opening bar.
		if s == nil {
			b.last[t.C] = &series{}
		}
		return
	case "c", "x":
	default:
		return
	}
	if !(t.LTP > 0) || !finite(t.LTP) {
		return
	}
	minute := clock.Minute(recv)
	var delta uint64
	prints := false
	switch {
	case strings.HasSuffix(t.C, "|IDX"):
		prints = true
	case s == nil:
		s = &series{vol: t.Vol}
		b.last[t.C] = s
		if t.Phase == "c" && !recv.After(b.open.Add(time.Minute)) {
			delta, prints = t.Vol, true
		} else {
			prints = t.LTT != 0 && clock.Minute(bus.FromMs(t.LTT)).Equal(minute)
		}
	case t.Vol > s.vol:
		delta, prints = t.Vol-s.vol, true
		s.vol = t.Vol
	case t.Vol < s.vol:
		// A reset upstream; take the new level as the baseline.
		s.vol = t.Vol
		prints = t.LTT != 0 && t.LTT != s.ltt
	default:
		prints = t.LTT != 0 && t.LTT != s.ltt
	}
	if s != nil && t.LTT != 0 {
		s.ltt = t.LTT
	}

	k := barKey{t.C, minute.UnixMilli()}
	br := b.bars[k]
	if !prints {
		// An open-interest change without a trade still updates the bar.
		if br != nil && t.OI != 0 && t.OI <= math.MaxInt64 && (!br.hasOI || br.oi != int64(t.OI)) {
			br.oi, br.hasOI = int64(t.OI), true
			b.dirty[k] = struct{}{}
		}
		return
	}
	if br == nil {
		br = &bar{o: t.LTP, h: t.LTP, l: t.LTP}
		b.bars[k] = br
	}
	br.h = max(br.h, t.LTP)
	br.l = min(br.l, t.LTP)
	br.c = t.LTP
	br.v = int64(min(uint64(br.v)+min(delta, math.MaxInt64), math.MaxInt64))
	if t.OI != 0 && t.OI <= math.MaxInt64 {
		br.oi, br.hasOI = int64(t.OI), true
	}
	b.dirty[k] = struct{}{}
	b.maxTS = max(b.maxTS, k.ts)
}

// Pending is the number of bars changed since the last commit.
func (b *Bars) Pending() int { return len(b.dirty) }

// The in-memory bar holds the whole minute, so a rewrite replaces close and
// raises volume; GREATEST/LEAST keep a restarted recorder from shrinking a bar
// the previous process wrote.
const barsUpsert = `INSERT INTO bars_1m (ts, contract, source, open, high, low, close, volume, oi)
SELECT u.ts, u.c, 'live', u.o, u.h, u.l, u.cl, u.v, u.oi
FROM unnest($1::timestamptz[], $2::text[], $3::float8[], $4::float8[], $5::float8[], $6::float8[],
            $7::int8[], $8::int8[]) AS u(ts, c, o, h, l, cl, v, oi)
ON CONFLICT (contract, ts, source) DO UPDATE SET
    high   = GREATEST(bars_1m.high, EXCLUDED.high),
    low    = LEAST(bars_1m.low, EXCLUDED.low),
    close  = EXCLUDED.close,
    volume = GREATEST(bars_1m.volume, EXCLUDED.volume),
    oi     = COALESCE(EXCLUDED.oi, bars_1m.oi)`

// Flush writes every changed bar inside tx.
func (b *Bars) Flush(ctx context.Context, tx pgx.Tx) (int, error) {
	n := len(b.dirty)
	if n == 0 {
		return 0, nil
	}
	ts := make([]time.Time, 0, n)
	cs := make([]string, 0, n)
	o, h, l, c := make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n), make([]float64, 0, n)
	v := make([]int64, 0, n)
	oi := make([]*int64, 0, n)
	for k := range b.dirty {
		br := b.bars[k]
		if br == nil {
			continue
		}
		ts = append(ts, time.UnixMilli(k.ts).UTC())
		cs = append(cs, k.c)
		o, h, l, c = append(o, br.o), append(h, br.h), append(l, br.l), append(c, br.c)
		v = append(v, br.v)
		if br.hasOI {
			x := br.oi
			oi = append(oi, &x)
		} else {
			oi = append(oi, nil)
		}
	}
	tag, err := tx.Exec(ctx, barsUpsert, ts, cs, o, h, l, c, v, oi)
	if err != nil {
		return 0, fmt.Errorf("upsert bars: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Committed forgets the written changes and drops bars too old to be touched.
func (b *Bars) Committed() {
	clear(b.dirty)
	cut := b.maxTS - keepBars.Milliseconds()
	for k := range b.bars {
		if k.ts < cut {
			delete(b.bars, k)
		}
	}
}
