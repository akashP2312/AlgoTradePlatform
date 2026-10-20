// Package harvest pulls Kite's minute candles (with OI for derivatives) after the
// close and writes them to bars_1m as source 'kite', which replaces the live bars
// built from ticks. harvest_log keeps one row per contract per trade date, so both
// the nightly run and the day-one backfill resume where they stopped.
//
// Only contracts in today's instrument dump are harvested: Kite tokens are reused
// after expiry, and whether Kite serves history for expired contracts is to verify.
package harvest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
	"pmt/internal/store"
)

// Historian is the slice of the Kite REST client the harvester needs.
type Historian interface {
	Historical(ctx context.Context, token uint32, interval string, from, to time.Time, oi bool) ([]kite.Candle, error)
}

// Groups for targets that are not an underlying's chain.
const (
	GroupExtras       = "extras"
	GroupConstituents = "constituents"
)

// Target is one contract to harvest. Group is the underlying name, or one of the
// Group constants; failures are reported as one recorder_gaps row per group.
type Target struct {
	refdata.Inst
	Group string
}

func isDeriv(k contract.Kind) bool { return k == contract.FUT || k.IsOption() }

// Targets lists what to harvest on date: for every underlying with harvest set, the
// index, its live futures and every live option of the name; then the extras and
// the index constituents. Tokens are de-duplicated. Non-options come first, then
// options by expiry, strike and CE before PE, so a run cut short still has the near
// expiries.
func Targets(u config.Universe, insts []refdata.Inst, constituents []string, date time.Time) ([]Target, []string) {
	var out []Target
	var warn []string
	seen := map[uint32]bool{}
	add := func(in refdata.Inst, group string) {
		if seen[in.Token] {
			return
		}
		seen[in.Token] = true
		out = append(out, Target{Inst: in, Group: group})
	}
	live := refdata.Live(insts, date)
	for _, ul := range u.Underlyings {
		if !ul.Harvest {
			continue
		}
		exch, sym, _ := strings.Cut(ul.Index, ":")
		if in, ok := refdata.BySymbol(insts, exch, sym); ok {
			add(in, ul.Name)
		} else {
			warn = append(warn, fmt.Sprintf("%s: index %q not in instruments", ul.Name, ul.Index))
		}
		for _, f := range refdata.Futures(insts, ul.DerivExchange, ul.Name, date) {
			add(f, ul.Name)
		}
		n := 0
		for _, in := range live {
			if in.Key.Kind.IsOption() && in.Exchange == ul.DerivExchange && in.Key.Name == ul.Name {
				add(in, ul.Name)
				n++
			}
		}
		if n == 0 {
			warn = append(warn, fmt.Sprintf("%s: no live options on %s", ul.Name, ul.DerivExchange))
		}
	}
	for _, spec := range u.Extras {
		in, err := refdata.ResolveExtra(insts, spec, date)
		if err != nil {
			warn = append(warn, err.Error())
			continue
		}
		add(in, GroupExtras)
	}
	for _, sym := range constituents {
		if in, ok := refdata.BySymbol(insts, "NSE", sym); ok {
			add(in, GroupConstituents)
		} else {
			warn = append(warn, fmt.Sprintf("constituent %s not in NSE instruments", sym))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Key, out[j].Key
		if a.Kind.IsOption() != b.Kind.IsOption() {
			return !a.Kind.IsOption()
		}
		if !a.Kind.IsOption() {
			return false
		}
		if !a.Expiry.Equal(b.Expiry) {
			return a.Expiry.Before(b.Expiry)
		}
		if a.Strike != b.Strike {
			return a.Strike < b.Strike
		}
		return a.Kind == contract.CE && b.Kind == contract.PE
	})
	return out, warn
}

// Window is one backfill request span: calendar days From..To (IST dates) and the
// trading days inside them.
type Window struct {
	From, To time.Time
	Days     []time.Time
}

// Windows splits the lookbackDays calendar days ending on end into blocks of at
// most windowDays calendar days, newest first.
func Windows(cal *calendar.Calendar, end time.Time, lookbackDays, windowDays int) []Window {
	if lookbackDays < 1 || windowDays < 1 {
		return nil
	}
	end = clock.Date(end)
	first := end.AddDate(0, 0, -(lookbackDays - 1))
	var out []Window
	for to := end; !to.Before(first); {
		from := to.AddDate(0, 0, -(windowDays - 1))
		if from.Before(first) {
			from = first
		}
		out = append(out, Window{From: from, To: to, Days: cal.TradingDays(from, to)})
		to = from.AddDate(0, 0, -1)
	}
	return out
}

// Stats summarises one run. OK and Empty count contract-days; Failed counts
// contracts that still failed after a retry. Skipped counts contracts already
// harvested for the day in Day, and windows already covered in Backfill.
type Stats struct {
	Targets, Skipped, Requests int
	OK, Empty, Failed          int
	Rows                       int64
	EOD                        int
	Took                       time.Duration
}

func (s Stats) String() string {
	return fmt.Sprintf("targets=%d skipped=%d requests=%d ok=%d empty=%d failed=%d rows=%d eod=%d took=%s",
		s.Targets, s.Skipped, s.Requests, s.OK, s.Empty, s.Failed, s.Rows, s.EOD, s.Took.Round(time.Second))
}

// Harvester writes harvested candles. Kite's rate limits are enforced by the REST
// client's guard, so Workers only bounds concurrency.
type Harvester struct {
	Pool    *pgxpool.Pool
	Hist    Historian
	Cal     *calendar.Calendar
	Workers int // default 4
	// Late lists exchanges still trading when the harvest runs (MCX until 23:30 or
	// 23:55, NSE IX almost round the clock; verify both). Their nightly run also
	// refetches the previous trading day, which completes yesterday's partial pull.
	// nil means DefaultLate.
	Late       map[string]bool
	Force      bool          // refetch days harvest_log already has as ok or empty
	RetryDelay time.Duration // before the one retry of a failed request; default 3s
}

var DefaultLate = map[string]bool{"MCX": true, "NSEIX": true}

func (h *Harvester) workers() int {
	if h.Workers > 0 {
		return h.Workers
	}
	return 4
}

func (h *Harvester) late(exch string) bool {
	if h.Late == nil {
		return DefaultLate[exch]
	}
	return h.Late[exch]
}

func (h *Harvester) retryDelay() time.Duration {
	if h.RetryDelay > 0 {
		return h.RetryDelay
	}
	return 3 * time.Second
}

// tally collects stats and per-group outcomes from the workers.
type tally struct {
	mu     sync.Mutex
	s      Stats
	total  map[string]int
	done   map[string]int
	failed map[string][]string
}

func newTally(targets []Target) *tally {
	t := &tally{total: map[string]int{}, done: map[string]int{}, failed: map[string][]string{}}
	t.s.Targets = len(targets)
	for _, tg := range targets {
		t.total[tg.Group]++
	}
	return t
}

func (t *tally) add(fn func(*Stats)) {
	t.mu.Lock()
	fn(&t.s)
	t.mu.Unlock()
}

func (t *tally) finish(tg Target, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if failed {
		t.s.Failed++
		t.failed[tg.Group] = append(t.failed[tg.Group], tg.Contract)
		return
	}
	t.done[tg.Group]++
}

// fatal errors stop the whole run: an expired or missing token, or cancellation.
func fatal(ctx context.Context, err error) bool {
	return kite.IsTokenError(err) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

// run feeds targets to the workers. fn returns an error only when the run must stop.
func (h *Harvester) run(ctx context.Context, targets []Target, fn func(context.Context, Target) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ch := make(chan Target)
	var wg sync.WaitGroup
	var mu sync.Mutex
	finished := 0
	for i := 0; i < h.workers(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				if err := fn(ctx, t); err != nil {
					cancel(err)
					continue
				}
				mu.Lock()
				finished++
				if finished%500 == 0 {
					slog.Info("harvest: progress", "done", finished, "of", len(targets))
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, t := range targets {
		select {
		case ch <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// candles fetches one span, retrying once unless the error is fatal.
func (h *Harvester) candles(ctx context.Context, t Target, interval string, from, to time.Time, tl *tally) ([]kite.Candle, error) {
	oi := interval == "minute" && isDeriv(t.Key.Kind)
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(h.retryDelay()):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		tl.add(func(s *Stats) { s.Requests++ })
		var cs []kite.Candle
		if cs, err = h.Hist.Historical(ctx, t.Token, interval, from, to, oi); err == nil {
			return cs, nil
		}
		if fatal(ctx, err) {
			return nil, err
		}
	}
	return nil, err
}

func dayKey(t time.Time) string { return clock.Date(t).Format("2006-01-02") }

// countByDay counts candles per IST trade date.
func countByDay(cs []kite.Candle) map[string]int {
	out := map[string]int{}
	for _, c := range cs {
		out[dayKey(c.TS)]++
	}
	return out
}

// dedupe keeps the last candle per timestamp; one INSERT ... ON CONFLICT DO UPDATE
// cannot touch the same row twice.
func dedupe(cs []kite.Candle) []kite.Candle {
	idx := make(map[int64]int, len(cs))
	out := cs[:0:0]
	for _, c := range cs {
		k := c.TS.UnixNano()
		if i, ok := idx[k]; ok {
			out[i] = c
			continue
		}
		idx[k] = len(out)
		out = append(out, c)
	}
	return out
}

const insertBars = `INSERT INTO bars_1m (ts, contract, source, open, high, low, close, volume, oi)
	SELECT u.ts, $1, 'kite', u.o, u.h, u.l, u.c, u.v, u.oi
	FROM unnest($2::timestamptz[], $3::float8[], $4::float8[], $5::float8[], $6::float8[], $7::int8[], $8::int8[])
		AS u(ts, o, h, l, c, v, oi)
	ON CONFLICT (contract, ts, source) DO UPDATE SET open = EXCLUDED.open, high = EXCLUDED.high,
		low = EXCLUDED.low, close = EXCLUDED.close, volume = EXCLUDED.volume, oi = EXCLUDED.oi`

const upsertLog = `INSERT INTO harvest_log (trade_date, contract, token, status, rows, fetched_at, error)
	SELECT u.d, $1, $2, u.s, u.n, now(), NULLIF($6, '')
	FROM unnest($3::date[], $4::text[], $5::int4[]) AS u(d, s, n)
	ON CONFLICT (trade_date, contract) DO UPDATE SET token = EXCLUDED.token, status = EXCLUDED.status,
		rows = EXCLUDED.rows, fetched_at = EXCLUDED.fetched_at, error = EXCLUDED.error`

// A failed refetch must not hide data an earlier run stored.
const upsertLogError = upsertLog + ` WHERE harvest_log.status = 'error'`

// harvest fetches minute candles for t over the calendar days from..to and writes
// them, with a harvest_log row for each of days and for any other date that came
// back with candles. It returns the candle count and whether the contract failed;
// err is set only when the run must stop.
func (h *Harvester) harvest(ctx context.Context, t Target, from, to time.Time, days []time.Time, tl *tally) (int, bool, error) {
	start, end := clock.Date(from), clock.At(clock.Date(to), 23, 59, 59)
	cs, ferr := h.candles(ctx, t, "minute", start, end, tl)
	if ferr != nil {
		if fatal(ctx, ferr) {
			return 0, false, ferr
		}
		slog.Warn("harvest: fetch failed", "contract", t.Contract, "err", kite.Describe(ferr))
		ds := make([]time.Time, len(days))
		st := make([]string, len(days))
		n := make([]int32, len(days))
		for i, d := range days {
			ds[i], st[i] = d, "error"
		}
		if _, err := h.Pool.Exec(ctx, upsertLogError, t.Contract, int64(t.Token), ds, st, n, kite.Describe(ferr)); err != nil {
			return 0, false, fmt.Errorf("harvest: log %s: %w", t.Contract, err)
		}
		return 0, true, nil
	}
	cs = dedupe(cs)
	per := countByDay(cs)
	logDays := map[string]time.Time{}
	for _, d := range days {
		logDays[dayKey(d)] = clock.Date(d)
	}
	for _, c := range cs {
		if k := dayKey(c.TS); logDays[k].IsZero() {
			logDays[k] = clock.Date(c.TS)
		}
	}
	var ds []time.Time
	var st []string
	var nr []int32
	ok, empty := 0, 0
	for k, d := range logDays {
		ds = append(ds, d)
		if n := per[k]; n > 0 {
			st, nr = append(st, "ok"), append(nr, int32(n))
			ok++
		} else {
			st, nr = append(st, "empty"), append(nr, 0)
			empty++
		}
	}
	err := pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		if len(cs) > 0 {
			ts := make([]time.Time, len(cs))
			o, hi, lo, c := make([]float64, len(cs)), make([]float64, len(cs)), make([]float64, len(cs)), make([]float64, len(cs))
			v := make([]int64, len(cs))
			oi := make([]*int64, len(cs))
			deriv := isDeriv(t.Key.Kind)
			for i, x := range cs {
				ts[i], o[i], hi[i], lo[i], c[i], v[i] = x.TS, x.O, x.H, x.L, x.C, x.V
				if deriv {
					oi[i] = &cs[i].OI
				}
			}
			if _, err := tx.Exec(ctx, insertBars, t.Contract, ts, o, hi, lo, c, v, oi); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, upsertLog, t.Contract, int64(t.Token), ds, st, nr, "")
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("harvest: write %s: %w", t.Contract, err)
	}
	tl.add(func(s *Stats) { s.OK += ok; s.Empty += empty; s.Rows += int64(len(cs)) })
	if t.Key.Kind == contract.IDX {
		if err := h.eod(ctx, t, start, end, tl); err != nil {
			if fatal(ctx, err) {
				return 0, false, err
			}
			slog.Warn("harvest: index day candles", "contract", t.Contract, "err", kite.Describe(err))
		}
	}
	return len(cs), false, nil
}

// eod stores Kite's day candles for an index. NSE's own files, loaded later, take
// precedence, so existing rows are left alone.
func (h *Harvester) eod(ctx context.Context, t Target, from, to time.Time, tl *tally) error {
	cs, err := h.candles(ctx, t, "day", from, to, tl)
	if err != nil || len(cs) == 0 {
		return err
	}
	byDay := map[string]kite.Candle{}
	for _, c := range cs {
		byDay[dayKey(c.TS)] = c
	}
	var d []time.Time
	var o, hi, lo, c []float64
	for _, x := range byDay {
		d, o, hi, lo, c = append(d, clock.Date(x.TS)), append(o, x.O), append(hi, x.H), append(lo, x.L), append(c, x.C)
	}
	tag, err := h.Pool.Exec(ctx, `INSERT INTO index_eod (trade_date, contract, open, high, low, close, source)
		SELECT u.d, $1, u.o, u.h, u.l, u.c, 'kite'
		FROM unnest($2::date[], $3::float8[], $4::float8[], $5::float8[], $6::float8[]) AS u(d, o, h, l, c)
		ON CONFLICT (trade_date, contract) DO NOTHING`, t.Contract, d, o, hi, lo, c)
	if err != nil {
		return fmt.Errorf("harvest: index_eod %s: %w", t.Contract, err)
	}
	tl.add(func(s *Stats) { s.EOD += int(tag.RowsAffected()) })
	return nil
}

// logged returns harvest_log statuses for one contract between two dates, keyed by
// yyyy-mm-dd.
func (h *Harvester) logged(ctx context.Context, contract string, from, to time.Time) (map[string]string, error) {
	rows, err := h.Pool.Query(ctx, `SELECT trade_date, status FROM harvest_log
		WHERE contract = $1 AND trade_date BETWEEN $2 AND $3`, contract, clock.Date(from), clock.Date(to))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	var d time.Time
	var s string
	_, err = pgx.ForEachRow(rows, []any{&d, &s}, func() error {
		out[d.Format("2006-01-02")] = s
		return nil
	})
	return out, err
}

// covered reports whether every day is logged ok or empty, and whether all are empty.
func covered(logged map[string]string, days []time.Time) (done, allEmpty bool) {
	done, allEmpty = true, true
	for _, d := range days {
		switch logged[dayKey(d)] {
		case "ok":
			allEmpty = false
		case "empty":
		default:
			return false, false
		}
	}
	return done, allEmpty
}

// days returns the trade dates the nightly run covers for t.
func (h *Harvester) days(t Target, date time.Time) []time.Time {
	if h.late(t.Exchange) {
		return []time.Time{h.Cal.Prev(date), date}
	}
	return []time.Time{date}
}

// Day harvests date's minute candles for targets. Contracts that still fail after a
// retry are logged as errors and retried by the next run; each group with failures
// gets a 'harvest' row in recorder_gaps, which a clean rerun marks filled.
func (h *Harvester) Day(ctx context.Context, targets []Target, date time.Time) (Stats, error) {
	began := time.Now()
	date = clock.Date(date)
	tl := newTally(targets)
	err := h.run(ctx, targets, func(ctx context.Context, t Target) error {
		days := h.days(t, date)
		if !h.Force {
			logged, err := h.logged(ctx, t.Contract, days[0], days[len(days)-1])
			if err != nil {
				return fmt.Errorf("harvest: read log: %w", err)
			}
			if done, _ := covered(logged, days); done {
				tl.add(func(s *Stats) { s.Skipped++ })
				tl.finish(t, false)
				return nil
			}
		}
		_, failed, err := h.harvest(ctx, t, days[0], days[len(days)-1], days, tl)
		if err != nil {
			return err
		}
		tl.finish(t, failed)
		return nil
	})
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if gerr := h.gaps(gctx, date, tl, err); gerr != nil && err == nil {
		err = gerr
	}
	tl.s.Took = time.Since(began)
	return tl.s, err
}

// gaps writes or closes one recorder_gaps row per group for the nightly run.
func (h *Harvester) gaps(ctx context.Context, date time.Time, tl *tally, runErr error) error {
	sess := h.Cal.Session(date)
	open, closeT := sess.Open, sess.Close
	if !sess.Trading() {
		open, closeT = clock.MustAtHMS(date, calendar.DefaultOpen), clock.MustAtHMS(date, calendar.DefaultClose)
	}
	minutes := int(closeT.Sub(open) / time.Minute)
	groups := make([]string, 0, len(tl.total))
	for g := range tl.total {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		total, failed := tl.total[g], tl.failed[g]
		missing := total - tl.done[g]
		if missing == 0 {
			if _, err := h.Pool.Exec(ctx, `UPDATE recorder_gaps SET filled_minutes = minutes, filled_by = 'harvest',
				detail = COALESCE(detail, '') || ' (filled on rerun)'
				WHERE trade_date = $1 AND kind = 'harvest' AND underlying = $2 AND filled_minutes < minutes`, date, g); err != nil {
				return fmt.Errorf("harvest: close gap %s: %w", g, err)
			}
			continue
		}
		sort.Strings(failed)
		var b strings.Builder
		fmt.Fprintf(&b, "%d/%d contracts failed", len(failed), total)
		if len(failed) > 0 {
			shown := failed
			if len(shown) > 5 {
				shown = shown[:5]
			}
			b.WriteString(": ")
			b.WriteString(strings.Join(shown, ", "))
			if more := len(failed) - len(shown); more > 0 {
				fmt.Fprintf(&b, " (and %d more)", more)
			}
		}
		if n := missing - len(failed); n > 0 {
			fmt.Fprintf(&b, "; %d not attempted", n)
		}
		if runErr != nil {
			fmt.Fprintf(&b, "; run stopped: %s", kite.Describe(runErr))
		}
		if err := store.UpsertGap(ctx, h.Pool, store.Gap{TradeDate: date, Kind: "harvest", Underlying: g,
			Start: open, End: closeT, Minutes: minutes, Detail: b.String()}); err != nil {
			return fmt.Errorf("harvest: gap %s: %w", g, err)
		}
	}
	return nil
}

// Backfill harvests the lookbackDays calendar days ending on end, newest window
// first, skipping windows harvest_log already covers. end should be a completed
// session. A derivative stops at its first window with no candles, which is taken
// to be before its listing; indices and equities get the full lookback. Failures are
// logged as errors for the next run to retry; the backfill writes no gaps.
func (h *Harvester) Backfill(ctx context.Context, targets []Target, end time.Time, lookbackDays, windowDays int) (Stats, error) {
	began := time.Now()
	if max := int(kite.Intervals["minute"] / (24 * time.Hour)); windowDays > max {
		windowDays = max
	}
	wins := Windows(h.Cal, end, lookbackDays, windowDays)
	tl := newTally(targets)
	err := h.run(ctx, targets, func(ctx context.Context, t Target) error {
		if len(wins) == 0 {
			tl.finish(t, false)
			return nil
		}
		logged, err := h.logged(ctx, t.Contract, wins[len(wins)-1].From, wins[0].To)
		if err != nil {
			return fmt.Errorf("harvest: read log: %w", err)
		}
		deriv := isDeriv(t.Key.Kind)
		failed := false
		for _, w := range wins {
			if len(w.Days) == 0 {
				continue
			}
			if done, empty := covered(logged, w.Days); done && !h.Force {
				tl.add(func(s *Stats) { s.Skipped++ })
				if empty && deriv {
					break
				}
				continue
			}
			n, f, err := h.harvest(ctx, t, w.From, w.To, w.Days, tl)
			if err != nil {
				return err
			}
			if f {
				failed = true
				break
			}
			if n == 0 && deriv {
				break
			}
		}
		tl.finish(t, failed)
		return nil
	})
	tl.s.Took = time.Since(began)
	return tl.s, err
}
