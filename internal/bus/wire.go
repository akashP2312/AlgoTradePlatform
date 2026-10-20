package bus

import (
	"fmt"
	"time"
)

// Tick is one normalised quote, published as JSON on tick.<contract subject>.
// Keys are short because this is the highest-volume message on the bus. Times
// are Unix milliseconds; zero means the feed did not send the field.
type Tick struct {
	C     string  `json:"c"`            // contract key, NFO|NIFTY|2026-10-27|25000|CE
	T     uint32  `json:"t"`            // Kite instrument token
	Mode  string  `json:"m"`            // l (ltp) | q (quote) | f (full)
	Recv  int64   `json:"r"`            // local receive time
	Exch  int64   `json:"e,omitempty"`  // exchange timestamp
	LTT   int64   `json:"lt,omitempty"` // last trade time
	LTP   float64 `json:"p"`
	LTQ   uint32  `json:"q,omitempty"`
	Vol   uint64  `json:"v,omitempty"` // cumulative day volume
	OI    uint64  `json:"oi,omitempty"`
	OIHi  uint64  `json:"oh,omitempty"`
	OILo  uint64  `json:"ol,omitempty"`
	Bid   float64 `json:"b,omitempty"`
	Ask   float64 `json:"a,omitempty"`
	BidQ  uint32  `json:"bq,omitempty"`
	AskQ  uint32  `json:"aq,omitempty"`
	TBQ   uint64  `json:"tb,omitempty"` // total buy quantity
	TSQ   uint64  `json:"ts,omitempty"` // total sell quantity
	Avg   float64 `json:"ap,omitempty"`
	Open  float64 `json:"o,omitempty"`
	High  float64 `json:"h,omitempty"`
	Low   float64 `json:"lo,omitempty"`
	Close float64 `json:"cl,omitempty"` // previous close, as Kite sends it
	Phase string  `json:"ph"`           // p pre-open | c continuous | a after | x other exchange
	Depth *Depth  `json:"d,omitempty"`  // full mode only; kept in KV, not in the ticks table
}

type Level struct {
	P float64 `json:"p"`
	Q uint32  `json:"q"`
	N uint16  `json:"n"`
}

type Depth struct {
	Buy  [5]Level `json:"b"`
	Sell [5]Level `json:"s"`
}

func Ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func FromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// Chain is one minute's snapshot of one expiry, published on chain.<und>.<yyyymmdd>
// with message ID ws-<und>-<yyyymmdd>-<unix minute> so a restarted ticker cannot
// double-record a minute.
type Chain struct {
	TS     int64      `json:"ts"`  // the IST minute the snapshot represents
	Source string     `json:"src"` // ws | sweep | backup
	Und    string     `json:"u"`   // NIFTY
	Expiry string     `json:"x"`   // 2026-10-27
	Spot   float64    `json:"s,omitempty"`
	Fut    float64    `json:"f,omitempty"` // future of the same expiry when listed, else the nearest
	Rows   []ChainRow `json:"rows"`
}

type ChainRow struct {
	C        string  `json:"c"`
	T        uint32  `json:"t"`
	Strike   float64 `json:"k"`
	Type     string  `json:"o"` // CE | PE
	Bid      float64 `json:"b,omitempty"`
	Ask      float64 `json:"a,omitempty"`
	BidQ     uint32  `json:"bq,omitempty"`
	AskQ     uint32  `json:"aq,omitempty"`
	LTP      float64 `json:"p,omitempty"`
	LTQ      uint32  `json:"q,omitempty"`
	LTT      int64   `json:"lt,omitempty"`
	Exch     int64   `json:"e,omitempty"`
	QuoteAge int64   `json:"qa"` // ticker: ms from the quote's receipt to the minute; REST: from its exchange time to the fetch; -1 unknown
	TradeAge int64   `json:"ta"` // ms since the last trade, -1 when unknown
	OI       uint64  `json:"oi,omitempty"`
	Vol      uint64  `json:"v,omitempty"`
	Avg      float64 `json:"ap,omitempty"`
	TBQ      uint64  `json:"tb,omitempty"`
	TSQ      uint64  `json:"ts,omitempty"`
}

// ChainSubject is chain.<und>.<yyyymmdd>.
func ChainSubject(und string, expiry time.Time) string {
	return fmt.Sprintf("chain.%s.%s", lower(und), expiry.Format("20060102"))
}

// ChainMsgID dedupes a snapshot on the CHAIN stream.
func ChainMsgID(source, und string, expiry, minute time.Time) string {
	return fmt.Sprintf("%s-%s-%s-%d", source, lower(und), expiry.Format("20060102"), minute.Unix()/60)
}

// Health is published every few seconds on sys.<service>.health.
type Health struct {
	Service string            `json:"service"`
	At      int64             `json:"at"`
	OK      bool              `json:"ok"`
	Detail  map[string]string `json:"detail,omitempty"`
}

func HealthSubject(service string) string { return "sys." + lower(service) + ".health" }

// Session is the Kite login stored in KV kite_session/current and in the token file.
type Session struct {
	AccessToken string    `json:"access_token"`
	APIKey      string    `json:"api_key"`
	UserID      string    `json:"user_id,omitempty"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Valid reports whether the session has a token and has not passed its expiry.
func (s Session) Valid(now time.Time) bool {
	return s.AccessToken != "" && now.Before(s.ExpiresAt)
}

// String never includes the token.
func (s Session) String() string {
	return fmt.Sprintf("kite session user=%s issued=%s expires=%s", s.UserID,
		s.IssuedAt.Format(time.RFC3339), s.ExpiresAt.Format(time.RFC3339))
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
