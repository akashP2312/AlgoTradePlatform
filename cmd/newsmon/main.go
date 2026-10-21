// Command newsmon turns the scheduled-event calendar into the news risk state
// (NORMAL, CAUTION, NO_NEW_ENTRIES) on news.risk and the calendar on news.events,
// and logs each state change. It is a filter only: it never places, changes or
// cancels orders.
package main

import (
	"time"

	"pmt/internal/app"
	"pmt/internal/newsmon"
	"pmt/internal/notify"
)

func main() {
	ctx, cancel, cfg := app.Start("newsmon")
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
	conn, err := app.Bus(ctx, cfg, "pmt-newsmon")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	n := notify.FromEnv("newsmon")
	n.DB = pool
	s := &newsmon.Service{Cfg: cfg, Bus: conn, DB: pool, Notify: n, Cal: cal}
	go app.Heartbeat(ctx, conn, "newsmon", 5*time.Second, s.Health)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("newsmon", err)
	}
}
