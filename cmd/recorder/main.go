// Command recorder drains the TICKS, CHAIN and ORDERS streams into Timescale.
// Each consumer commits its stream offset with the rows, so restarts neither
// lose nor duplicate data while the stream still holds the messages.
package main

import (
	"strconv"
	"time"

	"pmt/internal/app"
	"pmt/internal/recorder"
)

func main() {
	ctx, cancel, cfg := app.Start("recorder")
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
	conn, err := app.Bus(ctx, cfg, "pmt-recorder")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	rec := &recorder.Recorder{Pool: pool, JS: conn.JS, Cfg: cfg.Recorder,
		Open: func(d time.Time) time.Time { return cal.Session(d).Open }}

	// Healthy while every stream that has flushed did so recently, or none has
	// had anything to flush yet.
	go app.Heartbeat(ctx, conn, "recorder", 5*time.Second, func() (bool, map[string]string) {
		d := rec.Detail()
		ok := true
		now := time.Now().UnixMilli() // LastFlush is wall-clock, not the dry-run clock
		for _, c := range []*recorder.Counters{&rec.Ticks, &rec.Chain, &rec.Orders} {
			if c.Errors.Load() > 0 && now-c.LastFlush.Load() > int64(time.Minute/time.Millisecond) {
				ok = false
			}
		}
		d["nats"] = strconv.FormatBool(conn.NC.IsConnected())
		return ok && conn.NC.IsConnected(), d
	})

	if err := rec.Run(ctx); err != nil {
		app.Fatal("recorder", err)
	}
}
