package kite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

// ErrNoSession means no usable login is stored yet.
var ErrNoSession = errors.New("kite: no valid session")

// SessionExpiry is when a token issued at t stops working: 06:00 IST the next
// morning (verify against Kite's documentation; the token may die earlier if the
// user logs out of Kite web, which the terminal itself never does).
func SessionExpiry(t time.Time) time.Time {
	d := clock.Date(t)
	exp := clock.At(d, 6, 0, 0)
	if !t.In(clock.IST).Before(exp) {
		exp = clock.At(d.AddDate(0, 0, 1), 6, 0, 0)
	}
	return exp
}

// SaveSession stores s in KV kite_session/current.
func SaveSession(ctx context.Context, kv jetstream.KeyValue, s bus.Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = kv.Put(ctx, bus.SessionKey, b)
	return err
}

// LoadSession reads the stored session; ErrNoSession if absent or expired.
func LoadSession(ctx context.Context, kv jetstream.KeyValue) (bus.Session, error) {
	e, err := kv.Get(ctx, bus.SessionKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return bus.Session{}, ErrNoSession
	}
	if err != nil {
		return bus.Session{}, err
	}
	var s bus.Session
	if err := json.Unmarshal(e.Value(), &s); err != nil {
		return bus.Session{}, fmt.Errorf("kite session: %w", err)
	}
	if !s.Valid(clock.Now()) {
		return s, ErrNoSession
	}
	return s, nil
}

// WatchSession delivers each new valid session (including the current one, if
// any) until ctx ends.
func WatchSession(ctx context.Context, kv jetstream.KeyValue) (<-chan bus.Session, error) {
	w, err := kv.Watch(ctx, bus.SessionKey)
	if err != nil {
		return nil, err
	}
	out := make(chan bus.Session, 1)
	go func() {
		defer close(out)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-w.Updates():
				if !ok {
					return
				}
				if e == nil || e.Operation() != jetstream.KeyValuePut { // nil marks end of initial values
					continue
				}
				var s bus.Session
				if err := json.Unmarshal(e.Value(), &s); err != nil {
					slog.Warn("kite session: unreadable entry in KV", "err", err)
					continue
				}
				if !s.Valid(clock.Now()) {
					// Only the user and expiry are logged, never the token.
					slog.Warn("kite session: stored session is not valid now; run login", "user", s.UserID, "expires", s.ExpiresAt.In(clock.IST).Format(time.DateTime))
					continue
				}
				select {
				case out <- s:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// WriteTokenFile writes s as JSON with mode 0600 via an atomic rename, so the
// Python bot and the backup recorder never read a half-written file.
func WriteTokenFile(path string, s bus.Session) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".kite-token-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadTokenFile reads a session written by WriteTokenFile; ErrNoSession if the
// file is missing or the token has expired.
func ReadTokenFile(path string) (bus.Session, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return bus.Session{}, ErrNoSession
	}
	if err != nil {
		return bus.Session{}, err
	}
	var s bus.Session
	if err := json.Unmarshal(b, &s); err != nil {
		return bus.Session{}, fmt.Errorf("token file: %w", err)
	}
	if !s.Valid(clock.Now()) {
		return s, ErrNoSession
	}
	return s, nil
}

// Exchange trades a request token from the login redirect for an access token.
// The API secret is used only here and never stored.
func (r *REST) Exchange(ctx context.Context, requestToken, apiSecret string) (bus.Session, error) {
	us, err := r.Kite.GenerateSession(requestToken, apiSecret)
	if err != nil {
		return bus.Session{}, err
	}
	now := clock.Now()
	s := bus.Session{AccessToken: us.AccessToken, APIKey: r.apiKey, UserID: us.UserID,
		IssuedAt: now, ExpiresAt: SessionExpiry(now)}
	r.SetAccessToken(s.AccessToken)
	return s, nil
}
