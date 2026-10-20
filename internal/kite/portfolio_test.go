package kite

import (
	"context"
	"net/http"
	"testing"
)

const positionsBody = `{"status":"success","data":{"net":[
 {"tradingsymbol":"NIFTY26OCT25500CE","exchange":"NFO","instrument_token":"12345","product":"NRML",
  "quantity":-150,"overnight_quantity":-150,"multiplier":1,"average_price":120.5,"close_price":110,
  "last_price":100.25,"value":18075,"pnl":3037.5,"m2m":1462.5,"unrealised":3037.5,"realised":0,
  "buy_quantity":0,"buy_price":0,"buy_value":0,"sell_quantity":150,"sell_price":120.5,"sell_value":18075,
  "day_buy_quantity":0,"day_buy_price":0,"day_buy_value":0,"day_sell_quantity":0,"day_sell_price":0,"day_sell_value":0},
 {"tradingsymbol":"NIFTY26OCTFUT","exchange":"NFO","instrument_token":678,"product":"MIS",
  "quantity":75,"overnight_quantity":0,"multiplier":null,"average_price":25010,"close_price":null,
  "last_price":25020,"value":-1875750,"pnl":750,"m2m":750,"unrealised":750,"realised":0,
  "buy_quantity":75,"buy_price":25010,"buy_value":1875750,"sell_quantity":0,"sell_price":0,"sell_value":0,
  "day_buy_quantity":75,"day_buy_price":25010,"day_buy_value":1875750,"day_sell_quantity":0,"day_sell_price":0,"day_sell_value":0}
],"day":[]}}`

func TestPositions(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/portfolio/positions" {
			t.Errorf("path %q", req.URL.Path)
		}
		if req.Header.Get("Authorization") != "token key:tok" {
			t.Error("missing auth header")
		}
		w.Write([]byte(positionsBody))
	})
	ps, err := r.Positions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Net) != 2 || len(ps.Day) != 0 {
		t.Fatalf("net %d day %d", len(ps.Net), len(ps.Day))
	}
	o := ps.Net[0]
	if o.Token != 12345 || o.Quantity != -150 || o.AveragePrice != 120.5 || o.PnL != 3037.5 || o.Product != "NRML" {
		t.Errorf("option leg %+v", o)
	}
	f := ps.Net[1]
	if f.Multiplier != 1 || f.ClosePrice != 0 || f.Product != "MIS" || f.OvernightQuantity != 0 {
		t.Errorf("future leg %+v", f)
	}
}

func TestMargins(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/user/margins" {
			t.Errorf("path %q", req.URL.Path)
		}
		w.Write([]byte(`{"status":"success","data":{"equity":{"enabled":true,"net":412000.5,
		 "available":{"adhoc_margin":0,"cash":500000,"opening_balance":500000,"live_balance":412000.5,"collateral":250000,"intraday_payin":0},
		 "utilised":{"debits":338000,"exposure":61000,"m2m_realised":0,"m2m_unrealised":-1200,"option_premium":0,"payout":0,
		  "span":277000,"holding_sales":0,"turnover":0,"liquid_collateral":0,"stock_collateral":250000,"delivery":0}},
		 "commodity":{"enabled":false,"net":0,"available":{},"utilised":{}}}}`))
	})
	m, err := r.Margins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !m.Equity.Enabled || m.Equity.Net != 412000.5 || m.Equity.Utilised.Span != 277000 || m.Equity.Utilised.Exposure != 61000 ||
		m.Equity.Available.Collateral != 250000 || m.Equity.Utilised.M2MUnrealised != -1200 {
		t.Errorf("equity %+v", m.Equity)
	}
	if m.Commodity.Enabled {
		t.Error("commodity enabled")
	}
}

func TestPositionsTokenError(t *testing.T) {
	r := testREST(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"status":"error","message":"Incorrect api_key or access_token.","error_type":"TokenException"}`))
	})
	if _, err := r.Positions(context.Background()); !IsTokenError(err) {
		t.Fatalf("err %v, want a token error", err)
	}
}
