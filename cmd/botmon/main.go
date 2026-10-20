// Command botmon watches an external algo bot through its SQLite ledger: it reads
// the ledger read-only, alerts when the daily run is missed, and in live mode
// reconciles the ledger's holdings with the broker's. It never writes the ledger
// and never places, changes or cancels orders.
package main

import (
	"time"

	"pmt/internal/app"
	"pmt/internal/botmon"
	"pmt/internal/notify"
)

func main() {
	ctx, cancel, cfg := app.Start("botmon")
	defer cancel()
	pool, err := app.DB(ctx, cfg)
	if err != nil {
		app.Fatal("db", err)
	}
	defer pool.Close()
	cal, err := app.Calendar(cfg)
	if err != nil {
		app.Fatal("calendar", err)
	}
	conn, err := app.Bus(ctx, cfg, "pmt-botmon")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	n := notify.FromEnv("botmon")
	n.DB = pool
	s := &botmon.Service{Cfg: cfg, Bus: conn, Notify: n, Cal: cal}
	go app.Heartbeat(ctx, conn, "botmon", 5*time.Second, s.Health)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("botmon", err)
	}
}
