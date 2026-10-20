package kite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

// MaxQuoteKeys is the most instruments one /quote call accepts.
const MaxQuoteKeys = 500

// Quote is one instrument from /quote, reduced to what the recorders keep.
type Quote struct {
	Token     uint32
	Timestamp time.Time // exchange time of the quote; zero when Kite sends none
	LTT       time.Time // last trade time; zero for a contract that has not traded
	LTP, Avg  float64
	Open      float64
	High      float64
	Low       float64
	Close     float64 // previous close
	LTQ, Vol  int64
	TBQ, TSQ  int64
	OI        int64
	OIHi      int64
	OILo      int64
	Bid, Ask  float64
	BidQ      int64
	AskQ      int64
	Depth     *bus.Depth // nil for indices
}

// num accepts a JSON number, a numeric string or null. Kite sends integers for
// quantities and floats for prices, but has sent both in either place.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("kite: bad number %q", b)
	}
	*n = num(f)
	return nil
}

// kiteTime parses Kite's "2006-01-02 15:04:05" IST stamps; empty, null and the
// 1970 placeholder Kite uses for untraded contracts all read as zero.
type kiteTime time.Time

func (t *kiteTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*t = kiteTime{}
		return nil
	}
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, clock.IST)
	if err != nil {
		if v, err = time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("kite: bad time %q", s)
		}
	}
	if v.Year() <= 1970 {
		v = time.Time{}
	}
	*t = kiteTime(v)
	return nil
}

type rawLevel struct {
	Price    num `json:"price"`
	Quantity num `json:"quantity"`
	Orders   num `json:"orders"`
}

type rawQuote struct {
	Token     num      `json:"instrument_token"`
	Timestamp kiteTime `json:"timestamp"`
	LTT       kiteTime `json:"last_trade_time"`
	LTP       num      `json:"last_price"`
	LTQ       num      `json:"last_quantity"`
	TBQ       num      `json:"buy_quantity"`
	TSQ       num      `json:"sell_quantity"`
	Vol       num      `json:"volume"`
	Avg       num      `json:"average_price"`
	OI        num      `json:"oi"`
	OIHi      num      `json:"oi_day_high"`
	OILo      num      `json:"oi_day_low"`
	OHLC      struct {
		Open  num `json:"open"`
		High  num `json:"high"`
		Low   num `json:"low"`
		Close num `json:"close"`
	} `json:"ohlc"`
	Depth *struct {
		Buy  []rawLevel `json:"buy"`
		Sell []rawLevel `json:"sell"`
	} `json:"depth"`
}

func (q rawQuote) quote() Quote {
	out := Quote{
		Token: uint32(q.Token), Timestamp: time.Time(q.Timestamp), LTT: time.Time(q.LTT),
		LTP: float64(q.LTP), Avg: float64(q.Avg),
		Open: float64(q.OHLC.Open), High: float64(q.OHLC.High), Low: float64(q.OHLC.Low), Close: float64(q.OHLC.Close),
		LTQ: int64(q.LTQ), Vol: int64(q.Vol), TBQ: int64(q.TBQ), TSQ: int64(q.TSQ),
		OI: int64(q.OI), OIHi: int64(q.OIHi), OILo: int64(q.OILo),
	}
	if q.Depth != nil && (len(q.Depth.Buy) > 0 || len(q.Depth.Sell) > 0) {
		var d bus.Depth
		for i := 0; i < len(d.Buy) && i < len(q.Depth.Buy); i++ {
			l := q.Depth.Buy[i]
			d.Buy[i] = bus.Level{P: float64(l.Price), Q: uint32(l.Quantity), N: uint16(l.Orders)}
		}
		for i := 0; i < len(d.Sell) && i < len(q.Depth.Sell); i++ {
			l := q.Depth.Sell[i]
			d.Sell[i] = bus.Level{P: float64(l.Price), Q: uint32(l.Quantity), N: uint16(l.Orders)}
		}
		out.Depth = &d
		// An empty side comes back as a zero-price level; that is no quote, not a bid of 0.
		if d.Buy[0].P > 0 {
			out.Bid, out.BidQ = d.Buy[0].P, int64(d.Buy[0].Q)
		}
		if d.Sell[0].P > 0 {
			out.Ask, out.AskQ = d.Sell[0].P, int64(d.Sell[0].Q)
		}
	}
	return out
}

// Quotes fetches full quotes for keys ("NFO:NIFTY26OCT25000CE", "NSE:NIFTY 50"),
// at most MaxQuoteKeys per call. Unknown keys are simply absent from the result.
func (r *REST) Quotes(ctx context.Context, keys []string) (map[string]Quote, error) {
	if len(keys) == 0 {
		return map[string]Quote{}, nil
	}
	if len(keys) > MaxQuoteKeys {
		return nil, fmt.Errorf("kite: %d quote keys, limit %d", len(keys), MaxQuoteKeys)
	}
	body, err := r.get(ctx, "/quote?"+keyQuery(keys))
	if err != nil {
		return nil, err
	}
	var env struct {
		Data map[string]rawQuote `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("kite: decode quote: %w", err)
	}
	out := make(map[string]Quote, len(env.Data))
	for k, q := range env.Data {
		out[k] = q.quote()
	}
	return out, nil
}

// LTP fetches last prices for keys via /quote/ltp (same per-call limit as Quotes).
func (r *REST) LTP(ctx context.Context, keys []string) (map[string]float64, error) {
	if len(keys) == 0 {
		return map[string]float64{}, nil
	}
	if len(keys) > MaxQuoteKeys {
		return nil, fmt.Errorf("kite: %d ltp keys, limit %d", len(keys), MaxQuoteKeys)
	}
	body, err := r.get(ctx, "/quote/ltp?"+keyQuery(keys))
	if err != nil {
		return nil, err
	}
	var env struct {
		Data map[string]struct {
			LTP num `json:"last_price"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("kite: decode ltp: %w", err)
	}
	out := make(map[string]float64, len(env.Data))
	for k, v := range env.Data {
		out[k] = float64(v.LTP)
	}
	return out, nil
}

func keyQuery(keys []string) string {
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString("i=")
		b.WriteString(url.QueryEscape(k))
	}
	return b.String()
}

// Candle is one historical bar. OI is zero unless requested.
type Candle struct {
	TS         time.Time
	O, H, L, C float64
	V, OI      int64
}

// Intervals Kite serves, with the longest span one call may cover (verify the
// limits against the current Kite docs before changing the harvester's windows).
var Intervals = map[string]time.Duration{
	"minute": 60 * 24 * time.Hour, "3minute": 100 * 24 * time.Hour, "5minute": 100 * 24 * time.Hour,
	"10minute": 100 * 24 * time.Hour, "15minute": 200 * 24 * time.Hour, "30minute": 200 * 24 * time.Hour,
	"60minute": 400 * 24 * time.Hour, "day": 2000 * 24 * time.Hour,
}

// Historical fetches candles for token between from and to (inclusive, IST).
func (r *REST) Historical(ctx context.Context, token uint32, interval string, from, to time.Time, oi bool) ([]Candle, error) {
	if _, ok := Intervals[interval]; !ok {
		return nil, fmt.Errorf("kite: unknown interval %q", interval)
	}
	q := url.Values{}
	q.Set("from", from.In(clock.IST).Format("2006-01-02 15:04:05"))
	q.Set("to", to.In(clock.IST).Format("2006-01-02 15:04:05"))
	if oi {
		q.Set("oi", "1")
	}
	body, err := r.get(ctx, fmt.Sprintf("/instruments/historical/%d/%s?%s", token, interval, q.Encode()))
	if err != nil {
		return nil, err
	}
	return ParseCandles(body)
}

// ParseCandles decodes a historical response body.
func ParseCandles(body []byte) ([]Candle, error) {
	var env struct {
		Data struct {
			Candles [][]json.RawMessage `json:"candles"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("kite: decode candles: %w", err)
	}
	out := make([]Candle, 0, len(env.Data.Candles))
	for i, row := range env.Data.Candles {
		if len(row) < 6 {
			return nil, fmt.Errorf("kite: candle %d has %d fields", i, len(row))
		}
		var s string
		if err := json.Unmarshal(row[0], &s); err != nil {
			return nil, fmt.Errorf("kite: candle %d time: %w", i, err)
		}
		ts, err := time.Parse("2006-01-02T15:04:05-0700", s)
		if err != nil {
			if ts, err = time.Parse(time.RFC3339, s); err != nil {
				return nil, fmt.Errorf("kite: candle %d time %q", i, s)
			}
		}
		var f [6]num
		for j := 1; j < len(row) && j <= 6; j++ {
			if err := json.Unmarshal(row[j], &f[j-1]); err != nil {
				return nil, fmt.Errorf("kite: candle %d field %d: %w", i, j, err)
			}
		}
		out = append(out, Candle{TS: ts.In(clock.IST), O: float64(f[0]), H: float64(f[1]), L: float64(f[2]), C: float64(f[3]), V: int64(f[4]), OI: int64(f[5])})
	}
	return out, nil
}
