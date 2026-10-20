package kite

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"pmt/internal/bus"
)

// Kite ticker binary format. A frame is [n uint16] then n × ([len uint16][packet]).
// A 1-byte frame is a heartbeat. Packet lengths identify the mode:
//
//	8    ltp           28  index quote     32  index full
//	44   quote         184 full (with 5-level depth)
//
// The low byte of the token is the segment, which sets the price divisor.
const (
	lenLTP       = 8
	lenIndexQ    = 28
	lenIndexFull = 32
	lenQuote     = 44
	lenFull      = 184
)

const (
	SegNseCM   = 1
	SegNseFO   = 2
	SegNseCD   = 3
	SegBseCM   = 4
	SegBseFO   = 5
	SegBseCD   = 6
	SegMcxFO   = 7
	SegMcxSX   = 8
	SegIndices = 9
	SegNco     = 12
)

var ErrMalformed = errors.New("kite: malformed ticker frame")

// Packet is one decoded quote with prices already in rupees. Times are Unix seconds.
type Packet struct {
	Token uint32
	Mode  byte // 'l' ltp | 'q' quote | 'f' full
	Index bool

	LTP, Avg, Open, High, Low, Close float64
	LTQ, Vol, TBQ, TSQ               uint32
	LTT, Exch                        int64
	OI, OIHi, OILo                   uint32
	Depth                            *bus.Depth
}

func (p Packet) Segment() uint32 { return p.Token & 0xff }

func divisor(seg uint32) float64 {
	switch seg {
	case SegNseCD:
		return 1e7
	case SegBseCD, SegNco:
		return 1e4
	}
	return 100
}

// Decode walks a binary frame and calls fn for each packet. Unknown packet lengths
// are skipped; a frame whose lengths overrun its size returns ErrMalformed after
// delivering the packets before the fault.
func Decode(frame []byte, fn func(Packet)) (int, error) {
	if len(frame) < 2 {
		return 0, nil // heartbeat
	}
	n := int(binary.BigEndian.Uint16(frame))
	j, got := 2, 0
	for i := 0; i < n; i++ {
		if j+2 > len(frame) {
			return got, fmt.Errorf("%w: packet %d header past end", ErrMalformed, i)
		}
		l := int(binary.BigEndian.Uint16(frame[j:]))
		j += 2
		if j+l > len(frame) {
			return got, fmt.Errorf("%w: packet %d len %d past end", ErrMalformed, i, l)
		}
		if p, ok := decodePacket(frame[j : j+l]); ok {
			fn(p)
			got++
		}
		j += l
	}
	return got, nil
}

func decodePacket(b []byte) (Packet, bool) {
	u32 := func(o int) uint32 { return binary.BigEndian.Uint32(b[o:]) }
	if len(b) < lenLTP {
		return Packet{}, false
	}
	p := Packet{Token: u32(0)}
	seg := p.Segment()
	p.Index = seg == SegIndices
	d := divisor(seg)
	price := func(o int) float64 { return float64(u32(o)) / d }
	p.LTP = price(4)
	switch len(b) {
	case lenLTP:
		p.Mode = 'l'
	case lenIndexQ, lenIndexFull:
		p.Mode = 'q'
		p.High, p.Low, p.Open, p.Close = price(8), price(12), price(16), price(20)
		if len(b) == lenIndexFull {
			p.Mode = 'f'
			p.Exch = int64(u32(28))
		}
	case lenQuote, lenFull:
		p.Mode = 'q'
		p.LTQ, p.Avg, p.Vol, p.TBQ, p.TSQ = u32(8), price(12), u32(16), u32(20), u32(24)
		p.Open, p.High, p.Low, p.Close = price(28), price(32), price(36), price(40)
		if len(b) == lenFull {
			p.Mode = 'f'
			p.LTT, p.OI, p.OIHi, p.OILo, p.Exch = int64(u32(44)), u32(48), u32(52), u32(56), int64(u32(60))
			dp := &bus.Depth{}
			for i := 0; i < 5; i++ {
				bo, so := 64+12*i, 124+12*i
				dp.Buy[i] = bus.Level{Q: u32(bo), P: price(bo + 4), N: binary.BigEndian.Uint16(b[bo+8:])}
				dp.Sell[i] = bus.Level{Q: u32(so), P: price(so + 4), N: binary.BigEndian.Uint16(b[so+8:])}
			}
			p.Depth = dp
		}
	default:
		return Packet{}, false
	}
	return p, true
}

// Tick normalises a packet for the bus. phase is set by the caller from the session clock.
func (p Packet) Tick(contract string, recv time.Time, phase string) bus.Tick {
	t := bus.Tick{
		C: contract, T: p.Token, Mode: string(p.Mode), Recv: bus.Ms(recv), Phase: phase,
		LTP: p.LTP, LTQ: p.LTQ, Vol: uint64(p.Vol), TBQ: uint64(p.TBQ), TSQ: uint64(p.TSQ), Avg: p.Avg,
		Open: p.Open, High: p.High, Low: p.Low, Close: p.Close,
		OI: uint64(p.OI), OIHi: uint64(p.OIHi), OILo: uint64(p.OILo), Depth: p.Depth,
	}
	if p.Exch > 0 {
		t.Exch = p.Exch * 1000
	}
	if p.LTT > 0 {
		t.LTT = p.LTT * 1000
	}
	if p.Depth != nil {
		t.Bid, t.BidQ = p.Depth.Buy[0].P, p.Depth.Buy[0].Q
		t.Ask, t.AskQ = p.Depth.Sell[0].P, p.Depth.Sell[0].Q
	}
	return t
}

// Encode builds a frame in Kite's format; fakekite and the tests use it.
func Encode(pkts []Packet) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(len(pkts)))
	for _, p := range pkts {
		b := encodePacket(p)
		out = binary.BigEndian.AppendUint16(out, uint16(len(b)))
		out = append(out, b...)
	}
	return out
}

func encodePacket(p Packet) []byte {
	d := divisor(p.Segment())
	var b []byte
	put := func(v uint32) { b = binary.BigEndian.AppendUint32(b, v) }
	price := func(v float64) { put(uint32(math.Round(v * d))) }
	put(p.Token)
	price(p.LTP)
	if p.Mode == 'l' {
		return b
	}
	if p.Token&0xff == SegIndices {
		price(p.High)
		price(p.Low)
		price(p.Open)
		price(p.Close)
		put(0) // net change, ignored by readers
		if p.Mode == 'f' {
			put(uint32(p.Exch))
		}
		return b
	}
	put(p.LTQ)
	price(p.Avg)
	put(p.Vol)
	put(p.TBQ)
	put(p.TSQ)
	price(p.Open)
	price(p.High)
	price(p.Low)
	price(p.Close)
	if p.Mode != 'f' {
		return b
	}
	put(uint32(p.LTT))
	put(p.OI)
	put(p.OIHi)
	put(p.OILo)
	put(uint32(p.Exch))
	dp := p.Depth
	if dp == nil {
		dp = &bus.Depth{}
	}
	for _, side := range [][5]bus.Level{dp.Buy, dp.Sell} {
		for _, l := range side {
			put(l.Q)
			price(l.P)
			b = binary.BigEndian.AppendUint16(b, l.N)
			b = append(b, 0, 0)
		}
	}
	return b
}
