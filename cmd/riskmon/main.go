// Command riskmon follows risk.book and book.margins, keeps the day's P&L line with
// its peak and drawdown, snapshots it each minute, and alerts against risk.limits.
// It reads the book only: it never places, changes or cancels orders.
package main

import (
	"time"

	"pmt/internal/app"
	"pmt/internal/notify"
	"pmt/internal/riskmon"
)

func main() {
	ctx, cancel, cfg := app.Start("riskmon")
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
	conn, err := app.Bus(ctx, cfg, "pmt-riskmon")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	n := notify.FromEnv("riskmon")
	n.DB = pool
	s := &riskmon.Service{Cfg: cfg, Bus: conn, DB: pool, Notify: n, Cal: cal}
	go app.Heartbeat(ctx, conn, "riskmon", 5*time.Second, s.Health)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("riskmon", err)
	}
}
