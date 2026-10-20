package refdata

import (
	"context"
	"os"
	"testing"
	"time"

	"pmt/internal/contract"
	"pmt/internal/store"
)

func opt(tok uint32, strike float64, lot int) contract.Instrument {
	return contract.Instrument{Token: tok, ExchangeToken: tok >> 8, TradingSymbol: "NIFTY26OCT" + contract.FormatStrike(strike) + "CE",
		Name: "NIFTY", Expiry: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), Strike: strike, TickSize: 0.05,
		LotSize: lot, InstrumentType: "CE", Segment: "NFO-OPT", Exchange: "NFO"}
}

func TestResolve(t *testing.T) {
	idx := contract.Instrument{Token: 256265, TradingSymbol: "NIFTY 50", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"}
	spread := contract.Instrument{Token: 9, InstrumentType: "SPD", Exchange: "NFO"}
	got := Resolve([]contract.Instrument{idx, opt(1, 25000, 75), opt(1, 25100, 75), spread})
	if len(got) != 2 || got[0].Contract != "NSE|NIFTY|||IDX" || got[1].Contract != "NFO|NIFTY|2026-10-27|25000|CE" {
		t.Fatalf("%+v", got)
	}
}

func TestLoadSCD(t *testing.T) {
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
	pool.Exec(ctx, `TRUNCATE instrument_history, instrument_dumps`)
	d1 := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(0, 0, 1)
	if _, err := Load(ctx, pool, d1, []contract.Instrument{opt(1, 25000, 75), opt(2, 25100, 75)}, "a", "p", d1); err != nil {
		t.Fatal(err)
	}
	// Day 2: token 1 lot size changes, token 2 disappears, token 3 appears. Load twice.
	for i := 0; i < 2; i++ {
		st, err := Load(ctx, pool, d2, []contract.Instrument{opt(1, 25000, 65), opt(3, 25200, 65)}, "b", "p", d2)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && (st.Closed != 2 || st.Opened != 2) {
			t.Fatalf("stats %+v", st)
		}
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM instrument_history`).Scan(&n)
	if n != 4 {
		t.Fatalf("history rows %d, want 4", n)
	}
	cur, err := Current(ctx, pool, "NIFTY")
	if err != nil || len(cur) != 2 {
		t.Fatalf("current %d %v", len(cur), err)
	}
	c, err := AsOf(ctx, pool, 2, d1)
	if err != nil || c != "NFO|NIFTY|2026-10-27|25100|CE" {
		t.Fatalf("as of %q %v", c, err)
	}
	if _, err := AsOf(ctx, pool, 2, d2); err == nil {
		t.Fatal("token 2 should be closed on day 2")
	}
}

func TestResolveExtras(t *testing.T) {
	today := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	fut := func(tok uint32, exch, name string, exp time.Time) contract.Instrument {
		return contract.Instrument{Token: tok, TradingSymbol: name + exp.Format("06Jan") + "FUT", Name: name, Expiry: exp,
			InstrumentType: "FUT", Segment: exch + "-FUT", Exchange: exch}
	}
	vix := contract.Instrument{Token: 264969, TradingSymbol: "INDIA VIX", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"}
	insts := Resolve([]contract.Instrument{vix,
		fut(10, "MCX", "CRUDEOIL", today.AddDate(0, 0, -1)), // expired
		fut(12, "MCX", "CRUDEOIL", today.AddDate(0, 1, 0)),
		fut(11, "MCX", "CRUDEOIL", today.AddDate(0, 0, 3)),
	})
	if in, err := ResolveExtra(insts, "NSE:INDIA VIX", today); err != nil || in.Token != 264969 {
		t.Fatal(in, err)
	}
	if in, err := ResolveExtra(insts, "MCX:CRUDEOIL:FUT:1", today); err != nil || in.Token != 11 {
		t.Fatal(in, err)
	}
	if _, err := ResolveExtra(insts, "MCX:CRUDEOIL:FUT:3", today); err == nil {
		t.Fatal("want error for missing third future")
	}
	syms, err := ReadConstituents("../../config/constituents/nifty50.csv")
	if err != nil || len(syms) < 40 {
		t.Fatalf("constituents %d %v", len(syms), err)
	}
}
