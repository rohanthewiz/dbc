package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// isolateDemo puts the process somewhere with no dbc.toml and points $HOME at
// a scratch directory, so the loader is forced down the no-config path and the
// bytdb demo file lands under the temp dir rather than the real cache dir.
// (os.UserCacheDir derives from $HOME on the platforms we build for.)
func isolateDemo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "") // otherwise linux ignores the $HOME we just set
	return home
}

// With no config anywhere the app must come up on both embedded engines, with
// bytdb the one that starts active.
func TestDemoFallbackRegistersBothEngines(t *testing.T) {
	isolateDemo(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Demo {
		t.Fatal("no config found, so Demo should be set")
	}
	if cfg.DefaultConnection != DemoBytdb {
		t.Errorf("default = %q, want %q", cfg.DefaultConnection, DemoBytdb)
	}
	if len(cfg.Connections) != 2 {
		t.Fatalf("connections = %v, want both demos", cfg.Connections)
	}
	// active first — several call sites fall back to Connections[0]
	if cfg.Connections[0].Name != DemoBytdb || cfg.Connections[0].Driver != "bytdb" {
		t.Errorf("first connection = %+v, want the bytdb demo", cfg.Connections[0])
	}
	if cfg.Connections[1].Name != DemoSQLite || cfg.Connections[1].Driver != "sqlite" {
		t.Errorf("second connection = %+v, want the sqlite demo", cfg.Connections[1])
	}
	// the sqlite demo keeps its historical in-memory DSN, so the shipped
	// scripts that name "demo" behave exactly as before
	if !strings.Contains(cfg.Connections[1].DSN, "mode=memory") {
		t.Errorf("sqlite demo DSN = %q, want the in-memory one", cfg.Connections[1].DSN)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

// -demo sqlite flips which one is active without dropping the other.
func TestDemoFallbackSQLiteActive(t *testing.T) {
	isolateDemo(t)

	cfg, err := LoadDemo("", DemoEngineSQLite)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DefaultConnection != DemoSQLite {
		t.Errorf("default = %q, want %q", cfg.DefaultConnection, DemoSQLite)
	}
	if len(cfg.Connections) != 2 || cfg.Connections[0].Name != DemoSQLite {
		t.Errorf("connections = %+v, want the sqlite demo first and bytdb still present", cfg.Connections)
	}
	if _, ok := cfg.ConnByName(DemoBytdb); !ok {
		t.Error("the bytdb demo should still be selectable")
	}
}

// The bytdb demo needs a real file — bytdb has no in-memory mode — and it
// belongs in the cache directory, not the user's working directory.
func TestDemoBytdbPathIsInCacheDir(t *testing.T) {
	home := isolateDemo(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dsn := cfg.Connections[0].DSN
	if !filepath.IsAbs(dsn) {
		t.Errorf("bytdb demo DSN = %q, want an absolute path", dsn)
	}
	if !strings.HasPrefix(dsn, home) {
		t.Errorf("bytdb demo DSN = %q, want it under the cache dir below %q", dsn, home)
	}
	if filepath.Base(filepath.Dir(dsn)) != "dbc" {
		t.Errorf("bytdb demo DSN = %q, want it in a dbc/ subdirectory", dsn)
	}
}

// A config file wins over the demo fallback entirely, whatever -demo said.
func TestDemoEngineIgnoredWithConfig(t *testing.T) {
	isolateDemo(t)

	cfg, err := LoadDemo(writeConfig(t, `
[[connection]]
name = "pg"
driver = "postgres"
dsn = "postgres://localhost/db"
`), DemoEngineSQLite)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Demo {
		t.Error("a config file was found, so Demo should be false")
	}
	if len(cfg.Connections) != 1 || cfg.Connections[0].Name != "pg" {
		t.Errorf("connections = %+v, want only the configured one", cfg.Connections)
	}
}

func TestParseDemoEngine(t *testing.T) {
	cases := []struct {
		in   string
		want DemoEngine
		ok   bool
	}{
		{"", DemoDefault, true},
		{"bytdb", DemoEngineBytdb, true},
		{" SQLite ", DemoEngineSQLite, true},
		{"sqlite3", DemoEngineSQLite, true},
		{"postgres", DemoDefault, false}, // a server driver is not a demo engine
		{"nope", DemoDefault, false},
	}
	for _, c := range cases {
		got, err := ParseDemoEngine(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParseDemoEngine(%q) error = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if got != c.want {
			t.Errorf("ParseDemoEngine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
