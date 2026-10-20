package recorder

import (
	"encoding/json"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

var day = time.Date(2026, 10, 19, 0, 0, 0, 0, clock.IST)

func at(hh, mm, ss int) int64 { return clock.At(day, hh, mm, ss).UnixMilli() }

func barAt(t *testing.T, b *Bars, c string, hh, mm int) *bar {
	t.Helper()
	return b.bars[barKey{c, clock.At(day, hh, mm, 0).UnixMilli()}]
}

func TestBarsOpeningAndDeltas(t *testing.T) {
	b := NewBars(nil)
	const c = "NFO|NIFTY|2026-10-27|25000|CE"
	// Pre-open sets the baseline only.
	b.Add(&bus.Tick{C: c, Recv: at(9, 7, 0), LTP: 100, Vol: 500, Phase: "p"})
	if len(b.bars) != 0 {
		t.Fatalf("pre-open made a bar")
	}
	// First continuous tick in the opening minute carries the whole day volume.
	b.Add(&bus.Tick{C: c, Recv: at(9, 15, 2), LTT: at(9, 15, 1), LTP: 101, Vol: 900, OI: 1000, Phase: "c"})
	b.Add(&bus.Tick{C: c, Recv: at(9, 15, 30), LTT: at(9, 15, 29), LTP: 99, Vol: 950, Phase: "c"})
	br := barAt(t, b, c, 9, 15)
	if br == nil || br.o != 101 || br.h != 101 || br.l != 99 || br.c != 99 || br.v != 950 || !br.hasOI || br.oi != 1000 {
		t.Fatalf("09:15 bar %+v", br)
	}
	// OI-only update with no trade changes the bar's OI, not its prices.
	b.Add(&bus.Tick{C: c, Recv: at(9, 15, 40), LTT: at(9, 15, 29), LTP: 99, Vol: 950, OI: 1200, Phase: "c"})
	if br.oi != 1200 || br.v != 950 || br.c != 99 {
		t.Fatalf("after OI update %+v", br)
	}
	// Upstream volume reset: new baseline, no negative volume.
	b.Add(&bus.Tick{C: c, Recv: at(9, 16, 1), LTT: at(9, 16, 0), LTP: 98, Vol: 10, Phase: "c"})
	if br := barAt(t, b, c, 9, 16); br == nil || br.v != 0 || br.c != 98 {
		t.Fatalf("09:16 bar after reset %+v", br)
	}
	b.Add(&bus.Tick{C: c, Recv: at(9, 16, 20), LTT: at(9, 16, 19), LTP: 97, Vol: 40, Phase: "c"})
	if br := barAt(t, b, c, 9, 16); br.v != 30 || br.l != 97 {
		t.Fatalf("09:16 bar %+v", br)
	}
	if b.Pending() != 2 {
		t.Fatalf("pending %d", b.Pending())
	}
}

func TestBarsRestartBaseline(t *testing.T) {
	b := NewBars(nil)
	const c = "NFO|NIFTY|2026-10-27||FUT"
	// First tick after a mid-session restart: baseline only, and it prints only
	// when its trade happened in this minute.
	b.Add(&bus.Tick{C: c, Recv: at(11, 0, 5), LTT: at(10, 59, 50), LTP: 25000, Vol: 1e6, Phase: "c"})
	if len(b.bars) != 0 {
		t.Fatalf("stale first tick made a bar")
	}
	b.Add(&bus.Tick{C: c, Recv: at(11, 0, 9), LTT: at(11, 0, 8), LTP: 25001, Vol: 1e6 + 75, Phase: "c"})
	if br := barAt(t, b, c, 11, 0); br == nil || br.v != 75 || br.o != 25001 {
		t.Fatalf("11:00 bar %+v", br)
	}
	// Ignored: zero price, after-market, unknown phase.
	b.Add(&bus.Tick{C: c, Recv: at(11, 0, 10), LTP: 0, Vol: 2e6, Phase: "c"})
	b.Add(&bus.Tick{C: c, Recv: at(15, 45, 0), LTP: 1, Vol: 3e6, Phase: "a"})
	if br := barAt(t, b, c, 11, 0); br.v != 75 || len(b.bars) != 1 {
		t.Fatalf("ignored ticks changed bars: %+v, %d bars", br, len(b.bars))
	}
}

func TestBarsIndexAndCustomOpen(t *testing.T) {
	// A Muhurat-style session opening at 18:00: the opening rule follows Open.
	b := NewBars(func(d time.Time) time.Time { return clock.At(d, 18, 0, 0) })
	const fut = "NFO|NIFTY|2026-10-27||FUT"
	b.Add(&bus.Tick{C: fut, Recv: at(18, 0, 3), LTT: at(17, 59, 0), LTP: 10, Vol: 400, Phase: "c"})
	if br := barAt(t, b, fut, 18, 0); br == nil || br.v != 400 {
		t.Fatalf("opening bar %+v", br)
	}
	const idx = "NSE|NIFTY|||IDX"
	b.Add(&bus.Tick{C: idx, Recv: at(18, 1, 0), LTP: 25000, Phase: "c"})
	b.Add(&bus.Tick{C: idx, Recv: at(18, 1, 30), LTP: 25010, Phase: "c"})
	if br := barAt(t, b, idx, 18, 1); br == nil || br.h != 25010 || br.v != 0 || br.hasOI {
		t.Fatalf("index bar %+v", br)
	}
}

func TestBarsCommittedEvicts(t *testing.T) {
	b := NewBars(nil)
	const idx = "NSE|NIFTY|||IDX"
	for m := 0; m < 10; m++ {
		b.Add(&bus.Tick{C: idx, Recv: at(10, m, 0), LTP: 1, Phase: "c"})
	}
	b.Committed()
	if b.Pending() != 0 {
		t.Fatalf("pending %d after commit", b.Pending())
	}
	// Newest is 10:09; 10:04..10:09 stay.
	if len(b.bars) != 6 || barAt(t, b, idx, 10, 3) != nil || barAt(t, b, idx, 10, 4) == nil {
		t.Fatalf("%d bars kept", len(b.bars))
	}
}

func TestRuns(t *testing.T) {
	want := minutes(clock.At(day, 9, 15, 0), clock.At(day, 9, 25, 0))
	// missing 9:16-9:18 (9:17 by sweep, 9:18 by backup), 9:22, 9:24.
	state := map[int]string{16: "", 17: "sweep", 18: "backup", 22: "", 24: "sweep"}
	gs := runs(want, func(m time.Time) (bool, bool, string) {
		by, ok := state[m.Minute()]
		return ok, by != "", by
	})
	if len(gs) != 3 {
		t.Fatalf("%d runs: %+v", len(gs), gs)
	}
	g := gs[0]
	if g.Start.Minute() != 16 || g.End.Minute() != 19 || g.Minutes != 3 || g.Filled != 2 || g.FilledBy != "backup" {
		t.Errorf("first run %+v", g)
	}
	if gs[1].Minutes != 1 || gs[1].Filled != 0 || gs[1].FilledBy != "" {
		t.Errorf("second run %+v", gs[1])
	}
	if gs[2].Start.Minute() != 24 || gs[2].End.Minute() != 25 || gs[2].FilledBy != "sweep" {
		t.Errorf("third run %+v", gs[2])
	}
	if len(runs(want, func(time.Time) (bool, bool, string) { return false, false, "" })) != 0 {
		t.Error("no missing minutes should give no runs")
	}
}

func TestOrderParsing(t *testing.T) {
	var o kiteOrder
	err := json.Unmarshal([]byte(`{"order_id":"1","instrument_token":"12345","quantity":75,"filled_quantity":null,
		"pending_quantity":"","price":"101.5","average_price":0,"trigger_price":1.5,
		"order_timestamp":"2026-10-19 09:15:03","exchange_timestamp":"0001-01-01 00:00:00"}`), &o)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InstrumentToken.Valid || o.InstrumentToken.V != 12345 || o.Quantity.qty() != int32(75) {
		t.Errorf("token %+v qty %v", o.InstrumentToken, o.Quantity.qty())
	}
	if o.FilledQuantity.qty() != nil || o.PendingQuantity.qty() != nil {
		t.Errorf("null and empty should be nil: %v %v", o.FilledQuantity.qty(), o.PendingQuantity.qty())
	}
	if o.Price.price() != 101.5 || o.AveragePrice.price() != 0.0 || o.TriggerPrice.qty() != nil {
		t.Errorf("price %v avg %v trigger qty %v", o.Price.price(), o.AveragePrice.price(), o.TriggerPrice.qty())
	}
	if !o.OrderTimestamp.Equal(clock.At(day, 9, 15, 3)) || !o.ExchangeTimestamp.IsZero() {
		t.Errorf("times %v %v", o.OrderTimestamp, o.ExchangeTimestamp)
	}
	var k kiteTime
	if err := json.Unmarshal([]byte(`"2026-10-19T03:45:03Z"`), &k); err != nil || !k.Equal(clock.At(day, 9, 15, 3)) {
		t.Errorf("RFC 3339 %v %v", k, err)
	}
	if err := json.Unmarshal([]byte(`"yesterday"`), &k); err == nil {
		t.Error("bad time accepted")
	}
	var n optNum
	if err := json.Unmarshal([]byte(`"abc"`), &n); err == nil {
		t.Error("bad number accepted")
	}
}

func TestChainSinkValidation(t *testing.T) {
	s := &chainSink{}
	ok := bus.Chain{TS: at(9, 15, 0), Source: "ws", Und: "NIFTY", Expiry: "2026-10-27", Rows: []bus.ChainRow{
		{C: "a", Strike: 25000, Type: "CE"}, {C: "b", Strike: 25000, Type: "XX"}, {C: "", Strike: 1, Type: "PE"},
	}}
	if err := s.addChain(&ok); err != nil || s.size() != 1 {
		t.Fatalf("valid chain: %v, %d rows", err, s.size())
	}
	for name, c := range map[string]bus.Chain{
		"no ts":     {Source: "ws", Und: "N", Expiry: "2026-10-27"},
		"source":    {TS: 1, Source: "nse", Und: "N", Expiry: "2026-10-27"},
		"und":       {TS: 1, Source: "ws", Expiry: "2026-10-27"},
		"expiry":    {TS: 1, Source: "ws", Und: "N", Expiry: "27-10-2026"},
		"none kept": {TS: 1, Source: "ws", Und: "N", Expiry: "2026-10-27", Rows: []bus.ChainRow{{C: "x", Type: "CE"}}},
	} {
		if err := s.addChain(&c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s.size() != 1 {
		t.Errorf("rejected chains left rows: %d", s.size())
	}
}
