package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// Where Go scripts live. scripts_dir used to be used as written, and its
// default was "scripts", so it resolved against the working directory of
// whatever started dbc: ./scripts in a terminal, ~/scripts for dbc.app
// (whose helper starts in $HOME). Scripts now have one home, settled once
// at load and stored back in Config.ScriptsDir as an absolute path, so
// every reader (TUI, dbc web, headless) agrees whatever the cwd:
//
//	scripts_dir in config?
//	 ├─ no ─────────────────► ~/.config/dbc/scripts   (beside consoles/, history)
//	 └─ yes ── ${VAR}s expanded, then
//	     ├─ "~" or "~/…" ───► against $HOME
//	     ├─ absolute ───────► as is
//	     └─ relative ───────► joined to the config file's directory
//
// Relative to the config file, as tls_* paths are (see ExpandTLS): a
// checkout's ./dbc.toml with scripts_dir = "scripts" still means ./scripts,
// because that file is in cwd, while the same line in
// ~/.config/dbc/config.toml means ~/.config/dbc/scripts from anywhere.
//
// The directory is not created here: it appears on the first save, so a
// user who never writes a script never gets an empty one.

// legacyScriptsDir is the old cwd-relative default, kept as the fallback
// when no home directory is known (a stripped-down container, say), where
// there is nowhere better to look.
const legacyScriptsDir = "scripts"

// DefaultScriptsDir is where scripts live when the config does not say:
// ~/.config/dbc/scripts, or the old cwd-relative "scripts" without a home.
func DefaultScriptsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return legacyScriptsDir
	}
	return filepath.Join(home, ".config", "dbc", "scripts")
}

// ResolveScriptsDir turns scripts_dir as written into the directory dbc
// reads, by the rules above. base is the config file's directory ("" when
// there is no file, and then a relative value stays relative to cwd — it
// can only come from a test or a caller building a Config by hand). The
// warnings name unset ${VAR}s, as ExpandDSN's do.
func ResolveScriptsDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultScriptsDir()), nil
	}
	var warns []string
	p := os.Expand(raw, func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok {
			warns = append(warns, fmt.Sprintf(
				"scripts_dir references unset env var $%s (expanded to empty)", k))
		}
		return v
	})
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if base != "" && !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return absOr(filepath.Clean(p)), warns
}

// absOr makes p absolute when it can, so the path shown to the user (and
// handed to scripts) does not change meaning if anything later chdirs.
func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// FindScript turns the argument of `dbc script ARG` into a file to run:
//
//	ARG names an existing file (as a path from cwd) ─► that file, as before
//	ARG has no path separator ─► ScriptsDir/ARG, then ScriptsDir/ARG.go
//	otherwise ─► an error naming both places looked
//
// A file in cwd wins over a script of the same name so that every command
// line that worked before keeps running the same file; the lookup only
// adds meaning to arguments that used to fail.
func (c *Config) FindScript(arg string) (string, error) {
	if st, err := os.Stat(arg); err == nil && !st.IsDir() {
		return arg, nil
	}
	if !strings.ContainsRune(arg, filepath.Separator) && !strings.ContainsRune(arg, '/') {
		names := []string{arg}
		if !strings.HasSuffix(arg, ".go") {
			names = append(names, arg+".go")
		}
		for _, name := range names {
			p := filepath.Join(c.ScriptsDir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
		}
		return "", fmt.Errorf("no script %q: not a file here, and not in %s", arg, c.ScriptsDir)
	}
	return "", fmt.Errorf("no script file %q", arg)
}

// legacyScriptsWarning explains a move nobody would otherwise notice: the
// cwd has a ./scripts with scripts in it — where dbc used to look — but
// scripts are now read from somewhere else that has none. Only that
// combination warns; once the user has scripts in the new place, or there
// is no ./scripts, nothing is said on every start.
//
// Two kinds of file in ./scripts do not count, since moving them would
// gain nothing: an exact copy of a sample the binary carries (a dbc
// checkout's ./scripts is the samples, which dbc now offers as Examples),
// and a Go file of another package (the checkout's scripts/embed.go).
func legacyScriptsWarning(resolved string) string {
	old := absOr(legacyScriptsDir)
	if old == resolved {
		return ""
	}
	infos, _ := userdata.ListScripts(old) // a missing ./scripts lists none
	own := 0
	for _, in := range infos {
		if src, err := os.ReadFile(filepath.Join(old, in.Name)); err == nil && !scripts.IsExample(src) {
			own++
		}
	}
	if own == 0 {
		return ""
	}
	if newFiles, _ := filepath.Glob(filepath.Join(resolved, "*.go")); len(newFiles) > 0 {
		return ""
	}
	return fmt.Sprintf("scripts are read from %s now, not ./scripts (%d .go files there): "+
		"move them, or set scripts_dir in the config", resolved, own)
}

// TildePath shortens a path under the home directory to "~/…" for display
// (a title, a log line), where the full /Users/name prefix is noise. Paths
// elsewhere come back unchanged; never pass the result to the filesystem.
func TildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}
