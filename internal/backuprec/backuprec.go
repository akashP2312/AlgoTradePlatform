// Package backuprec is the second-host recorder: once a minute it snapshots the
// core option window over REST and appends it to a local JSONL file. It needs no
// database or NATS; the files are copied to the main host (rsync over Tailscale)
// and loaded with `jobs merge-backup`, which fills only the minutes the main
// recorder missed.
package backuprec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
	"pmt/internal/refdata"
	"pmt/internal/sweep"
)

// Path is where a day's snapshots are written.
func Path(dir string, day time.Time) string {
	return filepath.Join(dir, clock.Date(day).Format(time.DateOnly), "chains.jsonl")
}

// Writer appends snapshots, one JSON object per line.
type Writer struct{ Dir string }

// Write appends chains for day in one write and syncs, so a crash loses at most
// the minute being written and never leaves half a line before a whole one.
func (w Writer) Write(day time.Time, chains []bus.Chain) error {
	if len(chains) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range chains {
		if err := enc.Encode(&chains[i]); err != nil {
			return err
		}
	}
	p := Path(w.Dir, day)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Client is the part of the Kite REST client the backup recorder uses.
type Client interface {
	sweep.Quoter
	LTP(ctx context.Context, keys []string) (map[string]float64, error)
	FetchInstruments(ctx context.Context) ([]byte, error)
}

type Counters struct {
	Polls, Chains, Rows, Errors, NoSession atomic.Int64
	LastOK                                 atomic.Int64 // epoch ms of the last written minute
}

type Service struct {
	Cfg *config.Config
	Cal *calendar.Calendar
	// Dial builds a client for a session; nil uses kite.NewREST.
	Dial func(s bus.Session) Client
	Out  Writer
	C    Counters

	sess    bus.Session
	client  Client
	instDay time.Time
	insts   []refdata.Inst
}

const pollTimeout = 40 * time.Second

// Run polls until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	if s.Dial == nil {
		k := s.Cfg.Kite
		s.Dial = func(se bus.Session) Client {
			key := se.APIKey
			if key == "" {
				key = k.APIKey
			}
			return kite.NewREST(k, key, se.AccessToken)
		}
	}
	for {
		at, ok := s.next(clock.Now())
		if !ok {
			return fmt.Errorf("backuprec: calendar has no trading day after %s", clock.Today().Format(time.DateOnly))
		}
		if !clock.SleepUntil(ctx.Done(), at) {
			return ctx.Err()
		}
		pctx, cancel := context.WithTimeout(ctx, pollTimeout)
		err := s.Poll(pctx, clock.Minute(at))
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("backuprec: poll failed", "minute", clock.Minute(at).Format("15:04"), "err", err)
		}
	}
}

// next is when the poll for the next session minute is due: PollSecond past the
// minute, for minutes from open to close inclusive.
func (s *Service) next(now time.Time) (time.Time, bool) {
	off := time.Duration(s.Cfg.BackupRec.PollSecond) * time.Second
	m := clock.Minute(now)
	if !now.Before(m.Add(off)) {
		m = m.Add(time.Minute)
	}
	for range 30 {
		d := clock.Date(m)
		se := s.Cal.Session(d)
		if se.Trading() {
			switch {
			case m.Before(se.Open):
				return se.Open.Add(off), true
			case !m.After(se.Close):
				return m.Add(off), true
			}
		}
		m = d.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}

// Poll snapshots minute and appends it.
func (s *Service) Poll(ctx context.Context, minute time.Time) error {
	s.C.Polls.Add(1)
	err := s.poll(ctx, minute)
	if err != nil {
		s.C.Errors.Add(1)
	}
	return err
}

func (s *Service) poll(ctx context.Context, minute time.Time) error {
	if err := s.session(); err != nil {
		return err
	}
	day := clock.Date(minute)
	if !s.instDay.Equal(day) || len(s.insts) == 0 {
		raw, err := s.client.FetchInstruments(ctx)
		if err != nil {
			return s.fail(fmt.Errorf("instruments: %w", err))
		}
		rows, err := kite.ParseInstruments(raw)
		if err != nil {
			return fmt.Errorf("instruments: %w", err)
		}
		s.insts, s.instDay = refdata.Resolve(rows), day
	}
	spot, err := s.spot(ctx)
	if err != nil {
		// A chain without spot is still worth keeping; the window falls back to
		// the middle strike.
		slog.Warn("backuprec: spot", "err", kite.Describe(err))
		if kite.IsTokenError(err) {
			return s.fail(err)
		}
	}
	targets, warns := sweep.Core(s.Cfg, s.insts, spot, day)
	for _, w := range warns {
		slog.Debug("backuprec: " + w)
	}
	if len(targets) == 0 {
		return errors.New("no targets: no underlying has backup set, or none is listed")
	}
	chains, st, err := sweep.Snap(ctx, s.client, targets, minute, "backup")
	if err != nil && len(chains) == 0 {
		return s.fail(err)
	}
	if werr := s.Out.Write(day, chains); werr != nil {
		return fmt.Errorf("write: %w", werr)
	}
	rows := 0
	for i := range chains {
		rows += len(chains[i].Rows)
	}
	s.C.Chains.Add(int64(len(chains)))
	s.C.Rows.Add(int64(rows))
	s.C.LastOK.Store(minute.UnixMilli())
	slog.Debug("backuprec: wrote", "minute", minute.Format("15:04"), "stats", st.String())
	if err != nil {
		return fmt.Errorf("partial snapshot: %w", err)
	}
	return nil
}

// session loads the token file when there is no client, or when it has changed.
func (s *Service) session() error {
	se, err := kite.ReadTokenFile(s.Cfg.Abs(s.Cfg.BackupRec.TokenFile))
	if err != nil {
		s.client = nil
		if errors.Is(err, kite.ErrNoSession) {
			s.C.NoSession.Add(1)
		}
		return err
	}
	if s.client == nil || se.AccessToken != s.sess.AccessToken {
		s.sess, s.client = se, s.Dial(se)
	}
	return nil
}

// fail drops the client on a token error so the next poll rereads the file.
func (s *Service) fail(err error) error {
	if kite.IsTokenError(err) {
		s.client, s.sess = nil, bus.Session{}
	}
	return err
}

func (s *Service) spot(ctx context.Context) (map[string]float64, error) {
	keys := map[string]string{}
	var ks []string
	for _, ul := range s.Cfg.Universe.Underlyings {
		if ul.Backup && strings.Contains(ul.Index, ":") {
			keys[ul.Index] = ul.Name
			ks = append(ks, ul.Index)
		}
	}
	out := map[string]float64{}
	if len(ks) == 0 {
		return out, nil
	}
	ltp, err := s.client.LTP(ctx, ks)
	if err != nil {
		return out, err
	}
	for k, v := range ltp {
		if n, ok := keys[k]; ok {
			out[n] = v
		}
	}
	return out, nil
}
