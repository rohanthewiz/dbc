package config

import (
	"path/filepath"
	"testing"
)

// files_dir: absent, the home directory — not the directory dbc started
// in, which is the whole point (a pipeline's relative paths must mean the
// same files from a shell and from dbc web); as written, resolved as
// scripts_dir is, a relative one beside the config file. Load leaves it
// absolute either way, the demo fallback included.
func TestFilesDir(t *testing.T) {
	home, cwd := isolate(t)
	t.Setenv("DBC_TEST_FILES", "/srv/data")
	for _, c := range []struct{ raw, base, want string }{
		{"", "/cfg", home},
		{"  ", "", home},
		{"~/data", "/cfg", filepath.Join(home, "data")},
		{"${DBC_TEST_FILES}/in", "/cfg", "/srv/data/in"},
		{"/abs/files", "/cfg", "/abs/files"},
		{"data", "/cfg", "/cfg/data"},
		{".", "/proj", "/proj"}, // a checkout's ./dbc.toml: its own directory
	} {
		if got, _ := ResolveFilesDir(c.raw, c.base); got != c.want {
			t.Errorf("ResolveFilesDir(%q, %q) = %q, want %q", c.raw, c.base, got, c.want)
		}
	}

	cfg, err := Load("") // the demo fallback
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FilesDir != home || cfg.FilesDir == cwd {
		t.Errorf("demo FilesDir = %q, want %q", cfg.FilesDir, home)
	}
	conn := "\n[[connection]]\nname = \"pg\"\ndriver = \"postgres\"\ndsn = \"postgres://x@localhost/db\"\n"
	path := writeConfig(t, "files_dir = \"exports\"\n"+conn)
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "exports"); evalOr(cfg.FilesDir) != evalOr(want) {
		t.Errorf("relative FilesDir = %q, want %q (beside the config file)", cfg.FilesDir, want)
	}
}
