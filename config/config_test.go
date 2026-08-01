package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig drops a config file in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dbc.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadExpandsEnvVars(t *testing.T) {
	t.Setenv("DBC_TEST_PASS", "hunter2")
	cfg, err := Load(writeConfig(t, `
[[connection]]
name = "pg"
driver = "postgres"
dsn = "postgres://app:${DBC_TEST_PASS}@localhost/db"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Connections[0].DSN; !strings.Contains(got, "hunter2") {
		t.Errorf("DSN = %q, want the env var expanded", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

// A typo'd env var must warn by name rather than silently expanding to ""
// and surfacing later as a baffling auth failure.
func TestLoadWarnsOnUnsetEnvVar(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[[connection]]
name = "pg"
driver = "postgres"
dsn = "postgres://app:${DBC_TEST_NO_SUCH_VAR}@localhost/db"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Warnings) != 1 ||
		!strings.Contains(cfg.Warnings[0], "DBC_TEST_NO_SUCH_VAR") ||
		!strings.Contains(cfg.Warnings[0], `"pg"`) {
		t.Errorf("warnings = %v, want one naming the var and the connection", cfg.Warnings)
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[connection]]
name = "db"
driver = "sqlite"
dsn = "a.db"

[[connection]]
name = "db"
driver = "sqlite"
dsn = "b.db"
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want a duplicate-name error", err)
	}
}

func TestLoadRejectsUnknownDefaultConnection(t *testing.T) {
	_, err := Load(writeConfig(t, `
default_connection = "nope"

[[connection]]
name = "db"
driver = "sqlite"
dsn = "a.db"
`))
	if err == nil || !strings.Contains(err.Error(), "default_connection") {
		t.Errorf("err = %v, want a default_connection error", err)
	}
}
