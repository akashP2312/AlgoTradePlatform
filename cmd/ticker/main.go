// Command ticker holds the Kite WebSocket connections and publishes every tick,
// a minute chain per underlying and expiry, and order postbacks onto NATS. It
// waits for a login rather than failing, and never places or changes orders.
package main

import (
	"pmt/internal/app"
	"pmt/internal/notify"
	"pmt/internal/ticker"
)

func main() {
	ctx, cancel, cfg := app.Start("ticker")
	defer cancel()
	cal, err := app.Calendar(cfg)
	if err != nil {
		app.Fatal("calendar", err)
	}
	pool, err := app.DB(ctx, cfg)
	if err != nil {
		app.Fatal("db", err)
	}
	defer pool.Close()
	conn, err := app.Bus(ctx, cfg, "pmt-ticker")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	s := &ticker.Service{Cfg: cfg, Bus: conn, DB: pool, Cal: cal, Notify: notify.FromEnv("ticker")}
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("ticker", err)
	}
}
