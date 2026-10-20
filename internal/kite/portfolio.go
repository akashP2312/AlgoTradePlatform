package kite

import (
	"context"
	"encoding/json"
	"fmt"
)

// Position is one row of /portfolio/positions, as Kite reports it. Quantities are
// in units (lots × lot size); prices in rupees per unit.
type Position struct {
	TradingSymbol     string  `json:"tradingsymbol"`
	Exchange          string  `json:"exchange"`
	Token             uint32  `json:"instrument_token"`
	Product           string  `json:"product"` // NRML, MIS, CNC
	Quantity          int64   `json:"quantity"`
	OvernightQuantity int64   `json:"overnight_quantity"`
	Multiplier        float64 `json:"multiplier"`
	AveragePrice      float64 `json:"average_price"`
	ClosePrice        float64 `json:"close_price"` // previous close, the m2m base for overnight legs
	LastPrice         float64 `json:"last_price"`
	Value             float64 `json:"value"`
	PnL               float64 `json:"pnl"`
	M2M               float64 `json:"m2m"`
	Unrealised        float64 `json:"unrealised"`
	Realised          float64 `json:"realised"`
	BuyQuantity       int64   `json:"buy_quantity"`
	BuyPrice          float64 `json:"buy_price"`
	BuyValue          float64 `json:"buy_value"`
	SellQuantity      int64   `json:"sell_quantity"`
	SellPrice         float64 `json:"sell_price"`
	SellValue         float64 `json:"sell_value"`
	DayBuyQuantity    int64   `json:"day_buy_quantity"`
	DayBuyPrice       float64 `json:"day_buy_price"`
	DayBuyValue       float64 `json:"day_buy_value"`
	DaySellQuantity   int64   `json:"day_sell_quantity"`
	DaySellPrice      float64 `json:"day_sell_price"`
	DaySellValue      float64 `json:"day_sell_value"`
}

// Positions is the net book and the day's trades.
type Positions struct {
	Net []Position `json:"net"`
	Day []Position `json:"day"`
}

// rawPosition decodes with num, since Kite has sent numbers as strings and nulls.
type rawPosition struct {
	TradingSymbol     string `json:"tradingsymbol"`
	Exchange          string `json:"exchange"`
	Token             num    `json:"instrument_token"`
	Product           string `json:"product"`
	Quantity          num    `json:"quantity"`
	OvernightQuantity num    `json:"overnight_quantity"`
	Multiplier        num    `json:"multiplier"`
	AveragePrice      num    `json:"average_price"`
	ClosePrice        num    `json:"close_price"`
	LastPrice         num    `json:"last_price"`
	Value             num    `json:"value"`
	PnL               num    `json:"pnl"`
	M2M               num    `json:"m2m"`
	Unrealised        num    `json:"unrealised"`
	Realised          num    `json:"realised"`
	BuyQuantity       num    `json:"buy_quantity"`
	BuyPrice          num    `json:"buy_price"`
	BuyValue          num    `json:"buy_value"`
	SellQuantity      num    `json:"sell_quantity"`
	SellPrice         num    `json:"sell_price"`
	SellValue         num    `json:"sell_value"`
	DayBuyQuantity    num    `json:"day_buy_quantity"`
	DayBuyPrice       num    `json:"day_buy_price"`
	DayBuyValue       num    `json:"day_buy_value"`
	DaySellQuantity   num    `json:"day_sell_quantity"`
	DaySellPrice      num    `json:"day_sell_price"`
	DaySellValue      num    `json:"day_sell_value"`
}

func (p rawPosition) clean() Position {
	m := float64(p.Multiplier)
	if m == 0 {
		m = 1
	}
	return Position{
		TradingSymbol: p.TradingSymbol, Exchange: p.Exchange, Token: uint32(p.Token), Product: p.Product,
		Quantity: int64(p.Quantity), OvernightQuantity: int64(p.OvernightQuantity), Multiplier: m,
		AveragePrice: float64(p.AveragePrice), ClosePrice: float64(p.ClosePrice), LastPrice: float64(p.LastPrice),
		Value: float64(p.Value), PnL: float64(p.PnL), M2M: float64(p.M2M),
		Unrealised: float64(p.Unrealised), Realised: float64(p.Realised),
		BuyQuantity: int64(p.BuyQuantity), BuyPrice: float64(p.BuyPrice), BuyValue: float64(p.BuyValue),
		SellQuantity: int64(p.SellQuantity), SellPrice: float64(p.SellPrice), SellValue: float64(p.SellValue),
		DayBuyQuantity: int64(p.DayBuyQuantity), DayBuyPrice: float64(p.DayBuyPrice), DayBuyValue: float64(p.DayBuyValue),
		DaySellQuantity: int64(p.DaySellQuantity), DaySellPrice: float64(p.DaySellPrice), DaySellValue: float64(p.DaySellValue),
	}
}

// Positions reads /portfolio/positions. Read-only.
func (r *REST) Positions(ctx context.Context) (Positions, error) {
	body, err := r.get(ctx, "/portfolio/positions")
	if err != nil {
		return Positions{}, err
	}
	var env struct {
		Data struct {
			Net []rawPosition `json:"net"`
			Day []rawPosition `json:"day"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Positions{}, fmt.Errorf("kite: positions: %w", err)
	}
	out := Positions{Net: make([]Position, 0, len(env.Data.Net)), Day: make([]Position, 0, len(env.Data.Day))}
	for _, p := range env.Data.Net {
		out.Net = append(out.Net, p.clean())
	}
	for _, p := range env.Data.Day {
		out.Day = append(out.Day, p.clean())
	}
	return out, nil
}

// SegmentMargin is one segment of /user/margins.
type SegmentMargin struct {
	Enabled   bool      `json:"enabled"`
	Net       float64   `json:"net"` // what is left to trade with
	Available Available `json:"available"`
	Utilised  Utilised  `json:"utilised"`
}

type Available struct {
	AdhocMargin    float64 `json:"adhoc_margin"`
	Cash           float64 `json:"cash"`
	OpeningBalance float64 `json:"opening_balance"`
	LiveBalance    float64 `json:"live_balance"`
	Collateral     float64 `json:"collateral"`
	IntradayPayin  float64 `json:"intraday_payin"`
}

type Utilised struct {
	Debits           float64 `json:"debits"`
	Exposure         float64 `json:"exposure"`
	M2MRealised      float64 `json:"m2m_realised"`
	M2MUnrealised    float64 `json:"m2m_unrealised"`
	OptionPremium    float64 `json:"option_premium"`
	Payout           float64 `json:"payout"`
	Span             float64 `json:"span"`
	HoldingSales     float64 `json:"holding_sales"`
	Turnover         float64 `json:"turnover"`
	LiquidCollateral float64 `json:"liquid_collateral"`
	StockCollateral  float64 `json:"stock_collateral"`
	Delivery         float64 `json:"delivery"`
}

// Margins is /user/margins: the equity segment (which carries F&O) and commodity.
type Margins struct {
	Equity    SegmentMargin `json:"equity"`
	Commodity SegmentMargin `json:"commodity"`
}

type rawSegment struct {
	Enabled   bool `json:"enabled"`
	Net       num  `json:"net"`
	Available struct {
		AdhocMargin    num `json:"adhoc_margin"`
		Cash           num `json:"cash"`
		OpeningBalance num `json:"opening_balance"`
		LiveBalance    num `json:"live_balance"`
		Collateral     num `json:"collateral"`
		IntradayPayin  num `json:"intraday_payin"`
	} `json:"available"`
	Utilised struct {
		Debits           num `json:"debits"`
		Exposure         num `json:"exposure"`
		M2MRealised      num `json:"m2m_realised"`
		M2MUnrealised    num `json:"m2m_unrealised"`
		OptionPremium    num `json:"option_premium"`
		Payout           num `json:"payout"`
		Span             num `json:"span"`
		HoldingSales     num `json:"holding_sales"`
		Turnover         num `json:"turnover"`
		LiquidCollateral num `json:"liquid_collateral"`
		StockCollateral  num `json:"stock_collateral"`
		Delivery         num `json:"delivery"`
	} `json:"utilised"`
}

func (s rawSegment) clean() SegmentMargin {
	a, u := s.Available, s.Utilised
	return SegmentMargin{
		Enabled: s.Enabled, Net: float64(s.Net),
		Available: Available{
			AdhocMargin: float64(a.AdhocMargin), Cash: float64(a.Cash), OpeningBalance: float64(a.OpeningBalance),
			LiveBalance: float64(a.LiveBalance), Collateral: float64(a.Collateral), IntradayPayin: float64(a.IntradayPayin),
		},
		Utilised: Utilised{
			Debits: float64(u.Debits), Exposure: float64(u.Exposure), M2MRealised: float64(u.M2MRealised),
			M2MUnrealised: float64(u.M2MUnrealised), OptionPremium: float64(u.OptionPremium), Payout: float64(u.Payout),
			Span: float64(u.Span), HoldingSales: float64(u.HoldingSales), Turnover: float64(u.Turnover),
			LiquidCollateral: float64(u.LiquidCollateral), StockCollateral: float64(u.StockCollateral), Delivery: float64(u.Delivery),
		},
	}
}

// Margins reads /user/margins. Read-only.
func (r *REST) Margins(ctx context.Context) (Margins, error) {
	body, err := r.get(ctx, "/user/margins")
	if err != nil {
		return Margins{}, err
	}
	var env struct {
		Data struct {
			Equity    rawSegment `json:"equity"`
			Commodity rawSegment `json:"commodity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Margins{}, fmt.Errorf("kite: margins: %w", err)
	}
	return Margins{Equity: env.Data.Equity.clean(), Commodity: env.Data.Commodity.clean()}, nil
}
