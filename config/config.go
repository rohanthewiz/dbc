package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rohanthewiz/serr"
)

const (
	defaultMaxRows = 1000

	// DefaultAIContextRows mirrors ai.DefaultContextRows. It is repeated
	// rather than imported so config stays a leaf package that every other
	// one can depend on.
	DefaultAIContextRows = 10

	// defaultMaxDisplayRows sits above defaultMaxRows on purpose: at the stock
	// max_rows the display cap never bites, and it only starts doing anything
	// once someone raises max_rows for an export.
	defaultMaxDisplayRows = 2000
)

// Names of the built-in demo connections. Both are registered when no config
// file is found, so the two embedded engines can be queried side by side
// without writing a config first.
const (
	// DemoBytdb is the bytdb-backed demo, and the one active by default.
	DemoBytdb = "demo-bytdb"

	// DemoSQLite keeps the name the demo has always had, so the shipped
	// scripts and any muscle memory that says s.Query("demo", …) still land
	// on the same connection they used to.
	DemoSQLite = "demo"
)

// DemoEngine names which built-in demo connection starts out active. Both
// connections exist either way — this only picks the default.
type DemoEngine string

const (
	DemoDefault      DemoEngine = "" // same as DemoEngineBytdb
	DemoEngineBytdb  DemoEngine = "bytdb"
	DemoEngineSQLite DemoEngine = "sqlite"
)

// ParseDemoEngine resolves a -demo flag or $DBC_DEMO value. An empty string is
// the default (bytdb); anything unrecognized is an error rather than a silent
// fallback, because silently ignoring it would leave the user staring at the
// engine they just asked not to use.
func ParseDemoEngine(s string) (DemoEngine, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DemoDefault, nil
	case "bytdb":
		return DemoEngineBytdb, nil
	case "sqlite", "sqlite3":
		return DemoEngineSQLite, nil
	}
	return DemoDefault, serr.New("unknown demo engine (use bytdb or sqlite)", "demo", s)
}

// Connection describes one database host connection.
type Connection struct {
	Name   string `toml:"name"`
	Driver string `toml:"driver"` // postgres | mysql | sqlite | bytdb
	DSN    string `toml:"dsn"`    // env vars are expanded, e.g. ${PGPASS}

	// Migrations is the directory `dbc migrate` reads for this connection,
	// relative to the working directory. Per connection rather than global
	// because a config typically lists several databases and each has its
	// own schema. Empty means the -dir flag (or ".") decides.
	Migrations string `toml:"migrations"`

	// AIRows lets the AI assistant see this connection's result ROWS (up to
	// ai_context_rows of them). Off by default and per connection, because
	// rows are the database's contents and sending them to a hosted model is
	// a decision about that data: fine for a scratch database, not
	// something a production connection should do because a global switch
	// was flipped. The query text and errors go regardless — see package ai.
	AIRows bool `toml:"ai_rows"`
}

// Config is the application configuration.
type Config struct {
	ScriptsDir string `toml:"scripts_dir"`
	MaxRows    int    `toml:"max_rows"` // rows fetched from the server

	// MaxDisplayRows caps how many of those rows the TUI table renders. It is
	// separate from MaxRows because the two are limited by different things:
	// fetching more is a question of memory and patience, rendering more costs
	// a widget per value and shows up as a sluggish scroll. Raising max_rows
	// for the sake of an export should not make the table crawl. Zero means no
	// display cap.
	MaxDisplayRows int `toml:"max_display_rows"`

	// The AI assistant (package ai). AIAgent picks the ACP backend —
	// "copilot" (the default), "claude" or "gemini"; AIModel is a preferred
	// model id, applied when the agent offers it. AIContextRows caps how many
	// result rows go with a question on connections that allow rows at all
	// (ai_rows); 0 sends none, and unset means ai.DefaultContextRows.
	AIAgent       string `toml:"ai_agent"`
	AIModel       string `toml:"ai_model"`
	AIContextRows int    `toml:"ai_context_rows"`

	DefaultConnection string       `toml:"default_connection"`
	Connections       []Connection `toml:"connection"`

	Path string `toml:"-"` // file the config was loaded from ("" if none)
	Demo bool   `toml:"-"` // true when running with the built-in demo connection

	// Warnings are load-time findings worth telling the user about but not
	// worth refusing to start over, e.g. a DSN referencing an unset env var.
	Warnings []string `toml:"-"`
}

// Load reads the config from an explicit path, or searches ./dbc.toml then
// ~/.config/dbc/config.toml. With no config anywhere, it falls back to the
// built-in demo connections so the app is usable immediately.
func Load(explicit string) (*Config, error) {
	return LoadDemo(explicit, DemoDefault)
}

// LoadDemo is Load with a say in which built-in demo connection starts out
// active. The choice only matters when there is no config file: a real config
// names its own default_connection.
func LoadDemo(explicit string, demo DemoEngine) (*Config, error) {
	cfg := &Config{ScriptsDir: "scripts", MaxRows: defaultMaxRows,
		MaxDisplayRows: defaultMaxDisplayRows, AIContextRows: DefaultAIContextRows}

	path := explicit
	if path == "" {
		for _, p := range searchPaths() {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}

	if path == "" {
		cfg.demoFallback(demo)
		return cfg, nil
	}

	// The defaults above survive decoding for keys the file leaves out, which
	// is what makes an explicit ai_context_rows = 0 ("send no rows")
	// distinguishable from an absent key without a pointer field.
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, serr.Wrap(err, "config_path", path)
	}
	cfg.Path = path
	if cfg.AIContextRows < 0 {
		cfg.AIContextRows = DefaultAIContextRows
	}

	if cfg.MaxRows <= 0 {
		cfg.MaxRows = defaultMaxRows
	}
	if cfg.MaxDisplayRows < 0 {
		cfg.MaxDisplayRows = defaultMaxDisplayRows
	}
	if cfg.ScriptsDir == "" {
		cfg.ScriptsDir = "scripts"
	}
	if len(cfg.Connections) == 0 {
		return nil, serr.New("config has no [[connection]] entries", "config_path", path)
	}
	seen := make(map[string]bool, len(cfg.Connections))
	for i := range cfg.Connections {
		cn := &cfg.Connections[i]
		if seen[cn.Name] {
			return nil, serr.New("duplicate connection name — ConnByName would always pick the first",
				"name", cn.Name, "config_path", path)
		}
		seen[cn.Name] = true
		// expand env vars ourselves rather than via os.ExpandEnv, so a typo'd
		// ${PGPASS} warns by name instead of silently becoming "" and
		// surfacing later as a baffling auth failure
		cn.DSN = os.Expand(cn.DSN, func(key string) string {
			v, ok := os.LookupEnv(key)
			if !ok {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"connection %q: DSN references unset env var $%s (expanded to empty)",
					cn.Name, key))
			}
			return v
		})
	}
	if cfg.DefaultConnection != "" && !seen[cfg.DefaultConnection] {
		return nil, serr.New("default_connection names no configured connection",
			"default_connection", cfg.DefaultConnection, "config_path", path)
	}
	return cfg, nil
}

// demoFallback fills cfg with the built-in demo connections, used when there
// is no config file anywhere.
//
// Both embedded engines are registered so they can be compared side by side —
// the same seed data lands in each — and `demo` keeps its historical name and
// DSN so the shipped scripts still work. Only which one starts out active
// changes with the demo argument.
//
// bytdb has no in-memory mode, so unlike the SQLite demo it needs a real file.
// It goes in the OS cache directory: somewhere the OS already understands as
// disposable, out of the user's working directory, and persistent enough that
// edits made in the demo survive a restart. If that directory cannot be
// prepared the bytdb demo is dropped with a warning rather than failing the
// launch — a broken cache dir should not stop the app from starting.
func (c *Config) demoFallback(demo DemoEngine) {
	c.Demo = true

	sqliteConn := Connection{
		Name: DemoSQLite, Driver: "sqlite",
		DSN: "file:dbcdemo?mode=memory&cache=shared",
	}

	path, err := DemoBytdbPath()
	if err != nil {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"could not prepare the bytdb demo file (%v) — starting with the sqlite demo only", err))
		c.Connections = []Connection{sqliteConn}
		c.DefaultConnection = DemoSQLite
		return
	}
	bytdbConn := Connection{Name: DemoBytdb, Driver: "bytdb", DSN: path}

	// active connection first: several call sites treat Connections[0] as the
	// stand-in when a default cannot be resolved, and the TUI lists them in
	// this order
	if demo == DemoEngineSQLite {
		c.Connections = []Connection{sqliteConn, bytdbConn}
		c.DefaultConnection = DemoSQLite
		return
	}
	c.Connections = []Connection{bytdbConn, sqliteConn}
	c.DefaultConnection = DemoBytdb
}

// DemoBytdbPath returns the file backing the built-in bytdb demo, creating its
// parent directory. It prefers the OS cache directory and falls back to the
// temp directory on systems where the cache directory is undefined (a bare
// container with no $HOME, say).
func DemoBytdbPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "dbc")
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", serr.Wrap(err, "dir", dir)
	}
	return filepath.Join(dir, "demo.bytdb"), nil
}

// ConnByName finds a connection config by name.
func (c *Config) ConnByName(name string) (Connection, bool) {
	for _, cn := range c.Connections {
		if cn.Name == name {
			return cn, true
		}
	}
	return Connection{}, false
}

func searchPaths() []string {
	paths := []string{"dbc.toml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "dbc", "config.toml"))
	}
	return paths
}
