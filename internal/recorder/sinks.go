package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/refdata"
)

// ---- ticks ----------------------------------------------------------------

var tickCols = []string{"recv_ts", "exch_ts", "ltt", "contract", "token", "ltp", "ltq", "volume", "oi",
	"bid", "ask", "bid_qty", "ask_qty", "tbq", "tsq", "avg_price", "open", "high", "low", "close", "phase"}

// ticksSink copies raw ticks into ticks and folds them into live 1-minute bars.
// Market depth is not stored; the chain snapshots keep the top of book.
type ticksSink struct {
	rows [][]any
	bars *Bars
}

func newTicksSink(batch int, open func(time.Time) time.Time) *ticksSink {
	return &ticksSink{rows: make([][]any, 0, batch), bars: NewBars(open)}
}

func (s *ticksSink) size() int     { return len(s.rows) }
func (s *ticksSink) fetchMax() int { return 2000 }

func (s *ticksSink) add(data []byte, _ *jetstream.MsgMetadata) error {
	var t bus.Tick
	if err := json.Unmarshal(data, &t); err != nil {
		return fmt.Errorf("tick: %w", err)
	}
	row, err := tickRow(&t)
	if err != nil {
		return err
	}
	s.rows = append(s.rows, row)
	s.bars.Add(&t)
	return nil
}

func tickRow(t *bus.Tick) ([]any, error) {
	switch {
	case t.Recv <= 0:
		return nil, errors.New("tick: no receive time")
	case t.C == "":
		return nil, errors.New("tick: no contract")
	case !finite(t.LTP):
		return nil, fmt.Errorf("tick %s: ltp %v", t.C, t.LTP)
	}
	phase := "?"
	if r, _ := utf8.DecodeRuneInString(t.Phase); r != utf8.RuneError {
		phase = string(r)
	}
	return []any{
		bus.FromMs(t.Recv), msTime(t.Exch), msTime(t.LTT), t.C, int64(t.T), t.LTP,
		u64(uint64(t.LTQ)), u64(t.Vol), u64(t.OI),
		f64(t.Bid), f64(t.Ask), u64(uint64(t.BidQ)), u64(uint64(t.AskQ)), u64(t.TBQ), u64(t.TSQ),
		f64(t.Avg), f64(t.Open), f64(t.High), f64(t.Low), f64(t.Close), phase,
	}, nil
}

func (s *ticksSink) flush(ctx context.Context, tx pgx.Tx) (int, error) {
	n := 0
	if len(s.rows) > 0 {
		c, err := tx.CopyFrom(ctx, pgx.Identifier{"ticks"}, tickCols, pgx.CopyFromRows(s.rows))
		if err != nil {
			return 0, fmt.Errorf("copy ticks: %w", err)
		}
		n = int(c)
	}
	if _, err := s.bars.Flush(ctx, tx); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *ticksSink) reset() {
	clear(s.rows)
	s.rows = s.rows[:0]
	s.bars.Committed()
}

// ---- chain snapshots --------------------------------------------------------

var chainCols = []string{"ts", "contract", "source", "underlying", "expiry", "strike", "opt_type", "token",
	"bid", "ask", "bid_qty", "ask_qty", "ltp", "ltq", "ltt", "exch_ts", "quote_age_ms", "trade_age_ms",
	"oi", "volume", "avg_price", "tbq", "tsq", "spot", "fut"}

var chainSources = map[string]bool{"ws": true, "sweep": true, "backup": true}

// chainSink writes snapshot rows. Rows go through a temp table so that a
// replayed or merged snapshot already present is skipped instead of failing the
// batch (COPY has no ON CONFLICT).
type chainSink struct {
	rows [][]any
}

func (s *chainSink) size() int     { return len(s.rows) }
func (s *chainSink) fetchMax() int { return 20 }

func (s *chainSink) add(data []byte, _ *jetstream.MsgMetadata) error {
	var c bus.Chain
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	return s.addChain(&c)
}

// addChain buffers one snapshot. Malformed rows are skipped; the snapshot is
// rejected only when none of its rows can be stored.
func (s *chainSink) addChain(c *bus.Chain) error {
	switch {
	case c.TS <= 0:
		return errors.New("chain: no timestamp")
	case !chainSources[c.Source]:
		return fmt.Errorf("chain: unknown source %q", c.Source)
	case c.Und == "":
		return errors.New("chain: no underlying")
	}
	exp, err := time.Parse("2006-01-02", c.Expiry)
	if err != nil {
		return fmt.Errorf("chain %s: expiry %q", c.Und, c.Expiry)
	}
	ts := bus.FromMs(c.TS)
	spot, fut := f64(c.Spot), f64(c.Fut)
	kept := 0
	for i := range c.Rows {
		r := &c.Rows[i]
		if r.C == "" || (r.Type != "CE" && r.Type != "PE") || !(r.Strike > 0) || !finite(r.Strike) {
			continue
		}
		s.rows = append(s.rows, []any{
			ts, r.C, c.Source, c.Und, exp, r.Strike, r.Type, tok(r.T),
			f64(r.Bid), f64(r.Ask), u64(uint64(r.BidQ)), u64(uint64(r.AskQ)), f64(r.LTP), u64(uint64(r.LTQ)),
			msTime(r.LTT), msTime(r.Exch), age(r.QuoteAge), age(r.TradeAge),
			n64(r.OI), n64(r.Vol), f64(r.Avg), n64(r.TBQ), n64(r.TSQ), spot, fut,
		})
		kept++
	}
	if kept == 0 && len(c.Rows) > 0 {
		return fmt.Errorf("chain %s %s: no valid rows of %d", c.Und, c.Expiry, len(c.Rows))
	}
	return nil
}

func (s *chainSink) flush(ctx context.Context, tx pgx.Tx) (int, error) {
	if len(s.rows) == 0 {
		return 0, nil
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS chain_stage
		(LIKE chain_snapshots INCLUDING DEFAULTS) ON COMMIT DELETE ROWS`); err != nil {
		return 0, fmt.Errorf("chain stage: %w", err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"chain_stage"}, chainCols, pgx.CopyFromRows(s.rows)); err != nil {
		return 0, fmt.Errorf("copy chain: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO chain_snapshots SELECT * FROM chain_stage ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, fmt.Errorf("insert chain: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func (s *chainSink) reset() {
	clear(s.rows)
	s.rows = s.rows[:0]
}

// ---- order updates ----------------------------------------------------------

// ordersSink records Kite order postbacks. The terminal never places orders;
// these are updates for orders placed elsewhere on the account.
type ordersSink struct {
	rows []orderRow
}

type orderRow struct {
	received time.Time
	o        kiteOrder
	raw      []byte
}

// kiteOrder is the subset of a Kite order update the orders table keeps.
type kiteOrder struct {
	OrderID           string   `json:"order_id"`
	ExchangeOrderID   string   `json:"exchange_order_id"`
	Status            string   `json:"status"`
	StatusMessage     string   `json:"status_message"`
	Exchange          string   `json:"exchange"`
	TradingSymbol     string   `json:"tradingsymbol"`
	InstrumentToken   optNum   `json:"instrument_token"`
	TransactionType   string   `json:"transaction_type"`
	OrderType         string   `json:"order_type"`
	Product           string   `json:"product"`
	Variety           string   `json:"variety"`
	Quantity          optNum   `json:"quantity"`
	FilledQuantity    optNum   `json:"filled_quantity"`
	PendingQuantity   optNum   `json:"pending_quantity"`
	Price             optNum   `json:"price"`
	TriggerPrice      optNum   `json:"trigger_price"`
	AveragePrice      optNum   `json:"average_price"`
	Tag               string   `json:"tag"`
	OrderTimestamp    kiteTime `json:"order_timestamp"`
	ExchangeTimestamp kiteTime `json:"exchange_timestamp"`
}

// optNum accepts a number, a numeric string or null.
type optNum struct {
	V     float64
	Valid bool
}

func (n *optNum) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*n = optNum{}
		return nil
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil || !finite(f) {
		return fmt.Errorf("bad number %q", b)
	}
	*n = optNum{V: f, Valid: true}
	return nil
}

func (n optNum) price() any {
	if !n.Valid {
		return nil
	}
	return n.V
}

func (n optNum) qty() any {
	if !n.Valid || n.V != float64(int64(n.V)) {
		return nil
	}
	return i32(int64(n.V))
}

// kiteTime reads Kite's "2006-01-02 15:04:05" IST stamps (RFC 3339 also
// accepted); null, empty and pre-2000 placeholders read as zero.
type kiteTime struct{ time.Time }

func (t *kiteTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	t.Time = time.Time{}
	if s == "" || s == "null" {
		return nil
	}
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, clock.IST)
	if err != nil {
		if v, err = time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("bad time %q", s)
		}
	}
	if v.Year() >= 2000 {
		t.Time = v
	}
	return nil
}

func (s *ordersSink) size() int     { return len(s.rows) }
func (s *ordersSink) fetchMax() int { return 100 }

func (s *ordersSink) add(data []byte, md *jetstream.MsgMetadata) error {
	var o kiteOrder
	if err := json.Unmarshal(data, &o); err != nil {
		return fmt.Errorf("order: %w", err)
	}
	if o.OrderID == "" {
		return errors.New("order: no order_id")
	}
	if tk := o.InstrumentToken; tk.Valid && (tk.V < 0 || tk.V > 1<<32-1 || tk.V != float64(uint32(tk.V))) {
		o.InstrumentToken = optNum{}
	}
	received := time.Now()
	if md != nil && !md.Timestamp.IsZero() {
		received = md.Timestamp
	}
	s.rows = append(s.rows, orderRow{received: received, o: o, raw: bytes.Clone(data)})
	return nil
}

func (s *ordersSink) flush(ctx context.Context, tx pgx.Tx) (int, error) {
	for _, r := range s.rows {
		o := &r.o
		var contract any
		if o.InstrumentToken.Valid && o.InstrumentToken.V > 0 {
			day := clock.Date(r.received)
			if !o.OrderTimestamp.IsZero() {
				day = clock.Date(o.OrderTimestamp.Time)
			}
			c, err := refdata.AsOf(ctx, tx, uint32(o.InstrumentToken.V), day)
			switch {
			case err == nil:
				contract = c
			case !errors.Is(err, pgx.ErrNoRows):
				return 0, fmt.Errorf("order %s contract: %w", o.OrderID, err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO orders (received_at, order_id, exchange_order_id, status,
			status_message, exchange, tradingsymbol, contract, transaction_type, order_type, product, variety,
			quantity, filled_quantity, pending_quantity, price, trigger_price, average_price, tag,
			order_timestamp, exchange_timestamp, raw)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
			r.received, o.OrderID, optStr(o.ExchangeOrderID), optStr(o.Status),
			optStr(o.StatusMessage), optStr(o.Exchange), optStr(o.TradingSymbol), contract,
			optStr(o.TransactionType), optStr(o.OrderType), optStr(o.Product), optStr(o.Variety),
			o.Quantity.qty(), o.FilledQuantity.qty(), o.PendingQuantity.qty(),
			o.Price.price(), o.TriggerPrice.price(), o.AveragePrice.price(), optStr(o.Tag),
			optTime(o.OrderTimestamp.Time), optTime(o.ExchangeTimestamp.Time), json.RawMessage(r.raw))
		if err != nil {
			return 0, fmt.Errorf("insert order %s: %w", o.OrderID, err)
		}
	}
	return len(s.rows), nil
}

func (s *ordersSink) reset() {
	clear(s.rows)
	s.rows = s.rows[:0]
}
