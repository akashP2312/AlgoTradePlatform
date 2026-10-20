// Package config loads config/pmt.yaml. ${VAR} references are expanded from the
// environment before parsing; secrets are never stored in the file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
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
