package kite

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"pmt/internal/config"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		method, path string
		class        Class
		ok           bool
	}{
		{"GET", "/quote", ClassQuote, true},
		{"GET", "/quote/ltp", ClassQuote, true},
		{"GET", "/quote/ohlc", ClassQuote, true},
		{"GET", "/instruments", ClassOther, true},
		{"GET", "/instruments/NFO", ClassOther, true},
		{"GET", "/instruments/historical/256265/minute", ClassHistorical, true},
		{"GET", "/user/profile", ClassOther, true},
		{"GET", "/user/margins", ClassOther, true},
		{"GET", "/user/margins/equity", ClassOther, true},
		{"GET", "/portfolio/positions", ClassOther, true},
		{"GET", "/portfolio/holdings", ClassOther, true},
		{"GET", "/orders", ClassOther, true},
		{"GET", "/trades", ClassOther, true},
		{"POST", "/session/token", ClassOther, true},

		{"POST", "/orders/regular", "", false},
		{"PUT", "/orders/regular/123", "", false},
		{"DELETE", "/orders/regular/123", "", false},
		{"DELETE", "/session/token", "", false},
		{"POST", "/margins/orders", "", false},
		{"POST", "/gtt/triggers", "", false},
		{"PUT", "/portfolio/positions", "", false},
		{"POST", "/mf/orders", "", false},
		{"GET", "/orders/123", "", false},
		{"GET", "/instruments/historical", "", false},
		{"GET", "/quote/../orders/regular", "", false},
		{"POST", "/quote", "", false},
		{"GET", "/session/token", "", false},
	}
	for _, c := range cases {
		cl, ok := Classify(c.method, c.path)
		if ok != c.ok || cl != c.class {
			t.Errorf("Classify(%s %s) = %q,%v want %q,%v", c.method, c.path, cl, ok, c.class, c.ok)
		}
	}
}

// upstream fails the test if a refused request ever reaches it.
func upstream(t *testing.T, hits *atomic.Int64, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		p := strings.TrimPrefix(r.URL.Path, "/kite")
		if _, ok := Classify(r.Method, p); !ok {
			t.Errorf("refused request reached upstream: %s %s", r.Method, r.URL.Path)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ok(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"success","data":` + data + `}`))
}

func TestGuardBlocksOrderAndLogoutCalls(t *testing.T) {
	var hits atomic.Int64
	srv := upstream(t, &hits, func(w http.ResponseWriter, r *http.Request) { ok(w, `{}`) })
	r := NewREST(config.Kite{RootURL: srv.URL + "/kite", QuotePerSec: 100, HistoricalPerSec: 100, OtherPerSec: 100}, "key", "tok")

	if _, err := r.Kite.PlaceOrder(kiteconnect.VarietyRegular, kiteconnect.OrderParams{Exchange: "NFO", Tradingsymbol: "NIFTY", Quantity: 75}); err == nil {
		t.Error("PlaceOrder succeeded")
	}
	if _, err := r.Kite.ModifyOrder(kiteconnect.VarietyRegular, "1", kiteconnect.OrderParams{}); err == nil {
		t.Error("ModifyOrder succeeded")
	}
	if _, err := r.Kite.CancelOrder(kiteconnect.VarietyRegular, "1", nil); err == nil {
		t.Error("CancelOrder succeeded")
	}
	if _, err := r.Kite.InvalidateAccessToken(); err == nil {
		t.Error("InvalidateAccessToken succeeded")
	}
	if _, err := r.Kite.GetOrderHistory("1"); err == nil {
		t.Error("GetOrderHistory succeeded (only exact /orders is allowed)")
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hit %d times by refused calls", hits.Load())
	}
	if s := r.Guard.Stats(); s.Refused != 5 || s.Requests != 0 {
		t.Fatalf("stats = %+v, want 5 refused, 0 requests", s)
	}

	// The transport error itself says why.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/kite/orders/regular", nil)
	if _, err := r.Guard.RoundTrip(req); !errors.Is(err, ErrRefused) {
		t.Fatalf("RoundTrip err = %v, want ErrRefused", err)
	}

	// Allowed calls go through with the base path stripped for matching.
	if _, err := r.Kite.GetUserProfile(); err != nil {
		t.Fatalf("GetUserProfile: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestGuardRetries429(t *testing.T) {
	var hits atomic.Int64
	srv := upstream(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		if hits.Load() <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"status":"error","error_type":"NetworkException","message":"Too many requests"}`))
			return
		}
		ok(w, `{"NSE:INFY":{"instrument_token":408065,"last_price":1500.5}}`)
	})
	r := NewREST(config.Kite{RootURL: srv.URL, QuotePerSec: 100}, "key", "tok")
	r.Guard.backoff = 5 * time.Millisecond
	q, err := r.Kite.GetLTP("NSE:INFY")
	if err != nil {
		t.Fatalf("GetLTP: %v", err)
	}
	if q["NSE:INFY"].LastPrice != 1500.5 || hits.Load() != 3 {
		t.Fatalf("ltp=%v hits=%d", q["NSE:INFY"].LastPrice, hits.Load())
	}
	if s := r.Guard.Stats(); s.Throttled != 2 {
		t.Fatalf("throttled = %d, want 2", s.Throttled)
	}

	// A POST is never retried.
	hits.Store(0)
	_, err = r.Kite.GenerateSession("req", "secret")
	if hits.Load() != 1 || err == nil || !IsThrottled(err) {
		t.Fatalf("POST hits=%d err=%v; want one attempt and a 429", hits.Load(), err)
	}
}

func TestGuardSpacesRequests(t *testing.T) {
	var hits atomic.Int64
	srv := upstream(t, &hits, func(w http.ResponseWriter, r *http.Request) { ok(w, `{}`) })
	r := NewREST(config.Kite{RootURL: srv.URL, QuotePerSec: 10, OtherPerSec: 10}, "key", "tok")
	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := r.Kite.GetLTP("NSE:INFY"); err != nil {
			t.Fatal(err)
		}
	}
	// 10/s with 5% headroom is 105 ms apart: 4 gaps.
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("5 quote calls took %v; the limiter is not spacing them", el)
	}
	// Classes are independent: an /instruments call is not held behind quotes.
	start = time.Now()
	if _, err := r.Kite.GetUserProfile(); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 80*time.Millisecond {
		t.Fatalf("other-class call waited %v behind the quote class", el)
	}
}

func TestTokenError(t *testing.T) {
	var hits atomic.Int64
	srv := upstream(t, &hits, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"status":"error","error_type":"TokenException","message":"Incorrect api_key or access_token."}`))
	})
	r := NewREST(config.Kite{RootURL: srv.URL}, "key", "tok")
	_, err := r.Kite.GetLTP("NSE:INFY")
	if !IsTokenError(err) {
		t.Fatalf("GetLTP err %v not a token error", err)
	}
	_, err = r.FetchInstruments(t.Context())
	if !IsTokenError(err) {
		t.Fatalf("FetchInstruments err %v not a token error", err)
	}
	if IsTokenError(errors.New("x")) || IsTokenError(nil) {
		t.Fatal("false positive")
	}
}
