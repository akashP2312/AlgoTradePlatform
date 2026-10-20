package ticker

import (
	"testing"
	"time"

	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/refdata"
)

var today = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// synth lists the NIFTY index, two futures and five weekly expiries of 24000..26000.
func synth() []refdata.Inst {
	rows := []contract.Instrument{{Token: 256265, TradingSymbol: "NIFTY 50", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"}}
	tok := uint32(1000)
	for i := 0; i < 2; i++ {
		exp := time.Date(2026, 10+time.Month(i), 27, 0, 0, 0, 0, time.UTC)
		rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: "NIFTYFUT", Name: "NIFTY", Expiry: exp,
			InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO", LotSize: 75})
		tok++
	}
	for w := 0; w < 5; w++ {
		exp := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*w)
		for k := 24000.0; k <= 26000; k += 50 {
			for _, typ := range []string{"CE", "PE"} {
				rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: "NIFTYOPT", Name: "NIFTY", Expiry: exp, Strike: k,
					InstrumentType: typ, Segment: "NFO-OPT", Exchange: "NFO", LotSize: 75})
				tok++
			}
		}
	}
	return refdata.Resolve(rows)
}

func cfg(perConn int) *config.Config {
	return &config.Config{
		Kite: config.Kite{WSConnections: 2, TokensPerConn: perConn},
		Universe: config.Universe{Underlyings: []config.Underlying{{Name: "NIFTY", Index: "NSE:NIFTY 50", DerivExchange: "NFO",
			Futures: 2, WideExpiries: 4, WideStrikes: 5}}},
	}
}

func TestBuild(t *testing.T) {
	u := Build(cfg(3000), synth(), map[string]float64{"NIFTY": 25012}, today)
	if len(u.Warn) != 0 {
		t.Fatalf("warnings %v", u.Warn)
	}
	// 1 index + 2 futures + 4 expiries x 11 strikes x 2.
	if u.Count() != 91 || len(u.Chains) != 4 || u.Index["NIFTY"] != 256265 {
		t.Fatalf("count %d chains %d", u.Count(), len(u.Chains))
	}
	c := u.Chains[0]
	if c.Strikes[0] != 24750 || c.Strikes[10] != 25250 || len(c.CE) != 11 || len(c.PE) != 11 {
		t.Fatalf("window %v", c.Strikes)
	}
	if c.Fut != 1000 || u.Step["NIFTY"] != 50 {
		t.Fatalf("fut %d step %v", c.Fut, u.Step["NIFTY"])
	}
	if !u.NeedsRecentre("NIFTY", 25520, 10) || u.NeedsRecentre("NIFTY", 25400, 10) {
		t.Fatal("recentre threshold")
	}

	// Without a spot the window centres on the median listed strike.
	u = Build(cfg(3000), synth(), nil, today)
	if u.Centre["NIFTY"] != 25000 {
		t.Fatalf("centre %v", u.Centre["NIFTY"])
	}

	// 60 tokens: 3 + 8*(2w+1) <= 60 gives w = 3, so two strikes per side are trimmed.
	u = Build(cfg(30), synth(), map[string]float64{"NIFTY": 25012}, today)
	if u.Trimmed != 2 || u.Count() != 59 || len(u.Warn) != 1 {
		t.Fatalf("trimmed %d count %d warn %v", u.Trimmed, u.Count(), u.Warn)
	}
}

func TestWindowEdges(t *testing.T) {
	ks := []float64{100, 200, 300, 400}
	if w := Window(ks, 90, 1); len(w) != 2 || w[0] != 100 {
		t.Fatal(w)
	}
	if w := Window(ks, 1000, 1); len(w) != 2 || w[1] != 400 {
		t.Fatal(w)
	}
	if w := Window(ks, 250, 0); len(w) != 1 || w[0] != 200 {
		t.Fatal(w) // ties go to the lower strike
	}
}

func TestAssignSticky(t *testing.T) {
	modes := map[uint32]byte{1: 'f', 2: 'f', 3: 'f', 4: 'f'}
	a, sets := Assign(modes, nil, 2, 10)
	if len(sets[0]) != 2 || len(sets[1]) != 2 {
		t.Fatal(sets)
	}
	delete(modes, 1)
	modes[5] = 'f'
	b, _ := Assign(modes, a, 2, 10)
	for _, tok := range []uint32{2, 3, 4} {
		if a[tok] != b[tok] {
			t.Fatalf("token %d moved", tok)
		}
	}
	if b[5] != a[1] {
		t.Fatalf("new token should fill the freed slot")
	}
}
