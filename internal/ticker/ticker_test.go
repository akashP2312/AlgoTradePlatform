package ticker

import (
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
)

func TestCut(t *testing.T) {
	u := Build(cfg(3000), synth(), map[string]float64{"NIFTY": 25010}, today)
	cs := u.Chains[0]
	minute := time.Date(2026, 10, 19, 10, 31, 0, 0, clock.IST)
	ms := bus.Ms(minute)
	atm := cs.CE[25000]
	last := map[uint32]bus.Tick{
		u.Index["NIFTY"]: {LTP: 25012.5, Recv: ms - 200},
		cs.Fut:           {LTP: 25080, Recv: ms - 900},
		atm:              {C: "NFO:NIFTY26O2025000CE", LTP: 120, Bid: 119.5, Ask: 120.5, LTT: ms - 4000, Recv: ms - 1500, OI: 1e6},
		cs.PE[25000]:     {LTP: 110, Recv: ms + 50},        // arrived after the boundary under the lock
		cs.PE[24950]:     {LTP: 90, Recv: ms - 86_400_000}, // yesterday: must be skipped
	}
	chains := Cut(u, last, minute, "ws")
	if len(chains) != 1 {
		t.Fatalf("got %d chains, want 1 (expiries with no ticks are skipped)", len(chains))
	}
	c := chains[0]
	if c.Spot != 25012.5 || c.Fut != 25080 || c.TS != ms || c.Source != "ws" || c.Expiry != cs.Expiry.Format("2006-01-02") {
		t.Fatalf("header %+v", c)
	}
	if len(c.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(c.Rows))
	}
	ce := c.Rows[0]
	if ce.Type != "CE" || ce.T != atm || ce.QuoteAge != 1500 || ce.TradeAge != 4000 || ce.Bid != 119.5 || ce.OI != 1e6 {
		t.Fatalf("ce row %+v", ce)
	}
	pe := c.Rows[1]
	if pe.Type != "PE" || pe.QuoteAge != 0 || pe.TradeAge != -1 {
		t.Fatalf("pe row %+v", pe)
	}
	if Cut(nil, last, minute, "ws") != nil {
		t.Fatal("nil universe must cut nothing")
	}
}

func TestPhase(t *testing.T) {
	cal, err := calendar.Read(stringsReader("date,segment,kind,open,close,note\n2026-10-20,FO,holiday,,,Dussehra\n2026-11-08,FO,special,18:00,19:00,Muhurat\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := func(y int, m time.Month, d, hh, mm int) time.Time { return time.Date(y, m, d, hh, mm, 0, 0, clock.IST) }
	cases := []struct {
		now  time.Time
		exch string
		want string
	}{
		{at(2026, 10, 19, 9, 5), "NFO", "p"},
		{at(2026, 10, 19, 9, 15), "NFO", "c"},
		{at(2026, 10, 19, 15, 30), "NSE", "c"},
		{at(2026, 10, 19, 15, 31), "NFO", "a"},
		{at(2026, 10, 19, 11, 0), "MCX", "x"},
		{at(2026, 10, 20, 11, 0), "NFO", "a"}, // holiday
		{at(2026, 10, 24, 11, 0), "NFO", "a"}, // Saturday
		{at(2026, 11, 8, 18, 30), "NFO", "c"}, // Sunday special session
		{at(2026, 11, 8, 17, 59), "NFO", "p"},
	}
	for _, c := range cases {
		if got := Phase(cal.Session(c.now), c.exch, c.now); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.now.Format("2006-01-02 15:04"), c.exch, got, c.want)
		}
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestOrderMsgID(t *testing.T) {
	a := []byte(`{"type":"order","data":{"order_id":"1","status":"OPEN"}}`)
	b := []byte(`{"type":"order","data":{"order_id":"1","status":"COMPLETE"}}`)
	if OrderMsgID(a) != OrderMsgID(append([]byte(nil), a...)) || OrderMsgID(a) != OrderMsgID(append(a, '\n')) {
		t.Error("the same update from two sockets must share an id")
	}
	if OrderMsgID(a) == OrderMsgID(b) {
		t.Error("a status change must get a new id")
	}
}
