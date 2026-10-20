// Command fakekite is a stand-in for Kite Connect used in dry runs and on a
// laptop: one HTTP server answering the read-only REST calls the services make
// (instruments, quotes, LTP, historical candles, the session exchange) and the
// ticker WebSocket, with synthetic NIFTY and BANKNIFTY chains on a random walk.
//
// It only knows fake tokens. Every path outside the read-only set, orders and
// logout included, is refused with 403, so a stray write shows up in the logs
// of the service that tried it. It honours PMT_CLOCK_OFFSET like the services,
// so a dry run in the evening sees a market that is open.
//
//	fakekite -listen 127.0.0.1:8099
//	KITE_ROOT_URL=http://127.0.0.1:8099 KITE_WS_URL=ws://127.0.0.1:8099/ws ...
//	login -request-token fake-request-token
package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"pmt/internal/app"
	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8099", "address to serve on (loopback, Tailscale, or any inside a container)")
	apiKey := flag.String("api-key", "fake-api-key", "API key clients must send")
	token := flag.String("access-token", "fake-access-token", "access token handed out by /session/token and required afterwards")
	orderEvery := flag.Duration("order-every", 0, "send a fake order update on each socket this often (0 = never)")
	vol := flag.Float64("vol", 0.18, "annualised volatility of the random walk")
	cfgDir := filepath.Dir(config.Path())
	calPath := flag.String("calendar", filepath.Join(cfgDir, "calendar", "holidays.csv"), "holiday calendar: no candles on holidays, expiries shift off them")
	rulesPath := flag.String("rules", filepath.Join(cfgDir, "expiry_rules.csv"), "expiry rules the listed expiries are projected from")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := app.PrivateAddr(*listen); err != nil {
		slog.Error("fakekite: refusing to listen", "addr", *listen, "err", err)
		os.Exit(1)
	}
	cal, err := calendar.Load(*calPath)
	if err != nil {
		slog.Error("fakekite: calendar", "err", err)
		os.Exit(1)
	}
	rules, err := calendar.LoadRules(*rulesPath)
	if err != nil {
		slog.Error("fakekite: expiry rules", "err", err)
		os.Exit(1)
	}
	if !cal.Covers(clock.Today()) {
		slog.Warn("fakekite: calendar does not cover today; holidays are not skipped", "today", clock.Today().Format(time.DateOnly))
	}
	m := newMarket(clock.Today(), *vol, cal, rules)
	go m.walk()
	s := &server{m: m, apiKey: *apiKey, token: *token, orderEvery: *orderEvery, book: sampleBook(m, clock.Now())}
	slog.Info("fakekite: serving", "addr", *listen, "instruments", len(m.insts), "clock_offset", clock.Offset().String())
	if err := http.ListenAndServe(*listen, s.routes()); err != nil {
		slog.Error("fakekite", "err", err)
		os.Exit(1)
	}
}

// ---- market ----

type inst struct {
	token        uint32
	exch, seg    string // exchange and Kite segment column
	symbol, name string
	itype        string // EQ, FUT, CE, PE
	expiry       time.Time
	strike       float64
	tick         float64
	lot          int
	under        string // the walk this instrument is priced from
	index        bool
	scaleTo      float64 // EQ price relative to the walk
}

type market struct {
	mu     sync.RWMutex
	cal    *calendar.Calendar
	spot   map[string]float64
	open   map[string]float64
	vol    float64
	insts  []*inst
	byTok  map[uint32]*inst
	byKey  map[string]*inst // exchange:tradingsymbol
	volume map[uint32]uint32
}

type seed struct {
	name, index, deriv string
	spot, step         float64
	lot                int
	weeklies, strikes  int
}

// Lot sizes are plausible, not authoritative (verify before using fakekite output
// as a reference for anything). Expiries come from the expiry rules and calendar.
var seeds = []seed{
	{name: "NIFTY", index: "NIFTY 50", deriv: "NFO", spot: 25000, step: 50, lot: 75, weeklies: 4, strikes: 60},
	{name: "BANKNIFTY", index: "NIFTY BANK", deriv: "NFO", spot: 56000, step: 100, lot: 35, weeklies: 0, strikes: 50},
}

var equities = map[string]float64{"RELIANCE": 1400, "HDFCBANK": 1950, "ICICIBANK": 1420, "INFY": 1500, "TCS": 3100}

func newMarket(today time.Time, vol float64, cal *calendar.Calendar, rules []calendar.Rule) *market {
	m := &market{cal: cal, spot: map[string]float64{}, open: map[string]float64{}, vol: vol,
		byTok: map[uint32]*inst{}, byKey: map[string]*inst{}, volume: map[uint32]uint32{}}
	next := uint32(1)
	add := func(in *inst, seg uint32) {
		in.token = next<<8 | seg
		next++
		m.insts = append(m.insts, in)
		m.byTok[in.token] = in
		m.byKey[in.exch+":"+in.symbol] = in
	}
	var month time.Time // the near NIFTY monthly, which the other futures share
	for _, s := range seeds {
		m.spot[s.name], m.open[s.name] = s.spot, s.spot
		add(&inst{exch: "NSE", seg: "INDICES", symbol: s.index, name: s.index, itype: "EQ", tick: 0, under: s.name, index: true, scaleTo: 1}, kite.SegIndices)
		exps, months := expiries(cal, rules, s.name, today, s.weeklies, 3)
		if s.name == "NIFTY" {
			month = months[0]
		}
		for _, d := range months {
			add(&inst{exch: s.deriv, seg: "NFO-FUT", symbol: fmt.Sprintf("%s%sFUT", s.name, strings.ToUpper(d.Format("06Jan"))),
				name: s.name, itype: "FUT", expiry: d, tick: 0.1, lot: s.lot, under: s.name}, kite.SegNseFO)
		}
		atm := math.Round(s.spot/s.step) * s.step
		for _, d := range exps {
			for k := -s.strikes; k <= s.strikes; k++ {
				strike := atm + float64(k)*s.step
				for _, t := range []string{"CE", "PE"} {
					add(&inst{exch: s.deriv, seg: "NFO-OPT", symbol: fmt.Sprintf("%s%s%.0f%s", s.name, strings.ToUpper(d.Format("06Jan02")), strike, t),
						name: s.name, itype: t, expiry: d, strike: strike, tick: 0.05, lot: s.lot, under: s.name}, kite.SegNseFO)
				}
			}
		}
	}
	m.spot["INDIA VIX"], m.open["INDIA VIX"] = 13, 13
	add(&inst{exch: "NSE", seg: "INDICES", symbol: "INDIA VIX", name: "INDIA VIX", itype: "EQ", under: "INDIA VIX", index: true, scaleTo: 1}, kite.SegIndices)
	for name, px := range equities {
		m.spot[name], m.open[name] = px, px
		add(&inst{exch: "NSE", seg: "NSE", symbol: name, name: name, itype: "EQ", tick: 0.05, lot: 1, under: name, scaleTo: 1}, kite.SegNseCM)
	}
	extra := []struct {
		exch, seg, name string
		px, tick        float64
		lot             int
		segID           uint32
	}{
		{"CDS", "CDS-FUT", "USDINR", 88.5, 0.0025, 1000, kite.SegNseCD},
		{"MCX", "MCX-FUT", "CRUDEOIL", 5600, 1, 100, kite.SegMcxFO},
		{"NSEIX", "NSEIX-FUT", "GIFT NIFTY", 25080, 0.5, 1, kite.SegNseFO}, // segment byte is a guess
	}
	for _, e := range extra {
		m.spot[e.name], m.open[e.name] = e.px, e.px
		add(&inst{exch: e.exch, seg: e.seg, symbol: fmt.Sprintf("%s%sFUT", strings.ReplaceAll(e.name, " ", ""), strings.ToUpper(month.Format("06Jan"))),
			name: e.name, itype: "FUT", expiry: month, tick: e.tick, lot: e.lot, under: e.name}, e.segID)
	}
	return m
}

// expiries lists und's next weeklies (up to weeklies of them) and next monthlies
// from today, as the expiry rules project them: a date that falls on a holiday
// moves to the trading day before, and a weekly on the monthly's date is the
// monthly. all holds both, in order; months only the monthlies.
func expiries(cal *calendar.Calendar, rules []calendar.Rule, und string, today time.Time, weeklies, monthlies int) (all, months []time.Time) {
	nw := 0
	for _, e := range calendar.Project(cal, rules, und, today, today.AddDate(0, 0, 40*monthlies)) {
		switch {
		case e.Cycle == "monthly" && len(months) < monthlies:
			months = append(months, e.Date)
		case e.Cycle == "weekly" && nw < weeklies:
			nw++
		default:
			continue
		}
		all = append(all, e.Date)
	}
	return all, months
}

// walk moves every underlying once a second.
func (m *market) walk() {
	step := m.vol / math.Sqrt(252*375*60)
	for range time.Tick(time.Second) {
		m.mu.Lock()
		for k, v := range m.spot {
			m.spot[k] = v * math.Exp(step*rand.NormFloat64())
		}
		for t := range m.byTok {
			m.volume[t] += uint32(rand.IntN(40))
		}
		m.mu.Unlock()
	}
}

// price is the instrument's last price at spot s.
func (in *inst) price(s float64, now time.Time, vol float64) float64 {
	t := math.Max(in.expiry.Add(15*time.Hour+30*time.Minute).Sub(now).Hours()/24/365, 1.0/365/24)
	f := s * math.Exp(rate*t)
	switch in.itype {
	case "FUT":
		return round(f, in.tick)
	case "CE", "PE":
		v := smileVol(math.Log(in.strike/f), t, vol)
		return round(math.Max(math.Exp(-rate*t)*black76(in.itype == "CE", f, in.strike, t, v), 0.05), in.tick)
	}
	return round(s*in.scaleTo, in.tick)
}

// The smile is SSVI (Gatheral and Jacquier) around the walk's vol at the money, so
// the IV engine fits a skewed, butterfly-free surface instead of a flat one. These
// give a 25-delta risk reversal near -0.67*rho*eta*vol (about -3 vol points at 18%)
// at every expiry; plausible for NIFTY, not calibrated (verify). eta*(1+|rho|) <= 2
// keeps the surface free of static arbitrage.
const (
	smileRho   = -0.4
	smileEta   = 0.6
	smileGamma = 0.5
)

// smileVol is the implied vol at log-moneyness k for t years, given the ATM vol.
func smileVol(k, t, atm float64) float64 {
	th := atm * atm * t
	phi := smileEta / (math.Pow(th, smileGamma) * math.Pow(1+th, 1-smileGamma))
	x := phi*k + smileRho
	w := th / 2 * (1 + smileRho*phi*k + math.Sqrt(x*x+1-smileRho*smileRho))
	return math.Sqrt(w / t)
}

// rate carries futures to the forward and discounts options, so put-call parity,
// the futures and the IV engine (whose default rate this is) agree on the forward.
const rate = 0.065

// black76 is the undiscounted option price on forward f.
func black76(call bool, f, k, t, v float64) float64 {
	sd := v * math.Sqrt(t)
	d1 := (math.Log(f/k) + sd*sd/2) / sd
	d2 := d1 - sd
	n := func(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }
	if call {
		return f*n(d1) - k*n(d2)
	}
	return k*n(-d2) - f*n(-d1)
}

func round(v, tick float64) float64 {
	if tick <= 0 {
		return math.Round(v*100) / 100
	}
	return math.Round(v/tick) * tick
}

// stable is a per-token number that does not change during the day.
func stable(tok uint32, lo, hi int) int {
	h := fnv.New32a()
	fmt.Fprint(h, tok)
	return lo + int(h.Sum32()%uint32(hi-lo+1))
}

type snap struct {
	in                     *inst
	ltp, open, high, low   float64
	close                  float64
	vol, oi, tbq, tsq, ltq uint32
}

func (m *market) snap(in *inst, now time.Time) snap {
	m.mu.RLock()
	s, o, v := m.spot[in.under], m.open[in.under], m.volume[in.token]
	m.mu.RUnlock()
	ltp := in.price(s, now, m.vol)
	op := in.price(o, now, m.vol)
	sn := snap{in: in, ltp: ltp, open: op, high: math.Max(ltp, op), low: math.Min(ltp, op), close: in.price(o*0.998, now, m.vol), vol: v}
	if !in.index {
		sn.ltq = uint32(in.lot)
		sn.tbq, sn.tsq = uint32(stable(in.token, 1, 400)*in.lot), uint32(stable(in.token+1, 1, 400)*in.lot)
	}
	if in.itype != "EQ" {
		sn.oi = uint32(stable(in.token, 10, 5000) * in.lot)
	}
	return sn
}

func (sn snap) depth() *bus.Depth {
	var d bus.Depth
	tick := math.Max(sn.in.tick, 0.05)
	for i := range 5 {
		q := uint32(sn.in.lot * (i + 1) * 3)
		d.Buy[i] = bus.Level{P: round(sn.ltp-tick*float64(i+1), sn.in.tick), Q: q, N: uint16(i + 1)}
		d.Sell[i] = bus.Level{P: round(sn.ltp+tick*float64(i+1), sn.in.tick), Q: q, N: uint16(i + 1)}
	}
	return &d
}

func (sn snap) packet(mode byte, now time.Time) kite.Packet {
	p := kite.Packet{Token: sn.in.token, Mode: mode, Index: sn.in.index, LTP: sn.ltp,
		Open: sn.open, High: sn.high, Low: sn.low, Close: sn.close, Exch: now.Unix()}
	if !sn.in.index {
		p.LTQ, p.Vol, p.TBQ, p.TSQ, p.Avg = sn.ltq, sn.vol, sn.tbq, sn.tsq, round((sn.open+sn.ltp)/2, sn.in.tick)
		p.LTT, p.OI, p.OIHi, p.OILo = now.Unix(), sn.oi, sn.oi+uint32(sn.in.lot*10), sn.oi-min(sn.oi, uint32(sn.in.lot*10))
		if mode == 'f' {
			p.Depth = sn.depth()
		}
	}
	return p
}

// ---- HTTP ----

type server struct {
	m          *market
	apiKey     string
	token      string
	orderEvery time.Duration
	up         websocket.Upgrader
	book       []leg // the sample positions; built once at start so trade prices stay fixed
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session/token", s.sessionToken)
	mux.HandleFunc("GET /instruments", s.auth(s.instruments))
	mux.HandleFunc("GET /instruments/{exch}", s.auth(s.instruments))
	mux.HandleFunc("GET /quote", s.auth(s.quote))
	mux.HandleFunc("GET /quote/ohlc", s.auth(s.quote))
	mux.HandleFunc("GET /quote/ltp", s.auth(s.ltp))
	mux.HandleFunc("GET /instruments/historical/{token}/{interval}", s.auth(s.historical))
	mux.HandleFunc("GET /user/profile", s.auth(s.profile))
	mux.HandleFunc("GET /portfolio/positions", s.auth(s.positions))
	mux.HandleFunc("GET /portfolio/holdings", s.auth(s.holdings))
	mux.HandleFunc("GET /user/margins", s.auth(s.margins))
	mux.HandleFunc("GET /user/margins/{seg}", s.auth(s.margins))
	mux.HandleFunc("GET /ws", s.ws)
	mux.HandleFunc("GET /{$}", s.ws) // the real ticker serves on the root path
	mux.HandleFunc("/", s.refuse)
	return logged(mux)
}

func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the path: query strings carry tokens on the socket.
		slog.Debug("fakekite", "method", r.Method, "path", r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, etype, msg string) {
	writeJSON(w, code, map[string]any{"status": "error", "error_type": etype, "message": msg})
}

func ok(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": data})
}

func (s *server) refuse(w http.ResponseWriter, r *http.Request) {
	slog.Warn("fakekite: refused", "method", r.Method, "path", r.URL.Path)
	fail(w, http.StatusForbidden, "PermissionException", "fakekite: refused, read-only terminal")
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+s.apiKey+":"+s.token {
			fail(w, http.StatusForbidden, "TokenException", "Incorrect `api_key` or `access_token`.")
			return
		}
		h(w, r)
	}
}

func (s *server) sessionToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("api_key") != s.apiKey || r.Form.Get("request_token") == "" {
		fail(w, http.StatusForbidden, "TokenException", "Token is invalid or has expired.")
		return
	}
	now := clock.Now()
	ok(w, map[string]any{
		"user_type": "individual", "email": "", "user_name": "Fake User", "user_shortname": "fake",
		"broker": "ZERODHA", "exchanges": []string{"NSE", "NFO", "CDS", "MCX"}, "products": []string{},
		"order_types": []string{}, "avatar_url": nil, "user_id": "FK0001", "api_key": s.apiKey,
		"access_token": s.token, "public_token": "fake-public-token", "refresh_token": "",
		"enctoken": "", "login_time": now.Format(time.DateTime), "meta": map[string]any{"demat_consent": ""},
	})
}

func (s *server) profile(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"user_id": "FK0001", "user_name": "Fake User", "broker": "ZERODHA", "exchanges": []string{"NSE", "NFO"}})
}

func (s *server) instruments(w http.ResponseWriter, r *http.Request) {
	exch := r.PathValue("exch")
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	cw.Write([]string{"instrument_token", "exchange_token", "tradingsymbol", "name", "last_price", "expiry",
		"strike", "tick_size", "lot_size", "instrument_type", "segment", "exchange"})
	for _, in := range s.m.insts {
		if exch != "" && in.exch != exch {
			continue
		}
		exp := ""
		if !in.expiry.IsZero() {
			exp = in.expiry.Format(time.DateOnly)
		}
		cw.Write([]string{strconv.FormatUint(uint64(in.token), 10), strconv.FormatUint(uint64(in.token>>8), 10),
			in.symbol, strings.Trim(strconv.Quote(in.name), `"`), "0", exp, strconv.FormatFloat(in.strike, 'f', -1, 64),
			strconv.FormatFloat(in.tick, 'f', -1, 64), strconv.Itoa(in.lot), in.itype, in.seg, in.exch})
	}
	cw.Flush()
	w.Header().Set("Content-Type", "text/csv")
	w.Write(buf.Bytes())
}

func (s *server) keys(r *http.Request) []*inst {
	var out []*inst
	for _, k := range r.URL.Query()["i"] {
		if in, ok := s.m.byKey[k]; ok {
			out = append(out, in)
		} else if t, err := strconv.ParseUint(k, 10, 32); err == nil && s.m.byTok[uint32(t)] != nil {
			out = append(out, s.m.byTok[uint32(t)])
		}
	}
	return out
}

func key(in *inst) string { return in.exch + ":" + in.symbol }

func (s *server) quote(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()["i"]) > kite.MaxQuoteKeys {
		fail(w, http.StatusBadRequest, "InputException", "Too many instruments")
		return
	}
	now := clock.Now()
	data := map[string]any{}
	for _, in := range s.keys(r) {
		sn := s.m.snap(in, now)
		lvl := func(side [5]bus.Level) []map[string]any {
			var out []map[string]any
			for _, l := range side {
				out = append(out, map[string]any{"price": l.P, "quantity": l.Q, "orders": l.N})
			}
			return out
		}
		q := map[string]any{
			"instrument_token": in.token, "timestamp": now.Format(time.DateTime), "last_price": sn.ltp,
			"ohlc": map[string]float64{"open": sn.open, "high": sn.high, "low": sn.low, "close": sn.close},
		}
		if !in.index {
			d := sn.depth()
			q["last_trade_time"] = now.Format(time.DateTime)
			q["last_quantity"], q["buy_quantity"], q["sell_quantity"] = sn.ltq, sn.tbq, sn.tsq
			q["volume"], q["average_price"] = sn.vol, round((sn.open+sn.ltp)/2, in.tick)
			q["oi"], q["oi_day_high"], q["oi_day_low"] = sn.oi, sn.oi, sn.oi
			q["depth"] = map[string]any{"buy": lvl(d.Buy), "sell": lvl(d.Sell)}
		}
		data[key(in)] = q
	}
	ok(w, data)
}

func (s *server) ltp(w http.ResponseWriter, r *http.Request) {
	now := clock.Now()
	data := map[string]any{}
	for _, in := range s.keys(r) {
		data[key(in)] = map[string]any{"instrument_token": in.token, "last_price": s.m.snap(in, now).ltp}
	}
	ok(w, data)
}

// historical serves deterministic candles over each trading day's session, so
// the harvester sees the same history on every run.
func (s *server) historical(w http.ResponseWriter, r *http.Request) {
	t, err := strconv.ParseUint(r.PathValue("token"), 10, 32)
	in := s.m.byTok[uint32(t)]
	if err != nil || in == nil {
		fail(w, http.StatusBadRequest, "InputException", "invalid token")
		return
	}
	iv := r.PathValue("interval")
	span, known := kite.Intervals[iv]
	if !known {
		fail(w, http.StatusBadRequest, "InputException", "invalid interval")
		return
	}
	from, err1 := time.ParseInLocation(time.DateTime, r.URL.Query().Get("from"), clock.IST)
	to, err2 := time.ParseInLocation(time.DateTime, r.URL.Query().Get("to"), clock.IST)
	if err1 != nil || err2 != nil || to.Before(from) {
		fail(w, http.StatusBadRequest, "InputException", "invalid from or to")
		return
	}
	if to.Sub(from) > span {
		fail(w, http.StatusBadRequest, "InputException", "interval exceeds max limit")
		return
	}
	step := 24 * time.Hour
	if iv != "day" {
		n, _ := strconv.Atoi(strings.TrimSuffix(iv, "minute"))
		step = time.Duration(max(n, 1)) * time.Minute
	}
	withOI := r.URL.Query().Get("oi") == "1"
	if now := clock.Now(); to.After(now) {
		to = now
	}
	s.m.mu.RLock()
	base := s.m.open[in.under]
	s.m.mu.RUnlock()
	var rows [][]any
	for d := clock.Date(from); !d.After(to); d = d.AddDate(0, 0, 1) {
		ses := s.m.cal.Session(d)
		if !ses.Trading() {
			continue
		}
		open, end := ses.Open, ses.Close
		if !in.expiry.IsZero() && d.After(in.expiry) {
			break
		}
		if step == 24*time.Hour {
			rows = append(rows, s.candle(in, d, base, withOI, 375))
			continue
		}
		for ts := open; ts.Before(end); ts = ts.Add(step) {
			if ts.Before(from) || ts.After(to) {
				continue
			}
			rows = append(rows, s.candle(in, ts, base, withOI, int(step/time.Minute)))
		}
	}
	ok(w, map[string]any{"candles": rows})
}

func (s *server) candle(in *inst, ts time.Time, base float64, withOI bool, minutes int) []any {
	h := fnv.New64a()
	fmt.Fprint(h, in.under, ts.Unix()/60)
	rng := rand.New(rand.NewPCG(h.Sum64(), 7))
	und := base * (1 + 0.01*math.Sin(float64(ts.Unix())/86400) + 0.002*rng.NormFloat64())
	c := in.price(und, ts, s.m.vol)
	o := in.price(und*(1+0.0005*rng.NormFloat64()), ts, s.m.vol)
	hi := math.Max(o, c) + round(math.Abs(rng.NormFloat64())*c*0.001, in.tick)
	lo := math.Max(math.Min(o, c)-round(math.Abs(rng.NormFloat64())*c*0.001, in.tick), in.tick)
	row := []any{ts.Format("2006-01-02T15:04:05-0700"), o, hi, lo, c, rng.IntN(500*minutes) * max(in.lot, 1)}
	if withOI {
		oi := 0
		if in.itype != "EQ" {
			oi = stable(in.token, 10, 5000) * in.lot
		}
		row = append(row, oi)
	}
	return row
}

// ---- ticker socket ----

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("api_key") != s.apiKey || q.Get("access_token") != s.token {
		http.Error(w, "invalid api_key or access_token", http.StatusForbidden)
		return
	}
	c, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	slog.Info("fakekite: socket open", "remote", r.RemoteAddr)

	var mu sync.Mutex
	subs := map[uint32]byte{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var msg struct {
				A string          `json:"a"`
				V json.RawMessage `json:"v"`
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			mu.Lock()
			switch msg.A {
			case "subscribe":
				var ts []uint32
				if json.Unmarshal(msg.V, &ts) == nil {
					for _, t := range ts {
						if _, ok := subs[t]; !ok {
							subs[t] = 'q'
						}
					}
				}
			case "unsubscribe":
				var ts []uint32
				if json.Unmarshal(msg.V, &ts) == nil {
					for _, t := range ts {
						delete(subs, t)
					}
				}
			case "mode":
				var v []json.RawMessage
				var mode string
				var ts []uint32
				if json.Unmarshal(msg.V, &v) == nil && len(v) == 2 && json.Unmarshal(v[0], &mode) == nil && json.Unmarshal(v[1], &ts) == nil {
					b := map[string]byte{"ltp": 'l', "quote": 'q', "full": 'f'}[mode]
					for _, t := range ts {
						if b != 0 {
							subs[t] = b
						}
					}
				}
			}
			mu.Unlock()
		}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var orders <-chan time.Time
	if s.orderEvery > 0 {
		ot := time.NewTicker(s.orderEvery)
		defer ot.Stop()
		orders = ot.C
	}
	seq := 0
	for {
		select {
		case <-done:
			slog.Info("fakekite: socket closed", "remote", r.RemoteAddr)
			return
		case <-orders:
			seq++
			now := clock.Now()
			body, _ := json.Marshal(map[string]any{"type": "order", "data": map[string]any{
				"order_id": fmt.Sprintf("FAKE-%d-%d", now.Unix(), seq), "status": "COMPLETE", "tradingsymbol": "NIFTYFAKE",
				"exchange": "NFO", "transaction_type": "BUY", "quantity": 75, "filled_quantity": 75, "average_price": 100,
				"order_timestamp": now.Format(time.DateTime), "exchange_timestamp": now.Format(time.DateTime),
			}})
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if c.WriteMessage(websocket.TextMessage, body) != nil {
				return
			}
		case <-tick.C:
			now := clock.Now()
			mu.Lock()
			var pkts []kite.Packet
			for t, mode := range subs {
				if in := s.m.byTok[t]; in != nil {
					pkts = append(pkts, s.m.snap(in, now).packet(mode, now))
				}
			}
			mu.Unlock()
			c.SetWriteDeadline(time.Now().Add(5 * time.Second))
			var err error
			if len(pkts) == 0 {
				err = c.WriteMessage(websocket.BinaryMessage, []byte{0}) // heartbeat
			} else {
				for i := 0; i < len(pkts) && err == nil; i += 500 {
					err = c.WriteMessage(websocket.BinaryMessage, kite.Encode(pkts[i:min(i+500, len(pkts))]))
				}
			}
			if err != nil {
				return
			}
		}
	}
}
