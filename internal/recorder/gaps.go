package recorder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/store"
)

// Gap detection compares what was recorded against the session's expected
// minutes and rewrites recorder_gaps for the day. It never touches the 'harvest'
// rows, which the harvester owns.
//
//   - snapshot: a chain series (underlying, expiry) with no 'ws' snapshot in a
//     minute from open to close inclusive. A minute is filled when a 'backup' or
//     'sweep' snapshot exists for it.
//   - ticks: a minute in [open, close) with no tick for the underlying's index.
//     It is filled when the nightly 'kite' bar for that minute exists.

// settle is how long after a minute it is judged, so that a run during the
// session does not report the minute still being written.
const settle = 2 * time.Minute

type Report struct {
	Date    time.Time
	Series  int // series checked
	Gaps    []store.Gap
	Missing int // minutes missing in total
	Open    int // missing and not filled
}

// Gaps checks date and replaces its snapshot and tick gap rows.
func Gaps(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cal *calendar.Calendar, date, now time.Time) (Report, error) {
	date = clock.Date(date)
	rep := Report{Date: date}
	se := cal.Session(date)
	if !se.Trading() {
		return rep, nil
	}
	last := se.Close
	if lim := clock.Minute(now).Add(-settle); lim.Before(last) {
		last = lim
	}
	if last.Before(se.Open) {
		return rep, nil
	}
	snapMinutes := minutes(se.Open, last.Add(time.Minute))
	tickEnd := se.Close
	if lim := clock.Minute(now).Add(-settle + time.Minute); lim.Before(tickEnd) {
		tickEnd = lim
	}
	tickMinutes := minutes(se.Open, tickEnd)

	for _, ul := range cfg.Universe.Underlyings {
		if ul.WideExpiries > 0 {
			exps, err := chainSeries(ctx, pool, ul, date)
			if err != nil {
				return rep, fmt.Errorf("%s expiries: %w", ul.Name, err)
			}
			for _, exp := range exps {
				gs, err := snapshotGaps(ctx, pool, ul.Name, exp, date, snapMinutes)
				if err != nil {
					return rep, fmt.Errorf("%s %s: %w", ul.Name, exp.Format(time.DateOnly), err)
				}
				if err := replaceGaps(ctx, pool, date, "snapshot", ul.Name, &exp, gs); err != nil {
					return rep, err
				}
				rep.add(gs)
			}
		}
		if ul.Index == "" || len(tickMinutes) == 0 {
			continue
		}
		gs, err := tickGaps(ctx, pool, ul, date, tickMinutes)
		if err != nil {
			return rep, fmt.Errorf("%s ticks: %w", ul.Name, err)
		}
		if err := replaceGaps(ctx, pool, date, "ticks", ul.Name, nil, gs); err != nil {
			return rep, err
		}
		rep.add(gs)
	}
	return rep, nil
}

func (r *Report) add(gs []store.Gap) {
	r.Series++
	for _, g := range gs {
		r.Gaps = append(r.Gaps, g)
		r.Missing += g.Minutes
		r.Open += g.Minutes - g.Filled
	}
}

// minutes lists every minute in [from, to).
func minutes(from, to time.Time) []time.Time {
	var out []time.Time
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		out = append(out, m)
	}
	return out
}

// chainSeries is the expiries that should have been snapshotted on date: the
// nearest WideExpiries listed that day, plus any series that has ws rows.
func chainSeries(ctx context.Context, q store.Querier, ul config.Underlying, date time.Time) ([]time.Time, error) {
	d := contractDate(date)
	rows, err := q.Query(ctx, `
		(SELECT DISTINCT expiry FROM instrument_history
		 WHERE underlying = $1 AND exchange = $2 AND kind IN ('CE','PE')
		   AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3) AND expiry >= $3
		 ORDER BY expiry LIMIT $4)
		UNION
		SELECT DISTINCT expiry FROM chain_snapshots
		WHERE underlying = $1 AND source = 'ws' AND ts >= $5 AND ts < $6
		ORDER BY 1`,
		ul.Name, ul.DerivExchange, d, ul.WideExpiries, date, date.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (time.Time, error) {
		var t time.Time
		err := r.Scan(&t)
		return contractDate(t), err
	})
}

// contractDate is a date column value: midnight UTC of the calendar day, which
// is how pgx sends and returns 'date'.
func contractDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

type minuteCover struct{ have, backup, sweep bool }

func snapshotGaps(ctx context.Context, q store.Querier, und string, exp, date time.Time, want []time.Time) ([]store.Gap, error) {
	rows, err := q.Query(ctx, `
		SELECT date_trunc('minute', ts), bool_or(source = 'ws'), bool_or(source = 'backup'), bool_or(source = 'sweep')
		FROM chain_snapshots
		WHERE underlying = $1 AND expiry = $2 AND ts >= $3 AND ts < $4
		GROUP BY 1`,
		und, exp, want[0], want[len(want)-1].Add(time.Minute))
	if err != nil {
		return nil, err
	}
	cover := map[int64]minuteCover{}
	var (
		ts            time.Time
		ws, bk, sweep bool
	)
	_, err = pgx.ForEachRow(rows, []any{&ts, &ws, &bk, &sweep}, func() error {
		cover[ts.UnixMilli()] = minuteCover{ws, bk, sweep}
		return nil
	})
	if err != nil {
		return nil, err
	}
	gs := runs(want, func(m time.Time) (missing, filled bool, by string) {
		c := cover[m.UnixMilli()]
		switch {
		case c.have:
			return false, false, ""
		case c.backup:
			return true, true, "backup"
		case c.sweep:
			return true, true, "sweep"
		}
		return true, false, ""
	})
	e := exp
	for i := range gs {
		gs[i].TradeDate, gs[i].Kind, gs[i].Underlying, gs[i].Expiry = contractDate(date), "snapshot", und, &e
	}
	return gs, nil
}

func tickGaps(ctx context.Context, q store.Querier, ul config.Underlying, date time.Time, want []time.Time) ([]store.Gap, error) {
	exch, sym, ok := strings.Cut(ul.Index, ":")
	if !ok {
		return nil, fmt.Errorf("index %q is not EXCHANGE:SYMBOL", ul.Index)
	}
	var contract string
	err := q.QueryRow(ctx, `SELECT contract FROM instrument_history
		WHERE exchange = $1 AND tradingsymbol = $2 AND valid_from <= $3 AND (valid_to IS NULL OR valid_to > $3)
		LIMIT 1`, exch, sym, contractDate(date)).Scan(&contract)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("index %q not in instrument_history on %s", ul.Index, date.Format(time.DateOnly))
	}
	if err != nil {
		return nil, err
	}
	from, to := want[0], want[len(want)-1].Add(time.Minute)
	have := map[int64]bool{}
	rows, err := q.Query(ctx, `SELECT DISTINCT date_trunc('minute', recv_ts) FROM ticks
		WHERE contract = $1 AND recv_ts >= $2 AND recv_ts < $3`, contract, from, to)
	if err != nil {
		return nil, err
	}
	var ts time.Time
	if _, err := pgx.ForEachRow(rows, []any{&ts}, func() error { have[ts.UnixMilli()] = true; return nil }); err != nil {
		return nil, err
	}
	harvested := map[int64]bool{}
	rows, err = q.Query(ctx, `SELECT ts FROM bars_1m
		WHERE contract = $1 AND source = 'kite' AND ts >= $2 AND ts < $3`, contract, from, to)
	if err != nil {
		return nil, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&ts}, func() error { harvested[ts.UnixMilli()] = true; return nil }); err != nil {
		return nil, err
	}
	gs := runs(want, func(m time.Time) (bool, bool, string) {
		k := m.UnixMilli()
		if have[k] {
			return false, false, ""
		}
		return true, harvested[k], "harvest"
	})
	for i := range gs {
		gs[i].TradeDate, gs[i].Kind, gs[i].Underlying, gs[i].Detail = contractDate(date), "ticks", ul.Name, contract
	}
	return gs, nil
}

// runs groups consecutive missing minutes into half-open [Start, End) gaps.
// FilledBy names the best source seen in the run ("backup" beats "sweep").
func runs(want []time.Time, check func(time.Time) (missing, filled bool, by string)) []store.Gap {
	var out []store.Gap
	var cur *store.Gap
	for _, m := range want {
		missing, filled, by := check(m)
		if !missing {
			cur = nil
			continue
		}
		if cur == nil || !cur.End.Equal(m) {
			out = append(out, store.Gap{Start: m, End: m})
			cur = &out[len(out)-1]
		}
		cur.End = m.Add(time.Minute)
		cur.Minutes++
		if filled {
			cur.Filled++
			if cur.FilledBy == "" || by == "backup" {
				cur.FilledBy = by
			}
		}
	}
	return out
}

// replaceGaps swaps one series' gap rows for gs in a single transaction.
func replaceGaps(ctx context.Context, pool *pgxpool.Pool, date time.Time, kind, und string, exp *time.Time, gs []store.Gap) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM recorder_gaps
			WHERE trade_date = $1 AND kind = $2 AND underlying = $3 AND expiry IS NOT DISTINCT FROM $4`,
			contractDate(date), kind, und, exp); err != nil {
			return fmt.Errorf("clear %s gaps: %w", kind, err)
		}
		for _, g := range gs {
			if err := store.UpsertGap(ctx, tx, g); err != nil {
				return fmt.Errorf("write %s gap: %w", kind, err)
			}
		}
		return nil
	})
}
