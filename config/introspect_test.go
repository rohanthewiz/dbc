package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ParseIntrospect takes the levels in any case, trims, and keeps "" as
// "not set"; anything else comes back as typed, not ok.
func TestParseIntrospect(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  ", "", true},
		{"full", IntrospectFull, true},
		{" Names ", IntrospectNames, true},
		{"OFF", IntrospectOff, true},
		{"none", "none", false},
		{" Lazy ", "Lazy", false},
	} {
		got, ok := ParseIntrospect(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseIntrospect(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The global key is every connection's default and a connection's own key
// wins over it; a connection derived onto another database reads at its
// base's level, and a name the config does not have at the default.
func TestIntrospectFor(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
introspect = "Names"

[[connection]]
name = "scratch"
driver = "sqlite"
dsn = "file:scratch.db"

[[connection]]
name = "prod"
driver = "postgres"
dsn = "postgres://localhost/app"
introspect = "off"

[[connection]]
name = "dev"
driver = "postgres"
dsn = "postgres://localhost/dev"
introspect = "full"
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("warnings %q for a valid file", cfg.Warnings)
	}
	if cfg.Introspect != IntrospectNames {
		t.Errorf("global = %q, want it normalized to names", cfg.Introspect)
	}
	for name, want := range map[string]string{
		"scratch":        IntrospectNames, // the global default
		"prod":           IntrospectOff,   // its own
		"dev":            IntrospectFull,  // its own, above the default
		"prod/analytics": IntrospectOff,   // derived: its base's
		"dev/other":      IntrospectFull,
		"nobody":         IntrospectNames, // unknown: the default
	} {
		if got := cfg.IntrospectFor(name); got != want {
			t.Errorf("IntrospectFor(%q) = %q, want %q", name, got, want)
		}
	}
}

// A level that is none warns and falls back — the global key to full, a
// connection's to the global default — rather than failing the load: the
// key decides how much is read, never whether a connection works.
func TestIntrospectTypoWarns(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
introspect = "lazy"

[[connection]]
name = "prod"
driver = "postgres"
dsn = "postgres://localhost/app"
introspect = "name"
`))
	if err != nil {
		t.Fatalf("a misspelled level failed the load: %v", err)
	}
	if cfg.Introspect != IntrospectFull || cfg.IntrospectFor("prod") != IntrospectFull {
		t.Errorf("global %q, prod %q; want both full", cfg.Introspect, cfg.IntrospectFor("prod"))
	}
	joined := strings.Join(cfg.Warnings, "\n")
	for _, want := range []string{`introspect = "lazy" is not a level`, `connection "prod": introspect = "name" is not a level`,
		`"full", "names" or "off"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q lack %q", cfg.Warnings, want)
		}
	}
}

// A Config that never went through Load — a test's, the demo's, an ad-hoc
// one — reads at full, as dbc always did; so does a nil one.
func TestIntrospectDefaultsToFull(t *testing.T) {
	cfg := &Config{Connections: []Connection{{Name: "c", Driver: "sqlite"}}}
	if got := cfg.IntrospectFor("c"); got != IntrospectFull {
		t.Errorf("unloaded config: %q, want full", got)
	}
	var none *Config
	if got := none.IntrospectFor("c"); got != IntrospectFull {
		t.Errorf("nil config: %q, want full", got)
	}
	demo, err := LoadDemo("", DemoDefault)
	if err != nil {
		t.Fatal(err)
	}
	if demo.Path == "" && demo.IntrospectFor(demo.DefaultConnection) != IntrospectFull {
		t.Errorf("demo: %q, want full", demo.IntrospectFor(demo.DefaultConnection))
	}
}

// A saved connection keeps its level across the file, leaves the key out
// when it has none (so it goes on following the global default), and a
// level that is none merges at the default with a warning.
func TestSavedIntrospect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	st := OpenSaved(path)
	for _, sc := range []SavedConn{
		{Name: "big", Driver: "sqlite", DSN: "file:big.db", Introspect: IntrospectNames},
		{Name: "plain", Driver: "sqlite", DSN: "file:plain.db"},
		{Name: "typo", Driver: "sqlite", DSN: "file:typo.db", Introspect: "nmes"},
	} {
		if err := st.Add(sc); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ReadSaved(path)
	if err != nil || len(list) != 3 {
		t.Fatalf("read back %d entries, err %v", len(list), err)
	}
	if list[0].Introspect != IntrospectNames || list[1].Introspect != "" {
		t.Errorf("levels read back: %q, %q", list[0].Introspect, list[1].Introspect)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "introspect = "); n != 2 {
		t.Errorf("the file names introspect %d times, want 2: unset is left out", n)
	}

	cfg := &Config{Introspect: IntrospectOff}
	warns := cfg.MergeSaved(list, nil)
	for name, want := range map[string]string{"big": IntrospectNames, "plain": IntrospectOff, "typo": IntrospectOff} {
		if got := cfg.IntrospectFor(name); got != want {
			t.Errorf("merged %s reads at %q, want %q", name, got, want)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], `"typo": introspect = "nmes" is not a level`) {
		t.Errorf("warnings %q, want one about typo's level", warns)
	}
}
