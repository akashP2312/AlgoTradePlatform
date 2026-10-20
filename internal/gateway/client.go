package gateway

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"pmt/internal/bus"
)

var errTooMany = errors.New("too many subscriptions for one tab")

const (
	writeWait  = 5 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 20 * time.Second
)

type client struct {
	h  *Hub
	ws *websocket.Conn

	mu      sync.Mutex
	subs    map[string]struct{}
	pats    map[string]struct{}
	pending map[string]json.RawMessage
	kick    chan struct{} // flush now, after a subscribe paints from the cache
}

func (c *client) wants(subject string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wantsLocked(subject)
}

func (c *client) wantsLocked(subject string) bool {
	if _, ok := c.subs[subject]; ok {
		return true
	}
	for p := range c.pats {
		if match(p, subject) {
			return true
		}
	}
	return false
}

func (c *client) queue(subject string, v json.RawMessage) {
	c.mu.Lock()
	c.queueLocked(subject, v)
	c.mu.Unlock()
}

func (c *client) queueLocked(subject string, v json.RawMessage) {
	if _, ok := c.pending[subject]; ok {
		c.h.Conflated.Add(1)
	}
	c.pending[subject] = v
}

func (c *client) take() map[string]json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	p := c.pending
	c.pending = make(map[string]json.RawMessage, len(p))
	return p
}

type frame struct {
	T     int64                      `json:"t"`
	Hello *hello                     `json:"hello,omitempty"`
	U     map[string]json.RawMessage `json:"u,omitempty"`
	Err   string                     `json:"err,omitempty"`
}

type hello struct {
	FlushMs int64 `json:"flush_ms"`
}

type request struct {
	Op string   `json:"op"`
	S  []string `json:"s"`
}

func (c *client) write(f frame) error {
	f.T = bus.Ms(c.h.Now())
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
		return err
	}
	c.h.Frames.Add(1)
	c.h.Bytes.Add(int64(len(b)))
	return nil
}

// writeLoop owns every write to the socket. errs carries replies from the
// reader so the two never write concurrently.
func (c *client) writeLoop(errs <-chan string, done <-chan struct{}) {
	flush := time.NewTicker(c.h.Flush)
	ping := time.NewTicker(pingPeriod)
	defer flush.Stop()
	defer ping.Stop()
	defer c.ws.Close()
	if c.write(frame{Hello: &hello{FlushMs: c.h.Flush.Milliseconds()}}) != nil {
		return
	}
	for {
		var err error
		select {
		case <-done:
			return
		case e := <-errs:
			err = c.write(frame{Err: e})
		case <-c.kick:
			if u := c.take(); u != nil {
				err = c.write(frame{U: u})
			}
		case <-flush.C:
			if u := c.take(); u != nil {
				err = c.write(frame{U: u})
			}
		case <-ping.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err = c.ws.WriteMessage(websocket.PingMessage, nil)
		}
		if err != nil {
			c.h.Dropped.Add(1)
			return
		}
	}
}

func (c *client) readLoop(errs chan<- string) {
	c.ws.SetReadLimit(maxReadSize)
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error { return c.ws.SetReadDeadline(time.Now().Add(pongWait)) })
	reply := func(e string) {
		select {
		case errs <- e:
		default:
		}
	}
	for {
		_, b, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var r request
		if err := json.Unmarshal(b, &r); err != nil {
			reply("bad request")
			continue
		}
		switch r.Op {
		case "sub":
			added, err := c.h.subscribe(c, r.S)
			if err != nil {
				reply(err.Error())
			}
			if added > 0 {
				select {
				case c.kick <- struct{}{}:
				default:
				}
			}
		case "unsub":
			c.h.unsubscribe(c, r.S)
		default:
			reply("unknown op " + r.Op)
		}
	}
}

// ServeHTTP upgrades one tab's connection and runs it until either side closes.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowedHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	up := websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 32 << 10,
		EnableCompression: true,
		CheckOrigin:       checkOrigin,
	}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader has already replied
	}
	ws.EnableWriteCompression(true)
	c := &client{h: h, ws: ws,
		subs: map[string]struct{}{}, pats: map[string]struct{}{},
		pending: map[string]json.RawMessage{}, kick: make(chan struct{}, 1)}
	h.add(c)
	slog.Debug("gateway: tab connected", "remote", r.RemoteAddr)
	errs := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		c.readLoop(errs)
		close(done)
	}()
	c.writeLoop(errs, done)
	h.remove(c)
	slog.Debug("gateway: tab closed", "remote", r.RemoteAddr)
}

// checkOrigin admits pages served from this machine or the tailnet. A browser
// always sends Origin; a missing one is a non-browser client.
func checkOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return allowedHost(u.Host)
}

// allowedHost checks a Host or Origin host: localhost, loopback, the Tailscale
// range or a MagicDNS name. Checking Host as well as Origin defeats DNS
// rebinding, where an outside page resolves its own name to 127.0.0.1.
func allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".ts.net") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return ip.IsLoopback() || cgnat.Contains(ip)
}
