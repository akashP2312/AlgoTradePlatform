package book

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
)

type stubRef map[string]refdata.Inst

func (s stubRef) Lookup(_ context.Context, exch, sym string) (refdata.Inst, bool) {
	in, ok := s[exch+":"+sym]
	return in, ok
}

func TestPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/portfolio/positions":
			w.Write([]byte(`{"status":"success","data":{"net":[
			 {"tradingsymbol":"NIFTY26OCT25400CE","exchange":"NFO","instrument_token":12345,"product":"NRML",
			  "quantity":-150,"overnight_quantity":-150,"multiplier":1,"average_price":120.5,"close_price":110,
			  "last_price":100.25,"pnl":3037.5,"m2m":1462.5,"sell_quantity":150,"sell_value":18075},
			 {"tradingsymbol":"MYSTERY","exchange":"NFO","instrument_token":9,"product":"MIS","quantity":0}
			],"day":[]}}`))
		case "/user/margins":
			w.Write([]byte(`{"status":"success","data":{"equity":{"enabled":true,"net":412000.5,
			 "available":{"cash":500000,"collateral":250000,"live_balance":412000.5},
			 "utilised":{"debits":338000,"span":277000,"exposure":61000,"m2m_unrealised":-1200}},
			 "commodity":{"enabled":false}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rest := kite.NewREST(config.Kite{RootURL: srv.URL, QuotePerSec: 50, HistoricalPerSec: 50, OtherPerSec: 50}, "k", "tok")

	exp := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	key := contract.Key{Exchange: "NFO", Name: "NIFTY", Expiry: exp, Strike: 25400, Kind: contract.CE}
	ref := stubRef{"NFO:NIFTY26OCT25400CE": {
		Instrument: contract.Instrument{Exchange: "NFO", TradingSymbol: "NIFTY26OCT25400CE", LotSize: 75},
		Key:        key, Contract: key.String(), Underlying: "NIFTY",
	}}

	bk, mg, err := Poll(context.Background(), rest, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(bk.Net) != 2 || len(bk.Day) != 0 || bk.TS == 0 {
		t.Fatalf("book %+v", bk)
	}
	o := bk.Net[0]
	if o.Subj != "tick."+key.Subject() || o.Und != "NIFTY" || o.Kind != "CE" || o.Lot != 75 ||
		o.Expiry != "2026-10-27" || o.Strike != 25400 || o.C == "" {
		t.Errorf("enrichment %+v", o)
	}
	if o.Qty != -150 || o.Overnight != -150 || o.SellValue != 18075 || o.PnL != 3037.5 || o.Close != 110 {
		t.Errorf("broker fields %+v", o)
	}
	if u := bk.Net[1]; u.C != "" || u.Subj != "" || u.Sym != "MYSTERY" {
		t.Errorf("unknown symbol should pass through bare: %+v", u)
	}
	if !mg.Enabled || mg.Net != 412000.5 || mg.Span != 277000 || mg.Debits != 338000 || mg.Collateral != 250000 {
		t.Errorf("margins %+v", mg)
	}
}

func TestPollTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"status":"error","message":"Incorrect api_key or access_token.","error_type":"TokenException"}`))
	}))
	defer srv.Close()
	rest := kite.NewREST(config.Kite{RootURL: srv.URL, QuotePerSec: 50, HistoricalPerSec: 50, OtherPerSec: 50}, "k", "tok")
	if _, _, err := Poll(context.Background(), rest, nil); !kite.IsTokenError(err) {
		t.Fatalf("err %v, want a token error through the wrap", err)
	}
}
