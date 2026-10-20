// Command gateway serves the browser: one WebSocket per tab at /ws carrying
// ticks, analytics and service health, conflated to the latest value per
// subject and flushed every gateway.flush (250 ms). It seeds its last-value
// cache from the KV buckets so a panel paints at once, serves the built web app
// at / when present, and binds to loopback or a Tailscale address only. It only
// reads: from the bus, and from Postgres over a read-only pool.
//
//	/ws        the tab socket
//	/api/*     contract terms, bars, straddle history (see gateway.API)
//	/          web/dist, when built
//	/metrics   Prometheus
//	/healthz   200 while NATS answers
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"pmt/internal/app"
	"pmt/internal/bus"
	"pmt/internal/gateway"
)

func main() {
	ctx, cancel, cfg := app.Start("gateway")
	defer cancel()
	if err := app.PrivateAddr(cfg.Gateway.Listen); err != nil {
		app.Fatal("listen", err)
	}
	conn, err := app.Bus(ctx, cfg, "pmt-gateway")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()

	hub := gateway.NewHub(cfg.Gateway.Flush)
	// Subscribe before seeding, so nothing published in between is lost and
	// the seed never overwrites a newer live value.
	for _, subj := range []string{bus.SubjTicks, bus.SubjAnalytics, bus.SubjHealth, bus.SubjFeed} {
		sub, err := conn.NC.Subscribe(subj, func(m *nats.Msg) { hub.Publish(m.Subject, m.Data) })
		if err != nil {
			app.Fatal("subscribe", err)
		}
		sub.SetPendingLimits(1<<20, 256<<20)
		defer sub.Unsubscribe()
	}
	for bucket, prefix := range map[string]string{bus.KVLast: bus.TickPrefix, bus.KVAnalytics: "analytics."} {
		n, err := seed(ctx, conn, hub, bucket, prefix)
		if err != nil {
			slog.Warn("gateway: seed from kv failed; panels fill on the next tick", "bucket", bucket, "err", err)
			continue
		}
		slog.Info("gateway: seeded", "bucket", bucket, "subjects", n)
	}

	go app.Heartbeat(ctx, conn, "gateway", 5*time.Second, func() (bool, map[string]string) {
		return conn.NC.IsConnected(), map[string]string{
			"tabs":      fmt.Sprint(hub.Clients()),
			"subjects":  fmt.Sprint(hub.Subjects()),
			"msgs":      fmt.Sprint(hub.Msgs.Load()),
			"frames":    fmt.Sprint(hub.Frames.Load()),
			"conflated": fmt.Sprint(hub.Conflated.Load()),
			"dropped":   fmt.Sprint(hub.Dropped.Load()),
		}
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	gauge := func(name, help string, f func() float64) {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f))
	}
	counter := func(name, help string, f func() float64) {
		reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, f))
	}
	gauge("pmt_gateway_tabs", "Connected browser tabs.", func() float64 { return float64(hub.Clients()) })
	gauge("pmt_gateway_subjects", "Subjects in the last-value cache.", func() float64 { return float64(hub.Subjects()) })
	counter("pmt_gateway_msgs_total", "Bus messages received.", func() float64 { return float64(hub.Msgs.Load()) })
	counter("pmt_gateway_invalid_total", "Bus messages dropped as invalid JSON.", func() float64 { return float64(hub.Invalid.Load()) })
	counter("pmt_gateway_frames_total", "Frames written to tabs.", func() float64 { return float64(hub.Frames.Load()) })
	counter("pmt_gateway_bytes_total", "Bytes written to tabs before compression.", func() float64 { return float64(hub.Bytes.Load()) })
	counter("pmt_gateway_conflated_total", "Updates replaced by a newer one before a flush.", func() float64 { return float64(hub.Conflated.Load()) })
	counter("pmt_gateway_dropped_total", "Tabs closed on a write error.", func() float64 { return float64(hub.Dropped.Load()) })

	api := &gateway.API{}
	go func() {
		// The API waits for Postgres without holding up the live feed.
		pool, err := app.DBReadOnly(ctx, cfg, 24*time.Hour)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("gateway: postgres unavailable; /api answers 503", "err", err)
			}
			return
		}
		api.SetPool(pool)
		slog.Info("gateway: api connected to postgres (read-only)")
		<-ctx.Done()
		pool.Close()
	}()

	mux := http.NewServeMux()
	mux.Handle("GET /ws", hub)
	api.Register(mux)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !conn.NC.IsConnected() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	if dir := cfg.Abs(cfg.Gateway.Static); dirExists(dir) {
		mux.Handle("GET /", http.FileServer(http.Dir(dir)))
		slog.Info("gateway: serving web app", "dir", dir)
	} else {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("pmt gateway: web app not built; connect to /ws\n"))
		})
	}

	srv := &http.Server{Addr: cfg.Gateway.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	slog.Info("gateway: listening", "addr", cfg.Gateway.Listen, "flush", cfg.Gateway.Flush.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		app.Fatal("http", err)
	}
}

// seed loads every key of a last-value bucket into the hub's cache.
func seed(ctx context.Context, conn *bus.Conn, hub *gateway.Hub, bucket, prefix string) (int, error) {
	kv, err := conn.KV(ctx, bucket)
	if err != nil {
		return 0, err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	w, err := kv.WatchAll(wctx, jetstream.IgnoreDeletes())
	if err != nil {
		return 0, err
	}
	defer w.Stop()
	n := 0
	for {
		select {
		case <-wctx.Done():
			return n, wctx.Err()
		case e := <-w.Updates():
			if e == nil { // end of the initial values
				return n, nil
			}
			if hub.Seed(prefix+e.Key(), e.Value()) {
				n++
			}
		}
	}
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
