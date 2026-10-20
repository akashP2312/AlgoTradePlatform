package harvest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
	"pmt/internal/store"
)

var today = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

func ist(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, clock.IST) }

func utc(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func cal(t *testing.T) *calendar.Calendar {
	t.Helper()
	c, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWindows(t *testing.T) {
	c := cal(t)
	ws := Windows(c, ist(2026, 10, 19), 180, 60)
	want := [][2]string{{"2026-08-21", "2026-10-19"}, {"2026-06-22", "2026-08-20"}, {"2026-04-23", "2026-06-21"}}
	if len(ws) != len(want) {
		t.Fatalf("%d windows, want %d", len(ws), len(want))
	}
	for i, w := range ws {
		if got := [2]string{w.From.Format("2006-01-02"), w.To.Format("2006-01-02")}; got != want[i] {
			t.Errorf("window %d is %v, want %v", i, got, want[i])
		}
		for _, d := range w.Days {
			if !c.IsTradingDay(d) || d.Before(w.From) || d.After(w.To) {
				t.Errorf("window %d lists %s", i, d.Format("2006-01-02"))
			}
		}
	}
	for _, d := range ws[0].Days {
		if d.Format("2006-01-02") == "2026-10-02" {
			t.Error("Gandhi Jayanti listed as a trading day")
		}
	}
	if n := len(ws[0].Days); n < 38 || n > 44 {
		t.Errorf("%d trading days in the newest window", n)
	}

	if ws := Windows(c, ist(2026, 10, 19), 10, 60); len(ws) != 1 || ws[0].From.Format("2006-01-02") != "2026-10-10" {
		t.Errorf("short lookback gave %+v", ws)
	}
	if ws := Windows(c, ist(2026, 10, 19), 0, 60); ws != nil {
		t.Errorf("zero lookback gave %d windows", len(ws))
	}
	// A block of weekends and holidays still comes back, with no days.
	ws = Windows(c, ist(2026, 10, 18), 2, 1)
	if len(ws) != 2 || len(ws[0].Days)+len(ws[1].Days) != 0 {
		t.Errorf("weekend windows %+v", ws)
	}
}

func TestCountByDayUsesIST(t *testing.T) {
	cs := []kite.Candle{
		{TS: time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC)}, // 03:30 IST on the 19th
		{TS: time.Date(2026, 10, 19, 9, 15, 0, 0, clock.IST)},
		{TS: time.Date(2026, 10, 19, 18, 31, 0, 0, time.UTC)}, // 00:01 IST on the 20th
		{TS: time.Date(2026, 10, 16, 15, 29, 0, 0, clock.IST)},
	}
	got := countByDay(cs)
	if got["2026-10-19"] != 2 || got["2026-10-20"] != 1 || got["2026-10-16"] != 1 || len(got) != 3 {
		t.Errorf("countByDay = %v", got)
	}
}

func TestDedupe(t *testing.T) {
	a := time.Date(2026, 10, 19, 9, 15, 0, 0, clock.IST)
	cs := dedupe([]kite.Candle{{TS: a, C: 1}, {TS: a.Add(time.Minute), C: 2}, {TS: a.UTC(), C: 3}})
	if len(cs) != 2 || cs[0].C != 3 || cs[1].C != 2 {
		t.Errorf("dedupe = %+v", cs)
	}
}

func TestCovered(t *testing.T) {
	days := []time.Time{ist(2026, 10, 15), ist(2026, 10, 16)}
	for _, tc := range []struct {
		log         map[string]string
		done, empty bool
	}{
		{map[string]string{"2026-10-15": "ok", "2026-10-16": "empty"}, true, false},
		{map[string]string{"2026-10-15": "empty", "2026-10-16": "empty"}, true, true},
		{map[string]string{"2026-10-15": "ok", "2026-10-16": "error"}, false, false},
		{map[string]string{"2026-10-15": "ok"}, false, false},
	} {
		if done, empty := covered(tc.log, days); done != tc.done || empty != tc.empty {
			t.Errorf("covered(%v) = %v, %v", tc.log, done, empty)
		}
	}
}

func TestLateDays(t *testing.T) {
	h := &Harvester{Cal: cal(t)}
	mcx := Target{Inst: refdata.Inst{Instrument: contract.Instrument{Exchange: "MCX"}}}
	nfo := Target{Inst: refdata.Inst{Instrument: contract.Instrument{Exchange: "NFO"}}}
	if d := h.days(mcx, ist(2026, 10, 19)); len(d) != 2 || d[0].Format("2006-01-02") != "2026-10-16" {
		t.Errorf("MCX days %v", d)
	}
	if d := h.days(nfo, ist(2026, 10, 19)); len(d) != 1 {
		t.Errorf("NFO days %v", d)
	}
	h.Late = map[string]bool{}
	if d := h.days(mcx, ist(2026, 10, 19)); len(d) != 1 {
		t.Errorf("MCX days with Late cleared %v", d)
	}
}

func TestTargets(t *testing.T) {
	rows := []contract.Instrument{
		{Token: 1, TradingSymbol: "NIFTY 50", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 2, TradingSymbol: "INDIA VIX", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 3, TradingSymbol: "RELIANCE", Segment: "NSE", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 10, TradingSymbol: "NIFTY26NOVFUT", Name: "NIFTY", Expiry: utc(2026, 11, 24), InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"},
		{Token: 11, TradingSymbol: "NIFTY26OCTFUT", Name: "NIFTY", Expiry: utc(2026, 10, 27), InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"},
		{Token: 30, TradingSymbol: "BANKNIFTY26OCTFUT", Name: "BANKNIFTY", Expiry: utc(2026, 10, 27), InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"},
		{Token: 40, TradingSymbol: "CRUDEOIL26NOVFUT", Name: "CRUDEOIL", Expiry: utc(2026, 11, 19), InstrumentType: "FUT", Segment: "MCX-FUT", Exchange: "MCX"},
	}
	opt := func(tok uint32, exp time.Time, k float64, typ string) {
		rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: fmt.Sprintf("NIFTY%s%.0f%s", exp.Format("06Jan02"), k, typ),
			Name: "NIFTY", Expiry: exp, Strike: k, InstrumentType: typ, Segment: "NFO-OPT", Exchange: "NFO"})
	}
	opt(20, utc(2026, 10, 13), 24000, "CE") // expired
	opt(21, utc(2026, 10, 27), 24000, "CE")
	opt(22, utc(2026, 10, 20), 24050, "PE")
	opt(23, utc(2026, 10, 20), 24000, "PE")
	opt(24, utc(2026, 10, 20), 24000, "CE")
	rows = append(rows, contract.Instrument{Token: 31, TradingSymbol: "BANKNIFTY26OCT50000CE", Name: "BANKNIFTY",
		Expiry: utc(2026, 10, 27), Strike: 50000, InstrumentType: "CE", Segment: "NFO-OPT", Exchange: "NFO"})

	u := config.Universe{
		Underlyings: []config.Underlying{
			{Name: "NIFTY", Index: "NSE:NIFTY 50", DerivExchange: "NFO", Harvest: true},
			{Name: "BANKNIFTY", Index: "NSE:NIFTY BANK", DerivExchange: "NFO"},
		},
		Extras: []string{"NSE:INDIA VIX", "MCX:CRUDEOIL:FUT:1", "CDS:USDINR:FUT:1", "NSE:NIFTY 50"},
	}
	ts, warn := Targets(u, refdata.Resolve(rows), []string{"RELIANCE", "NOPE"}, today)
	var got []string
	for _, tg := range ts {
		got = append(got, fmt.Sprintf("%d/%s", tg.Token, tg.Group))
	}
	want := []string{"1/NIFTY", "11/NIFTY", "10/NIFTY", "2/extras", "40/extras", "3/constituents", "24/NIFTY", "23/NIFTY", "22/NIFTY", "21/NIFTY"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("targets\n got %v\nwant %v", got, want)
	}
	if len(warn) != 2 || !strings.Contains(warn[0], "USDINR") || !strings.Contains(warn[1], "NOPE") {
		t.Errorf("warnings %q", warn)
	}
}

// fakeHist serves canned candles, failing a token's first fail[token] requests or
// every request with a token error.
type fakeHist struct {
	mu       sync.Mutex
	minute   map[uint32][]kite.Candle
	day      map[uint32][]kite.Candle
	fail     map[uint32]int
	tokenErr bool
	calls    int
	oi       map[uint32]bool
}

func (f *fakeHist) Historical(ctx context.Context, token uint32, interval string, from, to time.Time, oi bool) ([]kite.Candle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.tokenErr {
		return nil, kiteconnect.Error{Code: 403, ErrorType: kiteconnect.TokenError, Message: "Invalid token"}
	}
	if f.fail[token] > 0 {
		f.fail[token]--
		return nil, errors.New("fake: upstream 502")
	}
	if interval == "minute" {
		f.oi[token] = oi
	}
	src := f.minute
	if interval == "day" {
		src = f.day
	}
	var out []kite.Candle
	for _, c := range src[token] {
		if !c.TS.Before(from) && !c.TS.After(to) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeHist) reset() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.calls
	f.calls = 0
	return n
}

func TestHarvestDB(t *testing.T) {
	url := os.Getenv("PMT_TEST_DB")
	if url == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		for _, q := range []string{
			`DELETE FROM bars_1m WHERE contract LIKE '%HVTEST%'`,
			`DELETE FROM harvest_log WHERE contract LIKE '%HVTEST%'`,
			`DELETE FROM index_eod WHERE contract LIKE '%HVTEST%'`,
			`DELETE FROM recorder_gaps WHERE kind = 'harvest' AND underlying = 'HVTEST'`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	clean()
	defer clean()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	status := func(c string, d time.Time) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM harvest_log WHERE contract = $1 AND trade_date = $2`, c, d).Scan(&s); err != nil {
			t.Fatalf("harvest_log %s %s: %v", c, d.Format("2006-01-02"), err)
		}
		return s
	}

	c := cal(t)
	exp := utc(2026, 10, 27)
	rows := []contract.Instrument{
		{Token: 9001, TradingSymbol: "HVTEST IDX", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 9002, TradingSymbol: "HVTEST26OCTFUT", Name: "HVTEST", Expiry: exp, InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"},
	}
	tok := uint32(9010)
	for _, k := range []float64{100, 110} {
		for _, typ := range []string{"CE", "PE"} {
			rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: fmt.Sprintf("HVTEST26OCT%.0f%s", k, typ), Name: "HVTEST",
				Expiry: exp, Strike: k, InstrumentType: typ, Segment: "NFO-OPT", Exchange: "NFO"})
			tok++
		}
	}
	u := config.Universe{Underlyings: []config.Underlying{{Name: "HVTEST", Index: "NSE:HVTEST IDX", DerivExchange: "NFO", Harvest: true}}}
	targets, warn := Targets(u, refdata.Resolve(rows), nil, today)
	if len(targets) != 6 || len(warn) != 0 {
		t.Fatalf("%d targets, warnings %q", len(targets), warn)
	}
	byTok := map[uint32]Target{}
	for _, tg := range targets {
		byTok[tg.Token] = tg
	}
	idx, fut, ce100, pe100, ce110, pe110 := byTok[9001], byTok[9002], byTok[9010], byTok[9011], byTok[9012], byTok[9013]
	if !strings.Contains(idx.Contract, "HVTEST") || idx.Key.Kind != contract.IDX {
		t.Fatalf("index target %q %v", idx.Contract, idx.Key.Kind)
	}

	// Three candles a session from each contract's listing day; 110 CE never trades.
	f := &fakeHist{minute: map[uint32][]kite.Candle{}, day: map[uint32][]kite.Candle{}, fail: map[uint32]int{}, oi: map[uint32]bool{}}
	listed := map[uint32]time.Time{9001: ist(2026, 4, 1), 9002: ist(2026, 7, 28), 9010: ist(2026, 9, 1), 9011: ist(2026, 10, 1), 9013: ist(2026, 9, 15)}
	for token, from := range listed {
		for i, d := range c.TradingDays(from, ist(2026, 10, 19)) {
			for _, hm := range [][2]int{{9, 15}, {9, 16}, {15, 29}} {
				p := 100 + float64(i)
				f.minute[token] = append(f.minute[token], kite.Candle{TS: clock.At(d, hm[0], hm[1], 0), O: p, H: p + 1, L: p - 1, C: p, V: 10, OI: 500})
			}
			if token == 9001 {
				f.day[token] = append(f.day[token], kite.Candle{TS: d, O: 100, H: 110, L: 90, C: 105})
			}
		}
	}
	h := &Harvester{Pool: pool, Hist: f, Cal: c, Workers: 3, RetryDelay: time.Millisecond}
	date := ist(2026, 10, 19)

	// Nightly run: 110 PE fails twice, so it fails after its retry.
	f.fail[9013] = 2
	s, err := h.Day(ctx, targets, date)
	if err != nil {
		t.Fatal(err)
	}
	if s.Targets != 6 || s.OK != 4 || s.Empty != 1 || s.Failed != 1 || s.Rows != 12 || s.EOD != 1 || s.Requests != 8 {
		t.Errorf("first run %s", s)
	}
	if f.oi[9001] || !f.oi[9002] || !f.oi[9010] {
		t.Errorf("oi requested: %v", f.oi)
	}
	if n := count(`SELECT count(*) FROM bars_1m WHERE contract = $1 AND source = 'kite' AND oi IS NULL`, idx.Contract); n != 3 {
		t.Errorf("%d index bars without oi, want 3", n)
	}
	if n := count(`SELECT count(*) FROM bars_1m WHERE contract = $1 AND oi = 500`, fut.Contract); n != 3 {
		t.Errorf("%d future bars with oi, want 3", n)
	}
	if s := status(ce110.Contract, date); s != "empty" {
		t.Errorf("110 CE logged %s", s)
	}
	if s := status(pe110.Contract, date); s != "error" {
		t.Errorf("110 PE logged %s", s)
	}
	if n := count(`SELECT count(*) FROM index_eod WHERE contract = $1 AND source = 'kite'`, idx.Contract); n != 1 {
		t.Errorf("%d index_eod rows", n)
	}
	var minutes, filled int
	var detail string
	gap := `SELECT minutes, filled_minutes, detail FROM recorder_gaps WHERE kind = 'harvest' AND underlying = 'HVTEST' AND trade_date = $1`
	if err := pool.QueryRow(ctx, gap, date).Scan(&minutes, &filled, &detail); err != nil {
		t.Fatal(err)
	}
	if minutes != 375 || filled != 0 || !strings.HasPrefix(detail, "1/6 contracts failed: "+pe110.Contract) {
		t.Errorf("gap %d/%d %q", filled, minutes, detail)
	}

	// Rerun: only 110 PE is fetched, and the gap closes.
	f.reset()
	if s, err = h.Day(ctx, targets, date); err != nil {
		t.Fatal(err)
	}
	if n := f.reset(); n != 1 || s.Skipped != 5 || s.OK != 1 || s.Failed != 0 {
		t.Errorf("rerun made %d requests: %s", n, s)
	}
	if err := pool.QueryRow(ctx, gap, date).Scan(&minutes, &filled, &detail); err != nil {
		t.Fatal(err)
	}
	if filled != minutes || !strings.Contains(detail, "filled on rerun") {
		t.Errorf("gap after rerun %d/%d %q", filled, minutes, detail)
	}

	// Forced rerun rewrites the same rows.
	h.Force = true
	if _, err = h.Day(ctx, targets, date); err != nil {
		t.Fatal(err)
	}
	h.Force = false
	f.reset()
	if n := count(`SELECT count(*) FROM bars_1m WHERE contract LIKE '%HVTEST%'`); n != 15 {
		t.Errorf("%d bars after the forced rerun, want 15", n)
	}

	// Backfill: the future fails its first window and is retried on the next run.
	f.fail[9002] = 2
	s, err = h.Backfill(ctx, targets, date, 180, 60)
	if err != nil {
		t.Fatal(err)
	}
	// index 3 windows + 3 day calls; future 2 (both attempts fail); 100 CE, 100 PE and
	// 110 PE 2 each (the second window is empty, which ends the walk back); 110 CE 1.
	if n := f.reset(); n != 15 || s.Failed != 1 {
		t.Errorf("backfill made %d requests: %s", n, s)
	}
	if s := status(fut.Contract, date); s != "ok" {
		t.Errorf("failed backfill window overwrote the future's ok day: %s", s)
	}
	if s := status(fut.Contract, ist(2026, 10, 16)); s != "error" {
		t.Errorf("future 16 Oct logged %s", s)
	}
	s, err = h.Backfill(ctx, targets, date, 180, 60)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.reset(); n != 3 || s.Failed != 0 {
		t.Errorf("backfill resume made %d requests, want the future's 3: %s", n, s)
	}
	if _, err = h.Backfill(ctx, targets, date, 180, 60); err != nil {
		t.Fatal(err)
	}
	if n := f.reset(); n != 0 {
		t.Errorf("complete backfill made %d requests", n)
	}
	sessions := len(c.TradingDays(ist(2026, 4, 23), date))
	if n := count(`SELECT count(*) FROM bars_1m WHERE contract = $1`, idx.Contract); n != 3*sessions {
		t.Errorf("%d index bars, want %d", n, 3*sessions)
	}
	if n := count(`SELECT count(*) FROM index_eod WHERE contract = $1`, idx.Contract); n != sessions {
		t.Errorf("%d index_eod rows, want %d", n, sessions)
	}
	if n := count(`SELECT count(*) FROM bars_1m WHERE contract = $1`, fut.Contract); n != 3*len(c.TradingDays(ist(2026, 7, 28), date)) {
		t.Errorf("%d future bars", n)
	}
	for _, tc := range []struct {
		t    Target
		from time.Time
	}{{ce100, ist(2026, 9, 1)}, {pe100, ist(2026, 10, 1)}, {pe110, ist(2026, 9, 15)}} {
		if n := count(`SELECT count(*) FROM bars_1m WHERE contract = $1`, tc.t.Contract); n != 3*len(c.TradingDays(tc.from, date)) {
			t.Errorf("%s has %d bars", tc.t.Contract, n)
		}
	}
	if n := count(`SELECT count(*) FROM harvest_log WHERE contract = $1 AND status = 'empty'`, ce110.Contract); n != len(c.TradingDays(ist(2026, 8, 21), date)) {
		t.Errorf("110 CE has %d empty days", n)
	}

	// An expired token stops the run, and the gap says so.
	f.tokenErr = true
	h.Force = true
	prev := ist(2026, 10, 16)
	_, err = h.Day(ctx, targets, prev)
	if !kite.IsTokenError(err) {
		t.Fatalf("token error run returned %v", err)
	}
	if n := f.reset(); n > h.Workers {
		t.Errorf("%d requests after a token error", n)
	}
	if err := pool.QueryRow(ctx, gap, prev).Scan(&minutes, &filled, &detail); err != nil {
		t.Fatal(err)
	}
	if filled != 0 || !strings.Contains(detail, "run stopped") {
		t.Errorf("token-error gap %d/%d %q", filled, minutes, detail)
	}
}
