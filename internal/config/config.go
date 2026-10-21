// Package config loads config/pmt.yaml. ${VAR} references are expanded from the
// environment before parsing; secrets are never stored in the file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"pmt/internal/clock"
)

type Config struct {
	NATSURL     string `yaml:"nats_url"`
	DatabaseURL string `yaml:"database_url"`
	DataDir     string `yaml:"data_dir"`

	Kite      Kite       `yaml:"kite"`
	Universe  Universe   `yaml:"universe"`
	Ticker    Ticker     `yaml:"ticker"`
	Recorder  Recorder   `yaml:"recorder"`
	Sweep     Sweep      `yaml:"sweep"`
	Harvest   Harvest    `yaml:"harvest"`
	Backup    Backup     `yaml:"backup"`
	BackupRec BackupRec  `yaml:"backuprec"`
	Loaders   Loaders    `yaml:"loaders"`
	Sys       Sys        `yaml:"sys"`
	Gateway   Gateway    `yaml:"gateway"`
	Risk      Risk       `yaml:"risk"`
	Bot       Bot        `yaml:"bot"`
	News      News       `yaml:"news"`
	Schedule  []Schedule `yaml:"schedule"`

	// Dir is the directory relative paths in the file resolve against (the repo root
	// in development, /app in the container).
	Dir string `yaml:"-"`
}

type Kite struct {
	RootURL          string `yaml:"root_url"`
	WSURL            string `yaml:"ws_url"`
	WSConnections    int    `yaml:"ws_connections"`
	TokensPerConn    int    `yaml:"tokens_per_conn"`
	QuotePerSec      int    `yaml:"quote_per_sec"`
	HistoricalPerSec int    `yaml:"historical_per_sec"`
	OtherPerSec      int    `yaml:"other_per_sec"`

	// From the environment only.
	APIKey    string `yaml:"-"`
	APISecret string `yaml:"-"`
}

type Underlying struct {
	Name          string `yaml:"name"`
	Index         string `yaml:"index"`
	DerivExchange string `yaml:"deriv_exchange"`
	Futures       int    `yaml:"futures"`
	WideExpiries  int    `yaml:"wide_expiries"`
	Monthlies     int    `yaml:"monthlies"` // the nearest n monthly expiries are streamed even past wide_expiries
	WideStrikes   int    `yaml:"wide_strikes"`
	CoreExpiries  int    `yaml:"core_expiries"`
	CoreStrikes   int    `yaml:"core_strikes"`
	Backup        bool   `yaml:"backup"`
	Sweep         bool   `yaml:"sweep"`
	Harvest       bool   `yaml:"harvest"`
}

type Universe struct {
	Underlyings  []Underlying `yaml:"underlyings"`
	Extras       []string     `yaml:"extras"`
	Constituents []string     `yaml:"constituents"`
}

type Ticker struct {
	RecentreSteps    int           `yaml:"recentre_steps"`
	StaleAfter       time.Duration `yaml:"stale_after"`
	HaltAfter        time.Duration `yaml:"halt_after"`
	SilenceReconnect time.Duration `yaml:"silence_reconnect"`
	KVFlush          time.Duration `yaml:"kv_flush"`
}

type Recorder struct {
	BatchRows  int           `yaml:"batch_rows"`
	FlushEvery time.Duration `yaml:"flush_every"`
}

type Sweep struct {
	At          string   `yaml:"at"`
	Underlyings []string `yaml:"underlyings"`
}

type Harvest struct {
	LookbackDays int `yaml:"lookback_days"`
	WindowDays   int `yaml:"window_days"`
}

type Backup struct {
	Outbox        string `yaml:"outbox"`
	RecipientEnv  string `yaml:"recipient_env"`
	KeepLocalDays int    `yaml:"keep_local_days"`
}

type BackupRec struct {
	OutDir     string `yaml:"out_dir"`
	PollSecond int    `yaml:"poll_second"`
	TokenFile  string `yaml:"token_file"`
}

type Loaders struct {
	UserAgent    string `yaml:"user_agent"`
	NSEArchive   string `yaml:"nse_archive"`
	NiftyIndices string `yaml:"niftyindices"`
}

type Sys struct {
	Listen string `yaml:"listen"`
}

// Gateway is the browser WebSocket service.
type Gateway struct {
	Listen string        `yaml:"listen"`
	Flush  time.Duration `yaml:"flush"`  // per-tab conflation window
	Static string        `yaml:"static"` // built web app served at /, when present
}

// Risk is the part of the risk: section the Go side reads; the riskengine (Python)
// reads the rest.
type Risk struct {
	Ref    string     `yaml:"ref"`
	Limits RiskLimits `yaml:"limits"`
}

// RiskLimits drives the riskmon alerts. Rupee figures are absolute; Capital is the
// base for WorstPct.
type RiskLimits struct {
	Capital    float64       `yaml:"capital"`
	LossLimit  float64       `yaml:"loss_limit"`
	LossWarn   float64       `yaml:"loss_warn"`
	WorstPct   float64       `yaml:"worst_pct"`
	WorstMove  float64       `yaml:"worst_move"` // only grid cells within this % spot move count
	MarginWarn float64       `yaml:"margin_warn"`
	MarginCrit float64       `yaml:"margin_crit"`
	DeltaBand  float64       `yaml:"delta_band"`
	Drawdown   float64       `yaml:"drawdown"`
	Stale      time.Duration `yaml:"stale"`
	FlagAfter  time.Duration `yaml:"flag_after"`
}

// Bot is the algo-trading bot botmon watches, read from its own SQLite ledger. botmon
// opens the ledger read-only and never writes to it or to the bot's orders.
type Bot struct {
	Name  string        `yaml:"name"`   // bot.<name>.state
	Dir   string        `yaml:"dir"`    // the bot's data directory; empty disables botmon
	Mode  string        `yaml:"mode"`   // paper | live: picks ledger_<mode>.db; only live is compared with Kite
	Poll  time.Duration `yaml:"poll"`   // how often the ledger is read
	RunAt string        `yaml:"run_at"` // when the bot's daily run starts, IST
	Grace time.Duration `yaml:"grace"`  // how long after run_at today's run may take
}

// Ledger is the bot's ledger file.
func (b Bot) Ledger() string { return filepath.Join(b.Dir, "ledger_"+b.Mode+".db") }

// News drives newsmon's risk filter: the scheduled-event calendar and the windows
// around each event in which the risk state reads CAUTION or NO_NEW_ENTRIES. It
// is a filter on new entries, never a trade signal, and newsmon places no orders.
type News struct {
	Events   string        `yaml:"events"`    // the editable event file, reloaded when it changes
	Expiries []string      `yaml:"expiries"`  // underlyings whose listed F&O expiries become events
	ExpiryAt string        `yaml:"expiry_at"` // the expiry event's time, IST
	Horizon  time.Duration `yaml:"horizon"`   // how far ahead news.events lists
	Publish  time.Duration `yaml:"publish"`   // news.risk is republished at least this often
	// Windows by impact (HIGH, MEDIUM, LOW); an impact with no entry has none.
	Windows map[string]Window `yaml:"windows"`
	// Categories replace the impact's window for every event in that category.
	Categories map[string]Window `yaml:"categories"`
	Feeds      Feeds             `yaml:"feeds"`
}

// Feeds is newsmon's headline ingestion: each source is polled on its own, with
// conditional GETs, backoff and a circuit breaker, and only where robots.txt
// allows it. Headlines are normalized and deduplicated into news.feed.
type Feeds struct {
	Poll       time.Duration `yaml:"poll"`        // how often each source is polled
	Timeout    time.Duration `yaml:"timeout"`     // per request
	UserAgent  string        `yaml:"user_agent"`  // sent as is; its first word is the robots.txt product token
	Keep       int           `yaml:"keep"`        // headlines news.feed carries
	DedupeTTL  time.Duration `yaml:"dedupe_ttl"`  // how long a headline suppresses its duplicates
	MaxAge     time.Duration `yaml:"max_age"`     // older headlines are ignored
	Similar    float64       `yaml:"similar"`     // title word overlap (Jaccard) at which two sources' headlines are one story
	Fails      int           `yaml:"fails"`       // consecutive failures that open a source's breaker
	Cool       time.Duration `yaml:"cool"`        // how long an open breaker waits before one trial poll
	BackoffMax time.Duration `yaml:"backoff_max"` // the longest wait between retries before the breaker opens
	Robots     time.Duration `yaml:"robots"`      // how often robots.txt is reread
	Sources    []Source      `yaml:"sources"`
}

// Source is one headline feed.
type Source struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Kind    string `yaml:"kind"`    // rss (RSS 2.0, RSS 1.0 or Atom), or paid (a stub, never polled)
	Trust   string `yaml:"trust"`   // official, exchange or media
	Region  string `yaml:"region"`  // IN, US, ...
	Enabled bool   `yaml:"enabled"` // a disabled source is listed but never polled
	TZ      string `yaml:"tz"`      // for timestamps without a zone; default Asia/Kolkata
	// TitleDesc appends the description to the title, for feeds whose title alone
	// says little (NSE's is the company name).
	TitleDesc bool   `yaml:"title_desc"`
	Note      string `yaml:"note"`
}

// Window is the span around an event's time: NO_NEW_ENTRIES from T-NoEntryBefore
// to T+NoEntryAfter, CAUTION from T-CautionBefore to T+CautionAfter.
type Window struct {
	NoEntryBefore time.Duration `yaml:"no_entry_before"`
	NoEntryAfter  time.Duration `yaml:"no_entry_after"`
	CautionBefore time.Duration `yaml:"caution_before"`
	CautionAfter  time.Duration `yaml:"caution_after"`
}

func (w Window) check() error {
	if w.NoEntryBefore < 0 || w.NoEntryAfter < 0 || w.CautionBefore < 0 || w.CautionAfter < 0 {
		return fmt.Errorf("durations must not be negative")
	}
	return nil
}

type Schedule struct {
	At             string `yaml:"at"`
	Job            string `yaml:"job"`
	Args           string `yaml:"args"`
	TradingDayOnly bool   `yaml:"trading_day_only"`
	// CatchUp runs the job when the scheduler starts if today's time has passed
	// without a successful run (for the morning reference-data jobs).
	CatchUp bool `yaml:"catch_up"`
}

// Path returns the config file to use: $PMT_CONFIG, else config/pmt.yaml.
func Path() string {
	if p := os.Getenv("PMT_CONFIG"); p != "" {
		return p
	}
	return "config/pmt.yaml"
}

// Load reads, expands and validates the config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw, filepath.Dir(filepath.Dir(absOr(path))))
}

func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Parse expands ${VAR} in raw and decodes it. dir is the repo root that relative
// paths resolve against.
func Parse(raw []byte, dir string) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.Dir = dir
	c.Kite.APIKey = os.Getenv("KITE_API_KEY")
	c.Kite.APISecret = os.Getenv("KITE_API_SECRET")
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	defI := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	defD := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.NATSURL, "nats://127.0.0.1:4222")
	def(&c.DataDir, filepath.Join(c.Dir, "data"))
	def(&c.Kite.RootURL, "https://api.kite.trade")
	def(&c.Kite.WSURL, "wss://ws.kite.trade")
	defI(&c.Kite.WSConnections, 2)
	defI(&c.Kite.TokensPerConn, 3000)
	defI(&c.Kite.QuotePerSec, 1)
	defI(&c.Kite.HistoricalPerSec, 3)
	defI(&c.Kite.OtherPerSec, 10)
	defI(&c.Ticker.RecentreSteps, 10)
	defD(&c.Ticker.StaleAfter, 5*time.Second)
	defD(&c.Ticker.HaltAfter, 30*time.Second)
	defD(&c.Ticker.SilenceReconnect, 10*time.Second)
	defF := func(f *float64, v float64) {
		if *f == 0 {
			*f = v
		}
	}
	def(&c.Risk.Ref, "NIFTY")
	defF(&c.Risk.Limits.LossWarn, 0.75)
	defF(&c.Risk.Limits.WorstMove, 5)
	defF(&c.Risk.Limits.MarginWarn, 0.8)
	defF(&c.Risk.Limits.MarginCrit, 0.9)
	defD(&c.Risk.Limits.Stale, 30*time.Second)
	defD(&c.Risk.Limits.FlagAfter, 2*time.Minute)
	def(&c.Bot.Name, "rotation")
	def(&c.Bot.Mode, "paper")
	defD(&c.Bot.Poll, time.Minute)
	def(&c.Bot.RunAt, "18:05:00")
	defD(&c.Bot.Grace, 30*time.Minute)
	if c.Bot.Dir != "" && !filepath.IsAbs(c.Bot.Dir) {
		c.Bot.Dir = filepath.Join(c.Dir, c.Bot.Dir)
	}
	def(&c.News.Events, "config/news/events.yaml")
	if !filepath.IsAbs(c.News.Events) {
		c.News.Events = filepath.Join(c.Dir, c.News.Events)
	}
	if c.News.Expiries == nil {
		c.News.Expiries = []string{"NIFTY", "BANKNIFTY"}
	}
	def(&c.News.ExpiryAt, "15:30:00")
	defD(&c.News.Horizon, 92*24*time.Hour)
	defD(&c.News.Publish, 15*time.Second)
	if c.News.Windows == nil {
		c.News.Windows = map[string]Window{"HIGH": {
			NoEntryBefore: 15 * time.Minute, NoEntryAfter: 15 * time.Minute,
			CautionBefore: 30 * time.Minute, CautionAfter: 45 * time.Minute,
		}}
	}
	f := &c.News.Feeds
	defD(&f.Poll, time.Minute)
	defD(&f.Timeout, 15*time.Second)
	def(&f.UserAgent, "pmt-news/0.1 (+personal RSS reader; polls every 60s)")
	defI(&f.Keep, 200)
	defD(&f.DedupeTTL, 24*time.Hour)
	defD(&f.MaxAge, 48*time.Hour)
	defF(&f.Similar, 0.8)
	defI(&f.Fails, 5)
	defD(&f.Cool, 15*time.Minute)
	defD(&f.BackoffMax, 10*time.Minute)
	defD(&f.Robots, 24*time.Hour)
	for i := range f.Sources {
		def(&f.Sources[i].Kind, "rss")
		def(&f.Sources[i].TZ, "Asia/Kolkata")
	}
	defD(&c.Ticker.KVFlush, time.Second)
	defI(&c.Recorder.BatchRows, 5000)
	defD(&c.Recorder.FlushEvery, time.Second)
	def(&c.Sweep.At, "15:25:00")
	defI(&c.Harvest.LookbackDays, 180)
	defI(&c.Harvest.WindowDays, 60)
	def(&c.Backup.RecipientEnv, "PMT_BACKUP_AGE_RECIPIENT")
	defI(&c.Backup.KeepLocalDays, 14)
	defI(&c.BackupRec.PollSecond, 20)
	def(&c.Sys.Listen, "127.0.0.1:8090")
	def(&c.Gateway.Listen, "127.0.0.1:8091")
	defD(&c.Gateway.Flush, 250*time.Millisecond)
	def(&c.Gateway.Static, "web/dist")
	// "${PMT_DATA_DIR}/x" with PMT_DATA_DIR unset expands to "/x"; re-root those.
	reroot := func(s *string, sub string) {
		if *s == "" || *s == "/"+sub {
			*s = filepath.Join(c.DataDir, sub)
		}
	}
	reroot(&c.Backup.Outbox, "backup/outbox")
	reroot(&c.BackupRec.OutDir, "backuprec")
	for i := range c.Universe.Underlyings {
		u := &c.Universe.Underlyings[i]
		def(&u.DerivExchange, "NFO")
	}
}

func (c *Config) validate() error {
	if c.Kite.WSConnections < 1 || c.Kite.WSConnections > 2 {
		return fmt.Errorf("config: kite.ws_connections must be 1 or 2 (the bot keeps the third)")
	}
	if len(c.Universe.Underlyings) == 0 {
		return fmt.Errorf("config: universe.underlyings is empty")
	}
	seen := map[string]bool{}
	for _, u := range c.Universe.Underlyings {
		if u.Name == "" || u.Index == "" {
			return fmt.Errorf("config: underlying needs name and index")
		}
		if seen[u.Name] {
			return fmt.Errorf("config: underlying %s listed twice", u.Name)
		}
		seen[u.Name] = true
		if u.CoreStrikes > u.WideStrikes || u.CoreExpiries > u.WideExpiries {
			// The core window must sit inside the wide one so ws snapshots cover it.
			if u.WideStrikes != 0 {
				return fmt.Errorf("config: %s core window exceeds the wide window", u.Name)
			}
		}
	}
	l := c.Risk.Limits
	if l.Capital < 0 || l.LossLimit < 0 || l.WorstPct < 0 || l.DeltaBand < 0 || l.Drawdown < 0 {
		return fmt.Errorf("config: risk.limits values must not be negative")
	}
	if l.LossWarn <= 0 || l.LossWarn > 1 {
		return fmt.Errorf("config: risk.limits.loss_warn must be in (0, 1]")
	}
	if l.MarginWarn <= 0 || l.MarginWarn > l.MarginCrit || l.MarginCrit > 1 {
		return fmt.Errorf("config: risk.limits needs 0 < margin_warn <= margin_crit <= 1")
	}
	if b := c.Bot; b.Mode != "paper" && b.Mode != "live" {
		return fmt.Errorf("config: bot.mode must be paper or live")
	} else if _, _, _, err := clock.ParseHMS(b.RunAt); err != nil {
		return fmt.Errorf("config: bot.run_at: %w", err)
	} else if strings.ContainsAny(b.Name, ".*> ") || b.Name == "" {
		return fmt.Errorf("config: bot.name must be one subject token")
	}
	if _, _, _, err := clock.ParseHMS(c.News.ExpiryAt); err != nil {
		return fmt.Errorf("config: news.expiry_at: %w", err)
	}
	for k, w := range c.News.Windows {
		if k != "HIGH" && k != "MEDIUM" && k != "LOW" {
			return fmt.Errorf("config: news.windows: %q is not HIGH, MEDIUM or LOW", k)
		}
		if err := w.check(); err != nil {
			return fmt.Errorf("config: news.windows.%s: %w", k, err)
		}
	}
	for k, w := range c.News.Categories {
		if err := w.check(); err != nil {
			return fmt.Errorf("config: news.categories.%s: %w", k, err)
		}
	}
	if err := c.News.Feeds.check(); err != nil {
		return fmt.Errorf("config: news.feeds: %w", err)
	}
	for _, s := range c.Schedule {
		if s.Job == "" || s.At == "" {
			return fmt.Errorf("config: schedule entry needs at and job")
		}
	}
	return nil
}

// Underlying returns the named underlying's settings.
func (c *Config) Underlying(name string) (Underlying, bool) {
	for _, u := range c.Universe.Underlyings {
		if u.Name == name {
			return u, true
		}
	}
	return Underlying{}, false
}

// Abs resolves a config-relative path.
func (c *Config) Abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

var (
	sourceKinds  = map[string]bool{"rss": true, "paid": true}
	sourceTrusts = map[string]bool{"official": true, "exchange": true, "media": true}
)

func (f Feeds) check() error {
	if f.Poll < 10*time.Second {
		return fmt.Errorf("poll must be at least 10s")
	}
	if f.Similar <= 0 || f.Similar > 1 {
		return fmt.Errorf("similar must be in (0, 1]")
	}
	if f.Fails < 1 || f.Keep < 1 {
		return fmt.Errorf("fails and keep must be at least 1")
	}
	if strings.TrimSpace(f.UserAgent) == "" {
		return fmt.Errorf("user_agent is required")
	}
	seen := map[string]bool{}
	for i, s := range f.Sources {
		switch {
		case s.ID == "" || strings.ContainsAny(s.ID, ".*> /"):
			return fmt.Errorf("sources[%d]: id must be one token", i)
		case seen[s.ID]:
			return fmt.Errorf("sources: duplicate id %q", s.ID)
		case !sourceKinds[s.Kind]:
			return fmt.Errorf("sources %s: kind %q is not rss or paid", s.ID, s.Kind)
		case !sourceTrusts[s.Trust]:
			return fmt.Errorf("sources %s: trust %q is not official, exchange or media", s.ID, s.Trust)
		case s.Kind == "rss" && !strings.HasPrefix(s.URL, "https://") && !strings.HasPrefix(s.URL, "http://"):
			return fmt.Errorf("sources %s: url must be http(s)", s.ID)
		}
		if _, err := time.LoadLocation(s.TZ); err != nil {
			return fmt.Errorf("sources %s: tz: %w", s.ID, err)
		}
		seen[s.ID] = true
	}
	return nil
}
