package main

import (
	"math"
	"net/http"
	"time"

	"pmt/internal/clock"
)

// ---- the sample book ----
//
// A small options book for the POS panel and the zero-shock check: a short NIFTY
// strangle on the next weekly carried from yesterday, a long BANKNIFTY monthly put,
// a NIFTY future bought this morning (MIS) and a NIFTY call scalped and closed
// today. P&L follows Kite's arithmetic so the panel's recomputation from ticks
// should match it to the paisa: pnl = sell_value − buy_value + qty·ltp·multiplier,
// m2m = day_sell_value − day_buy_value + qty·ltp − overnight_qty·close_price.

type leg struct {
	in        *inst
	product   string
	overnight int     // units carried from yesterday, signed
	ovPrice   float64 // the price the overnight units were traded at
	dayBuy    int     // units bought today
	daySell   int     // units sold today
	dayBuyAt  float64 // fixed for the day once the book is built
	daySellAt float64
}

// sampleBook picks its legs from the market's listed contracts; a leg whose
// contract is not listed is left out.
func sampleBook(m *market, now time.Time) []leg {
	today := clock.Today()
	pick := func(name, itype string, monthly bool, strike float64) *inst {
		var best *inst
		for _, in := range m.insts {
			if in.name != name || in.itype != itype || !in.expiry.After(today) {
				continue
			}
			if itype != "FUT" && in.strike != strike {
				continue
			}
			if monthly && itype != "FUT" && !isMonthly(m, in) {
				continue
			}
			if best == nil || in.expiry.Before(best.expiry) {
				best = in
			}
		}
		return best
	}
	var book []leg
	add := func(in *inst, l leg) {
		if in == nil {
			return
		}
		l.in = in
		sn := m.snap(in, now)
		// Overnight legs were put on above or below yesterday's close; the day's
		// trades at today's open, so the book shows both winners and losers.
		if l.overnight != 0 {
			l.ovPrice = round(sn.close*l.ovPrice, in.tick)
		}
		if l.dayBuy != 0 {
			l.dayBuyAt = round(sn.open, in.tick)
		}
		if l.daySell != 0 {
			l.daySellAt = round(sn.open*l.daySellAt, in.tick)
		}
		book = append(book, l)
	}
	const nLot, bLot = 75, 35
	add(pick("NIFTY", "CE", false, 25400), leg{product: "NRML", overnight: -2 * nLot, ovPrice: 1.15})
	add(pick("NIFTY", "PE", false, 24600), leg{product: "NRML", overnight: -2 * nLot, ovPrice: 1.12})
	add(pick("BANKNIFTY", "PE", true, 55000), leg{product: "NRML", overnight: bLot, ovPrice: 1.10})
	add(pick("NIFTY", "FUT", true, 0), leg{product: "MIS", dayBuy: nLot})
	add(pick("NIFTY", "CE", false, 25000), leg{product: "MIS", dayBuy: nLot, daySell: nLot, daySellAt: 1.04})
	return book
}

// isMonthly reports whether in expires on a date some future of its name does.
func isMonthly(m *market, in *inst) bool {
	for _, f := range m.insts {
		if f.itype == "FUT" && f.name == in.name && f.expiry.Equal(in.expiry) {
			return true
		}
	}
	return false
}

type position struct {
	TradingSymbol     string  `json:"tradingsymbol"`
	Exchange          string  `json:"exchange"`
	Token             uint32  `json:"instrument_token"`
	Product           string  `json:"product"`
	Quantity          int     `json:"quantity"`
	OvernightQuantity int     `json:"overnight_quantity"`
	Multiplier        float64 `json:"multiplier"`
	AveragePrice      float64 `json:"average_price"`
	ClosePrice        float64 `json:"close_price"`
	LastPrice         float64 `json:"last_price"`
	Value             float64 `json:"value"`
	PnL               float64 `json:"pnl"`
	M2M               float64 `json:"m2m"`
	Unrealised        float64 `json:"unrealised"`
	Realised          float64 `json:"realised"`
	BuyQuantity       int     `json:"buy_quantity"`
	BuyPrice          float64 `json:"buy_price"`
	BuyValue          float64 `json:"buy_value"`
	SellQuantity      int     `json:"sell_quantity"`
	SellPrice         float64 `json:"sell_price"`
	SellValue         float64 `json:"sell_value"`
	DayBuyQuantity    int     `json:"day_buy_quantity"`
	DayBuyPrice       float64 `json:"day_buy_price"`
	DayBuyValue       float64 `json:"day_buy_value"`
	DaySellQuantity   int     `json:"day_sell_quantity"`
	DaySellPrice      float64 `json:"day_sell_price"`
	DaySellValue      float64 `json:"day_sell_value"`
}

func paise(v float64) float64 { return math.Round(v*100) / 100 }

// rows is the leg as Kite's net row (overnight units folded into the buy or sell
// side at their price) and as its day row (today's trades alone), or nil when it
// has no trades today.
func (l leg) rows(ltp, close float64) (net position, day *position) {
	base := position{TradingSymbol: l.in.symbol, Exchange: l.in.exch, Token: l.in.token, Product: l.product,
		Multiplier: 1, ClosePrice: close, LastPrice: ltp,
		DayBuyQuantity: l.dayBuy, DayBuyPrice: l.dayBuyAt, DayBuyValue: paise(float64(l.dayBuy) * l.dayBuyAt),
		DaySellQuantity: l.daySell, DaySellPrice: l.daySellAt, DaySellValue: paise(float64(l.daySell) * l.daySellAt)}
	fill := func(p *position, bq int, bv float64, sq int, sv float64, ov int) {
		p.BuyQuantity, p.BuyValue, p.SellQuantity, p.SellValue = bq, paise(bv), sq, paise(sv)
		if bq > 0 {
			p.BuyPrice = paise(bv / float64(bq))
		}
		if sq > 0 {
			p.SellPrice = paise(sv / float64(sq))
		}
		p.Quantity, p.OvernightQuantity = bq-sq, ov
		switch {
		case p.Quantity > 0:
			p.AveragePrice = p.BuyPrice
		case p.Quantity < 0:
			p.AveragePrice = p.SellPrice
		}
		q := float64(p.Quantity)
		p.Value = paise(sv - bv)
		p.PnL = paise(sv - bv + q*ltp*p.Multiplier)
		p.M2M = paise(base.DaySellValue - base.DayBuyValue + q*ltp - float64(ov)*close)
		if p.Quantity == 0 {
			p.Realised = p.PnL
		} else {
			p.Unrealised = p.PnL
		}
	}
	bq, bv := l.dayBuy, float64(l.dayBuy)*l.dayBuyAt
	sq, sv := l.daySell, float64(l.daySell)*l.daySellAt
	if l.overnight > 0 {
		bq += l.overnight
		bv += float64(l.overnight) * l.ovPrice
	} else if l.overnight < 0 {
		sq -= l.overnight
		sv -= float64(l.overnight) * l.ovPrice
	}
	net = base
	fill(&net, bq, bv, sq, sv, l.overnight)
	if l.dayBuy == 0 && l.daySell == 0 {
		return net, nil
	}
	d := base
	fill(&d, l.dayBuy, float64(l.dayBuy)*l.dayBuyAt, l.daySell, float64(l.daySell)*l.daySellAt, 0)
	// The day row's m2m is measured from today's trades only.
	d.M2M = d.PnL
	return net, &d
}

func (s *server) positions(w http.ResponseWriter, r *http.Request) {
	now := clock.Now()
	net, day := []position{}, []position{}
	for _, l := range s.book {
		sn := s.m.snap(l.in, now)
		n, d := l.rows(sn.ltp, sn.close)
		net = append(net, n)
		if d != nil {
			day = append(day, *d)
		}
	}
	ok(w, map[string]any{"net": net, "day": day})
}

// holdings is always empty: the fake book is F&O only.
func (s *server) holdings(w http.ResponseWriter, r *http.Request) {
	ok(w, []any{})
}

// Margin figures are a crude stand-in for SPAN: 9% of the notional of every short
// option and future, 2% exposure on top, and premium paid for long options. They
// only need to move with the book (verify the field meanings against a real
// account before trusting the panel's margin line).
const (
	fakeCash       = 800000.0
	fakeCollateral = 400000.0
)

type segMargin struct {
	Enabled   bool               `json:"enabled"`
	Net       float64            `json:"net"`
	Available map[string]float64 `json:"available"`
	Utilised  map[string]float64 `json:"utilised"`
}

func (s *server) segments() map[string]segMargin {
	now := clock.Now()
	var span, exposure, premium, m2m, realised float64
	for _, l := range s.book {
		sn := s.m.snap(l.in, now)
		n, _ := l.rows(sn.ltp, sn.close)
		q := math.Abs(float64(n.Quantity))
		s.m.mu.RLock()
		spot := s.m.spot[l.in.under]
		s.m.mu.RUnlock()
		short := n.Quantity < 0 || l.in.itype == "FUT"
		if short && n.Quantity != 0 {
			span += 0.09 * spot * q
			exposure += 0.02 * spot * q
		}
		if (l.in.itype == "CE" || l.in.itype == "PE") && l.dayBuy > l.daySell {
			premium += float64(l.dayBuy-l.daySell) * l.dayBuyAt
		}
		if n.Quantity == 0 {
			realised += n.M2M
		} else {
			m2m += n.M2M
		}
	}
	debits := paise(span + exposure + premium - math.Min(m2m+realised, 0))
	eq := segMargin{
		Enabled: true,
		Net:     paise(fakeCash + fakeCollateral - debits),
		Available: map[string]float64{
			"adhoc_margin": 0, "cash": fakeCash, "opening_balance": fakeCash,
			"live_balance": paise(fakeCash - premium + m2m + realised), "collateral": fakeCollateral, "intraday_payin": 0,
		},
		Utilised: map[string]float64{
			"debits": debits, "exposure": paise(exposure), "m2m_realised": paise(-realised), "m2m_unrealised": paise(-m2m),
			"option_premium": paise(premium), "payout": 0, "span": paise(span), "holding_sales": 0, "turnover": 0,
			"liquid_collateral": 0, "stock_collateral": fakeCollateral, "delivery": 0,
		},
	}
	empty := segMargin{Available: map[string]float64{}, Utilised: map[string]float64{}}
	return map[string]segMargin{"equity": eq, "commodity": empty}
}

func (s *server) margins(w http.ResponseWriter, r *http.Request) {
	segs := s.segments()
	if seg := r.PathValue("seg"); seg != "" {
		m, found := segs[seg]
		if !found {
			fail(w, http.StatusBadRequest, "InputException", "Invalid segment.")
			return
		}
		ok(w, m)
		return
	}
	ok(w, segs)
}
