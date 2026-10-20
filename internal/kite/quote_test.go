package kite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pmt/internal/clock"
	"pmt/internal/config"
)

const quoteBody = `{"status":"success","data":{
 "NSE:NIFTY 50":{"instrument_token":256265,"timestamp":"2026-10-19 10:15:00","last_price":25012.35,
   "ohlc":{"open":24950,"high":25040.1,"low":24930,"close":24980.55},"net_change":0},
 "NFO:NIFTY26OCT25000CE":{"instrument_token":12345,"timestamp":"2026-10-19 10:15:00",
   "last_trade_time":"2026-10-19 10:14:58","last_price":112.5,"last_quantity":75,"buy_quantity":120000,
   "sell_quantity":98000,"volume":4500000,"average_price":108.2,"oi":5200000,"oi_day_high":5300000,"oi_day_low":4800000,
   "ohlc":{"open":95,"high":130,"low":90,"close":101.4},
   "depth":{"buy":[{"price":112.4,"quantity":1500,"orders":7},{"price":112.3,"quantity":900,"orders":3}],
            "sell":[{"price":112.6,"quantity":750,"orders":2}]}},
 "NFO:NIFTY26OCT30000CE":{"instrument_token":12399,"timestamp":"2026-10-19 10:15:00",
   "last_trade_time":"1970-01-01 05:30:00","last_price":0,"volume":0,"oi":0,
   "ohlc":{"open":0,"high":0,"low":0,"close":0},
   "depth":{"buy":[{"price":0,"quantity":0,"orders":0}],"sell":[{"price":0.05,"quantity":75,"orders":1}]}}}}`

func testREST(t *testing.T, h http.HandlerFunc) *REST {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewREST(config.Kite{RootURL: srv.URL, QuotePerSec: 50, HistoricalPerSec: 50, OtherPerSec: 50}, "key", "tok")
}

func TestQuotes(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/quote" {
			t.Errorf("path %q", req.URL.Path)
		}
		if got := req.URL.Query()["i"]; len(got) != 3 || got[0] != "NSE:NIFTY 50" {
			t.Errorf("keys %q (raw %q)", got, req.URL.RawQuery)
		}
		if req.Header.Get("Authorization") != "token key:tok" {
			t.Error("missing auth header")
		}
		w.Write([]byte(quoteBody))
	})
	qs, err := r.Quotes(context.Background(), []string{"NSE:NIFTY 50", "NFO:NIFTY26OCT25000CE", "NFO:NIFTY26OCT30000CE"})
	if err != nil {
		t.Fatal(err)
	}
	idx := qs["NSE:NIFTY 50"]
	want := time.Date(2026, 10, 19, 10, 15, 0, 0, clock.IST)
	if idx.Token != 256265 || idx.LTP != 25012.35 || !idx.Timestamp.Equal(want) || idx.Depth != nil || idx.Close != 24980.55 {
		t.Errorf("index %+v", idx)
	}
	o := qs["NFO:NIFTY26OCT25000CE"]
	if o.Bid != 112.4 || o.BidQ != 1500 || o.Ask != 112.6 || o.AskQ != 750 || o.OI != 5200000 || o.Vol != 4500000 {
		t.Errorf("option %+v", o)
	}
	if !o.LTT.Equal(time.Date(2026, 10, 19, 10, 14, 58, 0, clock.IST)) || o.Depth.Buy[1].N != 3 || o.Depth.Sell[1].P != 0 {
		t.Errorf("option ltt/depth %v %+v", o.LTT, o.Depth)
	}
	far := qs["NFO:NIFTY26OCT30000CE"]
	if !far.LTT.IsZero() || far.Bid != 0 || far.BidQ != 0 || far.Ask != 0.05 {
		t.Errorf("untraded %+v", far)
	}
}

func TestQuotesLimit(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) { t.Error("no request expected") })
	if _, err := r.Quotes(context.Background(), make([]string, MaxQuoteKeys+1)); err == nil {
		t.Error("expected limit error")
	}
}

func TestQuotesTokenError(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"status":"error","message":"Incorrect api_key or access_token.","error_type":"TokenException"}`))
	})
	_, err := r.Quotes(context.Background(), []string{"NSE:NIFTY 50"})
	if !IsTokenError(err) {
		t.Errorf("want token error, got %v", err)
	}
}

func TestHistorical(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/instruments/historical/12345/minute" {
			t.Errorf("path %q", req.URL.Path)
		}
		q := req.URL.Query()
		if q.Get("from") != "2026-10-16 00:00:00" || q.Get("to") != "2026-10-16 23:59:59" || q.Get("oi") != "1" {
			t.Errorf("query %q", req.URL.RawQuery)
		}
		w.Write([]byte(`{"status":"success","data":{"candles":[
			["2026-10-16T09:15:00+0530",100,104.5,99,103,15000,5100000],
			["2026-10-16T09:16:00+0530",103,103.5,101,102,9000,5110000]]}}`))
	})
	d := time.Date(2026, 10, 16, 0, 0, 0, 0, clock.IST)
	cs, err := r.Historical(context.Background(), 12345, "minute", d, d.Add(24*time.Hour-time.Second), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].H != 104.5 || cs[0].V != 15000 || cs[1].OI != 5110000 {
		t.Fatalf("candles %+v", cs)
	}
	if !cs[0].TS.Equal(time.Date(2026, 10, 16, 9, 15, 0, 0, clock.IST)) {
		t.Errorf("ts %v", cs[0].TS)
	}
}

func TestParseCandlesNoOI(t *testing.T) {
	cs, err := ParseCandles([]byte(`{"data":{"candles":[["2026-10-16T00:00:00+0530",1,2,0.5,1.5,0]]}}`))
	if err != nil || len(cs) != 1 || cs[0].OI != 0 || cs[0].C != 1.5 {
		t.Fatalf("%+v %v", cs, err)
	}
}
