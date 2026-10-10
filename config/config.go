package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	// DefaultResultTabs is how many result tabs one connection's result set
	// holds when result_tabs is not set (see Config.ResultTabs), and
	// MaxResultTabs the most it may be set to. Each tab keeps a whole
	// result — up to max_rows rows — alive for as long as its query tab
	// lives, per connection that tab has visited, so the ceiling is there
	// to keep a typo (1000) from quietly pinning a lot of memory.
	DefaultResultTabs = 10
	MaxResultTabs     = 50

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

	// TLSOpts holds the tls, tls_ca, tls_cert and tls_key keys: TLS for
	// postgres and mysql, one spelling for both — see tls.go. Embedded
	// untagged so the keys sit flat beside dsn. Load leaves the paths
	// absolute (ExpandTLS), so the db package never has to know which file
	// they were written in.
	TLSOpts

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

	// Web marks a connection added from inside dbc — dbc web's browser UI
	// or the TUI's connection form (package connedit) — and kept in the
	// saved-connections file (connections.toml, see SavedStore), not in the
	// config file. Never read from a file's key — MergeSaved sets it — so
	// both UIs can tell which ones they may edit or remove: the config file's
	// connections are the file's to change. Every dbc merges the saved ones,
	// so the TUI and headless runs list them too.
	Web bool `toml:"-"`

	// Base and Database describe a connection derived from a configured
	// one: the same server, credentials and TLS, opened on another database
	// of that server. ConnByName makes one up for a name of the form
	// "<base>/<database>" (see DatabaseSep); none is ever stored, in the
	// config or the saved-connections file. Database overrides whatever
	// database the DSN names when the pool is opened (db.openPool); "" keeps
	// the DSN's. Both are "" on a configured connection.
	Base     string `toml:"-"`
	Database string `toml:"-"`
}

// DatabaseSep joins a configured connection's name to one of its server's
// databases, in a derived connection's name: "ProdDr/analytics".
//
// A Postgres connection is bound to one database — every catalog the
// server has is per database, and there is no USE to move a session to
// another — so reaching a second database on the same host means opening a
// second pool. A MySQL pool is opened the same way, on the database its
// sidebar lists (see SupportsDatabases). Naming that pool "<base>/<database>" lets everything keyed by
// connection name (the Manager's pools and row counts, the pinned session,
// history, saved tabs, the web layout's per-connection keys) keep working
// with no second key, and a tab saved on a derived connection reopens on it
// after a restart, because the name alone says how to rebuild it.
const DatabaseSep = "/"

// DerivedName is the name of base's connection to database.
func DerivedName(base, database string) string { return base + DatabaseSep + database }

// SupportsDatabases reports whether a driver's connections can be derived
// onto another database of their server: Postgres, and MySQL (MariaDB
// too). Both tie what a connection's sidebar lists to the database its DSN
// names — Postgres because every catalog is per database, MySQL because
// dbc scopes its catalog queries to DATABASE() — so on both another
// database is another connection. MySQL could USE its way across instead,
// but a pool's connections are interchangeable and a USE would stick to
// only one of them; setting the database the pool dials with keeps every
// connection of it on the same one. SQLite and bytdb open one file, with
// no server to hold a second database.
//
// db.HasDatabases (the database pickers) is this same set, so a picker
// never offers a database ConnByName could not derive.
func SupportsDatabases(driver string) bool {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pg", "pgx", "mysql", "mariadb":
		return true
	}
	return false
}

// Config is the application configuration.
type Config struct {
	// ScriptsDir is where Go scripts live. Load leaves it absolute (see
	// ResolveScriptsDir in scripts.go): as written it is relative to the
	// config file, and absent it is ~/.config/dbc/scripts.
	ScriptsDir string `toml:"scripts_dir"`
	// PipelinesDir is where pipelines live (pipelines.go): absolute after
	// Load, ~/.config/dbc/pipelines when the file does not say.
	PipelinesDir string `toml:"pipelines_dir"`
	// JobsDir is where jobs live (jobs.go: DAGs of pipelines with
	// triggers), RunsDir where each run of a job or a pipeline leaves its
	// record; both absolute after Load, beside the scripts when the file
	// does not say.
	JobsDir string `toml:"jobs_dir"`
	RunsDir string `toml:"runs_dir"`
	// PluginsDir is where the user's pipeline plugins live (plugins.go:
	// Go files, each one kind of node), absolute after Load, beside the
	// scripts when the file does not say.
	PluginsDir string `toml:"plugins_dir"`
	// FilesDir is where a relative path in a pipeline's file node — and in
	// a script's s.Export or s.Path — resolves (files.go), whichever
	// process runs it: absolute after Load, the home directory when the
	// file does not say.
	FilesDir string `toml:"files_dir"`
	// RunsKeep is how many records of each job and of each pipeline are
	// kept, the oldest pruned after a run ends; 0 means DefaultRunsKeep.
	RunsKeep int `toml:"runs_keep"`
	MaxRows  int `toml:"max_rows"` // rows fetched from the server

	// PGBin is the directory holding PostgreSQL's client tools (pg_dump,
	// pg_restore) for dumps — `dbc dump` and both UIs' "Dump database…".
	// Empty means look for them (pgdump.Locate). Resolved like scripts_dir:
	// absolute after Load, relative to the config file as written.
	PGBin string `toml:"pg_bin"`

	// MaxDisplayRows caps how many of those rows the TUI table renders. It is
	// separate from MaxRows because the two are limited by different things:
	// fetching more is a question of memory and patience, rendering more costs
	// a widget per value and shows up as a sluggish scroll. Raising max_rows
	// for the sake of an export should not make the table crawl. Zero means no
	// display cap.
	MaxDisplayRows int `toml:"max_display_rows"`

	// ResultTabs caps the tabs in one connection's result set: each query
	// tab keeps a result set per connection it has been on, and a run
	// lands in the set's current tab, or in a new one when that tab is
	// pinned. Past the cap the oldest unpinned tab is dropped. Unset is
	// DefaultResultTabs; Load keeps it within 1…MaxResultTabs. Read it
	// through ResultTabLimit, which also covers a Config built in code.
	ResultTabs int `toml:"result_tabs"`

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

	// PlanTheme is the palette of the plan pictures the shell and the TUI
	// draw — `dbc explain -t pdf|jpeg|png` and the TUI's Save as PDF /
	// JPEG, and the schema diagrams of `dbc erd -t png|jpeg` and the TUI's
	// Diagram items: "dark" (the default, dbc's own slate) or "light" (paper, kinder
	// to a printer). Load normalizes it to one of the two, so readers can
	// hand it to theme.ByName without a second error to handle. dbc web
	// ignores it: its files follow the light or dark the view is wearing.
	// A string rather than a theme.Palette so config stays a leaf package.
	PlanTheme string `toml:"plan_theme"`

	DefaultConnection string `toml:"default_connection"`

	// Connections is written freely while the config is being built (Load,
	// the demo pruning, an ad-hoc --dsn), when nothing else can see it. Once
	// something may run beside a change — dbc web adding a connection while
	// its tabs connect and run — the change goes through AddConn/RemoveConn/
	// ReplaceConn and every reader through Conns/ConnByName, which take connMu.
	//
	// The writers are copy-on-write: AddConn, RemoveConn and ReplaceConn
	// build a new slice rather than edit the old one in place, so a slice a
	// reader got from Conns (or a range over the field that began before the
	// change) never sees an element change under it.
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
	cfg := &Config{MaxRows: defaultMaxRows,
		MaxDisplayRows: defaultMaxDisplayRows, AIContextRows: DefaultAIContextRows,
		ResultTabs:      DefaultResultTabs,
		ConnIdleTimeout: DefaultConnIdleTimeout, ConnectTimeout: DefaultConnectTimeout, PlanTheme: "dark"}

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
		cfg.resolveScripts("")
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
	cfg.checkResultTabs()
	cfg.checkDuration("conn_idle_timeout", &cfg.ConnIdleTimeout, DefaultConnIdleTimeout)
	cfg.checkDuration("connect_timeout", &cfg.ConnectTimeout, DefaultConnectTimeout)
	cfg.checkPlanTheme()
	if len(cfg.Connections) == 0 {
		return nil, serr.New("config has no [[connection]] entries", "config_path", path)
	}
	// Relative tls_* paths are relative to the config file (see ExpandTLS).
	// Made absolute here so a later chdir — none today, but a script may
	// one day — cannot move what they point at.
	cfgDir := filepath.Dir(path)
	if abs, err := filepath.Abs(path); err == nil {
		cfgDir = filepath.Dir(abs)
	}
	cfg.resolveScripts(cfgDir)
	seen := make(map[string]bool, len(cfg.Connections))
	for i := range cfg.Connections {
		cn := &cfg.Connections[i]
		if seen[cn.Name] {
			return nil, serr.New("duplicate connection name — ConnByName would always pick the first",
				"name", cn.Name, "config_path", path)
		}
		seen[cn.Name] = true
		var warns []string
		cn.DSN, warns = ExpandDSN(cn.Name, cn.Driver, cn.DSN)
		cfg.Warnings = append(cfg.Warnings, warns...)
		// A bad TLS setting fails the load, as a duplicate name does, rather
		// than becoming a warning: carrying on would mean connecting with
		// less protection than the file asked for.
		if err := cn.TLSOpts.Check(); err != nil {
			return nil, serr.Wrap(err, "connection", cn.Name, "config_path", path)
		}
		cn.TLSOpts, warns = ExpandTLS(cn.Name, cn.TLSOpts, cfgDir)
		cfg.Warnings = append(cfg.Warnings, warns...)
	}
	if cfg.DefaultConnection != "" && !seen[cfg.DefaultConnection] {
		return nil, serr.New("default_connection names no configured connection",
			"default_connection", cfg.DefaultConnection, "config_path", path)
	}
	return cfg, nil
}

// resolveScripts settles ScriptsDir (see scripts.go) and PGBin against the
// config file's directory, "" for the demo fallback, and adds the warnings:
// unset ${VAR}s, and scripts left behind in a ./scripts dbc no longer reads.
func (c *Config) resolveScripts(cfgDir string) {
	var warns []string
	c.ScriptsDir, warns = ResolveScriptsDir(c.ScriptsDir, cfgDir)
	var pw []string
	c.PipelinesDir, pw = ResolvePipelinesDir(c.PipelinesDir, cfgDir)
	warns = append(warns, pw...)
	c.JobsDir, pw = ResolveJobsDir(c.JobsDir, cfgDir)
	warns = append(warns, pw...)
	c.RunsDir, pw = ResolveRunsDir(c.RunsDir, cfgDir)
	warns = append(warns, pw...)
	c.PluginsDir, pw = ResolvePluginsDir(c.PluginsDir, cfgDir)
	warns = append(warns, pw...)
	c.FilesDir, pw = ResolveFilesDir(c.FilesDir, cfgDir)
	warns = append(warns, pw...)
	if c.RunsKeep <= 0 {
		c.RunsKeep = DefaultRunsKeep
	}
	c.Warnings = append(c.Warnings, warns...)
	if strings.TrimSpace(c.PGBin) != "" {
		c.PGBin, warns = resolvePath("pg_bin", c.PGBin, cfgDir)
		c.Warnings = append(c.Warnings, warns...)
	}
	if w := legacyScriptsWarning(c.ScriptsDir); w != "" {
		c.Warnings = append(c.Warnings, w)
	}
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

// checkPlanTheme normalizes plan_theme to "dark" or "light". An unknown
// value falls back to dark with a warning rather than failing the load: a
// misspelled cosmetic setting is not a reason to refuse to start, and the
// warning names the fix.
func (c *Config) checkPlanTheme() {
	switch v := strings.ToLower(strings.TrimSpace(c.PlanTheme)); v {
	case "", "dark":
		c.PlanTheme = "dark"
	case "light":
		c.PlanTheme = "light"
	default:
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"plan_theme = %q is not a theme (use \"light\" or \"dark\"); using dark", c.PlanTheme))
		c.PlanTheme = "dark"
	}
}

// checkResultTabs keeps result_tabs within 1…MaxResultTabs, saying so when
// it had to: a set of no tabs could hold no result at all, and one past the
// ceiling is more likely a typo than a wish.
func (c *Config) checkResultTabs() {
	switch {
	case c.ResultTabs < 1:
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"result_tabs = %d is too few (at least 1); using %d", c.ResultTabs, DefaultResultTabs))
		c.ResultTabs = DefaultResultTabs
	case c.ResultTabs > MaxResultTabs:
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"result_tabs = %d is more than %d; using %d", c.ResultTabs, MaxResultTabs, MaxResultTabs))
		c.ResultTabs = MaxResultTabs
	}
}

// ResultTabLimit is how many tabs one connection's result set may hold:
// ResultTabs, or DefaultResultTabs for a Config that never went through
// Load (a test's, an ad-hoc one) and so left it zero.
func (c *Config) ResultTabLimit() int {
	if c == nil || c.ResultTabs < 1 {
		return DefaultResultTabs
	}
	return min(c.ResultTabs, MaxResultTabs)
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

// ConnByName finds a connection config by name. A name no connection has
// exactly, of the form "<base>/<database>" where base is a Postgres or
// MySQL connection (SupportsDatabases), is that connection derived onto
// database (see DatabaseSep): a copy with Name set to name and Base and
// Database filled in.
//
// The exact match is tried first, so a configured connection whose own
// name contains a "/" is still found as itself. Then the name is matched
// against each such connection's name plus the separator, rather than
// split at a "/": a quoted Postgres database name may contain one, and so
// may a connection's name. Of two bases that both match ("a" and "a/b" for
// "a/b/c"), the longer wins, as the more specific.
func (c *Config) ConnByName(name string) (Connection, bool) {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	for _, cn := range c.Connections {
		if cn.Name == name {
			return cn, true
		}
	}
	best := -1
	for i, cn := range c.Connections {
		prefix := cn.Name + DatabaseSep
		if cn.Base == "" && SupportsDatabases(cn.Driver) && len(name) > len(prefix) &&
			strings.HasPrefix(name, prefix) && (best < 0 || len(cn.Name) > len(c.Connections[best].Name)) {
			best = i
		}
	}
	if best < 0 {
		return Connection{}, false
	}
	cn := c.Connections[best]
	cn.Base, cn.Database = cn.Name, name[len(cn.Name)+len(DatabaseSep):]
	cn.Name = name
	// a demo is seeded on open; a database derived from one is the user's
	// own pick, and must not be
	cn.Demo = false
	return cn, true
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

// ConnOrder lists the connection names in the order a new script's template
// wants them (scripts.Fill puts the first in "{{conn}}" and the second in
// "{{conn2}}"): first, when it names a connection (the tab's own, say); the
// default connection; then the rest in config order. Each name once. dbc web
// and the TUI both fill templates through it, so a script started from
// either names the same connections.
func (c *Config) ConnOrder(first string) []string {
	var out []string
	add := func(n string) {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if _, known := c.ConnByName(first); known {
		add(first)
	}
	if _, known := c.ConnByName(c.DefaultConnection); known {
		add(c.DefaultConnection)
	}
	for _, cn := range c.Conns() {
		add(cn.Name)
	}
	return out
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

// ErrConnNotFound is ReplaceConn finding no connection by the old name —
// removed (in another window, say) since the caller looked it up.
var ErrConnNotFound = errors.New("no connection by that name")

// ReplaceConn swaps the connection named old for cn, in place: the entry
// keeps its position, so an edit does not move it down the sidebar. cn may
// carry a new name; ErrConnExists refuses one another connection already
// has. Doing the lookup, the clash check and the swap under one lock is
// what makes two windows editing (or one editing while another removes)
// safe: the second of them fails here instead of leaving two entries by
// one name or resurrecting a removed one.
func (c *Config) ReplaceConn(old string, cn Connection) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	at := -1
	for i, x := range c.Connections {
		switch {
		case x.Name == old:
			at = i
		case x.Name == cn.Name:
			return serr.Wrap(ErrConnExists, "name", cn.Name)
		}
	}
	if at < 0 {
		return serr.Wrap(ErrConnNotFound, "name", old)
	}
	// copy-on-write: see Connections
	next := slices.Clone(c.Connections)
	next[at] = cn
	c.Connections = next
	return nil
}

// ExpandDSN expands ${VAR} and $VAR in a DSN from the environment, as Load
// does for the file's connections. It does this itself rather than via
// os.ExpandEnv so that a typo'd ${PGPASS} comes back as a warning naming it
// instead of silently becoming "" and surfacing later as a baffling auth
// failure. name is the connection's, for the warning.
//
// Each value is escaped for where it lands in the DSN of driver (any alias
// db.Driver accepts): inside a quoted Postgres value, in a URL's password,
// in a MySQL param — see dsnexpand.go. So a password kept in the environment
// may hold any character, as one typed into the DSN form's field may.
func ExpandDSN(name, driver, dsn string) (string, []string) {
	var warns []string
	lookup := func(key string) string {
		v, ok := os.LookupEnv(key)
		if !ok {
			warns = append(warns, fmt.Sprintf(
				"connection %q: DSN references unset env var $%s (expanded to empty)", name, key))
		}
		return v
	}
	if strings.Contains(dsn, "\x00") {
		// the markers below could not be told from the DSN's own text; no
		// real DSN holds a NUL, so plain expansion loses nothing real
		return os.Expand(dsn, lookup), warns
	}
	var vals []string
	marked := os.Expand(dsn, func(key string) string {
		vals = append(vals, lookup(key))
		return refMark(len(vals) - 1)
	})
	if len(vals) == 0 {
		return marked, warns
	}
	return fillRefs(shapeOf(driver, marked), marked, vals), warns
}

func searchPaths() []string {
	paths := []string{"dbc.toml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "dbc", "config.toml"))
	}
	return paths
}
