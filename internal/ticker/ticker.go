package ticker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
	"pmt/internal/notify"
	"pmt/internal/refdata"
)

// Service runs the ticker: it owns the WebSocket connections, the in-memory last
// value per token, and the minute chain cutter.
type Service struct {
	Cfg    *config.Config
	Bus    *bus.Conn
	DB     *pgxpool.Pool
	Cal    *calendar.Calendar
	Notify *notify.Notifier

	token  atomic.Value // string
	apiKey atomic.Value // string
	sess   atomic.Pointer[calendar.Session]
	kick   chan struct{} // asks the control loop to rebuild the universe
	lkv    jetstream.KeyValue

	mu       sync.Mutex
	insts    []refdata.Inst
	instDate string
	uni      *Universe
	assign   map[uint32]int
	guessed  map[string]bool // underlyings whose window was centred without a spot
	conns    []*kite.WS
	last     map[uint32]bus.Tick
	dirty    map[uint32]struct{}
	feed     FeedStatus
	lastAny  atomic.Int64 // ms of the last NSE/NFO/BSE/BFO tick on any token

	Ticks, Chains, Rebuilds, PublishErrors atomic.Int64
}

// Run blocks until ctx ends. It waits for a login and for today's instruments
// rather than failing, so the container can start before either exists.
func (s *Service) Run(ctx context.Context) error {
	s.kick = make(chan struct{}, 1)
	s.last = map[uint32]bus.Tick{}
	s.dirty = map[uint32]struct{}{}
	s.refreshSession(clock.Now())

	skv, err := s.Bus.KV(ctx, bus.KVSession)
	if err != nil {
		return err
	}
	lkv, err := s.Bus.KV(ctx, bus.KVLast)
	if err != nil {
		return err
	}
	s.lkv = lkv
	sessions, err := kite.WatchSession(ctx, skv)
	if err != nil {
		return err
	}
	slog.Info("ticker: waiting for a Kite session")
	select {
	case <-ctx.Done():
		return nil
	case se, ok := <-sessions:
		if !ok {
			return ctx.Err()
		}
		s.setSession(se)
	}

	if err := s.loadInstruments(ctx, true); err != nil {
		return err
	}
	spot := s.kvSpot(ctx, lkv)

	n := max(1, s.Cfg.Kite.WSConnections)
	for i := range n {
		w := &kite.WS{
			Name: fmt.Sprintf("ws%d", i+1), URL: s.Cfg.Kite.WSURL,
			Token: func() string { return s.token.Load().(string) }, Silence: s.Cfg.Ticker.SilenceReconnect,
			Max: s.Cfg.Kite.TokensPerConn, OnPacket: s.onPacket, OnText: s.onText,
		}
		w.APIKey = s.apiKey.Load().(string)
		s.conns = append(s.conns, w)
	}
	s.rebuild(spot, "start")
	for _, w := range s.conns {
		go w.Run(ctx)
	}

	var wg sync.WaitGroup
	for _, f := range []func(context.Context){
		func(ctx context.Context) { s.flushLoop(ctx, lkv) },
		s.minuteLoop,
		s.feedLoop,
		s.healthLoop,
	} {
		wg.Add(1)
		go func() { defer wg.Done(); f(ctx) }()
	}
	s.control(ctx, sessions)
	wg.Wait()
	return nil
}

func (s *Service) setSession(se bus.Session) {
	key := se.APIKey
	if key == "" {
		key = s.Cfg.Kite.APIKey
	}
	s.apiKey.Store(key)
	s.token.Store(se.AccessToken)
	slog.Info("ticker: session loaded", "session", se.String())
}

// control serialises universe rebuilds: on a new login, a new instrument dump,
// or a recentre request from the packet path.
func (s *Service) control(ctx context.Context, sessions <-chan bus.Session) {
	check := time.NewTicker(5 * time.Minute)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case se, ok := <-sessions:
			if !ok {
				return
			}
			s.setSession(se)
			if err := s.loadInstruments(ctx, false); err != nil {
				slog.Warn("ticker: instrument reload", "err", err)
			}
		case <-check.C:
			changed, err := s.dumpChanged(ctx)
			if err != nil {
				slog.Warn("ticker: instrument dump check", "err", err)
				continue
			}
			if changed {
				if err := s.loadInstruments(ctx, false); err == nil {
					s.rebuild(s.spots(), "new instruments")
				}
			} else if s.dayChanged() {
				s.rebuild(s.spots(), "new day")
			}
		case <-s.kick:
			s.rebuild(s.spots(), "recentre")
		}
	}
}

func (s *Service) latestDump(ctx context.Context) (string, error) {
	var d *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT max(dump_date) FROM instrument_dumps`).Scan(&d); err != nil {
		return "", err
	}
	if d == nil {
		return "", nil
	}
	return d.Format("2006-01-02"), nil
}

func (s *Service) dumpChanged(ctx context.Context) (bool, error) {
	d, err := s.latestDump(ctx)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return d != s.instDate, nil
}

// loadInstruments reads the open instruments. With wait it retries until rows
// exist (the instruments job may not have run yet on a fresh install).
func (s *Service) loadInstruments(ctx context.Context, wait bool) error {
	for {
		d, err := s.latestDump(ctx)
		var insts []refdata.Inst
		if err == nil {
			insts, err = refdata.Current(ctx, s.DB)
		}
		if err == nil && len(insts) > 0 {
			s.mu.Lock()
			s.insts, s.instDate = insts, d
			s.mu.Unlock()
			slog.Info("ticker: instruments loaded", "rows", len(insts), "dump", d)
			s.pruneLast(ctx, insts)
			return nil
		}
		if !wait {
			if err == nil {
				err = errors.New("no open instruments")
			}
			return err
		}
		slog.Warn("ticker: no instruments yet; run `jobs instruments`", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func (s *Service) dayChanged() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uni != nil && s.uni.day != clock.Today().Format("2006-01-02")
}

// kvSpot reads each underlying's last index level from the last-value bucket, so a
// restart mid-session centres the window correctly before the first tick.
func (s *Service) kvSpot(ctx context.Context, kv jetstream.KeyValue) map[string]float64 {
	out := map[string]float64{}
	s.mu.Lock()
	insts := s.insts
	s.mu.Unlock()
	for _, ul := range s.Cfg.Universe.Underlyings {
		exch, sym, ok := strings.Cut(ul.Index, ":")
		if !ok {
			continue
		}
		in, ok := refdata.BySymbol(insts, exch, sym)
		if !ok {
			continue
		}
		e, err := kv.Get(ctx, in.Key.Subject())
		if err != nil {
			continue
		}
		var t bus.Tick
		if json.Unmarshal(e.Value(), &t) == nil && t.LTP > 0 {
			out[ul.Name] = t.LTP
		}
	}
	return out
}

// spots returns the latest index LTP per underlying from live ticks.
func (s *Service) spots() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]float64{}
	if s.uni == nil {
		return out
	}
	for und, tok := range s.uni.Index {
		if t, ok := s.last[tok]; ok && t.LTP > 0 {
			out[und] = t.LTP
		}
	}
	return out
}

// rebuild selects the universe again and applies it with sticky connection
// assignment, so unchanged tokens are never resubscribed.
func (s *Service) rebuild(spot map[string]float64, reason string) {
	today := clock.Today()
	s.mu.Lock()
	u := Build(s.Cfg, s.insts, spot, today)
	u.day = today.Format("2006-01-02")
	assign, sets := Assign(u.Modes, s.assign, len(s.conns), s.Cfg.Kite.TokensPerConn)
	s.uni, s.assign = u, assign
	s.guessed = map[string]bool{}
	for _, ul := range s.Cfg.Universe.Underlyings {
		if spot[ul.Name] == 0 {
			s.guessed[ul.Name] = true
		}
	}
	for tok := range s.last {
		if _, ok := u.Modes[tok]; !ok {
			delete(s.last, tok)
			delete(s.dirty, tok)
		}
	}
	conns := s.conns
	s.mu.Unlock()

	s.Rebuilds.Add(1)
	for _, w := range u.Warn {
		slog.Warn("ticker: universe", "warn", w)
	}
	slog.Info("ticker: universe applied", "reason", reason, "tokens", u.Count(), "chains", len(u.Chains),
		"trimmed", u.Trimmed, "centre", fmt.Sprint(u.Centre))
	for i, w := range conns {
		if err := w.Set(sets[i]); err != nil {
			slog.Warn("ticker: subscribe", "conn", w.Name, "err", err)
		}
	}
}

func (s *Service) refreshSession(now time.Time) calendar.Session {
	cur := s.sess.Load()
	if cur != nil && cur.Date.Equal(clock.Date(now)) {
		return *cur
	}
	se := s.Cal.Session(now)
	s.sess.Store(&se)
	return se
}

// Phase labels a tick: x for exchanges outside the NSE/BSE equity-derivatives
// session (MCX, CDS, NSEIX), p before the open, c in the continuous session
// (close inclusive), a after the close or on a non-trading day.
func Phase(se calendar.Session, exchange string, now time.Time) string {
	switch exchange {
	case "NSE", "NFO", "BSE", "BFO":
	default:
		return "x"
	}
	switch {
	case !se.Trading():
		return "a"
	case now.Before(se.Open):
		return "p"
	case !now.After(se.Close):
		return "c"
	}
	return "a"
}

func (s *Service) onPacket(p kite.Packet, recv time.Time) {
	recv = recv.Add(clock.Offset()).In(clock.IST) // the dry run shifts every clock together
	se := s.refreshSession(recv)

	s.mu.Lock()
	in, ok := s.uni.Insts[p.Token]
	if !ok {
		s.mu.Unlock()
		return // a token from a previous universe still draining
	}
	ph := Phase(se, in.Exchange, recv)
	t := p.Tick(in.Contract, recv, ph)
	if ph != "x" {
		s.lastAny.Store(t.Recv)
	}
	s.last[p.Token] = t
	s.dirty[p.Token] = struct{}{}
	recentre := false
	if p.Index {
		for und, tok := range s.uni.Index {
			if tok == p.Token && t.LTP > 0 && (s.guessed[und] || s.uni.NeedsRecentre(und, t.LTP, s.Cfg.Ticker.RecentreSteps)) {
				s.guessed[und] = false
				recentre = true
			}
		}
	}
	s.mu.Unlock()

	if recentre {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	b, err := json.Marshal(t)
	if err == nil {
		err = s.Bus.NC.Publish(bus.TickPrefix+in.Key.Subject(), b)
	}
	if err != nil {
		s.PublishErrors.Add(1)
		return
	}
	s.Ticks.Add(1)
}

func (s *Service) onText(raw []byte) {
	var m kite.TextMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	switch m.Type {
	case "order":
		// Recorded only; the terminal never places, modifies or cancels orders.
		// Every socket receives the same update, so the payload hash is the
		// message id and the stream's duplicate window keeps one copy. verify: that
		// Kite sends byte-identical payloads on every socket.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.Bus.JS.Publish(ctx, bus.SubjOrder, m.Data, jetstream.WithMsgID(OrderMsgID(m.Data))); err != nil {
			s.PublishErrors.Add(1)
			slog.Warn("ticker: order update publish", "err", err)
		}
	case "error":
		slog.Warn("ticker: kite error message", "data", string(m.Data))
	default:
		slog.Info("ticker: kite message", "type", m.Type)
	}
}

// OrderMsgID identifies one order update across sockets: identical payloads
// share an id, and successive updates of an order differ in status or fills.
func OrderMsgID(data []byte) string {
	h := sha256.Sum256(bytes.TrimSpace(data))
	return "order-" + hex.EncodeToString(h[:16])
}

// pruneLast drops last values for contracts no longer listed (expired, or gone from
// a changed dump), so readers that seed from the bucket do not rebuild chains for
// them. The bucket is in memory with no TTL; nothing else removes them.
func (s *Service) pruneLast(ctx context.Context, insts []refdata.Inst) {
	if s.lkv == nil {
		return
	}
	live := make(map[string]bool, len(insts))
	for _, in := range insts {
		live[in.Key.Subject()] = true
	}
	keys, err := s.lkv.ListKeys(ctx)
	if err != nil {
		if !errors.Is(err, jetstream.ErrNoKeysFound) {
			slog.Warn("ticker: last-value prune", "err", err)
		}
		return
	}
	n := 0
	for k := range keys.Keys() {
		if !live[k] && s.lkv.Purge(ctx, k) == nil {
			n++
		}
	}
	if n > 0 {
		slog.Info("ticker: pruned last values", "keys", n)
	}
}

// flushLoop writes changed ticks to the last-value bucket asynchronously; the
// bucket is a cache, so a dropped write is repaired by the next tick.
func (s *Service) flushLoop(ctx context.Context, kv jetstream.KeyValue) {
	every := s.Cfg.Ticker.KVFlush
	tk := time.NewTicker(every)
	defer tk.Stop()
	prefix := "$KV." + kv.Bucket() + "."
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		s.mu.Lock()
		batch := make(map[string]bus.Tick, len(s.dirty))
		for tok := range s.dirty {
			if t, ok := s.last[tok]; ok {
				if in, ok := s.uni.Insts[tok]; ok {
					batch[in.Key.Subject()] = t
				}
			}
		}
		clear(s.dirty)
		s.mu.Unlock()
		for key, t := range batch {
			b, _ := json.Marshal(t)
			if _, err := s.Bus.JS.PublishAsync(prefix+key, b); err != nil {
				s.PublishErrors.Add(1)
			}
		}
		select {
		case <-s.Bus.JS.PublishAsyncComplete():
		case <-time.After(every * 5):
			slog.Warn("ticker: last-value flush slow", "pending", s.Bus.JS.PublishAsyncPending())
		case <-ctx.Done():
			return
		}
	}
}

// minuteLoop cuts one chain per recorded expiry at every IST minute boundary from
// the open to the close, inclusive.
func (s *Service) minuteLoop(ctx context.Context) {
	for {
		next := clock.Minute(clock.Now()).Add(time.Minute)
		if !clock.SleepUntil(ctx.Done(), next) {
			return
		}
		se := s.refreshSession(next)
		if !se.Trading() || next.Before(se.Open) || next.After(se.Close) {
			continue
		}
		s.mu.Lock()
		chains := Cut(s.uni, s.last, next, "ws")
		s.mu.Unlock()
		for _, c := range chains {
			s.publishChain(ctx, c)
		}
	}
}

func (s *Service) publishChain(ctx context.Context, c bus.Chain) {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := bus.PublishChain(pctx, s.Bus.JS, c); err != nil {
		s.PublishErrors.Add(1)
		slog.Warn("ticker: chain publish", "und", c.Und, "expiry", c.Expiry, "err", err)
		return
	}
	s.Chains.Add(1)
}

// Cut builds the snapshot for minute from the last tick of every subscribed
// option. Options that have not ticked today are left out, so a missing row means
// no data rather than a stale price.
func Cut(u *Universe, last map[uint32]bus.Tick, minute time.Time, source string) []bus.Chain {
	if u == nil {
		return nil
	}
	ms := bus.Ms(minute)
	dayStart := bus.Ms(clock.Date(minute))
	var out []bus.Chain
	for _, cs := range u.Chains {
		c := bus.Chain{TS: ms, Source: source, Und: cs.Und, Expiry: cs.Expiry.Format("2006-01-02")}
		if t, ok := last[u.Index[cs.Und]]; ok && t.Recv >= dayStart {
			c.Spot = t.LTP
		}
		if t, ok := last[cs.Fut]; ok && cs.Fut != 0 && t.Recv >= dayStart {
			c.Fut = t.LTP
		}
		for _, k := range cs.Strikes {
			for _, side := range [...]struct {
				typ  string
				toks map[float64]uint32
			}{{"CE", cs.CE}, {"PE", cs.PE}} {
				tok, ok := side.toks[k]
				if !ok {
					continue
				}
				t, ok := last[tok]
				if !ok || t.Recv < dayStart {
					continue
				}
				r := bus.ChainRow{
					C: t.C, T: tok, Strike: k, Type: side.typ,
					Bid: t.Bid, Ask: t.Ask, BidQ: t.BidQ, AskQ: t.AskQ,
					LTP: t.LTP, LTQ: t.LTQ, LTT: t.LTT, Exch: t.Exch,
					QuoteAge: max(0, ms-t.Recv), TradeAge: -1,
					OI: t.OI, Vol: t.Vol, Avg: t.Avg, TBQ: t.TBQ, TSQ: t.TSQ,
				}
				if t.LTT > 0 {
					r.TradeAge = max(0, ms-t.LTT)
				}
				c.Rows = append(c.Rows, r)
			}
		}
		if len(c.Rows) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// feedLoop classifies each underlying's feed every second from its heartbeat (the
// index and the nearest future), publishes the result on sys.ticker.feed, and alerts
// once on every change of the worst state during the session.
func (s *Service) feedLoop(ctx context.Context) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	th := Thresholds{Stale: s.Cfg.Ticker.StaleAfter, Halt: s.Cfg.Ticker.HaltAfter}
	prev := FeedClosed
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		now := clock.Now()
		phase := Phase(s.refreshSession(now), "NFO", now)
		st := s.feedStatus(now, phase, th)
		if b, err := json.Marshal(st); err == nil {
			s.Bus.NC.Publish(bus.SubjFeed, b)
		}
		if st.State != prev {
			s.alertFeed(ctx, prev, st)
			prev = st.State
		}
	}
}

func (s *Service) feedStatus(now time.Time, phase string, th Thresholds) FeedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	up := 0
	for _, w := range s.conns {
		if w.Up() {
			up++
		}
	}
	ex := Exchange{Last: bus.FromMs(s.lastAny.Load()), AllUp: up == len(s.conns)}
	st := FeedStatus{At: bus.Ms(now), Phase: phase, Sockets: fmt.Sprintf("%d/%d", up, len(s.conns)),
		LastAny: s.lastAny.Load(), Und: map[string]UndFeed{}}
	if s.uni == nil {
		st.State = worst(st.Und)
		s.feed = st
		return st
	}
	for und, idx := range s.uni.Index {
		fut := nearFut(s.uni, und)
		b := Beat{Index: s.recvOf(idx), Fut: s.recvOf(fut), SocketUp: s.connUp(idx) && (fut == 0 || s.connUp(fut))}
		f := UndFeed{State: Classify(now, phase, b, ex, th), IndexAge: ageMs(now, b.Index), FutAge: ageMs(now, b.Fut), Since: st.At}
		if old, ok := s.feed.Und[und]; ok && old.State == f.State {
			f.Since = old.Since
		}
		st.Und[und] = f
	}
	st.State = worst(st.Und)
	s.feed = st
	return st
}

// nearFut is the future of the earliest recorded expiry, 0 if none.
func nearFut(u *Universe, und string) uint32 {
	var best time.Time
	var tok uint32
	for _, c := range u.Chains {
		if c.Und == und && c.Fut != 0 && (tok == 0 || c.Expiry.Before(best)) {
			best, tok = c.Expiry, c.Fut
		}
	}
	return tok
}

func (s *Service) recvOf(tok uint32) time.Time {
	if tok == 0 {
		return time.Time{}
	}
	if t, ok := s.last[tok]; ok {
		return bus.FromMs(t.Recv)
	}
	return time.Time{}
}

// connUp reports whether the socket a token is assigned to is up.
func (s *Service) connUp(tok uint32) bool {
	i, ok := s.assign[tok]
	if !ok || i < 0 || i >= len(s.conns) {
		return false
	}
	return s.conns[i].Up()
}

func (s *Service) alertFeed(ctx context.Context, prev string, st FeedStatus) {
	var bad []string
	for und, f := range st.Und {
		if Worse(f.State, FeedOK) {
			bad = append(bad, strings.ToLower(und)+"="+f.State)
		}
	}
	sort.Strings(bad)
	switch {
	case Worse(st.State, FeedOK):
		s.Notify.Send(ctx, notify.Warn, "feed_"+st.State, "ticker feed %s: sockets %s, %s",
			st.State, st.Sockets, strings.Join(bad, " "))
	case Worse(prev, FeedOK):
		s.Notify.Send(ctx, notify.Info, "feed_recovered", "ticker feed recovered from %s: sockets %s", prev, st.Sockets)
	}
}

// healthLoop publishes sys.ticker.health every 5 s; ok follows the feed state.
func (s *Service) healthLoop(ctx context.Context) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	var prevTicks int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		now := clock.Now()
		ticks := s.Ticks.Load()
		rate := float64(ticks-prevTicks) / 5
		prevTicks = ticks

		s.mu.Lock()
		feed := s.feed
		tokens, trimmed := 0, 0
		if s.uni != nil {
			tokens, trimmed = s.uni.Count(), s.uni.Trimmed
		}
		s.mu.Unlock()

		h := bus.Health{Service: "ticker", At: bus.Ms(now), OK: !Worse(feed.State, FeedOK), Detail: map[string]string{
			"phase":   feed.Phase,
			"feed":    feed.State,
			"sockets": feed.Sockets,
			"tokens":  fmt.Sprint(tokens),
			"trimmed": fmt.Sprint(trimmed),
			"ticks_s": fmt.Sprintf("%.0f", rate),
			"chains":  fmt.Sprint(s.Chains.Load()),
			"errors":  fmt.Sprint(s.PublishErrors.Load()),
		}}
		for und, f := range feed.Und {
			k := strings.ToLower(und)
			h.Detail["feed."+k] = f.State
			h.Detail["index_age."+k] = fmtAge(f.IndexAge)
		}
		if b, err := json.Marshal(h); err == nil {
			s.Bus.NC.Publish(bus.HealthSubject("ticker"), b)
		}
	}
}

func fmtAge(ms int64) string {
	if ms < 0 {
		return "never"
	}
	return (time.Duration(ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}
