package main

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
	"pmt/internal/refdata"
)

// TestAgainstClient drives fakekite with the real REST client and ticker
// socket, so a change to either side's wire format fails here first.
func TestAgainstClient(t *testing.T) {
	m := testMarket(t, clock.Today())
	s := &server{m: m, apiKey: "k", token: "tok"}
	hs := httptest.NewServer(s.routes())
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := config.Kite{RootURL: hs.URL, QuotePerSec: 10, HistoricalPerSec: 10, OtherPerSec: 10}

	// The login exchange hands out the fake token.
	sess, err := kite.NewREST(cfg, "k", "").Exchange(ctx, "fake-request-token", "fake-secret")
	if err != nil || sess.AccessToken != "tok" {
		t.Fatalf("exchange: %v %q", err, sess.AccessToken)
	}

	rest := kite.NewREST(cfg, "k", "tok")
	raw, err := rest.FetchInstruments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := kite.ParseInstruments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(m.insts) || len(refdata.Resolve(rows)) == 0 {
		t.Fatalf("%d rows, %d instruments", len(rows), len(m.insts))
	}

	ltp, err := rest.LTP(ctx, []string{"NSE:NIFTY 50", "NSE:INDIA VIX"})
	if err != nil || ltp["NSE:NIFTY 50"] < 20000 || ltp["NSE:INDIA VIX"] <= 0 {
		t.Fatalf("ltp %v %v", ltp, err)
	}
	var opt *inst
	for _, in := range m.insts {
		if in.itype == "CE" && in.name == "NIFTY" {
			opt = in
			break
		}
	}
	qs, err := rest.Quotes(ctx, []string{key(opt)})
	if err != nil || qs[key(opt)].LTP <= 0 {
		t.Fatalf("quote %v %v", qs, err)
	}

	from := clock.Date(clock.Now()).AddDate(0, 0, -7)
	cs, err := rest.Historical(ctx, opt.token, "minute", from, from.AddDate(0, 0, 3), true)
	if err != nil || len(cs) == 0 {
		t.Fatalf("historical: %d candles, %v", len(cs), err)
	}

	// An order call is refused and is not mistaken for a token error.
	if _, err := rest.HTTP.Post(hs.URL+"/orders/regular", "application/x-www-form-urlencoded", strings.NewReader("")); err == nil || !errors.Is(err, kite.ErrRefused) {
		t.Fatalf("order post: %v", err)
	}
	// And fakekite refuses it on its own, past the guard.
	for _, c := range []struct{ method, path string }{{"POST", "/orders/regular"}, {"DELETE", "/session/token"}, {"PUT", "/orders/regular/1"}} {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "PermissionException") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}

	var got atomic.Int64
	ws := &kite.WS{Name: "t", URL: "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws", APIKey: "k",
		Token: func() string { return "tok" }, OnPacket: func(p kite.Packet, _ time.Time) {
			if p.Token == opt.token && p.LTP > 0 && p.Depth != nil {
				got.Add(1)
			}
		}}
	if err := ws.Set(map[uint32]byte{opt.token: 'f', m.byKey["NSE:NIFTY 50"].token: 'f'}); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	go ws.Run(wctx)
	defer wcancel()
	for got.Load() == 0 {
		if ctx.Err() != nil {
			t.Fatal("no full packet from the socket")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func testMarket(t *testing.T, today time.Time) *market {
	t.Helper()
	cal, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := calendar.LoadRules("../../config/expiry_rules.csv")
	if err != nil {
		t.Fatal(err)
	}
	return newMarket(today, 0.18, cal, rules)
}

// TestHolidayExpiries: expiries on a holiday move to the trading day before
// (2026-11-10 to 11-09, the 11-24 monthly to 11-23), and the dump never lists a
// holiday as an expiry.
func TestHolidayExpiries(t *testing.T) {
	day := func(s string) time.Time {
		d, _ := time.ParseInLocation(time.DateOnly, s, clock.IST)
		return d
	}
	m := testMarket(t, day("2026-10-19"))
	got := map[string][]string{}
	for _, in := range m.insts {
		if in.expiry.IsZero() {
			continue
		}
		if !m.cal.IsTradingDay(in.expiry) {
			t.Errorf("%s expires on %s, not a trading day", in.symbol, in.expiry.Format(time.DateOnly))
		}
		k := in.name + " " + in.itype
		if d := in.expiry.Format(time.DateOnly); !slices.Contains(got[k], d) {
			got[k] = append(got[k], d)
		}
	}
	for k, want := range map[string][]string{
		// 10-20 (Dussehra) moves to 10-19, which is today, so it is still listed;
		// 10-27 is the monthly, so the four weeklies run to 11-17.
		"NIFTY CE":     {"2026-10-19", "2026-10-27", "2026-11-03", "2026-11-09", "2026-11-17", "2026-11-23", "2026-12-29"},
		"NIFTY FUT":    {"2026-10-27", "2026-11-23", "2026-12-29"},
		"BANKNIFTY PE": {"2026-10-27", "2026-11-23", "2026-12-29"},
		"USDINR FUT":   {"2026-10-27"},
	} {
		slices.Sort(got[k])
		if !slices.Equal(got[k], want) {
			t.Errorf("%s: %v, want %v", k, got[k], want)
		}
	}
}

// TestSmileArbFree checks the smile's call prices fall and stay convex across
// strikes at every tenor fakekite lists, puts below the money carry more vol than
// calls above it, and the ATM vol is the walk's.
func TestSmileArbFree(t *testing.T) {
	const s, atm = 25000.0, 0.18
	for _, days := range []float64{0.05, 1, 5, 12, 33, 90} {
		tt := days / 365
		if v := smileVol(0, tt, atm); math.Abs(v-atm) > 0.01 {
			t.Errorf("%gd: ATM vol %.4f, want near %.2f", days, v, atm)
		}
		if lo, hi := smileVol(-0.03, tt, atm), smileVol(0.03, tt, atm); lo <= hi {
			t.Errorf("%gd: no put skew: %.4f at -3%%, %.4f at +3%%", days, lo, hi)
		}
		var prev []float64
		for k := 15000.0; k <= 35000; k += 50 {
			c := black76(true, s, k, tt, smileVol(math.Log(k/s), tt, atm))
			prev = append(prev, c)
			if n := len(prev); n >= 3 {
				a, b, d := prev[n-3], prev[n-2], prev[n-1]
				if d > b+1e-9 || a-2*b+d < -1e-6 {
					t.Fatalf("%gd: calls not decreasing and convex at %g: %.6f %.6f %.6f", days, k, a, b, d)
				}
			}
		}
	}
}
