// Package gateway fans bus messages out to browser tabs over one WebSocket per
// tab. Each tab subscribes to subjects or NATS-style patterns; the gateway keeps
// only the latest message per subject for it and flushes once per window, so a
// slow tab costs one pending value per subject and never backs up the bus.
//
// Wire format, JSON text frames:
//
//	browser -> gateway  {"op":"sub","s":["tick.nse.nifty_50._._.idx","tick.nfo.nifty.20261027.*.*"]}
//	                    {"op":"unsub","s":[...]}
//	gateway -> browser  {"t":<server now ms>,"hello":{"flush_ms":250}}
//	                    {"t":<server now ms>,"u":{"<subject>":<message>,...}}
//	                    {"t":<server now ms>,"err":"..."}
//
// t is the gateway's clock (shifted in a dry run), so the browser computes quote
// age as t minus the message's own receive time, not against its local clock.
package gateway

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"pmt/internal/clock"
)

// Limits per tab.
const (
	maxSubs     = 10000
	maxReadSize = 64 << 10
)

type Hub struct {
	Flush time.Duration
	Now   func() time.Time

	mu      sync.RWMutex
	last    map[string]json.RawMessage
	exact   map[string]map[*client]struct{}
	wild    map[*client]struct{} // tabs holding at least one pattern
	clients map[*client]struct{}

	Msgs, Invalid, Frames, Bytes, Conflated, Dropped atomic.Int64
}

func NewHub(flush time.Duration) *Hub {
	return &Hub{
		Flush:   flush,
		Now:     clock.Now,
		last:    map[string]json.RawMessage{},
		exact:   map[string]map[*client]struct{}{},
		wild:    map[*client]struct{}{},
		clients: map[*client]struct{}{},
	}
}

// Publish records the latest message on subject and queues it for every tab
// subscribed to it. Data must stay unmodified after the call.
func (h *Hub) Publish(subject string, data []byte) {
	if !json.Valid(data) {
		h.Invalid.Add(1)
		return
	}
	h.Msgs.Add(1)
	raw := json.RawMessage(data)
	h.mu.Lock()
	h.last[subject] = raw
	h.mu.Unlock()

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.exact[subject] {
		c.queue(subject, raw)
	}
	for c := range h.wild {
		if c.wants(subject) {
			c.queue(subject, raw)
		}
	}
}

// Seed fills the last-value cache from the KV buckets at start-up without
// overwriting anything that arrived live in the meantime.
func (h *Hub) Seed(subject string, data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.last[subject]; ok {
		return false
	}
	h.last[subject] = json.RawMessage(data)
	return true
}

// Last is the subject's latest value in the cache.
func (h *Hub) Last(subject string) (json.RawMessage, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	v, ok := h.last[subject]
	return v, ok
}

// Clients is the number of connected tabs.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Subjects is the size of the last-value cache.
func (h *Hub) Subjects() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.last)
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	delete(h.wild, c)
	c.mu.Lock()
	for s := range c.subs {
		h.unindex(s, c)
	}
	c.mu.Unlock()
}

func (h *Hub) unindex(s string, c *client) {
	if m := h.exact[s]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.exact, s)
		}
	}
}

// subscribe adds subjects for c and queues their cached values, so a new panel
// paints at once instead of waiting for the next tick.
func (h *Hub) subscribe(c *client, subs []string) (added int, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range subs {
		if e := checkSubject(s); e != nil {
			err = e
			continue
		}
		if _, ok := c.subs[s]; ok {
			continue
		}
		if len(c.subs)+len(c.pats) >= maxSubs {
			return added, errTooMany
		}
		added++
		if isPattern(s) {
			c.pats[s] = struct{}{}
			h.wild[c] = struct{}{}
			for subj, v := range h.last {
				if match(s, subj) {
					c.queueLocked(subj, v)
				}
			}
			continue
		}
		c.subs[s] = struct{}{}
		m := h.exact[s]
		if m == nil {
			m = map[*client]struct{}{}
			h.exact[s] = m
		}
		m[c] = struct{}{}
		if v, ok := h.last[s]; ok {
			c.queueLocked(s, v)
		}
	}
	return added, err
}

func (h *Hub) unsubscribe(c *client, subs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range subs {
		if _, ok := c.pats[s]; ok {
			delete(c.pats, s)
		} else if _, ok := c.subs[s]; ok {
			delete(c.subs, s)
			h.unindex(s, c)
		}
		for subj := range c.pending {
			if match(s, subj) && !c.wantsLocked(subj) {
				delete(c.pending, subj)
			}
		}
	}
	if len(c.pats) == 0 {
		delete(h.wild, c)
	}
}
