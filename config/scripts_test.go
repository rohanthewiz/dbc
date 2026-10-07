package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/scripts"
)

// isolate gives a test its own HOME and cwd, so neither the user's
// ~/.config/dbc nor a ./dbc.toml or ./scripts in the package dir leaks in.
func isolate(t *testing.T) (home, cwd string) {
	t.Helper()
	home, cwd = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)
	return home, cwd
}

func TestResolveScriptsDir(t *testing.T) {
	home, cwd := isolate(t)
	t.Setenv("DBC_TEST_SCRIPTS", "/srv/s")
	def := filepath.Join(home, ".config", "dbc", "scripts")
	for _, c := range []struct {
		raw, base, want string
		warn            bool
	}{
		{raw: "", base: "/cfg", want: def}, // unset: the home default, wherever the config is
		{raw: "  ", base: "", want: def},   // blank is unset
		{raw: "~/my/scripts", base: "/cfg", want: filepath.Join(home, "my/scripts")},
		{raw: "~", base: "/cfg", want: home},
		{raw: "/abs/scripts", base: "/cfg", want: "/abs/scripts"},
		{raw: "scripts", base: "/cfg", want: "/cfg/scripts"},            // relative to the config file
		{raw: "../shared/./s", base: "/cfg/dbc", want: "/cfg/shared/s"}, // and cleaned
		{raw: "${DBC_TEST_SCRIPTS}/etl", base: "/cfg", want: "/srv/s/etl"},
		{raw: "scripts", base: "", want: filepath.Join(cwd, "scripts")}, // no file: cwd, made absolute
		{raw: "${DBC_TEST_NO_SUCH}/s", base: "/cfg", want: "/s", warn: true},
	} {
		got, warns := ResolveScriptsDir(c.raw, c.base)
		// cwd may be a symlinked temp dir (macOS /var → /private/var);
		// compare what both resolve to
		if evalOr(got) != evalOr(c.want) {
			t.Errorf("ResolveScriptsDir(%q, %q) = %q, want %q", c.raw, c.base, got, c.want)
		}
		if (len(warns) > 0) != c.warn {
			t.Errorf("ResolveScriptsDir(%q) warnings = %q, want warn=%v", c.raw, warns, c.warn)
		}
	}
}

func evalOr(p string) string {
	if e, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(e, filepath.Base(p))
	}
	return p
}

// The bug this fixes: with no scripts_dir, where dbc looked depended on the
// directory it started in (dbc.app's helper starts in $HOME → ~/scripts).
// Now a config without the key, and no config at all, both land on
// ~/.config/dbc/scripts; a relative value follows the config file.
func TestLoadScriptsDir(t *testing.T) {
	home, _ := isolate(t)
	def := filepath.Join(home, ".config", "dbc", "scripts")

	cfg, err := Load("") // no config anywhere: the demo fallback
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ScriptsDir != def {
		t.Errorf("demo ScriptsDir = %q, want %q", cfg.ScriptsDir, def)
	}

	conn := "\n[[connection]]\nname = \"pg\"\ndriver = \"postgres\"\ndsn = \"postgres://x@localhost/db\"\n"
	path := writeConfig(t, conn)
	if cfg, err = Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ScriptsDir != def {
		t.Errorf("unset ScriptsDir = %q, want %q", cfg.ScriptsDir, def)
	}

	path = writeConfig(t, "scripts_dir = \"etl\"\n"+conn)
	if cfg, err = Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if want := filepath.Join(filepath.Dir(path), "etl"); evalOr(cfg.ScriptsDir) != evalOr(want) {
		t.Errorf("relative ScriptsDir = %q, want %q (beside the config file)", cfg.ScriptsDir, want)
	}
}

func TestFindScript(t *testing.T) {
	_, cwd := isolate(t)
	dir := t.TempDir()
	write := func(p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "copy_it.go"))
	write(filepath.Join(dir, "both.go"))
	write(filepath.Join(cwd, "both.go")) // the cwd's file wins: it always ran before
	write(filepath.Join(cwd, "here.go"))
	cfg := &Config{ScriptsDir: dir}

	for _, c := range []struct{ arg, want string }{
		{"copy_it", filepath.Join(dir, "copy_it.go")},
		{"copy_it.go", filepath.Join(dir, "copy_it.go")},
		{"both.go", "both.go"},
		{"here.go", "here.go"},
		{"./here.go", "./here.go"},
	} {
		got, err := cfg.FindScript(c.arg)
		if err != nil || got != c.want {
			t.Errorf("FindScript(%q) = %q, %v; want %q", c.arg, got, err, c.want)
		}
	}
	for _, arg := range []string{"nope", "sub/copy_it.go", "copy_it.go.go"} {
		if got, err := cfg.FindScript(arg); err == nil {
			t.Errorf("FindScript(%q) = %q, want an error", arg, got)
		} else if !strings.Contains(arg, "/") && !strings.Contains(err.Error(), dir) {
			t.Errorf("FindScript(%q) error %q should name the scripts dir", arg, err)
		}
	}
}

// Scripts left in a ./scripts dbc no longer reads are pointed out, but only
// while the new place is empty — not on every start forever.
func TestLegacyScriptsWarning(t *testing.T) {
	_, cwd := isolate(t)
	resolved := t.TempDir()
	if w := legacyScriptsWarning(resolved); w != "" {
		t.Errorf("no ./scripts: warned %q", w)
	}
	if err := os.Mkdir(filepath.Join(cwd, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "scripts", "old.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := legacyScriptsWarning(resolved); !strings.Contains(w, resolved) || !strings.Contains(w, "1 .go") {
		t.Errorf("warning = %q, want one naming %s and the file count", w, resolved)
	}
	if w := legacyScriptsWarning(filepath.Join(cwd, "scripts")); w != "" {
		t.Errorf("./scripts is the resolved dir: warned %q", w)
	}
	if err := os.WriteFile(filepath.Join(resolved, "new.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := legacyScriptsWarning(resolved); w != "" {
		t.Errorf("new place has scripts: warned %q", w)
	}
}

// A dbc checkout's ./scripts holds the samples (byte for byte the ones the
// binary carries) and embed.go: nothing there to move, so no warning. One
// edited sample is the user's own, and is.
func TestLegacyScriptsWarningSkipsSamples(t *testing.T) {
	_, cwd := isolate(t)
	resolved := t.TempDir()
	old := filepath.Join(cwd, "scripts")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(old, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, ex := range scripts.Examples() {
		write(ex.Name, ex.Text)
	}
	write("embed.go", "// Package scripts …\npackage scripts\n")
	if w := legacyScriptsWarning(resolved); w != "" {
		t.Errorf("samples only: warned %q", w)
	}
	ex := scripts.Examples()[0]
	write(ex.Name, ex.Text+"// mine now\n")
	if w := legacyScriptsWarning(resolved); !strings.Contains(w, "(1 .go") {
		t.Errorf("an edited sample: warning = %q, want one counting 1 file", w)
	}
}

func TestTildePath(t *testing.T) {
	home, _ := isolate(t)
	for in, want := range map[string]string{
		home: "~",
		filepath.Join(home, ".config/dbc/scripts"): "~/.config/dbc/scripts",
		home + "x/scripts":                         home + "x/scripts", // a sibling, not under home
		"/etc/dbc":                                 "/etc/dbc",
	} {
		if got := TildePath(in); got != want {
			t.Errorf("TildePath(%q) = %q, want %q", in, got, want)
		}
	}
}
