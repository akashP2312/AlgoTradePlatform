package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/calendar"
)

// ivcloseWindow is how close to the session close the ivengine's last payload for
// an expiry must be to stand as that day's live close row.
const ivcloseWindow = 10 * time.Minute

const ivcloseModel = "ivengine-live-1"

// ivPayload is the part of an analytics.<und>.iv.<yyyymmdd> payload (pmt_iv.engine)
// the close row keeps.
type ivPayload struct {
	Ts   int64    `json:"ts"`
	U    string   `json:"u"`
	X    string   `json:"x"`
	Tte  *float64 `json:"tte"`
	Dte  *float64 `json:"dte"`
	F    *float64 `json:"f"`
	Fsrc string   `json:"fsrc"`
	S    *float64 `json:"s"`
	Atm  *float64 `json:"atm"`
	Rr25 *float64 `json:"rr25"`
	Bf25 *float64 `json:"bf25"`
	Fit  *struct {
		A, B, Rho, M, S, Rmse *float64
		Ok                    bool
	} `json:"fit"`
	N int `json:"n"`
}

// ivRow is one iv_eod row.
type ivRow struct {
	Day, Expiry time.Time
	Und         string
	Ts          time.Time
	P           ivPayload
}

// ivCloseRows picks the payloads that stand as day d's close: for an expiry still
// open after d, measured within ivcloseWindow of the close (or after it). The
// reasons for the rest come back for the job's summary.
func ivCloseRows(s calendar.Session, raw map[string][]byte) ([]ivRow, []string) {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rows []ivRow
	var skip []string
	for _, k := range keys {
		var p ivPayload
		if err := json.Unmarshal(raw[k], &p); err != nil {
			skip = append(skip, k+": "+err.Error())
			continue
		}
		x, err := time.ParseInLocation(time.DateOnly, p.X, s.Date.Location())
		if err != nil || p.U == "" || p.F == nil || p.Tte == nil || p.Dte == nil {
			skip = append(skip, k+": incomplete payload")
			continue
		}
		ts := bus.FromMs(p.Ts).In(s.Date.Location())
		switch {
		case !sameDay(ts, s.Date):
			skip = append(skip, fmt.Sprintf("%s: last computed %s", k, ts.Format("2006-01-02 15:04")))
			continue
		case ts.Before(s.Close.Add(-ivcloseWindow)):
			skip = append(skip, fmt.Sprintf("%s: last computed %s, before the close window", k, ts.Format("15:04:05")))
			continue
		case !x.After(s.Date):
			continue // expired today
		}
		rows = append(rows, ivRow{Day: s.Date, Expiry: x, Und: p.U, Ts: ts, P: p})
	}
	return rows, skip
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// ivclose copies the ivengine's close-of-day smiles from KV analytics into iv_eod
// with source 'live'. Run it after the close, before the next session's ticks.
func ivclose(e *env, args []string) (string, error) {
	d, cal, err := e.tradingDay(args, 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	c, err := e.bus()
	if err != nil {
		return "", err
	}
	kv, err := c.KV(e.ctx, bus.KVAnalytics)
	if err != nil {
		return "", err
	}
	raw := map[string][]byte{}
	for _, u := range e.cfg.Universe.Underlyings {
		lister, err := kv.ListKeysFiltered(e.ctx, strings.ToLower(u.Name)+".iv.*")
		if err != nil {
			return "", err
		}
		for k := range lister.Keys() {
			en, err := kv.Get(e.ctx, k)
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return "", err
			}
			raw[k] = en.Value()
		}
	}
	rows, skip := ivCloseRows(cal.Session(d), raw)
	if len(rows) > 0 {
		if err := saveIV(e, rows, "live", ivcloseModel); err != nil {
			return "", err
		}
	}
	msg := fmt.Sprintf("ivclose %s: %d rows from %d payloads", d.Format(time.DateOnly), len(rows), len(raw))
	if len(skip) > 0 {
		if len(skip) > 5 {
			skip = append(skip[:5], "...")
		}
		msg += "; skipped " + strings.Join(skip, "; ")
	}
	if len(rows) == 0 {
		return msg, errors.New(msg + ": no close rows (was the ivengine running at the close?)")
	}
	return msg, nil
}

func saveIV(e *env, rows []ivRow, source, model string) error {
	b := &pgx.Batch{}
	for _, r := range rows {
		p := r.P
		var a, bb, rho, m, s, rmse *float64
		ok := false
		if p.Fit != nil {
			a, bb, rho, m, s, rmse, ok = p.Fit.A, p.Fit.B, p.Fit.Rho, p.Fit.M, p.Fit.S, p.Fit.Rmse, p.Fit.Ok
		}
		b.Queue(`INSERT INTO iv_eod (trade_date, underlying, expiry, source, ts, tte, dte, fwd, fsrc, spot,
		  atm_iv, rr25, bf25, fit_a, fit_b, fit_rho, fit_m, fit_s, rmse, ok, n_strikes, model_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (trade_date, underlying, expiry, source) DO UPDATE SET
		  ts = excluded.ts, tte = excluded.tte, dte = excluded.dte, fwd = excluded.fwd, fsrc = excluded.fsrc,
		  spot = excluded.spot, atm_iv = excluded.atm_iv, rr25 = excluded.rr25, bf25 = excluded.bf25,
		  fit_a = excluded.fit_a, fit_b = excluded.fit_b, fit_rho = excluded.fit_rho, fit_m = excluded.fit_m,
		  fit_s = excluded.fit_s, rmse = excluded.rmse, ok = excluded.ok, n_strikes = excluded.n_strikes,
		  model_version = excluded.model_version`,
			utcDate(r.Day), r.Und, utcDate(r.Expiry), source, r.Ts, *p.Tte, *p.Dte, *p.F, p.Fsrc, p.S,
			p.Atm, p.Rr25, p.Bf25, a, bb, rho, m, s, rmse, ok, p.N, model)
	}
	return e.pool.SendBatch(e.ctx, b).Close()
}

func utcDate(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
