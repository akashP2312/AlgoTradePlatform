package gateway

// The read-only HTTP API the screens use for what the bus does not carry:
// contract terms, minute bars, straddle history and the EOD vol tables. Every
// route is a GET over a read-only pool; nothing here writes, and nothing takes an
// order.
//
//	/api/ref                    lot and tick sizes, option expiries (live: streamed)
//	/api/bars?c=<contract>&days=N
//	/api/straddle?und=NIFTY&x=2026-10-27&days=5[&k=25000]
//	/api/oi0?und=NIFTY&x=2026-10-27   each strike's OI at the previous close
//	/api/risk-state             newsmon's news risk state, for strategies to poll
//	/api/risk-log?from=2026-10-01&to=2026-10-21   its logged changes, for backtests
//	/api/vol?und=NIFTY                the EOD vol tables' latest: realized, forecast,
//	                                  premium, term and skew, events, regime

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
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/refdata"
)

// API serves /api/*. Its pool is set once Postgres answers; until then every route
// returns 503 and the screens fall back to what the bus gives them.
type API struct {
	pool atomic.Pointer[pgxpool.Pool]
	// Universe says which expiries the ticker streams, so /api/ref can mark them.
	Universe []config.Underlying
	// Last reads the hub's last-value cache (news.risk for /api/risk-state).
	Last func(subject string) (json.RawMessage, bool)
}

func (a *API) SetPool(p *pgxpool.Pool) { a.pool.Store(p) }

// Register adds the routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ref", a.ref)
	mux.HandleFunc("GET /api/bars", a.bars)
	mux.HandleFunc("GET /api/straddle", a.straddle)
	mux.HandleFunc("GET /api/oi0", a.oi0)
	mux.HandleFunc("GET /api/risk-state", a.riskState)
	mux.HandleFunc("GET /api/risk-log", a.riskLog)
	mux.HandleFunc("GET /api/vol", a.vol)
}

// ---- /api/risk-state -------------------------------------------------------

// riskStale is how old news.risk may be before the answer says stale: newsmon
// republishes at least every news.publish (15s).
const riskStale = 60 * time.Second

// riskState answers newsmon's last news.risk: {state, reasons[], expires_at, ...}.
// It comes from the bus cache, not Postgres. 503 until newsmon has published; a
// value older than a minute is still returned, with "stale": true, and a strategy
// should treat stale as unknown, not as NORMAL.
func (a *API) riskState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	var v json.RawMessage
	ok := false
	if a.Last != nil {
		v, ok = a.Last("news.risk")
	}
	var m map[string]any
	if ok {
		ok = json.Unmarshal(v, &m) == nil
	}
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "newsmon has not published a risk state"})
		return
	}
	ts, _ := m["ts"].(float64)
	m["stale"] = clock.Now().Sub(time.UnixMilli(int64(ts))) > riskStale
	json.NewEncoder(w).Encode(m)
}

// ---- /api/risk-log ---------------------------------------------------------

// maxLogDays bounds one /api/risk-log request.
const maxLogDays = 400

type riskChange struct {
	TS        int64           `json:"ts"`
	State     string          `json:"state"`
	Prev      string          `json:"prev,omitempty"`
	Reasons   json.RawMessage `json:"reasons"`
	ExpiresAt int64           `json:"expires_at,omitempty"`
}

// riskLog answers news_risk_log between two IST dates, inclusive, oldest first,
// plus "prior", the last change before from, so a replay knows the state the
// range opens in. A change is logged only when the level changes; the log cannot
// tell when newsmon was down, so a replay should not read a gap as NORMAL.
func (a *API) riskLog(w http.ResponseWriter, r *http.Request) {
	var from, to time.Time
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		return queryRiskLog(ctx, p, from, to)
	}, func() (err error) {
		q := r.URL.Query()
		if from, err = time.ParseInLocation(time.DateOnly, q.Get("from"), clock.IST); err != nil {
			return bad("from must be a date, e.g. 2026-10-01")
		}
		to = clock.Today()
		if s := q.Get("to"); s != "" {
			if to, err = time.ParseInLocation(time.DateOnly, s, clock.IST); err != nil {
				return bad("to must be a date, e.g. 2026-10-21")
			}
		}
		if to.Before(from) || to.Sub(from) > maxLogDays*24*time.Hour {
			return bad("to must be on or after from, at most 400 days later")
		}
		return nil
	})
}

func queryRiskLog(ctx context.Context, q pgxQuerier, from, to time.Time) (any, error) {
	end := to.AddDate(0, 0, 1)
	rows, err := q.Query(ctx, `(SELECT ts, state, prev, reasons, expires_at FROM news_risk_log
		WHERE ts < $1 ORDER BY ts DESC LIMIT 1)
		UNION ALL
		(SELECT ts, state, prev, reasons, expires_at FROM news_risk_log WHERE ts >= $1 AND ts < $2)
		ORDER BY ts`, from, end)
	if err != nil {
		return nil, err
	}
	var prior *riskChange
	changes := []riskChange{}
	var ts time.Time
	var st string
	var prev *string
	var rs []byte
	var exp *time.Time
	_, err = pgx.ForEachRow(rows, []any{&ts, &st, &prev, &rs, &exp}, func() error {
		c := riskChange{TS: ts.UnixMilli(), State: st, Reasons: json.RawMessage(append([]byte(nil), rs...))}
		if prev != nil {
			c.Prev = *prev
		}
		if exp != nil {
			c.ExpiresAt = exp.UnixMilli()
		}
		if ts.Before(from) {
			prior = &c
		} else {
			changes = append(changes, c)
		}
		return nil
	})
	return map[string]any{"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "prior": prior, "changes": changes}, err
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
	Live    bool    `json:"live,omitempty"` // the ticker streams it, so it has ticks and an IV surface
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
		today := clock.Today()
		markLive(chains, a.Universe, today)
		return map[string]any{"insts": insts, "chains": chains, "today": today.Format(time.DateOnly)}, nil
	}, func() error { return nil })
}

// markLive flags the chains the ticker streams: per configured underlying, the
// nearest wide_expiries from today plus its monthlies, as the ticker picks them.
// Each underlying's chains are sorted by expiry.
func markLive(chains map[string][]refChain, univ []config.Underlying, today time.Time) {
	t := today.Format(time.DateOnly)
	for _, ul := range univ {
		cs := chains[ul.Name]
		var all []time.Time
		for _, c := range cs {
			if c.Exp >= t {
				x, _ := time.Parse(time.DateOnly, c.Exp)
				all = append(all, x)
			}
		}
		live := map[string]bool{}
		for _, x := range refdata.WithMonthlies(all, ul.WideExpiries, ul.Monthlies) {
			live[x.Format(time.DateOnly)] = true
		}
		for i := range cs {
			cs[i].Live = live[cs[i].Exp]
		}
	}
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

// ---- /api/vol ------------------------------------------------------------

// vol: what the EOD vol jobs (pmt-vol, pmt-ivhist, pmt-event, pmt-regime) last wrote
// for an index, as the VOL screen shows it. Each part is its table's latest day, so
// one job running late leaves the others current; every part says its own date.
func (a *API) vol(w http.ResponseWriter, r *http.Request) {
	und := r.URL.Query().Get("und")
	a.serve(w, r, func(ctx context.Context, p *pgxpool.Pool) (any, error) {
		return queryVol(ctx, p, und)
	}, func() error {
		if !undRe.MatchString(und) {
			return bad("und must be an underlying, e.g. NIFTY")
		}
		return nil
	})
}

const (
	volHorizon = 21  // the vrp_eod horizon the screen shows
	volSeries  = 252 // days of iv30 / VIX / forecast / realized history
	volLabels  = 60  // days of regime labels
	volExpiry  = 8   // expiries in the event table
)

// volParts are the queries behind /api/vol, each with $1 the underlying. Dates come
// back as text so they read yyyy-mm-dd.
var volParts = []struct{ key, sql string }{
	{"rv", `SELECT trade_date::text AS d, win, cc, pk, gk, yz FROM rv_eod
		WHERE underlying = $1 AND trade_date = (SELECT max(trade_date) FROM rv_eod WHERE underlying = $1)
		ORDER BY win`},
	{"har", `SELECT trade_date::text AS d, horizon, fc, lo, hi, rv21, vix FROM har_eod
		WHERE underlying = $1 AND trade_date = (SELECT max(trade_date) FROM har_eod WHERE underlying = $1)
		ORDER BY horizon`},
	{"score", `SELECT asof::text AS d, horizon, model, n, qlike, mse, bias, hit FROM vol_score
		WHERE underlying = $1 AND asof = (SELECT max(asof) FROM vol_score WHERE underlying = $1)
		ORDER BY horizon, qlike`},
	{"vrp", `SELECT trade_date::text AS d, iv30, vix, har_fc, rv21, rv_fwd, iv_ante, iv_post, vix_ante, vix_post,
		iv_rank, iv_pct, vix_rank, vix_pct, n_iv, n_vix FROM vrp_eod
		WHERE underlying = $1 AND horizon = ` + strconv.Itoa(volHorizon) + `
		ORDER BY trade_date DESC LIMIT 1`},
	// The latest day with an iv30, which can trail the VIX by a session until the
	// bhavcopy fits are in.
	{"iv", `SELECT trade_date::text AS d, iv30, iv_ante, iv_post, iv_rank, iv_pct, n_iv FROM vrp_eod
		WHERE underlying = $1 AND horizon = ` + strconv.Itoa(volHorizon) + ` AND iv30 IS NOT NULL
		ORDER BY trade_date DESC LIMIT 1`},
	{"series", `SELECT d, iv30, vix, har_fc, rv21 FROM (
			SELECT trade_date::text AS d, iv30, vix, har_fc, rv21 FROM vrp_eod
			WHERE underlying = $1 AND horizon = ` + strconv.Itoa(volHorizon) + `
			ORDER BY trade_date DESC LIMIT ` + strconv.Itoa(volSeries) + `) s ORDER BY d`},
	// Today's curve, a week (5 sessions) back and a month (21) back.
	{"term", `WITH ds AS (
			SELECT trade_date, row_number() OVER (ORDER BY trade_date DESC) - 1 AS ago
			FROM (SELECT DISTINCT trade_date FROM ivcm_eod WHERE underlying = $1
				ORDER BY trade_date DESC LIMIT 22) x)
		SELECT c.trade_date::text AS d, ds.ago, c.tenor, c.atm_iv, c.rr25, c.bf25, c.atm_rank, c.atm_pct,
			c.rr25_pct, c.bf25_pct, c.n_hist
		FROM ivcm_eod c JOIN ds USING (trade_date)
		WHERE c.underlying = $1 AND ds.ago IN (0, 5, 21)
		ORDER BY ds.ago, c.tenor`},
	{"events", `SELECT trade_date::text AS d, expiry::text AS x, dte, atm_iv, implied_move, seg_vol, event_move,
			hist_move, hist_n, events FROM event_eod
		WHERE underlying = $1 AND trade_date = (SELECT max(trade_date) FROM event_eod WHERE underlying = $1)
		ORDER BY dte LIMIT ` + strconv.Itoa(volExpiry)},
	// How the priced move has compared with the move that followed, by days to expiry
	// (pmt-event's buckets).
	{"priced", `SELECT lo, hi, count(*) AS n, avg(implied_move) AS implied, avg(real_move) AS realized,
			avg((real_move > implied_move)::int)::float8 AS over
		FROM event_eod e JOIN (VALUES (1, 2), (3, 5), (6, 10), (11, 21), (22, 63)) b(lo, hi)
			ON e.dte >= b.lo AND e.dte <= b.hi
		WHERE e.underlying = $1 AND e.real_move IS NOT NULL
		GROUP BY lo, hi ORDER BY lo`},
	{"regime", `SELECT trade_date::text AS d, level_src, level_pct, term_slope, rv_ratio, premium, level, term,
			trend, prem, label, rule FROM regime_eod
		WHERE underlying = $1 AND label IS NOT NULL ORDER BY trade_date DESC LIMIT 1`},
	{"labels", `SELECT d, label FROM (
			SELECT trade_date::text AS d, label FROM regime_eod WHERE underlying = $1 AND label IS NOT NULL
			ORDER BY trade_date DESC LIMIT ` + strconv.Itoa(volLabels) + `) s ORDER BY d`},
}

// queryVol returns {und, <part>: [rows]} with every row an object of its columns;
// a part with no rows is an empty list. vrp, iv and regime hold at most one row.
func queryVol(ctx context.Context, q pgxQuerier, und string) (any, error) {
	out := map[string]any{"und": und}
	for _, part := range volParts {
		rows, err := q.Query(ctx, part.sql, und)
		if err != nil {
			return nil, err
		}
		res, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			return nil, err
		}
		if res == nil {
			res = []map[string]any{}
		}
		out[part.key] = res
	}
	return out, nil
}
