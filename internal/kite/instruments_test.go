package kite

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"pmt/internal/config"
	"pmt/internal/contract"
)

const dump = "instrument_token,exchange_token,tradingsymbol,name,last_price,expiry,strike,tick_size,lot_size,instrument_type,segment,exchange\n" +
	"256265,1001,NIFTY 50,NIFTY 50,0,,0,0,0,EQ,INDICES,NSE\n" +
	"12345678,48225,NIFTY26OCT25000CE,NIFTY,0,2026-10-27,25000,0.05,75,CE,NFO-OPT,NFO\n" +
	"12345679,48226,NIFTY26OCTFUT,NIFTY,0,2026-10-27,0,0.1,75,FUT,NFO-FUT,NFO\n" +
	"738561,2885,RELIANCE,RELIANCE INDUSTRIES,0,,0,0.1,1,EQ,NSE,NSE\n" +
	"\"9999\",1,\"ODD,NAME\",\"A, B\",0,,0,0.05,1,EQ,NSE,NSE\n"

func TestParseInstruments(t *testing.T) {
	ins, err := ParseInstruments([]byte("\ufeff" + dump))
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 5 {
		t.Fatalf("got %d rows", len(ins))
	}
	ce := ins[1]
	if ce.Token != 12345678 || ce.ExchangeToken != 48225 || ce.Strike != 25000 || ce.LotSize != 75 ||
		ce.TickSize != 0.05 || !ce.Expiry.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("CE row = %+v", ce)
	}
	want := []string{"NSE|NIFTY|||IDX", "NFO|NIFTY|2026-10-27|25000|CE", "NFO|NIFTY|2026-10-27||FUT", "NSE|RELIANCE|||EQ"}
	for i, w := range want {
		k, ok := contract.FromInstrument(ins[i])
		if !ok || k.String() != w {
			t.Errorf("row %d key = %q,%v want %q", i, k.String(), ok, w)
		}
	}
	if ins[4].TradingSymbol != "ODD,NAME" || ins[4].Name != "A, B" {
		t.Fatalf("quoted row = %+v", ins[4])
	}

	if _, err := ParseInstruments([]byte("instrument_token,tradingsymbol\n1,X\n")); err == nil {
		t.Fatal("missing columns accepted")
	}
	if _, err := ParseInstruments([]byte(dump + "abc,1,X,X,0,,0,0,0,EQ,NSE,NSE\n")); err == nil {
		t.Fatal("bad token accepted")
	}
}

func TestFetchInstrumentsRaw(t *testing.T) {
	var hits atomic.Int64
	srv := upstream(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Kite-Version") != "3" || r.Header.Get("Authorization") != "token key:tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Write([]byte(dump))
	})
	r := NewREST(config.Kite{RootURL: srv.URL}, "key", "tok")
	raw, err := r.FetchInstruments(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != dump {
		t.Fatal("raw dump altered")
	}
}
