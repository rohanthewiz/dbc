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

// The row caps: absent means the defaults, an explicit 0 for the display cap
// means "draw them all", and a negative is a typo, not an instruction.
func TestLoadRowCaps(t *testing.T) {
	conn := `
[[connection]]
name = "db"
driver = "sqlite"
dsn = "a.db"
`
	cases := []struct {
		name                 string
		body                 string
		wantRows, wantScreen int
	}{
		{"absent", conn, defaultMaxRows, defaultMaxDisplayRows},
		{"set", "max_rows = 50000\nmax_display_rows = 500\n" + conn, 50000, 500},
		{"display cap off", "max_display_rows = 0\n" + conn, defaultMaxRows, 0},
		{"negative", "max_rows = -1\nmax_display_rows = -5\n" + conn,
			defaultMaxRows, defaultMaxDisplayRows},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, c.body))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.MaxRows != c.wantRows {
				t.Errorf("MaxRows = %d, want %d", cfg.MaxRows, c.wantRows)
			}
			if cfg.MaxDisplayRows != c.wantScreen {
				t.Errorf("MaxDisplayRows = %d, want %d", cfg.MaxDisplayRows, c.wantScreen)
			}
		})
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

// The AI keys: rows are off per connection unless it opts in, and an
// explicit ai_context_rows = 0 survives as "no rows" rather than being
// mistaken for an absent key.
func TestLoadAIKeys(t *testing.T) {
	body := `
ai_agent = "claude"
ai_model = "claude-sonnet-5"

[[connection]]
name = "scratch"
driver = "sqlite"
dsn = "a.db"
ai_rows = true

[[connection]]
name = "prod"
driver = "postgres"
dsn = "postgres://x"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AIAgent != "claude" || cfg.AIModel != "claude-sonnet-5" {
		t.Errorf("agent/model = %q/%q", cfg.AIAgent, cfg.AIModel)
	}
	if cfg.AIContextRows != DefaultAIContextRows {
		t.Errorf("absent ai_context_rows = %d, want the default", cfg.AIContextRows)
	}
	if c, _ := cfg.ConnByName("scratch"); !c.AIRows {
		t.Error("scratch opted in")
	}
	if c, _ := cfg.ConnByName("prod"); c.AIRows {
		t.Error("prod did not opt in, so its rows must stay home")
	}

	for _, c := range []struct {
		line string
		want int
	}{{"ai_context_rows = 0", 0}, {"ai_context_rows = 25", 25}, {"ai_context_rows = -3", DefaultAIContextRows}} {
		cfg, err := Load(writeConfig(t, c.line+"\n"+body[strings.Index(body, "[[connection]]"):]))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AIContextRows != c.want {
			t.Errorf("%s → %d, want %d", c.line, cfg.AIContextRows, c.want)
		}
	}

	// the no-config demo gets the default too
	isolateDemo(t)
	demo, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if demo.AIContextRows != DefaultAIContextRows {
		t.Errorf("demo AIContextRows = %d", demo.AIContextRows)
	}
}
