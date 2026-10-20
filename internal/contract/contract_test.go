package contract

import (
	"testing"
	"time"
)

func TestRoundTripAndSubject(t *testing.T) {
	exp := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		k       Key
		str     string
		subject string
	}{
		{Option("NFO", "NIFTY", exp, 25000, CE), "NFO|NIFTY|2026-10-27|25000|CE", "nfo.nifty.20261027.25000.ce"},
		{Option("NFO", "RELIANCE", exp, 1302.5, PE), "NFO|RELIANCE|2026-10-27|1302.5|PE", "nfo.reliance.20261027.1302p5.pe"},
		{Key{Exchange: "NFO", Name: "NIFTY", Expiry: exp, Kind: FUT}, "NFO|NIFTY|2026-10-27||FUT", "nfo.nifty.20261027._.fut"},
		{Key{Exchange: "NSE", Name: "NIFTY", Kind: IDX}, "NSE|NIFTY|||IDX", "nse.nifty._._.idx"},
		{Key{Exchange: "NSE", Name: "M&M", Kind: EQ}, "NSE|M&M|||EQ", "nse.m_m._._.eq"},
		{Key{Exchange: "NSE", Name: "BAJAJ-AUTO", Kind: EQ}, "NSE|BAJAJ-AUTO|||EQ", "nse.bajaj-auto._._.eq"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.str {
			t.Errorf("String = %q want %q", got, c.str)
		}
		if got := c.k.Subject(); got != c.subject {
			t.Errorf("Subject = %q want %q", got, c.subject)
		}
		back, err := Parse(c.str)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.str, err)
		}
		if back.String() != c.str {
			t.Errorf("round trip %q -> %q", c.str, back.String())
		}
	}
	for _, bad := range []string{"NFO|NIFTY|2026-10-27||CE", "NFO|NIFTY|||FUT", "a|b|c", "NSE|X|||ZZ"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestFromInstrument(t *testing.T) {
	exp := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	k, ok := FromInstrument(Instrument{Exchange: "NSE", Segment: "INDICES", TradingSymbol: "NIFTY BANK"})
	if !ok || k.String() != "NSE|BANKNIFTY|||IDX" {
		t.Fatalf("index = %v %v", k, ok)
	}
	k, ok = FromInstrument(Instrument{Exchange: "NFO", Segment: "NFO-OPT", Name: "NIFTY", InstrumentType: "PE", Expiry: exp, Strike: 24950})
	if !ok || k.String() != "NFO|NIFTY|2026-10-27|24950|PE" {
		t.Fatalf("option = %v %v", k, ok)
	}
	if _, ok := FromInstrument(Instrument{Exchange: "NFO", InstrumentType: "SPREAD"}); ok {
		t.Fatal("spread should be skipped")
	}
}
