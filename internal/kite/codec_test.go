package kite

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zerodha/gokiteconnect/v4/models"
	kiteticker "github.com/zerodha/gokiteconnect/v4/ticker"

	"pmt/internal/bus"
)

func randPackets(r *rand.Rand, n int) []Packet {
	segs := []uint32{SegNseCM, SegNseFO, SegNseCD, SegBseFO, SegMcxFO, SegIndices, SegNco}
	px := func(seg uint32) float64 {
		d := divisor(seg)
		return float64(r.IntN(5_000_000)) / d
	}
	out := make([]Packet, n)
	for i := range out {
		seg := segs[r.IntN(len(segs))]
		p := Packet{Token: uint32(r.IntN(1<<23))<<8 | seg, Index: seg == SegIndices}
		p.Mode = "lqf"[r.IntN(3)]
		p.LTP = px(seg)
		if p.Mode != 'l' {
			p.Open, p.High, p.Low, p.Close = px(seg), px(seg), px(seg), px(seg)
			if p.Mode == 'f' {
				p.Exch = 1760000000 + int64(r.IntN(100000))
			}
			if !p.Index {
				p.LTQ, p.Avg, p.Vol, p.TBQ, p.TSQ = r.Uint32(), px(seg), r.Uint32(), r.Uint32(), r.Uint32()
				if p.Mode == 'f' {
					p.LTT, p.OI, p.OIHi, p.OILo = 1760000000+int64(r.IntN(100000)), r.Uint32(), r.Uint32(), r.Uint32()
					p.Depth = &bus.Depth{}
					for k := 0; k < 5; k++ {
						p.Depth.Buy[k] = bus.Level{P: px(seg), Q: r.Uint32(), N: uint16(r.IntN(1000))}
						p.Depth.Sell[k] = bus.Level{P: px(seg), Q: r.Uint32(), N: uint16(r.IntN(1000))}
					}
				}
			}
		}
		out[i] = p
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDecodeRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	in := randPackets(r, 500)
	var got []Packet
	n, err := Decode(Encode(in), func(p Packet) { got = append(got, p) })
	if err != nil || n != len(in) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for i := range in {
		a, b := in[i], got[i]
		if a.Token != b.Token || a.Mode != b.Mode || !near(a.LTP, b.LTP) || a.Vol != b.Vol || a.OI != b.OI ||
			a.Exch != b.Exch || a.LTT != b.LTT || !near(a.Close, b.Close) {
			t.Fatalf("packet %d: %+v != %+v", i, a, b)
		}
		if (a.Depth == nil) != (b.Depth == nil) || (a.Depth != nil && *a.Depth != *b.Depth) {
			t.Fatalf("packet %d depth differs", i)
		}
	}
}

func TestDecodeMalformed(t *testing.T) {
	if n, err := Decode([]byte{0}, func(Packet) {}); n != 0 || err != nil {
		t.Fatal("heartbeat")
	}
	f := Encode(randPackets(rand.New(rand.NewPCG(3, 4)), 3))
	n, err := Decode(f[:len(f)-5], func(Packet) {})
	if n != 2 || err == nil {
		t.Fatalf("truncated: n=%d err=%v", n, err)
	}
	// An unknown length is skipped, not fatal.
	odd := []byte{0, 2, 0, 3, 1, 2, 3}
	odd = append(odd, Encode(randPackets(rand.New(rand.NewPCG(5, 6)), 1))[2:]...)
	if n, err := Decode(odd, func(Packet) {}); n != 1 || err != nil {
		t.Fatalf("odd: n=%d err=%v", n, err)
	}
}

// TestConformance feeds the same frames to the official gokiteconnect ticker and to
// Decode, and requires identical values.
func TestConformance(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	var frames [][]byte
	var want []Packet
	for i := 0; i < 40; i++ {
		ps := randPackets(r, 1+r.IntN(30))
		frames = append(frames, Encode(ps))
		want = append(want, ps...)
	}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := up.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for _, f := range frames {
			c.WriteMessage(websocket.BinaryMessage, f)
		}
		c.ReadMessage() // hold the socket until the client closes
	}))
	defer srv.Close()

	var mu sync.Mutex
	var official []models.Tick
	done := make(chan struct{})
	tk := kiteticker.New("k", "t")
	u, _ := url.Parse(strings.Replace(srv.URL, "http", "ws", 1))
	tk.SetRootURL(*u)
	tk.SetAutoReconnect(false)
	tk.OnTick(func(x models.Tick) {
		mu.Lock()
		official = append(official, x)
		if len(official) == len(want) {
			close(done)
		}
		mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go tk.ServeWithContext(ctx)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("official ticker delivered %d of %d", len(official), len(want))
	}
	tk.Stop()

	var ours []Packet
	for _, f := range frames {
		Decode(f, func(p Packet) { ours = append(ours, p) })
	}
	modes := map[byte]string{'l': "ltp", 'q': "quote", 'f': "full"}
	for i, o := range official {
		p := ours[i]
		bad := o.InstrumentToken != p.Token || o.Mode != modes[p.Mode] || o.IsIndex != p.Index ||
			!near(o.LastPrice, p.LTP) || !near(o.OHLC.Open, p.Open) || !near(o.OHLC.High, p.High) ||
			!near(o.OHLC.Low, p.Low) || !near(o.OHLC.Close, p.Close) || !near(o.AverageTradePrice, p.Avg) ||
			o.LastTradedQuantity != p.LTQ || o.VolumeTraded != p.Vol || o.TotalBuyQuantity != p.TBQ ||
			o.TotalSellQuantity != p.TSQ || o.OI != p.OI || o.OIDayHigh != p.OIHi || o.OIDayLow != p.OILo
		if p.Exch > 0 && o.Timestamp.Unix() != p.Exch || p.LTT > 0 && o.LastTradeTime.Unix() != p.LTT {
			bad = true
		}
		if p.Depth != nil {
			for k := 0; k < 5; k++ {
				ob, os := o.Depth.Buy[k], o.Depth.Sell[k]
				pb, ps := p.Depth.Buy[k], p.Depth.Sell[k]
				if !near(ob.Price, pb.P) || ob.Quantity != pb.Q || ob.Orders != uint32(pb.N) ||
					!near(os.Price, ps.P) || os.Quantity != ps.Q || os.Orders != uint32(ps.N) {
					bad = true
				}
			}
		}
		if bad {
			t.Fatalf("tick %d differs:\nofficial %+v\nours     %+v", i, o, p)
		}
	}
}

func newRand() *rand.Rand { return rand.New(rand.NewPCG(9, 10)) }
