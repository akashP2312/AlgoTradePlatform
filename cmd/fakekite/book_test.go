package main

import (
	"context"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
)

// TestBookAgainstClient reads the sample book through the real client and checks
// Kite's P&L identities hold on every row, since the POS panel's zero-shock check
// relies on them.
func TestBookAgainstClient(t *testing.T) {
	m := testMarket(t, clock.Today())
	s := &server{m: m, apiKey: "k", token: "tok", book: sampleBook(m, clock.Now())}
	if len(s.book) != 5 {
		t.Fatalf("book has %d legs, want 5", len(s.book))
	}
	hs := httptest.NewServer(s.routes())
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rest := kite.NewREST(config.Kite{RootURL: hs.URL, QuotePerSec: 10, HistoricalPerSec: 10, OtherPerSec: 10}, "k", "tok")

	ps, err := rest.Positions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Net) != 5 || len(ps.Day) != 2 {
		t.Fatalf("net %d day %d, want 5 and 2", len(ps.Net), len(ps.Day))
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.05 }
	var open, closed int
	for _, p := range append(ps.Net, ps.Day...) {
		q := float64(p.Quantity)
		if !near(p.PnL, p.SellValue-p.BuyValue+q*p.LastPrice*p.Multiplier) {
			t.Errorf("%s pnl %.2f breaks sell−buy+q·ltp", p.TradingSymbol, p.PnL)
		}
		if p.Quantity != p.BuyQuantity-p.SellQuantity {
			t.Errorf("%s qty %d != buy %d − sell %d", p.TradingSymbol, p.Quantity, p.BuyQuantity, p.SellQuantity)
		}
	}
	for _, p := range ps.Net {
		q, ov := float64(p.Quantity), float64(p.OvernightQuantity)
		if !near(p.M2M, p.DaySellValue-p.DayBuyValue+q*p.LastPrice-ov*p.ClosePrice) {
			t.Errorf("%s m2m %.2f breaks the day identity", p.TradingSymbol, p.M2M)
		}
		if p.Token == 0 || p.LastPrice <= 0 || p.ClosePrice <= 0 {
			t.Errorf("%s token %d ltp %.2f close %.2f", p.TradingSymbol, p.Token, p.LastPrice, p.ClosePrice)
		}
		if p.Quantity == 0 {
			closed++
			if p.Realised != p.PnL || p.Unrealised != 0 {
				t.Errorf("%s closed but realised %.2f unrealised %.2f", p.TradingSymbol, p.Realised, p.Unrealised)
			}
		} else {
			open++
		}
	}
	if open != 4 || closed != 1 {
		t.Errorf("open %d closed %d, want 4 and 1", open, closed)
	}

	mg, err := rest.Margins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := mg.Equity
	if !e.Enabled || e.Utilised.Span <= 0 || e.Utilised.Exposure <= 0 || e.Net <= 0 {
		t.Errorf("equity %+v", e)
	}
	if !near(e.Net, e.Available.Cash+e.Available.Collateral-e.Utilised.Debits) {
		t.Errorf("net %.2f != cash+collateral−debits", e.Net)
	}
}
