package kite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const DefaultWS = "wss://ws.kite.trade"

// WS is one ticker connection. It keeps the desired subscription and replays it on
// every reconnect. Kite allows 3000 instruments per connection and 3 connections per
// API key; the caller splits the universe and this type enforces the per-connection cap.
type WS struct {
	Name    string
	URL     string // ws root, DefaultWS when empty
	APIKey  string
	Token   func() string // current access token; read on every dial
	Silence time.Duration // no frame (heartbeats included) for this long forces a reconnect
	Max     int           // tokens per connection, 3000 when zero

	OnPacket func(p Packet, recv time.Time)
	OnText   func(raw []byte) // order updates and error messages
	OnState  func(up bool, err error)

	mu      sync.Mutex
	want    map[uint32]byte
	conn    *websocket.Conn
	writeMu sync.Mutex

	Frames, Packets, Malformed, Reconnects atomic.Int64
	LastFrame                              atomic.Int64 // unix ms
	up                                     atomic.Bool
}

var ErrAuth = errors.New("kite ws: handshake refused (token)")

func (w *WS) Up() bool { return w.up.Load() }

func (w *WS) max() int {
	if w.Max > 0 {
		return w.Max
	}
	return 3000
}

// Set replaces the desired subscription (token -> mode 'l'|'q'|'f') and applies the
// difference on the live socket.
func (w *WS) Set(want map[uint32]byte) error {
	if len(want) > w.max() {
		return fmt.Errorf("kite ws %s: %d tokens exceeds %d per connection", w.Name, len(want), w.max())
	}
	w.mu.Lock()
	old := w.want
	w.want = make(map[uint32]byte, len(want))
	for k, v := range want {
		w.want[k] = v
	}
	c := w.conn
	w.mu.Unlock()
	if c == nil {
		return nil
	}
	var drop []uint32
	for t := range old {
		if _, ok := want[t]; !ok {
			drop = append(drop, t)
		}
	}
	add := map[uint32]byte{}
	for t, m := range want {
		if old[t] != m {
			add[t] = m
		}
	}
	if len(drop) > 0 {
		if err := w.send(c, "unsubscribe", sorted(drop)); err != nil {
			return err
		}
	}
	return w.subscribe(c, add)
}

func (w *WS) subscribe(c *websocket.Conn, m map[uint32]byte) error {
	if len(m) == 0 {
		return nil
	}
	by := map[byte][]uint32{}
	var all []uint32
	for t, mode := range m {
		by[mode] = append(by[mode], t)
		all = append(all, t)
	}
	if err := w.send(c, "subscribe", sorted(all)); err != nil {
		return err
	}
	for mode, ts := range by {
		if err := w.send(c, "mode", []any{modeName(mode), sorted(ts)}); err != nil {
			return err
		}
	}
	return nil
}

func modeName(m byte) string {
	switch m {
	case 'f':
		return "full"
	case 'q':
		return "quote"
	}
	return "ltp"
}

func sorted(ts []uint32) []uint32 {
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	return ts
}

func (w *WS) send(c *websocket.Conn, action string, v any) error {
	b, err := json.Marshal(map[string]any{"a": action, "v": v})
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.WriteMessage(websocket.TextMessage, b)
}

// Run dials and reads until ctx ends, reconnecting with capped, jittered backoff.
// A refused handshake (expired token) waits for Token() to change before retrying.
func (w *WS) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		tok := w.Token()
		start := time.Now()
		err := w.session(ctx, tok)
		if ctx.Err() != nil {
			return
		}
		w.Reconnects.Add(1)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if errors.Is(err, ErrAuth) {
			slog.Warn("kite ws auth refused; waiting for a new token", "conn", w.Name)
			for ctx.Err() == nil && w.Token() == tok {
				sleepCtx(ctx, 5*time.Second)
			}
			continue
		}
		d := backoff/2 + rand.N(backoff)
		slog.Warn("kite ws reconnecting", "conn", w.Name, "in", d.Round(time.Millisecond), "err", err)
		sleepCtx(ctx, d)
		backoff = min(backoff*2, 30*time.Second)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (w *WS) session(ctx context.Context, token string) error {
	root := w.URL
	if root == "" {
		root = DefaultWS
	}
	u, err := url.Parse(root)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("api_key", w.APIKey)
	q.Set("access_token", token)
	u.RawQuery = q.Encode()

	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: http.ProxyFromEnvironment}
	c, resp, err := d.DialContext(ctx, u.String(), nil)
	if err != nil {
		if resp != nil && (resp.StatusCode == 403 || resp.StatusCode == 401) {
			return ErrAuth
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL carries the token; never log it
		}
		if resp != nil {
			return fmt.Errorf("dial: http %d: %v", resp.StatusCode, err)
		}
		return fmt.Errorf("dial: %v", err)
	}
	defer c.Close()

	w.mu.Lock()
	w.conn = c
	want := make(map[uint32]byte, len(w.want))
	for k, v := range w.want {
		want[k] = v
	}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.conn = nil
		w.mu.Unlock()
		w.up.Store(false)
		if w.OnState != nil {
			w.OnState(false, nil)
		}
	}()
	if err := w.subscribe(c, want); err != nil {
		return err
	}
	w.up.Store(true)
	if w.OnState != nil {
		w.OnState(true, nil)
	}
	slog.Info("kite ws connected", "conn", w.Name, "tokens", len(want))

	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	silence := w.Silence
	if silence <= 0 {
		silence = 10 * time.Second
	}
	for {
		c.SetReadDeadline(time.Now().Add(silence))
		mt, data, err := c.ReadMessage()
		if err != nil {
			var ne interface{ Timeout() bool }
			if errors.As(err, &ne) && ne.Timeout() {
				return fmt.Errorf("silent for %s", silence)
			}
			return err
		}
		now := time.Now()
		w.Frames.Add(1)
		w.LastFrame.Store(now.UnixMilli())
		switch mt {
		case websocket.BinaryMessage:
			n, err := Decode(data, func(p Packet) {
				if w.OnPacket != nil {
					w.OnPacket(p, now)
				}
			})
			w.Packets.Add(int64(n))
			if err != nil {
				w.Malformed.Add(1)
			}
		case websocket.TextMessage:
			if w.OnText != nil {
				w.OnText(data)
			}
		}
	}
}

// TextMessage is Kite's JSON envelope on the ticker socket.
type TextMessage struct {
	Type string          `json:"type"` // order | error | message
	Data json.RawMessage `json:"data"`
}
