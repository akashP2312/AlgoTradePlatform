// Package app is the shared start-up for the cmd binaries: logging, config,
// signals, and the Postgres, NATS and calendar handles most of them need.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/store"
)

// Start sets up JSON logging (PMT_LOG_LEVEL=debug for more), loads the config
// and returns a context cancelled on SIGINT or SIGTERM.
func Start(service string) (context.Context, context.CancelFunc, *config.Config) {
	lvl := slog.LevelInfo
	if strings.EqualFold(os.Getenv("PMT_LOG_LEVEL"), "debug") {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})).With("svc", service))
	cfg, err := config.Load(config.Path())
	if err != nil {
		Fatal("config", err)
	}
	if off := clock.Offset(); off != 0 {
		slog.Warn("clock offset in effect (dry run)", "offset", off.String(), "now", clock.Now().Format(time.DateTime))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx, cancel, cfg
}

// Fatal logs and exits 1.
func Fatal(what string, err error) {
	slog.Error(what, "err", err)
	os.Exit(1)
}

// DB opens Postgres, waiting up to a minute for it to come up.
func DB(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("database_url is empty")
	}
	return store.Open(ctx, cfg.DatabaseURL, time.Minute)
}

// Bus connects to NATS and makes sure the streams and buckets exist.
func Bus(ctx context.Context, cfg *config.Config, name string) (*bus.Conn, error) {
	c, err := bus.Connect(cfg.NATSURL, name)
	if err != nil {
		return nil, err
	}
	ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := c.Ensure(ectx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// CalendarPath and RulesPath are the seed files, relative to the repo root.
const (
	CalendarPath = "config/calendar/holidays.csv"
	RulesPath    = "config/expiry_rules.csv"
)

// Calendar loads the holiday calendar and warns when it does not cover today.
func Calendar(cfg *config.Config) (*calendar.Calendar, error) {
	cal, err := calendar.Load(cfg.Abs(CalendarPath))
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	if !cal.Covers(clock.Today()) {
		slog.Warn("calendar does not cover today; every weekday is treated as a normal session", "today", clock.Today().Format(time.DateOnly))
	}
	return cal, nil
}

// Rules loads the expiry rules.
func Rules(cfg *config.Config) ([]calendar.Rule, error) {
	return calendar.LoadRules(cfg.Abs(RulesPath))
}

// Heartbeat publishes a health message every interval until ctx ends.
func Heartbeat(ctx context.Context, nc *bus.Conn, service string, every time.Duration, check func() (bool, map[string]string)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ok, detail := check()
		if b, err := json.Marshal(bus.Health{Service: service, At: bus.Ms(clock.Now()), OK: ok, Detail: detail}); err == nil {
			nc.NC.Publish(bus.HealthSubject(service), b)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
