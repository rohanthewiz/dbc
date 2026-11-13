package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A misspelled top-level key is a warning that names it, and the nearest
// real key when there is one; the setting keeps its default. A whole table
// nothing reads is one warning, not one per key in it.
func TestLoadUnknownTopLevelKeyWarns(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
max_row = 50000
colour = "blue"

[ai]
model = "x"
[ai.deep]
k = 1

[[connection]]
name = "pg"
driver = "postgres"
dsn = "host=h"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxRows != defaultMaxRows {
		t.Errorf("max_rows = %d, want the default", cfg.MaxRows)
	}
	w := strings.Join(cfg.Warnings, "\n")
	for _, want := range []string{
		"unknown key max_row (did you mean max_rows?) is ignored",
		"unknown key colour is ignored",
		"unknown key table [ai] is ignored",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
	if len(cfg.Warnings) != 3 {
		t.Errorf("want 3 warnings (ai's own keys under its one), got:\n%s", w)
	}
}

// An unknown key inside a [[connection]] fails the load, naming each
// connection and key at once — the TLS one from the report among them —
// and listing what a connection takes, tls included.
func TestLoadUnknownConnectionKeyFails(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[connection]]
name = "a"
driver = "postgres"
dsn = "host=h"
ssl = "verify-full"

[[connection]]
name = "b"
driver = "postgres"
dsn = "host=h"
tls_mode = "require"
sub = { x = 1 }
demo = true

[[connection]]
name = "c"
driver = "sqlite"
dsn = "c.db"
`))
	if err == nil {
		t.Fatal("loaded a connection with an ssl key it would ignore")
	}
	msg := err.Error()
	for _, want := range []string{
		`connection "a": ssl (did you mean tls?)`,
		`connection "b": tls_mode (did you mean tls?)`,
		`connection "b": table [sub]`,
		// Demo is never read from a file (toml:"-"): it seeds a demo table
		`connection "b": demo`,
		"a connection takes name, driver, dsn, tls, tls_ca",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "sub.x") || strings.Contains(msg, `"c"`) {
		t.Errorf("error names what it should not:\n%s", msg)
	}
}

// A near miss of a connection key is suggested: dns for dsn.
func TestLoadUnknownConnectionKeySuggests(t *testing.T) {
	_, err := Load(writeConfig(t, `
[[connection]]
name = "a"
driver = "sqlite"
dns = "a.db"
`))
	if err == nil || !strings.Contains(err.Error(), `connection "a": dns (did you mean dsn?)`) {
		t.Fatalf("err = %v", err)
	}
}

// The suggestions: other tools' TLS names point at dbc's tls keys (the
// case the strict check is for, which no edit distance finds), a typo at
// the key it misspells, and nothing near enough at nothing.
func TestNearKey(t *testing.T) {
	for key, want := range map[string]string{
		"ssl": "tls", "SSLMode": "tls", "tls_mode": "tls", "sslrootcert": "tls_ca", "sslkey": "tls_key",
		"dns": "dsn", "drvier": "driver", "nme": "name", "ai_row": "ai_rows",
		"colour": "", "x": "", "migrate": "",
	} {
		if got := nearKey(key, connectionKeys); got != want {
			t.Errorf("nearKey(%q) = %q, want %q", key, got, want)
		}
	}
	// an alias only for a file level that has its key: no tls at the top
	if got := nearKey("sslmode", configKeys); got != "" {
		t.Errorf("top-level sslmode suggests %q", got)
	}
	if got := nearKey("max_row", configKeys); got != "max_rows" {
		t.Errorf("max_row suggests %q", got)
	}
}

// An inline array of connections has no header per entry, so the entry is
// not named, but the key still fails the load.
func TestLoadUnknownKeyInlineArray(t *testing.T) {
	_, err := Load(writeConfig(t, `
connection = [ { name = "a", driver = "sqlite", dsn = "a.db" },
               { name = "b", driver = "sqlite", dsn = "b.db", ssl = "x" } ]
`))
	if err == nil || !strings.Contains(err.Error(), "a [[connection]] entry: ssl") {
		t.Fatalf("err = %v", err)
	}
}

// An entry copied from connections.toml carries its added time, which a
// config file takes and ignores: savedDoc promises the copy works.
func TestLoadTakesCopiedSavedEntry(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[[connection]]
name = "a"
driver = "sqlite"
dsn = "a.db"
added = 2026-11-13T10:00:00Z
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings %q", cfg.Warnings)
	}
}

// In connections.toml an entry with an unknown key is skipped (the rest
// merge), a top-level one is a warning, and no change is written over
// either: the rewrite would drop the key and bring the entry back without
// it.
func TestSavedUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	body := `colour = "blue"

[[connection]]
name = "a"
driver = "sqlite"
dsn = "a.db"
ssl = "verify-full"

[[connection]]
name = "b"
driver = "sqlite"
dsn = "b.db"
migrations = "db/migrations"
`
	must(t, os.WriteFile(path, []byte(body), 0o600))

	c := &Config{Connections: []Connection{{Name: "file", Driver: "sqlite"}}}
	c.LoadSaved(path, nil)
	if got := fmt.Sprint(names(c.Connections)); got != "[file]" {
		t.Fatalf("merged = %s", got)
	}
	w := strings.Join(c.Warnings, "\n")
	for _, want := range []string{
		"unknown key colour is ignored",
		`saved connection "a" skipped: unknown key ssl`,
		// a config file's key, but not a saved connection's
		`saved connection "b" skipped: unknown key migrations`,
		"a saved connection takes name, driver, dsn, tls",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}

	err := OpenSaved(path).Add(SavedConn{Name: "new", Driver: "sqlite", DSN: "n.db"})
	if err == nil || !strings.Contains(err.Error(), `connection "a": ssl`) ||
		!strings.Contains(err.Error(), "colour") {
		t.Fatalf("an add over unknown keys = %v", err)
	}
	if bs, _ := os.ReadFile(path); string(bs) != body {
		t.Fatalf("the file was rewritten:\n%s", bs)
	}

	// the listing still has every entry: the edit form reads through it
	if all, err := ReadSaved(path); err != nil || savedNames(all) != "a,b" {
		t.Fatalf("ReadSaved = %v, %v", all, err)
	}
}

// A plain server DSN beside an unknown key: the start-up rewrite that
// would encrypt it is refused like any other, and the warning says the
// password stays in plain text and why.
func TestSavedUnknownKeyKeepsPlainDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	body := `[[connection]]
name = "a"
driver = "postgres"
dsn = "host=h password=pw"
tls_mod = "verify-full"
`
	must(t, os.WriteFile(path, []byte(body), 0o600))
	c := &Config{}
	c.LoadSaved(path, nil)
	w := strings.Join(c.Warnings, "\n")
	if !strings.Contains(w, "still in plain text") || !strings.Contains(w, `connection "a": tls_mod`) {
		t.Fatalf("warnings:\n%s", w)
	}
	if bs, _ := os.ReadFile(path); string(bs) != body {
		t.Fatalf("the file was rewritten:\n%s", bs)
	}
}

// The key lists come from the struct tags: a new setting is in every
// message the day it is added, and nothing internal is.
func TestTomlKeys(t *testing.T) {
	if got := strings.Join(connectionKeys, ","); got !=
		"name,driver,dsn,tls,tls_ca,tls_cert,tls_key,tls_key_password,migrations,ai_rows,introspect" {
		t.Errorf("connection keys = %s", got)
	}
	for _, internal := range []string{"Path", "path", "Demo", "demo", "Warnings", "connMu"} {
		for _, k := range configKeys {
			if k == internal {
				t.Errorf("config keys list %s", internal)
			}
		}
	}
}
