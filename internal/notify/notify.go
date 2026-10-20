// Package notify sends operational alerts to Telegram, records them in the alerts
// table, and falls back to the log when Telegram is not configured or fails.
// Messages carry levels, service names and counts only: never account IDs,
// tokens, URLs with credentials, or positions.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"pmt/internal/clock"
	"pmt/internal/store"
)

type Level string

const (
	Info Level = "info"
	Warn Level = "warn"
	Crit Level = "crit"
)

// Notifier is safe for concurrent use. The zero value logs only.
type Notifier struct {
	Service string
	DB      store.Querier // optional: alerts are recorded when set
	Dedupe  time.Duration // repeats of the same rule+message inside this window are dropped

	token, chat string
	api         string
	hc          *http.Client

	mu   sync.Mutex
	seen map[string]time.Time
}

// FromEnv builds a notifier from TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID; with
// either unset it only logs.
func FromEnv(service string) *Notifier {
	return New(service, os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"))
}

func New(service, token, chat string) *Notifier {
	return &Notifier{
		Service: service, Dedupe: 5 * time.Minute,
		token: token, chat: chat, api: "https://api.telegram.org",
		hc:   &http.Client{Timeout: 10 * time.Second},
		seen: map[string]time.Time{},
	}
}

// Enabled reports whether Telegram delivery is configured.
func (n *Notifier) Enabled() bool { return n != nil && n.token != "" && n.chat != "" }

// Send raises an alert. rule is a short stable identifier (feed_stale, gap,
// harvest_failed, ...) used for dedupe and in the alerts table.
func (n *Notifier) Send(ctx context.Context, lvl Level, rule, format string, args ...any) {
	if n == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	now := clock.Now()
	if n.duplicate(rule+"\x00"+msg, now) {
		return
	}
	text := fmt.Sprintf("%s %s · %s\n%s", icon(lvl), strings.ToUpper(string(lvl)), n.Service, msg)

	var deliverErr error
	delivered := false
	if n.Enabled() {
		if deliverErr = n.telegram(ctx, text); deliverErr == nil {
			delivered = true
		}
	}
	attrs := []any{"service", n.Service, "level", lvl, "rule", rule, "msg", msg}
	if deliverErr != nil {
		attrs = append(attrs, "delivery_err", deliverErr.Error())
	}
	switch lvl {
	case Crit:
		slog.Error("alert", attrs...)
	case Warn:
		slog.Warn("alert", attrs...)
	default:
		slog.Info("alert", attrs...)
	}
	if n.DB != nil {
		var de *string
		if deliverErr != nil {
			s := deliverErr.Error()
			de = &s
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := n.DB.Exec(dctx, `INSERT INTO alerts (rule, level, triggered_at, message, delivered, delivery_err)
			VALUES ($1,$2,$3,$4,$5,$6)`, rule, string(lvl), now, n.Service+": "+msg, delivered, de); err != nil {
			slog.Warn("alert not recorded", "err", err)
		}
	}
}

func (n *Notifier) duplicate(key string, now time.Time) bool {
	if n.Dedupe <= 0 {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	if t, ok := n.seen[key]; ok && now.Sub(t) < n.Dedupe {
		return true
	}
	n.seen[key] = now
	if len(n.seen) > 1000 {
		for k, t := range n.seen {
			if now.Sub(t) >= n.Dedupe {
				delete(n.seen, k)
			}
		}
	}
	return false
}

func (n *Notifier) telegram(ctx context.Context, text string) error {
	form := url.Values{"chat_id": {n.chat}, "text": {text}, "disable_web_page_preview": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.api+"/bot"+n.token+"/sendMessage",
		strings.NewReader(form.Encode()))
	if err != nil {
		return redact(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.hc.Do(req)
	if err != nil {
		return redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("telegram: %d %s", resp.StatusCode, body.Description)
	}
	return nil
}

// redact drops the request URL (which contains the bot token) from transport errors.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("telegram: %s: %v", ue.Op, ue.Err)
	}
	return errors.New("telegram: request failed")
}

func icon(l Level) string {
	switch l {
	case Crit:
		return "🔴"
	case Warn:
		return "🟠"
	}
	return "🔵"
}
