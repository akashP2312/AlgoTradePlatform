// Command backuprec is the second-host recorder. It reads the Kite session from
// the token file, snapshots the core option window over REST once a minute and
// appends it to JSONL under backuprec.out_dir. It needs neither Postgres nor
// NATS. With --listen it serves its counters on /metrics (bind it to localhost
// or the Tailscale address only).
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"pmt/internal/app"
	"pmt/internal/backuprec"
)

func main() {
	listen := flag.String("listen", "", "address for /metrics, e.g. 127.0.0.1:8091 (default: off)")
	flag.Parse()

	ctx, cancel, cfg := app.Start("backuprec")
	defer cancel()
	cal, err := app.Calendar(cfg)
	if err != nil {
		app.Fatal("calendar", err)
	}
	svc := &backuprec.Service{Cfg: cfg, Cal: cal, Out: backuprec.Writer{Dir: cfg.Abs(cfg.BackupRec.OutDir)}}

	if *listen != "" {
		if err := app.PrivateAddr(*listen); err != nil {
			app.Fatal("listen", err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			c := &svc.C
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			for _, m := range []struct {
				name string
				v    int64
			}{
				{"pmt_backuprec_polls_total", c.Polls.Load()},
				{"pmt_backuprec_chains_total", c.Chains.Load()},
				{"pmt_backuprec_rows_total", c.Rows.Load()},
				{"pmt_backuprec_errors_total", c.Errors.Load()},
				{"pmt_backuprec_no_session_total", c.NoSession.Load()},
			} {
				fmt.Fprintf(w, "%s %d\n", m.name, m.v)
			}
			fmt.Fprintf(w, "pmt_backuprec_last_ok_seconds %.3f\n", float64(c.LastOK.Load())/1000)
		})
		srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server", "err", err)
			}
		}()
		go func() { <-ctx.Done(); srv.Close() }()
	}

	slog.Info("backuprec: started", "out", svc.Out.Dir)
	if err := svc.Run(ctx); err != nil && ctx.Err() == nil {
		app.Fatal("backuprec", err)
	}
}
