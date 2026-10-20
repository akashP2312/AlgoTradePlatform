package sweep

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
)

var today = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

// synth lists NIFTY (index, two futures, three weekly expiries of 24000..26000 by
// 50) and BANKNIFTY (index, one future, one monthly of 40000..60000 by 100), plus an
// expired NIFTY series that must be ignored. Symbols are unique, as in the dump.
func synth() []refdata.Inst {
	rows := []contract.Instrument{
		{Token: 256265, TradingSymbol: "NIFTY 50", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 260105, TradingSymbol: "NIFTY BANK", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
	}
	tok := uint32(1000)
	fut := func(name string, exp time.Time) {
		rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: fmt.Sprintf("%s%sFUT", name, exp.Format("06Jan")),
			Name: name, Expiry: exp, InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"})
		tok++
	}
	opts := func(name string, exp time.Time, lo, hi, step float64) {
		for k := lo; k <= hi; k += step {
			for _, typ := range []string{"PE", "CE"} { // PE first: Snap must still order CE, PE
				rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: fmt.Sprintf("%s%s%.0f%s", name, exp.Format("06Jan02"), k, typ),
					Name: name, Expiry: exp, Strike: k, InstrumentType: typ, Segment: "NFO-OPT", Exchange: "NFO"})
				tok++
			}
		}
	}
	oct27, nov24 := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	fut("NIFTY", oct27)
	fut("NIFTY", nov24)
	fut("BANKNIFTY", oct27)
	opts("NIFTY", time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), 24000, 26000, 50) // expired
	for w := 0; w < 3; w++ {
		opts("NIFTY", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*w), 24000, 26000, 50)
	}
	opts("BANKNIFTY", oct27, 40000, 60000, 100)
	return refdata.Resolve(rows)
}

func cfg() *config.Config {
	return &config.Config{Universe: config.Universe{Underlyings: []config.Underlying{
		{Name: "NIFTY", Index: "NSE:NIFTY 50", DerivExchange: "NFO", CoreExpiries: 2, CoreStrikes: 3, Backup: true},
		{Name: "BANKNIFTY", Index: "NSE:NIFTY BANK", DerivExchange: "NFO", CoreExpiries: 3, CoreStrikes: 2, Backup: true},
	}}}
}

func TestAll(t *testing.T) {
	ts, warn := All(cfg(), synth(), []string{"NIFTY", "BANKNIFTY", "FINNIFTY"}, today)
	if len(ts) != 4 {
		t.Fatalf("%d targets, want 3 NIFTY + 1 BANKNIFTY", len(ts))
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "FINNIFTY") {
		t.Errorf("warnings %q", warn)
	}
	n := ts[0]
	if n.Und != "NIFTY" || n.Expiry.Format("2006-01-02") != "2026-10-20" || len(n.Opts) != 41*2 {
		t.Errorf("first target %s %s with %d options", n.Und, n.Expiry.Format("2006-01-02"), len(n.Opts))
	}
	if n.Index.Token != 256265 || n.Fut.TradingSymbol != "NIFTY26OctFUT" {
		t.Errorf("first target priced by %q and %q, want the index and the October future", n.Index.TradingSymbol, n.Fut.TradingSymbol)
	}
	if ts[2].Expiry.Format("2006-01-02") != "2026-11-03" || ts[2].Fut.TradingSymbol != "NIFTY26OctFUT" {
		t.Errorf("an expiry with no same-month future takes the nearest: got %q", ts[2].Fut.TradingSymbol)
	}
	if o := n.Opts; o[0].Key.Strike != 24000 || o[0].Key.Kind != contract.CE || o[1].Key.Kind != contract.PE || o[2].Key.Strike != 24050 {
		t.Errorf("options not ordered by strike, CE then PE: %v %v %v", o[0].Key, o[1].Key, o[2].Key)
	}
	if b := ts[3]; b.Und != "BANKNIFTY" || len(b.Opts) != 201*2 || b.Fut.TradingSymbol != "BANKNIFTY26OctFUT" {
		t.Errorf("BANKNIFTY target %s with %d options, fut %q", b.Und, len(b.Opts), b.Fut.TradingSymbol)
	}
}

func TestCore(t *testing.T) {
	ts, warn := Core(cfg(), synth(), map[string]float64{"NIFTY": 25012}, today)
	if len(ts) != 3 {
		t.Fatalf("%d targets, want 2 NIFTY + 1 BANKNIFTY (only one listed)", len(ts))
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "BANKNIFTY") {
		t.Errorf("warnings %q, want only BANKNIFTY's missing spot", warn)
	}
	if s := refdata.Strikes(ts[0].Opts); len(s) != 7 || s[0] != 24850 || s[6] != 25150 {
		t.Errorf("NIFTY core strikes %v, want 25000 +/- 3 steps", s)
	}
	if s := refdata.Strikes(ts[2].Opts); len(s) != 5 || s[2] != 50000 {
		t.Errorf("BANKNIFTY core strikes %v, want the middle listed strike +/- 2", s)
	}
	if k := Keys(ts); len(k) != 2+2+(7*2*2)+(5*2) {
		t.Errorf("%d keys, want indices, futures and options each once", len(k))
	}
}

// fakeQuoter prices every key it is asked for from fill, after failing the calls
// listed in fail.
type fakeQuoter struct {
	mu    sync.Mutex
	calls [][]string
	fail  map[int]error // call number (from 1) to its error
	drop  map[string]bool
}

func (f *fakeQuoter) Quotes(_ context.Context, keys []string) (map[string]kite.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), keys...))
	if err := f.fail[len(f.calls)]; err != nil {
		return nil, err
	}
	out := map[string]kite.Quote{}
	for _, k := range keys {
		if f.drop[k] {
			continue
		}
		out[k] = fill(k)
	}
	return out, nil
}

var exchAt = time.Date(2026, 10, 19, 15, 24, 59, 0, clock.IST)

func fill(k string) kite.Quote {
	switch {
	case k == "NSE:NIFTY 50":
		return kite.Quote{LTP: 25012.35, Timestamp: exchAt}
	case k == "NSE:NIFTY BANK":
		return kite.Quote{LTP: 55120.1, Timestamp: exchAt}
	case strings.HasSuffix(k, "FUT"):
		return kite.Quote{LTP: 25110, Timestamp: exchAt}
	case strings.HasSuffix(k, "24000CE"):
		return kite.Quote{LTP: 1010, Timestamp: exchAt} // never traded: LTT zero
	}
	return kite.Quote{LTP: 100, Bid: 99.5, Ask: 100.5, BidQ: 750, AskQ: 1500, OI: 12345, Vol: 678,
		Timestamp: exchAt, LTT: exchAt.Add(-30 * time.Second)}
}

func fixedClock(t *testing.T, at time.Time) {
	old := clock.Offset()
	clock.SetOffset(time.Until(at))
	t.Cleanup(func() { clock.SetOffset(old) })
}

func TestSnapBatchesAndRows(t *testing.T) {
	fixedClock(t, exchAt.Add(time.Second))
	ts, _ := All(cfg(), synth(), []string{"NIFTY", "BANKNIFTY"}, today)
	q := &fakeQuoter{drop: map[string]bool{"NFO:NIFTY26Oct2026000PE": true}}
	minute := time.Date(2026, 10, 19, 15, 25, 0, 0, clock.IST)
	chains, st, err := Snap(context.Background(), q, ts, minute, "sweep")
	if err != nil {
		t.Fatal(err)
	}
	// Two indices and two futures (no chain is priced by the November future).
	keys := 2 + 2 + 3*41*2 + 201*2
	if st.Keys != keys || st.Calls != 2 || len(q.calls) != st.Calls || st.Missing != 1 || st.Rows != keys-4-1 {
		t.Errorf("stats %+v, want %d keys", st, keys)
	}
	for i, c := range q.calls {
		if len(c) > kite.MaxQuoteKeys {
			t.Errorf("call %d asked for %d keys", i, len(c))
		}
	}
	if q.calls[0][0] != "NSE:NIFTY 50" {
		t.Errorf("first key %q, want the indices and futures before options", q.calls[0][0])
	}
	if len(chains) != 4 {
		t.Fatalf("%d chains", len(chains))
	}
	c := chains[0]
	if c.TS != bus.Ms(minute) || c.Source != "sweep" || c.Und != "NIFTY" || c.Expiry != "2026-10-20" || c.Spot != 25012.35 || c.Fut != 25110 {
		t.Errorf("chain header %+v", bus.Chain{TS: c.TS, Source: c.Source, Und: c.Und, Expiry: c.Expiry, Spot: c.Spot, Fut: c.Fut})
	}
	if len(c.Rows) != 41*2-1 {
		t.Errorf("%d rows, want one dropped", len(c.Rows))
	}
	r0, r1 := c.Rows[0], c.Rows[1]
	if r0.Strike != 24000 || r0.Type != "CE" || r1.Type != "PE" || r0.C != "NFO|NIFTY|2026-10-20|24000|CE" {
		t.Errorf("row order %+v %+v", r0, r1)
	}
	near := func(got, want int64) bool { return got >= want && got < want+500 } // the clock runs on in the test
	if r0.LTT != 0 || r0.TradeAge != -1 || !near(r0.QuoteAge, 1000) || r0.Exch != bus.Ms(exchAt) {
		t.Errorf("untraded row ages %+v", r0)
	}
	if !near(r1.TradeAge, 31000) || !near(r1.QuoteAge, 1000) || r1.Bid != 99.5 || r1.AskQ != 1500 || r1.OI != 12345 || r1.T == 0 {
		t.Errorf("traded row %+v", r1)
	}
}

func TestSnapRetriesThenSkips(t *testing.T) {
	RetryAfter = time.Millisecond
	t.Cleanup(func() { RetryAfter = 2 * time.Second })
	ts, _ := All(cfg(), synth(), []string{"NIFTY", "BANKNIFTY"}, today)
	flaky := errors.New("502 bad gateway")

	// The first call fails once and succeeds on retry.
	q := &fakeQuoter{fail: map[int]error{1: flaky}}
	_, st, err := Snap(context.Background(), q, ts, time.Now(), "sweep")
	if err != nil || st.Failed != 0 || st.Calls != 3 || st.Missing != 0 {
		t.Errorf("retry: %v %+v", err, st)
	}

	// The second batch fails twice: it is skipped and the rest still comes back.
	q = &fakeQuoter{fail: map[int]error{2: flaky, 3: flaky}}
	chains, st, err := Snap(context.Background(), q, ts, time.Now(), "sweep")
	if err != nil || st.Failed != 1 || st.Missing != 652-500 || len(chains) == 0 {
		t.Errorf("skip: %v %+v", err, st)
	}

	// Everything fails: an error.
	q = &fakeQuoter{fail: map[int]error{1: flaky, 2: flaky, 3: flaky, 4: flaky}}
	if _, _, err := Snap(context.Background(), q, ts, time.Now(), "sweep"); !errors.Is(err, flaky) {
		t.Errorf("all failed: %v", err)
	}
}

func TestSnapStopsOnTokenError(t *testing.T) {
	ts, _ := All(cfg(), synth(), []string{"NIFTY", "BANKNIFTY"}, today)
	tokErr := kiteconnect.Error{Code: 403, ErrorType: kiteconnect.TokenError, Message: "expired"}
	q := &fakeQuoter{fail: map[int]error{1: tokErr}}
	_, st, err := Snap(context.Background(), q, ts, time.Now(), "sweep")
	if !kite.IsTokenError(err) || len(q.calls) != 1 || st.Calls != 1 {
		t.Errorf("token error: %v after %d calls", err, len(q.calls))
	}
}
