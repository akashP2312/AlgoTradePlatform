package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/store"
)

func apiGet(t *testing.T, a *API, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	a.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: body %q: %v", path, w.Body.String(), err)
	}
	return w.Code, out
}

func TestAPIValidation(t *testing.T) {
	a := &API{} // no pool: valid requests get 503, invalid ones 400 first
	cases := []struct {
		path string
		code int
	}{
		{"/api/ref", 503},
		{"/api/bars?c=NFO|NIFTY|2026-10-27||FUT", 503},
		{"/api/bars?c=NFO|NIFTY|2026-10-27||FUT&days=10", 503},
		{"/api/bars", 400},
		{"/api/bars?c=nonsense", 400},
		{"/api/bars?c=NFO|NIFTY|2026-10-27||FUT&days=0", 400},
		{"/api/bars?c=NFO|NIFTY|2026-10-27||FUT&days=11", 400},
		{"/api/straddle?und=NIFTY&x=2026-10-27", 503},
		{"/api/straddle?und=NIFTY&x=2026-10-27&k=25000&days=3", 503},
		{"/api/straddle?und=nifty;drop&x=2026-10-27", 400},
		{"/api/straddle?und=NIFTY&x=27-10-2026", 400},
		{"/api/straddle?und=NIFTY&x=2026-10-27&k=-1", 400},
		{"/api/oi0?und=NIFTY&x=2026-10-27", 503},
		{"/api/oi0?und=NIFTY", 400},
		{"/api/oi0?und=nifty;drop&x=2026-10-27", 400},
	}
	for _, c := range cases {
		if code, body := apiGet(t, a, c.path); code != c.code {
			t.Errorf("%s: %d %v, want %d", c.path, code, body, c.code)
		}
	}
	mux := http.NewServeMux()
	a.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/ref", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/ref: %d, want 405", w.Code)
	}
}

// TestAPIQueries runs against a disposable database (PMT_TEST_DB) with rows for a
// made-up underlying, removed afterwards.
func TestAPIQueries(t *testing.T) {
	url := os.Getenv("PMT_TEST_DB")
	if url == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	ctx := context.Background()
	rw, err := store.Open(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := store.Migrate(ctx, rw); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		rw.Exec(ctx, `DELETE FROM bars_1m WHERE contract LIKE 'NFO|ZZT|%'`)
		rw.Exec(ctx, `DELETE FROM chain_snapshots WHERE underlying = 'ZZT'`)
		rw.Exec(ctx, `DELETE FROM eod_contracts WHERE underlying = 'ZZT'`)
	}
	clean()
	defer clean()

	today := clock.Today()
	prev := today.AddDate(0, 0, -1)
	at := func(d time.Time, hh, mm int) time.Time {
		return d.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	}
	fut := "NFO|ZZT|2099-01-29||FUT"
	for _, b := range []struct {
		ts     time.Time
		source string
		close  float64
	}{
		{at(prev, 9, 15), "live", 99},
		{at(today, 9, 15), "live", 100},
		{at(today, 9, 15), "kite", 101}, // preferred over live
		{at(today, 9, 16), "live", 102},
	} {
		if _, err := rw.Exec(ctx, `INSERT INTO bars_1m (ts, contract, source, open, high, low, close, volume, oi)
			VALUES ($1,$2,$3,$4,$4,$4,$4,10,NULL)`, b.ts, fut, b.source, b.close); err != nil {
			t.Fatal(err)
		}
	}
	// Two snapshots per session; forward 1010 makes 1000 ATM at first, then 1020
	// (forward 1018) at 09:16 today.
	snap := func(ts time.Time, fwd float64, k float64, ce, pe float64) {
		for _, leg := range []struct {
			typ string
			mid float64
		}{{"CE", ce}, {"PE", pe}} {
			c := "NFO|ZZT|2099-01-29|" + jsonNum(k) + "|" + leg.typ
			if _, err := rw.Exec(ctx, `INSERT INTO chain_snapshots (ts, contract, source, underlying, expiry, strike,
				opt_type, bid, ask, forward) VALUES ($1,$2,'ws','ZZT','2099-01-29',$3,$4,$5,$6,$7)`,
				ts, c, k, leg.typ, leg.mid-0.5, leg.mid+0.5, fwd); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range []time.Time{prev, today} {
		snap(at(d, 9, 15), 1010, 1000, 30, 20)
		snap(at(d, 9, 15), 1010, 1020, 18, 28)
		snap(at(d, 9, 16), 1018, 1000, 33, 17)
		snap(at(d, 9, 16), 1018, 1020, 20, 25)
	}

	// The same quotes on another expiry with forward left empty, as the recorder
	// writes them: parity gives 1010 at 09:15 and 1015 (1020+20-25) at 09:16.
	if _, err := rw.Exec(ctx, `INSERT INTO chain_snapshots (ts, contract, source, underlying, expiry, strike,
		opt_type, bid, ask) SELECT ts, replace(contract, '2099-01-29', '2099-03-26'), source, underlying,
		'2099-03-26', strike, opt_type, bid, ask FROM chain_snapshots WHERE underlying = 'ZZT'`); err != nil {
		t.Fatal(err)
	}

	ro, err := store.OpenReadOnly(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Exec(ctx, `DELETE FROM bars_1m WHERE false`); err == nil {
		t.Error("read-only pool accepted a write")
	}
	a := &API{}
	a.SetPool(ro)

	code, body := apiGet(t, a, "/api/bars?c="+fut)
	bars, _ := body["bars"].([]any)
	if code != 200 || len(bars) != 2 {
		t.Fatalf("bars today: %d %v", code, body)
	}
	if c := bars[0].([]any)[4].(float64); c != 101 {
		t.Errorf("09:15 close %v, want the kite bar 101", c)
	}
	if _, body = apiGet(t, a, "/api/bars?c="+fut+"&days=2"); len(body["bars"].([]any)) != 3 {
		t.Errorf("bars 2 days: %v", body)
	}

	code, body = apiGet(t, a, "/api/straddle?und=ZZT&x=2099-01-29&days=1")
	if code != 200 {
		t.Fatalf("straddle: %d %v", code, body)
	}
	if body["k"].(float64) != 1000 {
		t.Errorf("fixed strike %v, want today's opening ATM 1000", body["k"])
	}
	ss := body["sessions"].([]any)
	if len(ss) != 2 {
		t.Fatalf("sessions %v", ss)
	}
	pts := ss[1].(map[string]any)["pts"].([]any)
	want := [][]float64{{1010, 1000, 50, 50}, {1018, 1020, 45, 50}}
	for i, w := range want {
		p := pts[i].([]any)
		for j, v := range w {
			if p[j+1].(float64) != v {
				t.Errorf("today pt %d = %v, want %v", i, p, w)
				break
			}
		}
	}
	if _, body = apiGet(t, a, "/api/straddle?und=ZZT&x=2099-01-29&k=1020"); body["sessions"].([]any)[0].(map[string]any)["pts"].([]any)[0].([]any)[4].(float64) != 46 {
		t.Errorf("k=1020: %v", body)
	}
	_, body = apiGet(t, a, "/api/straddle?und=ZZT&x=2099-03-26&days=1")
	if ss, _ := body["sessions"].([]any); len(ss) != 2 {
		t.Errorf("parity forward: %v", body)
	} else if p := ss[1].(map[string]any)["pts"].([]any); p[0].([]any)[1].(float64) != 1010 || p[1].([]any)[1].(float64) != 1015 || p[1].([]any)[2].(float64) != 1020 {
		t.Errorf("parity forward pts: %v", p)
	}
	if _, body = apiGet(t, a, "/api/straddle?und=ZZT&x=2099-02-26"); len(body["sessions"].([]any)) != 0 {
		t.Errorf("no data: %v", body)
	}
}

func TestAPIOI0(t *testing.T) {
	url := os.Getenv("PMT_TEST_DB")
	if url == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	ctx := context.Background()
	rw, err := store.Open(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	if _, err := store.Migrate(ctx, rw); err != nil {
		t.Fatal(err)
	}
	clean := func() {
		rw.Exec(ctx, `DELETE FROM chain_snapshots WHERE underlying = 'ZZO'`)
		rw.Exec(ctx, `DELETE FROM eod_contracts WHERE underlying = 'ZZO'`)
	}
	clean()
	defer clean()
	today := clock.Today()
	prev := today.AddDate(0, 0, -1)
	snap := func(ts time.Time, k float64, typ string, oi int64) {
		c := "NFO|ZZO|2099-01-29|" + jsonNum(k) + "|" + typ
		if _, err := rw.Exec(ctx, `INSERT INTO chain_snapshots (ts, contract, source, underlying, expiry, strike,
			opt_type, oi) VALUES ($1,$2,'ws','ZZO','2099-01-29',$3,$4,$5)`, ts, c, k, typ, oi); err != nil {
			t.Fatal(err)
		}
	}
	snap(prev.Add(10*time.Hour), 1000, "CE", 100)
	snap(prev.Add(15*time.Hour), 1000, "CE", 150) // the session's last: the base
	snap(prev.Add(15*time.Hour), 1000, "PE", 90)
	snap(prev.Add(15*time.Hour), 1020, "PE", 40)
	snap(today.Add(10*time.Hour), 1000, "CE", 999) // today's: not the base

	ro, err := store.OpenReadOnly(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	a := &API{}
	a.SetPool(ro)

	_, body := apiGet(t, a, "/api/oi0?und=ZZO&x=2099-01-29")
	if body["src"] != "snap" || body["d"] != prev.Format(time.DateOnly) {
		t.Fatalf("snapshot base: %v", body)
	}
	if got, _ := json.Marshal(body["rows"]); string(got) != `[[1000,150,90],[1020,null,40]]` {
		t.Errorf("snapshot rows %s", got)
	}

	// A loaded bhavcopy wins over the snapshots.
	for _, e := range []struct {
		k   float64
		typ string
		oi  int64
	}{{1000, "CE", 160}, {1000, "PE", 95}} {
		c := "NFO|ZZO|2099-01-29|" + jsonNum(e.k) + "|" + e.typ
		if _, err := rw.Exec(ctx, `INSERT INTO eod_contracts (trade_date, contract, exchange, underlying, expiry,
			strike, kind, oi, volume_unit, source) VALUES ($1,$2,'NFO','ZZO','2099-01-29',$3,$4,$5,'contracts','test')`,
			prev, c, e.k, e.typ, e.oi); err != nil {
			t.Fatal(err)
		}
	}
	_, body = apiGet(t, a, "/api/oi0?und=ZZO&x=2099-01-29")
	if got, _ := json.Marshal(body["rows"]); body["src"] != "eod" || string(got) != `[[1000,160,95]]` {
		t.Errorf("eod base: %v", body)
	}
	if _, body = apiGet(t, a, "/api/oi0?und=ZZO&x=2099-02-26"); body["d"] != nil || len(body["rows"].([]any)) != 0 {
		t.Errorf("no data: %v", body)
	}
}

func jsonNum(k float64) string { b, _ := json.Marshal(k); return string(b) }

func TestMarkLive(t *testing.T) {
	ch := func(xs ...string) []refChain {
		out := make([]refChain, len(xs))
		for i, x := range xs {
			out[i] = refChain{Exp: x}
		}
		return out
	}
	chains := map[string][]refChain{
		// Yesterday's expiry is still listed; 12-01 is a weekly past wide_expiries.
		"NIFTY":  ch("2026-10-19", "2026-10-27", "2026-11-03", "2026-11-23", "2026-12-01", "2026-12-29", "2027-03-30"),
		"SENSEX": ch("2026-10-22"),
	}
	univ := []config.Underlying{{Name: "NIFTY", WideExpiries: 2, Monthlies: 3}}
	markLive(chains, univ, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	var got []string
	for _, c := range chains["NIFTY"] {
		if c.Live {
			got = append(got, c.Exp)
		}
	}
	if want := "[2026-10-27 2026-11-03 2026-11-23 2026-12-29]"; fmt.Sprint(got) != want {
		t.Errorf("live NIFTY chains %v, want %s", got, want)
	}
	if chains["SENSEX"][0].Live {
		t.Errorf("an underlying the ticker does not stream is marked live")
	}
}
