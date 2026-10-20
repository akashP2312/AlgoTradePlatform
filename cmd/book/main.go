// Command book polls the broker's positions and margins every few seconds and
// publishes them on book.positions and book.margins (and KV book). It reads only:
// it never places, changes or cancels orders.
package main

import (
	"time"

	"pmt/internal/app"
	"pmt/internal/book"
	"pmt/internal/notify"
)

func main() {
	ctx, cancel, cfg := app.Start("book")
	defer cancel()
	pool, err := app.DBReadOnly(ctx, cfg, time.Hour)
	if err != nil {
		app.Fatal("db", err)
	}
	defer pool.Close()
	conn, err := app.Bus(ctx, cfg, "pmt-book")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	s := &book.Service{Cfg: cfg, Bus: conn, DB: pool, Notify: notify.FromEnv("book")}
	go app.Heartbeat(ctx, conn, "book", 5*time.Second, s.Health)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("book", err)
	}
}
