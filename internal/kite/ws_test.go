package kite

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeWS struct {
	mu       sync.Mutex
	msgs     []string
	dials    atomic.Int64
	token    atomic.Value
	conns    chan *websocket.Conn
	stopBeat atomic.Bool
}

func (f *fakeWS) handler(t *testing.T) http.HandlerFunc {
	up := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		if r.URL.Query().Get("access_token") != f.token.Load().(string) {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		select {
		case f.conns <- c:
		default:
		}
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.msgs = append(f.msgs, string(m))
			f.mu.Unlock()
		}
	}
}

func (f *fakeWS) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWSSubscribeReconnectAuth(t *testing.T) {
	f := &fakeWS{conns: make(chan *websocket.Conn, 4)}
	f.token.Store("good")
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	var tok atomic.Value
	tok.Store("bad")
	var packets atomic.Int64
	w := &WS{
		Name: "t1", URL: strings.Replace(srv.URL, "http", "ws", 1), APIKey: "k",
		Token:    func() string { return tok.Load().(string) },
		Silence:  300 * time.Millisecond,
		OnPacket: func(Packet, time.Time) { packets.Add(1) },
	}
	if err := w.Set(map[uint32]byte{256265: 'f', 260105: 'q'}); err != nil {
		t.Fatal(err)
	}
	if err := w.Set(make(map[uint32]byte, 3001)); err != nil {
		t.Fatal("empty map must pass")
	}
	big := map[uint32]byte{}
	for i := uint32(0); i < 3001; i++ {
		big[i] = 'l'
	}
	if w.Set(big) == nil {
		t.Fatal("3001 tokens accepted")
	}
	w.Set(map[uint32]byte{256265: 'f', 260105: 'q'})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor(t, "auth refusal", func() bool { return f.dials.Load() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if f.dials.Load() != 1 {
		t.Fatalf("redialled %d times with the same refused token", f.dials.Load())
	}
	tok.Store("good")

	c := <-f.conns
	waitFor(t, "subscription", func() bool { return len(f.messages()) >= 3 })
	var sub struct {
		A string `json:"a"`
		V []uint32
	}
	json.Unmarshal([]byte(f.messages()[0]), &sub)
	if sub.A != "subscribe" || len(sub.V) != 2 {
		t.Fatalf("first message %s", f.messages()[0])
	}
	c.WriteMessage(websocket.BinaryMessage, Encode(randPackets(newRand(), 5)))
	waitFor(t, "packets", func() bool { return packets.Load() == 5 })

	// Changing the set sends unsubscribe + subscribe + mode.
	before := len(f.messages())
	w.Set(map[uint32]byte{256265: 'f', 1: 'l'})
	waitFor(t, "diff", func() bool { return len(f.messages()) >= before+3 })
	if !strings.Contains(f.messages()[before], "unsubscribe") {
		t.Fatalf("diff starts with %s", f.messages()[before])
	}

	// Silence forces a reconnect, which replays the whole subscription.
	c2 := <-f.conns
	_ = c2
	if w.Reconnects.Load() < 1 {
		t.Fatal("no reconnect after silence")
	}
	waitFor(t, "replay", func() bool {
		for _, m := range f.messages()[before+3:] {
			if strings.Contains(m, `"subscribe"`) && strings.Contains(m, "[1,256265]") {
				return true
			}
		}
		return false
	})
}
