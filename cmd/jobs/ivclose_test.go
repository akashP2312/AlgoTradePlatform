package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
)

func TestIVCloseRows(t *testing.T) {
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, clock.IST)
	s := calendar.Session{Date: day, Kind: calendar.Normal,
		Open: clock.At(day, 9, 15, 0), Close: clock.At(day, 15, 30, 0)}
	pl := func(at time.Time, x string) []byte {
		return []byte(fmt.Sprintf(`{"ts":%d,"u":"NIFTY","x":%q,"tte":0.01,"dte":2.8,"f":25010.5,"fsrc":"parity",
			"s":25000,"atm":0.12,"rr25":-0.03,"bf25":0.004,"fit":{"a":0.001,"b":0.02,"rho":-0.4,"m":0.01,"s":0.05,"rmse":0.002,"ok":true,"arb":[]},"n":31}`,
			bus.Ms(at), x))
	}
	raw := map[string][]byte{
		"nifty.iv.20261028": pl(clock.At(day, 15, 29, 58), "2026-10-28"),
		"nifty.iv.20261104": pl(clock.At(day, 14, 0, 0), "2026-11-04"),                    // stale: before the window
		"nifty.iv.20261021": pl(clock.At(day, 15, 29, 0), "2026-10-21"),                   // expired today
		"nifty.iv.20261125": pl(clock.At(day.AddDate(0, 0, -1), 15, 29, 0), "2026-11-25"), // yesterday's
		"nifty.iv.bad":      []byte(`{"u":"NIFTY"}`),
	}
	rows, skip := ivCloseRows(s, raw)
	if len(rows) != 1 || rows[0].Expiry.Format(time.DateOnly) != "2026-10-28" {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0].P
	if *r.F != 25010.5 || r.Fit == nil || !r.Fit.Ok || *r.Fit.Rho != -0.4 || r.N != 31 {
		t.Errorf("payload %+v fit %+v", r, r.Fit)
	}
	got := strings.Join(skip, "|")
	for _, want := range []string{"20261104: last computed 14:00:00", "20261125: last computed 2026-10-20", "bad: incomplete"} {
		if !strings.Contains(got, want) {
			t.Errorf("skip %q lacks %q", got, want)
		}
	}
}
