package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

	// DefaultConnIdleTimeout is how long a pooled connection may sit unused
	// before the pool closes it (conn_idle_timeout). database/sql's own
	// default is forever, which keeps server-side backends — their memory and
	// a slot in the server's max_connections — alive for hours after the
	// last query. An hour is long enough that interactive use (run, read,
	// think, run again) never pays a reconnect, and short enough that a dbc
	// left open overnight gives its connections back. It is a ceiling, not a
	// keepalive: a server or proxy that cuts idle connections sooner still
	// does, and the drivers' liveness checks on checkout replace those
	// transparently (for Postgres, with db.pgShouldPing's help: pgx alone
	// skips the check within a second of the last checkout). The editor's
	// pinned session is checked out, never idle,
	// so this never expires it.
	DefaultConnIdleTimeout = time.Hour

	// DefaultConnectTimeout bounds opening a connection (connect_timeout):
	// the dial, the handshake and the first ping together. Without it an
	// unreachable host fails only at the OS's TCP connect timeout — over a
	// minute — while the UI shows "connecting…". Five seconds is ample for
	// any reachable server, including one across a VPN, and short enough that
	// a wrong host or a down VPN is reported while the user is still looking.
	DefaultConnectTimeout = 5 * time.Second
)

// Names of the built-in demo connections. Both are registered when no config
// file is found, so the two embedded engines can be queried side by side
// without writing a config first.
const (
	// DemoBytdb is the bytdb-backed demo, and the one active by default.
	DemoBytdb = "demo-bytdb"

	// DemoSQLite is the in-memory SQLite demo. It was plain "demo" before the
	// bytdb demo arrived; the engine suffix now names both the same way. The
	// shipped scripts name it directly — their ? placeholders are SQLite's, so
	// they could not run on the bytdb demo ($1) under any name.
	DemoSQLite = "demo-sqlite"
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

	// Demo marks a built-in demo connection, the only kind db.Manager seeds
	// with the demo cats table when it opens it. Set by demoFallback alone,
	// never read from a file: seeding runs CREATE TABLE and DELETE FROM cats,
	// so it must not be possible to switch on for a real database. It is a
	// per-connection flag rather than Config.Demo because a demo config can
	// also carry the ad-hoc --dsn connection, which is the user's database.
	Demo bool `toml:"-"`

	// Web marks a connection added in dbc web's browser UI and kept in its
	// store (web.bytdb), not in this file. Never read from a file — the
	// store sets it when it merges its connections in — so dbc web can tell
	// which ones it may remove: a file's connections are the file's to
	// change.
	Web bool `toml:"-"`
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

	// ConnIdleTimeout is how long a pooled connection may sit idle before it
	// is closed; see DefaultConnIdleTimeout. Written as a duration string in
	// the file ("30m", "2h"); 0 keeps idle connections open indefinitely, as
	// database/sql does by default. It applies to every connection — it is a
	// question of how long dbc sits idle, not of which database it talks to.
	ConnIdleTimeout time.Duration `toml:"conn_idle_timeout"`

	// ConnectTimeout caps opening a connection; see DefaultConnectTimeout.
	// A duration string in the file ("10s"); 0 sets no limit of dbc's own,
	// leaving it to the driver (a DSN's own connect_timeout, say) and the OS.
	// A DSN-level timeout still applies either way — whichever is shorter
	// wins. Global for the same reason as ConnIdleTimeout.
	ConnectTimeout time.Duration `toml:"connect_timeout"`

	DefaultConnection string `toml:"default_connection"`

	// Connections is written freely while the config is being built (Load,
	// the demo pruning, an ad-hoc --dsn), when nothing else can see it. Once
	// something may run beside a change — dbc web adding a connection while
	// its tabs connect and run — the change goes through AddConn/RemoveConn
	// and every reader through Conns/ConnByName, which take connMu.
	//
	// The writers are copy-on-write: AddConn and RemoveConn build a new
	// slice rather than edit the old one in place, so a slice a reader got
	// from Conns (or a range over the field that began before the change)
	// never sees an element change under it.
	Connections []Connection `toml:"connection"`
	connMu      sync.RWMutex

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
		MaxDisplayRows: defaultMaxDisplayRows, AIContextRows: DefaultAIContextRows,
		ConnIdleTimeout: DefaultConnIdleTimeout, ConnectTimeout: DefaultConnectTimeout}

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
	cfg.checkDuration("conn_idle_timeout", &cfg.ConnIdleTimeout, DefaultConnIdleTimeout)
	cfg.checkDuration("connect_timeout", &cfg.ConnectTimeout, DefaultConnectTimeout)
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
		var warns []string
		cn.DSN, warns = ExpandDSN(cn.Name, cn.DSN)
		cfg.Warnings = append(cfg.Warnings, warns...)
	}
	if cfg.DefaultConnection != "" && !seen[cfg.DefaultConnection] {
		return nil, serr.New("default_connection names no configured connection",
			"default_connection", cfg.DefaultConnection, "config_path", path)
	}
	return cfg, nil
}

// checkDuration falls back to def, with a warning, for a duration setting
// that cannot be what was meant. key is the TOML key, for the message.
//
// The TOML decoder reads a duration string with time.ParseDuration (a typo
// there fails the load outright), but it reads a bare integer as nanoseconds:
// conn_idle_timeout = 3600 is 3.6 microseconds, which would close every
// connection the moment it went idle; connect_timeout = 5 is 5ns, which would
// fail every connect. Neither setting has a sensible sub-second value, so
// anything that short is taken as that mistake. A negative value is likewise
// not a timeout at all. 0 is left alone: it is the documented "no limit".
func (c *Config) checkDuration(key string, d *time.Duration, def time.Duration) {
	if *d == 0 || *d >= time.Second {
		return
	}
	c.Warnings = append(c.Warnings, fmt.Sprintf(
		"%s = %s is not a usable timeout (write a duration string such as \"30s\" or \"1h\", or 0 for no limit); using %s",
		key, *d, def))
	*d = def
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
		DSN: "file:dbcdemo?mode=memory&cache=shared", Demo: true,
	}

	path, err := DemoBytdbPath()
	if err != nil {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"could not prepare the bytdb demo file (%v) — starting with the sqlite demo only", err))
		c.Connections = []Connection{sqliteConn}
		c.DefaultConnection = DemoSQLite
		return
	}
	bytdbConn := Connection{Name: DemoBytdb, Driver: "bytdb", DSN: path, Demo: true}

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
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	for _, cn := range c.Connections {
		if cn.Name == name {
			return cn, true
		}
	}
	return Connection{}, false
}

// Conns is the connection list as it stands, for a reader that may run
// beside AddConn/RemoveConn. The slice is the one the writers replace
// wholesale, never edit, so it is safe to keep and range without the lock —
// callers must not modify it.
func (c *Config) Conns() []Connection {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.Connections
}

// ErrConnExists is AddConn refusing a name already in use: ConnByName would
// always find the first of two, so the second could never be reached.
var ErrConnExists = errors.New("a connection by that name already exists")

// AddConn appends a connection at runtime. The name must be new.
func (c *Config) AddConn(cn Connection) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	for _, x := range c.Connections {
		if x.Name == cn.Name {
			return serr.Wrap(ErrConnExists, "name", cn.Name)
		}
	}
	// copy-on-write: see Connections. append alone could write into spare
	// capacity a reader's slice shares.
	next := make([]Connection, len(c.Connections), len(c.Connections)+1)
	copy(next, c.Connections)
	c.Connections = append(next, cn)
	return nil
}

// RemoveConn drops a connection at runtime, reporting whether there was one
// by that name. A default_connection naming it is left alone: the lookups
// that honor it (workspace.New, dbc web's defaultConn) already fall back to
// the first connection when it names nothing.
func (c *Config) RemoveConn(name string) bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	next := make([]Connection, 0, len(c.Connections))
	for _, x := range c.Connections {
		if x.Name != name {
			next = append(next, x)
		}
	}
	if len(next) == len(c.Connections) {
		return false
	}
	c.Connections = next
	return true
}

// ExpandDSN expands ${VAR} and $VAR in a DSN from the environment, as Load
// does for the file's connections. It does this itself rather than via
// os.ExpandEnv so that a typo'd ${PGPASS} comes back as a warning naming it
// instead of silently becoming "" and surfacing later as a baffling auth
// failure. name is the connection's, for the warning.
func ExpandDSN(name, dsn string) (string, []string) {
	var warns []string
	out := os.Expand(dsn, func(key string) string {
		v, ok := os.LookupEnv(key)
		if !ok {
			warns = append(warns, fmt.Sprintf(
				"connection %q: DSN references unset env var $%s (expanded to empty)", name, key))
		}
		return v
	})
	return out, warns
}

func searchPaths() []string {
	paths := []string{"dbc.toml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "dbc", "config.toml"))
	}
	return paths
}
