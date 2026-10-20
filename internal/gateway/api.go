package gateway

// The read-only HTTP API the screens use for what the bus does not carry:
// contract terms, minute bars and straddle history. Every route is a GET over a
// read-only pool; nothing here writes, and nothing takes an order.
//
//	/api/ref                    lot and tick sizes, option expiries
//	/api/bars?c=<contract>&days=N
//	/api/straddle?und=NIFTY&x=2026-10-27&days=5[&k=25000]
//	/api/oi0?und=NIFTY&x=2026-10-27   each strike's OI at the previous close

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/clock"
	"pmt/internal/contract"
)

// API serves /api/*. Its pool is set once Postgres answers; until then every route
// returns 503 and the screens fall back to what the bus gives them.
type API struct {
	pool atomic.Pointer[pgxpool.Pool]
}

func (a *API) SetPool(p *pgxpool.Pool) { a.pool.Store(p) }

// Register adds the routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ref", a.ref)
	mux.HandleFunc("GET /api/bars", a.bars)
	mux.HandleFunc("GET /api/straddle", a.straddle)
	mux.HandleFunc("GET /api/oi0", a.oi0)
}

const (
	maxDays  = 10
	lookback = 40 * 24 * time.Hour // how far back a "last N sessions" search goes
)

var undRe = regexp.MustCompile(`^[A-Z0-9][A-Z0-9 &-]{0,23}$`)

type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

func bad(msg string) error { return apiError{http.StatusBadRequest, msg} }

func days(r *http.Request, def int) (int, error) {
	s := r.URL.Query().Get("days")
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxDays {
		return 0, bad("days must be 1-10")
	}
	return n, nil
}

func (a *API) serve(w http.ResponseWriter, r *http.Request, f func(ctx context.Context, p *pgxpool.Pool) (any, error), check func() error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	err := check()
	var out any
	if err == nil {
		p := a.pool.Load()
		if p == nil {
			err = apiError{http.StatusServiceUnavailable, "database not connected"}
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			out, err = f(ctx, p)
		}
	}
	if err != nil {
		var ae apiError
		if !errors.As(err, &ae) {
			slog.Warn("gateway: api query failed", "path", r.URL.Path, "err", err)
			ae = apiError{http.StatusInternalServerError, "query failed"}
		}
		w.WriteHeader(ae.code)
		json.NewEncoder(w).Encode(map[string]string{"error": ae.msg})
		return
	}
	json.NewEncoder(w).Encode(out)
}

// ---- /api/ref ------------------------------------------------------------

type refInst struct {
	C    string  `json:"c"`
	Und  string  `json:"u"`
	Kind string  `json:"k"`
	Exp  string  `json:"x,omitempty"`
	Lot  int     `json:"lot"`
	Tick float64 `json:"tick"`
}

type refChain struct {
	Exp     string  `json:"x"`
	Lot     int     `json:"lot"`
	Tick    float64 `json:"tick"`
	Strikes int     `json:"n"`
}

func (a *API) ref(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		rows, err := p.Query(ctx, `SELECT contract, underlying, kind, expiry, lot_size, tick_size
			FROM instrument_history WHERE valid_to IS NULL AND kind NOT IN ('CE','PE')
			ORDER BY underlying, kind, expiry NULLS FIRST`)
		if err != nil {
			return nil, err
		}
		insts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (refInst, error) {
			var in refInst
			var exp *time.Time
			err := r.Scan(&in.C, &in.Und, &in.Kind, &exp, &in.Lot, &in.Tick)
			in.Exp = day(exp)
			return in, err
		})
		if err != nil {
			return nil, err
		}
		rows, err = p.Query(ctx, `SELECT underlying, expiry, max(lot_size), max(tick_size), count(DISTINCT strike)
			FROM instrument_history WHERE valid_to IS NULL AND kind IN ('CE','PE')
			GROUP BY 1, 2 ORDER BY 1, 2`)
		if err != nil {
			return nil, err
		}
		chains := map[string][]refChain{}
		var und string
		var exp time.Time
		var c refChain
		_, err = pgx.ForEachRow(rows, []any{&und, &exp, &c.Lot, &c.Tick, &c.Strikes}, func() error {
			c.Exp = exp.Format(time.DateOnly)
			chains[und] = append(chains[und], c)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"insts": insts, "chains": chains, "today": clock.Today().Format(time.DateOnly)}, nil
	}, func() error { return nil })
}

func day(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

// ---- /api/bars -----------------------------------------------------------

func (a *API) bars(w http.ResponseWriter, r *http.Request) {
	c := r.URL.Query().Get("c")
	var n int
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		return queryBars(ctx, p, c, n)
	}, func() (err error) {
		if _, perr := contract.Parse(c); perr != nil {
			return bad("c must be a contract key, e.g. NFO|NIFTY|2026-10-27||FUT")
		}
		n, err = days(r, 1)
		return err
	})
}

// queryBars returns the contract's minute bars over its last n sessions, one per
// minute: the harvested Kite bar where there is one, else the live-built bar.
// Each row is [unix seconds, open, high, low, close, volume, oi|null].
func queryBars(ctx context.Context, q pgxQuerier, c string, n int) (any, error) {
	from, err := sessionStart(ctx, q, `SELECT DISTINCT (ts AT TIME ZONE 'Asia/Kolkata')::date FROM bars_1m
		WHERE contract = $1 AND ts >= $2 ORDER BY 1 DESC LIMIT $3`, c, since(), n)
	if err != nil || from.IsZero() {
		return map[string]any{"c": c, "bars": [][]any{}}, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (ts) ts, open, high, low, close, volume, oi FROM bars_1m
		WHERE contract = $1 AND ts >= $2 ORDER BY ts, (source = 'kite') DESC`, c, from)
	if err != nil {
		return nil, err
	}
	out := [][]any{}
	var ts time.Time
	var o, h, l, cl float64
	var v int64
	var oi *int64
	_, err = pgx.ForEachRow(rows, []any{&ts, &o, &h, &l, &cl, &v, &oi}, func() error {
		var oiv any
		if oi != nil {
			oiv = *oi
		}
		out = append(out, []any{ts.Unix(), o, h, l, cl, v, oiv})
		return nil
	})
	return map[string]any{"c": c, "bars": out}, err
}

type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func since() time.Time { return clock.Now().Add(-lookback) }

// sessionStart runs a query listing session dates newest first and returns the
// start of the oldest, IST midnight, or zero when there are none.
func sessionStart(ctx context.Context, q pgxQuerier, sql string, args ...any) (time.Time, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return time.Time{}, err
	}
	ds, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil || len(ds) == 0 {
		return time.Time{}, err
	}
	d := ds[len(ds)-1]
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, clock.IST), nil
}

// ---- /api/straddle -------------------------------------------------------

// The straddle mid of a strike at one snapshot: each leg's bid/ask mid when
// both sides quote, else its last price.
const straddleCTE = `WITH s AS (
	SELECT ts, strike, opt_type, forward,
		CASE WHEN bid > 0 AND ask > 0 THEN (bid + ask) / 2 ELSE NULLIF(ltp, 0) END AS mid
	FROM chain_snapshots WHERE underlying = $1 AND expiry = $2 AND ts >= $3),
p AS (
	SELECT ts, strike, avg(forward) AS fwd,
		max(mid) FILTER (WHERE opt_type = 'CE') AS ce, max(mid) FILTER (WHERE opt_type = 'PE') AS pe
	FROM s GROUP BY ts, strike),
-- The recorder leaves forward empty until analytics fills it; put-call parity at
-- the strike where the legs are closest stands in (discounting ignored: it scales
-- CE-PE, which is smallest there).
pf AS (
	SELECT DISTINCT ON (ts) ts, strike + ce - pe AS fwd FROM p
	WHERE ce IS NOT NULL AND pe IS NOT NULL ORDER BY ts, abs(ce - pe), strike),
pq AS (
	SELECT p.ts, p.strike, coalesce(p.fwd, pf.fwd) AS fwd, p.ce, p.pe FROM p JOIN pf USING (ts))`

func (a *API) straddle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	und := q.Get("und")
	var exp time.Time
	var n int
	var k *float64
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		return queryStraddle(ctx, p, und, exp, n, k)
	}, func() (err error) {
		if !undRe.MatchString(und) {
			return bad("und must be an underlying, e.g. NIFTY")
		}
		if exp, err = time.Parse(time.DateOnly, q.Get("x")); err != nil {
			return bad("x must be an expiry date, yyyy-mm-dd")
		}
		if s := q.Get("k"); s != "" {
			v, err := strconv.ParseFloat(s, 64)
			if err != nil || v <= 0 {
				return bad("k must be a strike")
			}
			k = &v
		}
		n, err = days(r, 5)
		return err
	})
}

type straddleSession struct {
	Day string `json:"d"`
	// [unix seconds, forward, ATM strike, ATM straddle, fixed-strike straddle|null]
	Pts [][]any `json:"pts"`
}

// queryStraddle returns, per session over the expiry's last n+1 sessions (today
// and the n before it), the minute series of the rolling ATM straddle (the strike
// nearest the forward with both legs priced) and of a fixed strike: k, or the
// latest session's opening ATM strike.
func queryStraddle(ctx context.Context, q pgxQuerier, und string, exp time.Time, n int, k *float64) (any, error) {
	empty := map[string]any{"und": und, "x": exp.Format(time.DateOnly), "k": k, "sessions": []straddleSession{}}
	from, err := sessionStart(ctx, q, `SELECT DISTINCT (ts AT TIME ZONE 'Asia/Kolkata')::date FROM chain_snapshots
		WHERE underlying = $1 AND expiry = $2 AND ts >= $3 ORDER BY 1 DESC LIMIT $4`, und, exp, since(), n+1)
	if err != nil || from.IsZero() {
		return empty, err
	}
	rows, err := q.Query(ctx, straddleCTE+`
		SELECT DISTINCT ON (ts) ts, fwd, strike, ce + pe FROM pq
		WHERE ce IS NOT NULL AND pe IS NOT NULL
		ORDER BY ts, abs(strike - fwd), strike`, und, exp, from)
	if err != nil {
		return nil, err
	}
	type pt struct {
		ts           time.Time
		f, atmK, atm float64
	}
	var pts []pt
	var x pt
	if _, err = pgx.ForEachRow(rows, []any{&x.ts, &x.f, &x.atmK, &x.atm}, func() error {
		pts = append(pts, x)
		return nil
	}); err != nil || len(pts) == 0 {
		return empty, err
	}
	if k == nil {
		last := pts[len(pts)-1].ts.In(clock.IST).Format(time.DateOnly)
		for _, p := range pts {
			if p.ts.In(clock.IST).Format(time.DateOnly) == last {
				v := p.atmK
				k = &v
				break
			}
		}
	}
	rows, err = q.Query(ctx, straddleCTE+`
		SELECT ts, ce + pe FROM p WHERE strike = $4 AND ce IS NOT NULL AND pe IS NOT NULL`, und, exp, from, *k)
	if err != nil {
		return nil, err
	}
	fixed := map[int64]float64{}
	var ts time.Time
	var v float64
	if _, err = pgx.ForEachRow(rows, []any{&ts, &v}, func() error {
		fixed[ts.Unix()] = v
		return nil
	}); err != nil {
		return nil, err
	}
	var out []straddleSession
	for _, p := range pts {
		d := p.ts.In(clock.IST).Format(time.DateOnly)
		if len(out) == 0 || out[len(out)-1].Day != d {
			out = append(out, straddleSession{Day: d})
		}
		var fv any
		if f, ok := fixed[p.ts.Unix()]; ok {
			fv = f
		}
		s := &out[len(out)-1]
		s.Pts = append(s.Pts, []any{p.ts.Unix(), p.f, p.atmK, p.atm, fv})
	}
	return map[string]any{"und": und, "x": exp.Format(time.DateOnly), "k": *k, "sessions": out}, nil
}

// ---- /api/oi0 ------------------------------------------------------------

func (a *API) oi0(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	und := q.Get("und")
	var exp time.Time
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		return queryOI0(ctx, p, und, exp)
	}, func() (err error) {
		if !undRe.MatchString(und) {
			return bad("und must be an underlying, e.g. NIFTY")
		}
		if exp, err = time.Parse(time.DateOnly, q.Get("x")); err != nil {
			return bad("x must be an expiry date, yyyy-mm-dd")
		}
		return nil
	})
}

// queryOI0 returns the OI each strike of an expiry closed the previous session
// with, the base for the chain's change in OI: the bhavcopy's when it has been
// loaded, else the last snapshot the recorder took that session.
// Rows are [strike, CE OI|null, PE OI|null].
func queryOI0(ctx context.Context, q pgxQuerier, und string, exp time.Time) (any, error) {
	today := clock.Today()
	out := map[string]any{"und": und, "x": exp.Format(time.DateOnly), "d": nil, "src": nil, "rows": [][]any{}}
	collect := func(src string, d time.Time, sql string, args ...any) (bool, error) {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return false, err
		}
		byK := map[float64][]any{}
		var ks []float64
		var k float64
		var typ string
		var oi int64
		if _, err = pgx.ForEachRow(rows, []any{&k, &typ, &oi}, func() error {
			row, ok := byK[k]
			if !ok {
				row = []any{k, nil, nil}
				byK[k] = row
				ks = append(ks, k)
			}
			if typ == "CE" {
				row[1] = oi
			} else {
				row[2] = oi
			}
			return nil
		}); err != nil || len(ks) == 0 {
			return false, err
		}
		res := make([][]any, len(ks))
		for i, k := range ks {
			res[i] = byK[k]
		}
		out["d"], out["src"], out["rows"] = d.Format(time.DateOnly), src, res
		return true, nil
	}

	// The bhavcopy's last session before today.
	d, err := lastDay(ctx, q, `SELECT max(trade_date) FROM eod_contracts
		WHERE underlying = $1 AND expiry = $2 AND trade_date < $3 AND kind IN ('CE','PE')`, und, exp, today)
	if err != nil {
		return nil, err
	}
	if !d.IsZero() {
		if ok, err := collect("eod", d, `SELECT strike, kind, oi FROM eod_contracts
			WHERE trade_date = $1 AND underlying = $2 AND expiry = $3 AND kind IN ('CE','PE') AND oi IS NOT NULL
			ORDER BY strike`, d, und, exp); ok || err != nil {
			return out, err
		}
	}
	// Else the recorder's last snapshot of each contract in its last session.
	d, err = lastDay(ctx, q, `SELECT max((ts AT TIME ZONE 'Asia/Kolkata')::date) FROM chain_snapshots
		WHERE underlying = $1 AND expiry = $2 AND ts >= $3 AND ts < $4`, und, exp, since(), today)
	if err != nil || d.IsZero() {
		return out, err
	}
	from := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, clock.IST)
	_, err = collect("snap", d, `SELECT strike, opt_type, oi FROM (
			SELECT DISTINCT ON (contract) strike, opt_type, oi FROM chain_snapshots
			WHERE underlying = $1 AND expiry = $2 AND ts >= $3 AND ts < $4 AND oi IS NOT NULL
			ORDER BY contract, ts DESC) s
		ORDER BY strike`, und, exp, from, today)
	return out, err
}

// lastDay runs a query returning one nullable date; zero when it is null.
func lastDay(ctx context.Context, q pgxQuerier, sql string, args ...any) (time.Time, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return time.Time{}, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[*time.Time])
	if err != nil || d == nil {
		return time.Time{}, err
	}
	return *d, nil
}
