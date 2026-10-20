package recorder

import (
	"math"
	"time"

	"pmt/internal/bus"
)

// Column value helpers. Every value is checked here so that pgx never fails to
// encode a buffered row: an encode error is not a Postgres error, so it would
// not be recognised as a bad batch and the flush would retry forever.

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// f64 is a price that Kite sends as 0 when there is none.
func f64(f float64) any {
	if f == 0 || !finite(f) {
		return nil
	}
	return f
}

// u64 is a quantity that Kite sends as 0 when it does not apply.
func u64(u uint64) any {
	if u == 0 || u > math.MaxInt64 {
		return nil
	}
	return int64(u)
}

// n64 is a count where 0 is a real value (open interest, volume).
func n64(u uint64) any {
	if u > math.MaxInt64 {
		return nil
	}
	return int64(u)
}

// i32 fits a value into an int column.
func i32(v int64) any {
	if v < math.MinInt32 || v > math.MaxInt32 {
		return nil
	}
	return int32(v)
}

// age is a staleness in ms; negative means unknown, and the column is an int.
func age(ms int64) any {
	if ms < 0 {
		return nil
	}
	return int32(min(ms, math.MaxInt32))
}

func tok(t uint32) any {
	if t == 0 {
		return nil
	}
	return int64(t)
}

// msTime is an epoch-ms stamp, 0 meaning none.
func msTime(ms int64) any {
	if ms == 0 {
		return nil
	}
	return bus.FromMs(ms)
}

func optTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
