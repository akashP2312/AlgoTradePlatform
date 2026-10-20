package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"tick.nse.nifty", "tick.nse.nifty", true},
		{"tick.nse.*", "tick.nse.nifty", true},
		{"tick.nse.*", "tick.nse.nifty.x", false},
		{"tick.>", "tick.nse.nifty.x", true},
		{"tick.>", "tick", false},
		{"tick.*.nifty", "tick.nfo.nifty", true},
		{"tick.*.nifty", "tick.nfo.banknifty", false},
		{"tick.nse", "tick.nse.nifty", false},
		{"sys.*.health", "sys.ticker.health", true},
	}
	for _, c := range cases {
		if got := match(c.p, c.s); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.p, c.s, got, c.want)
		}
	}
}

func TestCheckSubject(t *testing.T) {
	ok := []string{"tick.nse.nifty_50._._.idx", "tick.>", "analytics.iv.*", "sys.*.health"}
	bad := []string{"order.update", "orders.>", "tick.", "tick..x", "tick.>.x", "tick.ni*", ">", "kv.session", "tick.a b", strings.Repeat("tick.x", 50)}
	for _, s := range ok {
		if err := checkSubject(s); err != nil {
			t.Errorf("checkSubject(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if checkSubject(s) == nil {
			t.Errorf("checkSubject(%q) = nil, want an error", s)
		}
	}
}

func TestAllowedHost(t *testing.T) {
	for _, h := range []string{"localhost:8091", "127.0.0.1:8091", "[::1]:8091", "pmt.localhost", "box.tail1234.ts.net", "100.101.2.3:8091"} {
		if !allowedHost(h) {
			t.Errorf("allowedHost(%q) = false", h)
		}
	}
	for _, h := range []string{"evil.example", "evil.example:8091", "192.168.1.5:8091", "10.0.0.1", "ts.net.evil.example", "100.128.0.1"} {
		if allowedHost(h) {
			t.Errorf("allowedHost(%q) = true", h)
		}
	}
}

// harness runs a hub behind an httptest server.
func harness(t *testing.T, flush time.Duration) (*Hub, string) {
	t.Helper()
	h := NewHub(flush)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	var f frame
	read(t, ws, &f)
	if f.Hello == nil || f.T == 0 {
		t.Fatalf("first frame %+v, want hello", f)
	}
	return ws
}

func read(t *testing.T, ws *websocket.Conn, f *frame) {
	t.Helper()
	*f = frame{} // ReadJSON merges into an existing map
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := ws.ReadJSON(f); err != nil {
		t.Fatal(err)
	}
}

func send(t *testing.T, ws *websocket.Conn, op string, s ...string) {
	t.Helper()
	if err := ws.WriteJSON(request{Op: op, S: s}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotOnSubscribe(t *testing.T) {
	h, url := harness(t, time.Hour) // no timed flush: only the kick may deliver
	h.Seed("tick.nse.nifty", []byte(`{"ltp":1}`))
	h.Seed("tick.nse.other", []byte(`{"ltp":2}`))
	ws := dial(t, url)
	send(t, ws, "sub", "tick.nse.nifty")
	var f frame
	read(t, ws, &f)
	if len(f.U) != 1 || string(f.U["tick.nse.nifty"]) != `{"ltp":1}` {
		t.Fatalf("snapshot %v", f.U)
	}
}

func TestWildcardSnapshotAndLive(t *testing.T) {
	h, url := harness(t, 20*time.Millisecond)
	h.Seed("tick.nfo.nifty.20261027.25000.ce", []byte(`1`))
	h.Seed("tick.nfo.nifty.20261027.25000.pe", []byte(`2`))
	h.Seed("tick.nse.nifty", []byte(`3`))
	ws := dial(t, url)
	send(t, ws, "sub", "tick.nfo.nifty.20261027.*.*")
	var f frame
	read(t, ws, &f)
	if len(f.U) != 2 {
		t.Fatalf("snapshot %v, want the two options", f.U)
	}
	h.Publish("tick.nse.nifty", []byte(`4`))
	h.Publish("tick.nfo.nifty.20261027.25100.ce", []byte(`5`))
	read(t, ws, &f)
	if len(f.U) != 1 || string(f.U["tick.nfo.nifty.20261027.25100.ce"]) != `5` {
		t.Fatalf("live %v", f.U)
	}
}

func TestConflation(t *testing.T) {
	h, url := harness(t, 150*time.Millisecond)
	h.Seed("tick.nse.nifty", []byte(`0`))
	ws := dial(t, url)
	send(t, ws, "sub", "tick.nse.nifty")
	var f frame
	read(t, ws, &f) // snapshot: the subscription is live from here
	for i := 1; i <= 500; i++ {
		h.Publish("tick.nse.nifty", []byte(fmt.Sprint(i)))
	}
	frames := 0
	last := ""
	for last != "500" {
		read(t, ws, &f)
		frames++
		last = string(f.U["tick.nse.nifty"])
	}
	if frames > 2 {
		t.Errorf("%d frames for 500 updates in one window, want at most 2", frames)
	}
	if h.Conflated.Load() < 490 {
		t.Errorf("conflated %d, want nearly all 500", h.Conflated.Load())
	}
}

func TestUnsubscribe(t *testing.T) {
	h, url := harness(t, 20*time.Millisecond)
	h.Seed("tick.nse.a", []byte(`0`))
	ws := dial(t, url)
	send(t, ws, "sub", "tick.nse.a", "tick.nse.b")
	var f frame
	read(t, ws, &f)
	send(t, ws, "unsub", "tick.nse.a")
	// The unsub is processed in order before the probe below, so once the
	// probe's error comes back the unsubscribe has landed.
	send(t, ws, "bogus")
	read(t, ws, &f)
	if f.Err == "" {
		t.Fatalf("got %+v, want the probe error", f)
	}
	h.Publish("tick.nse.a", []byte(`1`))
	h.Publish("tick.nse.b", []byte(`2`))
	read(t, ws, &f)
	if _, ok := f.U["tick.nse.a"]; ok || string(f.U["tick.nse.b"]) != `2` {
		t.Fatalf("after unsub %v", f.U)
	}
	if h.Subjects() != 2 {
		t.Errorf("cache %d subjects, want 2", h.Subjects())
	}
}

func TestRejectsDisallowedSubjects(t *testing.T) {
	h, url := harness(t, 20*time.Millisecond)
	h.Publish("order.update", []byte(`{"x":1}`)) // never on the gateway's subscriptions, but prove it cannot leak
	ws := dial(t, url)
	send(t, ws, "sub", "order.update", ">")
	var f frame
	read(t, ws, &f)
	if f.Err == "" || f.U != nil {
		t.Fatalf("got %+v, want an error and no data", f)
	}
}

func TestInvalidJSONDropped(t *testing.T) {
	h := NewHub(time.Second)
	h.Publish("tick.nse.x", []byte(`{not json`))
	if h.Subjects() != 0 || h.Invalid.Load() != 1 {
		t.Fatalf("subjects %d invalid %d", h.Subjects(), h.Invalid.Load())
	}
}

func TestRejectsForeignOriginAndHost(t *testing.T) {
	_, url := harness(t, time.Second)
	hdr := http.Header{"Origin": {"https://evil.example"}}
	_, resp, err := websocket.DefaultDialer.Dial(url, hdr)
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err %v resp %v", err, resp)
	}

	// DNS rebinding: a foreign name pointed at loopback arrives with that Host.
	h := NewHub(time.Second)
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Host = "evil.example:8091"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign host: %d", rec.Code)
	}

	ok := http.Header{"Origin": {"http://localhost:5173"}}
	ws, _, err := websocket.DefaultDialer.Dial(url, ok)
	if err != nil {
		t.Fatalf("localhost origin rejected: %v", err)
	}
	ws.Close()
}

func TestFrameShape(t *testing.T) {
	b, _ := json.Marshal(frame{T: 5, U: map[string]json.RawMessage{"tick.a": json.RawMessage(`{"ltp":1}`)}})
	if string(b) != `{"t":5,"u":{"tick.a":{"ltp":1}}}` {
		t.Fatal(string(b))
	}
}
