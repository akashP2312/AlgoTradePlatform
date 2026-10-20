package botmon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	_ "modernc.org/sqlite"
)

// Ledger is what one read of the bot's SQLite ledger returns. The bot owns the file;
// botmon opens it read-only, inside one read transaction, and closes it again.
type Ledger struct {
	Meta      map[string]string
	Positions []LPos
	Pending   []LOrder
	Trades    []LTrade // newest first, at most recentTrades
	NTrades   int
	Realised  float64 // sum of pnl over every closed trade
	Curve     []EqPt  // oldest first, at most curvePoints
}

type LPos struct {
	Sym       string  `json:"sym"`
	Engine    string  `json:"engine"`
	Qty       int64   `json:"qty"`
	EntryDate string  `json:"entry_date"`
	Entry     float64 `json:"entry"`
	Stop      float64 `json:"stop"`
	High      float64 `json:"high"` // highest close since entry, the trailing stop's base
	InitRisk  float64 `json:"init_risk"`
}

type LOrder struct {
	ID      int64    `json:"id"`
	Created string   `json:"created"`
	Sym     string   `json:"sym"`
	Side    string   `json:"side"`
	Qty     int64    `json:"qty"`
	Stop    *float64 `json:"stop,omitempty"`
	Engine  string   `json:"engine"`
	Reason  string   `json:"reason"`
	Status  string   `json:"status"` // proposed | approved
}

type LTrade struct {
	Sym       string   `json:"sym"`
	Engine    string   `json:"engine"`
	Qty       int64    `json:"qty"`
	EntryDate string   `json:"entry_date"`
	Entry     float64  `json:"entry"`
	ExitDate  string   `json:"exit_date"`
	Exit      float64  `json:"exit"`
	PnL       float64  `json:"pnl"`
	R         *float64 `json:"r,omitempty"`
	Reason    string   `json:"reason"`
	Days      int64    `json:"days"`
}

type EqPt struct {
	Date   string  `json:"d"`
	Equity float64 `json:"eq"`
	Cash   float64 `json:"cash"`
	N      int64   `json:"n"`
}

const (
	recentTrades = 5
	curvePoints  = 30
)

// ErrBusy means the bot is writing (its rollback journal exists) or wrote while the
// read ran; the read is thrown away and tried on the next poll.
var ErrBusy = errors.New("ledger is being written")

// ReadLedger reads the ledger at path. It refuses to read while a -journal file
// sits next to it, and discards the read when the file changed under it.
func ReadLedger(ctx context.Context, path string) (*Ledger, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + "-journal"); err == nil {
		return nil, ErrBusy
	}
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	l, err := read(ctx, db)
	if err != nil {
		return nil, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		return nil, ErrBusy
	}
	return l, nil
}

func read(ctx context.Context, db *sql.DB) (*Ledger, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	l := &Ledger{Meta: map[string]string{}}

	q := func(what, query string, scan func(*sql.Rows) error) error {
		rows, err := tx.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("ledger %s: %w", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return fmt.Errorf("ledger %s: %w", what, err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("ledger %s: %w", what, err)
		}
		return nil
	}
	steps := []struct {
		what, query string
		scan        func(*sql.Rows) error
	}{
		{"meta", `SELECT key, COALESCE(value, '') FROM meta`, func(r *sql.Rows) error {
			var k, v string
			err := r.Scan(&k, &v)
			l.Meta[k] = v
			return err
		}},
		{"positions", `SELECT symbol, COALESCE(engine,''), qty, COALESCE(entry_date,''), COALESCE(entry_price,0),
			COALESCE(stop,0), COALESCE(highest_close,0), COALESCE(init_risk,0) FROM positions ORDER BY symbol`, func(r *sql.Rows) error {
			var p LPos
			err := r.Scan(&p.Sym, &p.Engine, &p.Qty, &p.EntryDate, &p.Entry, &p.Stop, &p.High, &p.InitRisk)
			l.Positions = append(l.Positions, p)
			return err
		}},
		{"pending_orders", `SELECT id, COALESCE(created,''), symbol, side, qty, stop, COALESCE(engine,''), COALESCE(reason,''), status
			FROM pending_orders WHERE status IN ('proposed', 'approved') ORDER BY id`, func(r *sql.Rows) error {
			var o LOrder
			var stop sql.NullFloat64
			err := r.Scan(&o.ID, &o.Created, &o.Sym, &o.Side, &o.Qty, &stop, &o.Engine, &o.Reason, &o.Status)
			if stop.Valid {
				o.Stop = &stop.Float64
			}
			l.Pending = append(l.Pending, o)
			return err
		}},
		{"trades", `SELECT symbol, COALESCE(engine,''), qty, COALESCE(entry_date,''), COALESCE(entry_price,0), COALESCE(exit_date,''),
			COALESCE(exit_price,0), COALESCE(pnl,0), r_multiple, COALESCE(reason,''), COALESCE(days_held,0)
			FROM trades ORDER BY id DESC LIMIT ` + strconv.Itoa(recentTrades), func(r *sql.Rows) error {
			var t LTrade
			var rm sql.NullFloat64
			err := r.Scan(&t.Sym, &t.Engine, &t.Qty, &t.EntryDate, &t.Entry, &t.ExitDate, &t.Exit, &t.PnL, &rm, &t.Reason, &t.Days)
			if rm.Valid {
				t.R = &rm.Float64
			}
			l.Trades = append(l.Trades, t)
			return err
		}},
		{"trade totals", `SELECT COUNT(*), COALESCE(SUM(pnl), 0) FROM trades`, func(r *sql.Rows) error {
			var n int
			err := r.Scan(&n, &l.Realised)
			l.NTrades = n
			return err
		}},
		{"equity_history", `SELECT date, COALESCE(equity,0), COALESCE(cash,0), COALESCE(n_positions,0)
			FROM equity_history ORDER BY date DESC LIMIT ` + strconv.Itoa(curvePoints), func(r *sql.Rows) error {
			var e EqPt
			err := r.Scan(&e.Date, &e.Equity, &e.Cash, &e.N)
			l.Curve = append(l.Curve, e)
			return err
		}},
	}
	for _, s := range steps {
		if err := q(s.what, s.query, s.scan); err != nil {
			return nil, err
		}
	}
	for i, j := 0, len(l.Curve)-1; i < j; i, j = i+1, j-1 {
		l.Curve[i], l.Curve[j] = l.Curve[j], l.Curve[i]
	}
	return l, nil
}

// Marks decodes meta last_marks: {symbol: pnl % against the entry price} as of the
// last run.
func (l *Ledger) Marks() map[string]float64 {
	out := map[string]float64{}
	if s := l.Meta["last_marks"]; s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

// MetaFloat reads a numeric meta value; ok is false when it is missing or bad.
func (l *Ledger) MetaFloat(k string) (float64, bool) {
	v, err := strconv.ParseFloat(l.Meta[k], 64)
	return v, err == nil && l.Meta[k] != ""
}
