// Command sys serves the bare SYS page and the Prometheus endpoint. It listens
// to sys.*.health for every service's heartbeat and reads job runs, today's
// recorder gaps and the session expiry (never the token). It binds to loopback
// or a Tailscale address only.
//
//	/          the SYS page
//	/metrics   Prometheus
//	/healthz   200 while NATS and Postgres answer
package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"pmt/internal/app"
	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/kite"
	"pmt/internal/store"
)

// staleAfter marks a service down when its heartbeat is this old.
const staleAfter = 20 * time.Second

type health struct {
	bus.Health
	Seen time.Time // local receive time
}

type sys struct {
	pool *pgxpool.Pool
	conn *bus.Conn
	kv   jetstream.KeyValue

	mu   sync.Mutex
	last map[string]health
}

func main() {
	ctx, cancel, cfg := app.Start("sys")
	defer cancel()
	if err := app.PrivateAddr(cfg.Sys.Listen); err != nil {
		app.Fatal("listen", err)
	}
	pool, err := app.DB(ctx, cfg)
	if err != nil {
		app.Fatal("db", err)
	}
	defer pool.Close()
	conn, err := app.Bus(ctx, cfg, "pmt-sys")
	if err != nil {
		app.Fatal("nats", err)
	}
	defer conn.Close()
	kv, err := conn.KV(ctx, bus.KVSession)
	if err != nil {
		app.Fatal("kv", err)
	}

	s := &sys{pool: pool, conn: conn, kv: kv, last: map[string]health{}}
	sub, err := conn.NC.Subscribe(bus.SubjHealth, s.onHealth)
	if err != nil {
		app.Fatal("subscribe", err)
	}
	defer sub.Unsubscribe()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), s)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !conn.NC.IsConnected() || pool.Ping(r.Context()) != nil {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: cfg.Sys.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	slog.Info("sys: listening", "addr", cfg.Sys.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		app.Fatal("http", err)
	}
}

func (s *sys) onHealth(m *nats.Msg) {
	var h bus.Health
	if err := json.Unmarshal(m.Data, &h); err != nil || h.Service == "" {
		return
	}
	s.mu.Lock()
	s.last[h.Service] = health{Health: h, Seen: time.Now()}
	s.mu.Unlock()
}

func (s *sys) services() []health {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]health, 0, len(s.last))
	for _, h := range s.last {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func up(h health) bool { return h.OK && time.Since(h.Seen) < staleAfter }

type gapRow struct {
	Kind, Underlying, Expiry, FilledBy, Detail string
	Start, End                                 time.Time
	Minutes, Filled                            int
}

func todayGaps(ctx context.Context, pool *pgxpool.Pool) ([]gapRow, error) {
	rows, err := pool.Query(ctx, `SELECT kind, underlying, COALESCE(to_char(expiry, 'YYYY-MM-DD'), ''), start_ts, end_ts,
			minutes, filled_minutes, COALESCE(filled_by, ''), COALESCE(detail, '')
		FROM recorder_gaps WHERE trade_date = $1 ORDER BY start_ts, kind, underlying LIMIT 500`, clock.Today())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (gapRow, error) {
		var g gapRow
		err := r.Scan(&g.Kind, &g.Underlying, &g.Expiry, &g.Start, &g.End, &g.Minutes, &g.Filled, &g.FilledBy, &g.Detail)
		return g, err
	})
}

// sessionInfo returns the user and expiry of the stored session; the token is
// never read into the page.
func (s *sys) sessionInfo(ctx context.Context) (user string, expires time.Time, ok bool) {
	se, err := kite.LoadSession(ctx, s.kv)
	return se.UserID, se.ExpiresAt, err == nil
}

// --- page

type view struct {
	Now      time.Time
	Offset   string
	Services []svcView
	Jobs     []store.JobSummary
	Gaps     []gapRow
	Session  string
	SessOK   bool
	Errors   []string
}

type svcView struct {
	Name   string
	Up     bool
	Age    string
	Detail [][2]string
}

func (s *sys) page(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	v := view{Now: clock.Now()}
	if off := clock.Offset(); off != 0 {
		v.Offset = off.String()
	}
	for _, h := range s.services() {
		sv := svcView{Name: h.Service, Up: up(h), Age: time.Since(h.Seen).Round(time.Second).String()}
		keys := make([]string, 0, len(h.Detail))
		for k := range h.Detail {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sv.Detail = append(sv.Detail, [2]string{k, h.Detail[k]})
		}
		v.Services = append(v.Services, sv)
	}
	var err error
	if v.Jobs, err = store.LastJobs(ctx, s.pool); err != nil {
		v.Errors = append(v.Errors, "jobs: "+err.Error())
	}
	if v.Gaps, err = todayGaps(ctx, s.pool); err != nil {
		v.Errors = append(v.Errors, "gaps: "+err.Error())
	}
	if user, exp, ok := s.sessionInfo(ctx); ok {
		v.SessOK, v.Session = true, "user "+user+", expires "+exp.In(clock.IST).Format("Mon 02 Jan 15:04")
	} else {
		v.Session = "no valid session: run login"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Refresh", "10")
	if err := pageTmpl.Execute(w, v); err != nil {
		slog.Warn("sys: render", "err", err)
	}
}

var pageTmpl = template.Must(template.New("sys").Funcs(template.FuncMap{
	"hm": func(t time.Time) string { return t.In(clock.IST).Format("15:04") },
	"dt": func(t time.Time) string { return t.In(clock.IST).Format("02 Jan 15:04:05") },
	"dtp": func(t *time.Time) string {
		if t == nil {
			return "running"
		}
		return t.In(clock.IST).Format("15:04:05")
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>PMT SYS</title><style>
:root{--bg:#fff;--fg:#111;--mut:#666;--ok:#16794a;--bad:#b42318;--line:#ddd}
@media (prefers-color-scheme: dark){:root{--bg:#111;--fg:#eee;--mut:#999;--ok:#4ade80;--bad:#f87171;--line:#333}}
body{background:var(--bg);color:var(--fg);font:14px/1.4 ui-monospace,Menlo,monospace;margin:16px}
table{border-collapse:collapse;margin:8px 0 20px;width:100%}td,th{border-bottom:1px solid var(--line);padding:3px 8px;text-align:left;vertical-align:top}
th{color:var(--mut);font-weight:normal}.ok{color:var(--ok)}.bad{color:var(--bad)}.mut{color:var(--mut)}h2{font-size:15px;margin:16px 0 4px}
details summary{cursor:pointer}</style></head><body>
<div><b>PMT SYS</b> <span class="mut">{{dt .Now}} IST{{if .Offset}} · clock offset {{.Offset}} (dry run){{end}}</span></div>
<div class="{{if .SessOK}}ok{{else}}bad{{end}}">Kite: {{.Session}}</div>
{{range .Errors}}<div class="bad">{{.}}</div>{{end}}
<h2>Services</h2><table><tr><th>service</th><th>state</th><th>last beat</th><th>detail</th></tr>
{{range .Services}}<tr><td>{{.Name}}</td><td class="{{if .Up}}ok{{else}}bad{{end}}">{{if .Up}}up{{else}}DOWN{{end}}</td><td>{{.Age}} ago</td>
<td><details><summary class="mut">{{len .Detail}} fields</summary>{{range .Detail}}{{index . 0}}={{index . 1}}<br>{{end}}</details></td></tr>
{{else}}<tr><td colspan="4" class="mut">no heartbeats yet</td></tr>{{end}}</table>
<h2>Jobs (latest run of each)</h2><table><tr><th>job</th><th>args</th><th>started</th><th>finished</th><th>status</th><th>detail</th></tr>
{{range .Jobs}}<tr><td>{{.Job}}</td><td>{{.Args}}</td><td>{{dt .StartedAt}}</td><td>{{dtp .FinishedAt}}</td>
<td class="{{if eq .Status "ok"}}ok{{else if eq .Status "running"}}mut{{else}}bad{{end}}">{{.Status}}</td><td>{{.Detail}}</td></tr>
{{else}}<tr><td colspan="6" class="mut">no job runs</td></tr>{{end}}</table>
<h2>Recorder gaps today</h2><table><tr><th>kind</th><th>underlying</th><th>expiry</th><th>from</th><th>to</th><th>min</th><th>filled</th><th>by</th><th>detail</th></tr>
{{range .Gaps}}<tr><td>{{.Kind}}</td><td>{{.Underlying}}</td><td>{{.Expiry}}</td><td>{{hm .Start}}</td><td>{{hm .End}}</td><td>{{.Minutes}}</td>
<td class="{{if ge .Filled .Minutes}}ok{{else}}bad{{end}}">{{.Filled}}</td><td>{{.FilledBy}}</td><td>{{.Detail}}</td></tr>
{{else}}<tr><td colspan="9" class="ok">none</td></tr>{{end}}</table>
</body></html>`))

// --- Prometheus

var (
	descUp      = prometheus.NewDesc("pmt_service_up", "1 while the service's last heartbeat is OK and recent.", []string{"service"}, nil)
	descAge     = prometheus.NewDesc("pmt_service_heartbeat_age_seconds", "Seconds since the service's last heartbeat.", []string{"service"}, nil)
	descDetail  = prometheus.NewDesc("pmt_service_detail", "Numeric heartbeat detail fields.", []string{"service", "key"}, nil)
	descJobOK   = prometheus.NewDesc("pmt_job_last_ok", "1 when the job's latest run succeeded.", []string{"job"}, nil)
	descJobAt   = prometheus.NewDesc("pmt_job_last_start_seconds", "Unix time of the job's latest start.", []string{"job"}, nil)
	descGapOpen = prometheus.NewDesc("pmt_gaps_open_minutes", "Today's unfilled gap minutes by kind.", []string{"kind"}, nil)
	descSess    = prometheus.NewDesc("pmt_kite_session_expires_seconds", "Unix time the stored Kite session expires; 0 when there is none.", nil, nil)
)

func (s *sys) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descUp, descAge, descDetail, descJobOK, descJobAt, descGapOpen, descSess} {
		ch <- d
	}
}

func (s *sys) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	for _, h := range s.services() {
		gauge(descUp, b2f(up(h)), h.Service)
		gauge(descAge, time.Since(h.Seen).Seconds(), h.Service)
		for k, val := range h.Detail {
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				gauge(descDetail, f, h.Service, k)
			} else if bv, err := strconv.ParseBool(val); err == nil {
				gauge(descDetail, b2f(bv), h.Service, k)
			}
		}
	}
	if jobs, err := store.LastJobs(ctx, s.pool); err == nil {
		for _, j := range jobs {
			gauge(descJobOK, b2f(j.Status == "ok"), j.Job)
			gauge(descJobAt, float64(j.StartedAt.Unix()), j.Job)
		}
	}
	if gaps, err := todayGaps(ctx, s.pool); err == nil {
		open := map[string]int{"snapshot": 0, "ticks": 0, "harvest": 0}
		for _, g := range gaps {
			if g.Minutes > g.Filled {
				open[g.Kind] += g.Minutes - g.Filled
			}
		}
		for k, v := range open {
			gauge(descGapOpen, float64(v), k)
		}
	}
	exp := 0.0
	if _, e, ok := s.sessionInfo(ctx); ok {
		exp = float64(e.Unix())
	}
	gauge(descSess, exp)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
