package main

import (
	"regexp"
	"slices"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/version"
)

// catsManifest is the slice of cats-plugin.toml these tests check. The cats
// host owns the full schema (cats/internal/plugin/manifest.go); decoding only
// the keys we assert on keeps this test from breaking when the host adds one.
type catsManifest struct {
	ID          string `toml:"id"`
	Version     string `toml:"version"`
	Bin         []string
	Completions []struct {
		Binary      string   `toml:"binary"`
		Subcommands []string `toml:"subcommands"`
		Flags       []string `toml:"flags"`
	} `toml:"completions"`
}

func loadCatsManifest(t *testing.T) catsManifest {
	t.Helper()
	var m catsManifest
	if _, err := toml.DecodeFile("cats-plugin.toml", &m); err != nil {
		t.Fatalf("cats-plugin.toml: %v", err)
	}
	return m
}

// TestVersion_IsSemver guards the shape the release workflow's auto-bump
// parses: a bare x.y.z, no "v" prefix and no pre-release suffix.
func TestVersion_IsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version.Version) {
		t.Fatalf("version.Version = %q, want x.y.z", version.Version)
	}
}

// TestVersion_MatchesCatsManifest keeps the manifest honest. The cats host does
// not compare the two numbers, so nothing on screen would explain a manifest
// advertising a different version from the binary it builds. Release CI bumps
// both in one commit; this catches a hand edit that touched only one.
func TestVersion_MatchesCatsManifest(t *testing.T) {
	m := loadCatsManifest(t)
	if m.Version != version.Version {
		t.Fatalf("cats-plugin.toml version = %q, version.Version = %q — edit both", m.Version, version.Version)
	}
}

// TestCatsManifest_CompletionsCoverCLI checks the static [[completions]] entry
// against the real command tree. The manifest lists candidates by hand
// (dbc has no completion code for catctl to call), and that list fell behind
// newCLI once already (--tx, --keep-going and explain's --theme were missing).
//
// Every subcommand must be offered, and every flag name and alias — from the
// root and from each subcommand, since catctl's static style offers flags in
// any word position — must appear with its dashes. The converse is checked
// too, so a flag that is renamed or dropped does not linger as a completion
// the CLI then rejects.
func TestCatsManifest_CompletionsCoverCLI(t *testing.T) {
	m := loadCatsManifest(t)
	if len(m.Completions) != 1 || m.Completions[0].Binary != "dbc" {
		t.Fatalf("want exactly one [[completions]] entry for binary dbc, got %+v", m.Completions)
	}
	comp := m.Completions[0]

	root := newCLI()
	// flagWords spells a flag the way a user types it: one dash for a
	// single-letter name, two otherwise (the cli package accepts both, but
	// that is how its help prints them and how the manifest lists them).
	var want []string
	flagWords := func(flags []cli.Flag) {
		for _, f := range flags {
			for _, n := range f.Names() {
				if len(n) == 1 {
					want = append(want, "-"+n)
				} else {
					want = append(want, "--"+n)
				}
			}
		}
	}
	flagWords(root.Flags)
	var subs []string
	for _, c := range root.Commands {
		subs = append(subs, c.Name)
		flagWords(c.Flags)
	}
	// Added by the cli package itself rather than declared in newCLI: --help
	// on every command, and --version/-v because the root sets Version.
	want = append(want, "--help", "--version", "-v")

	for _, s := range subs {
		if !slices.Contains(comp.Subcommands, s) {
			t.Errorf("completions: subcommand %q is missing", s)
		}
	}
	for _, s := range comp.Subcommands {
		if !slices.Contains(subs, s) {
			t.Errorf("completions: subcommand %q is not a dbc command", s)
		}
	}
	for _, f := range want {
		if !slices.Contains(comp.Flags, f) {
			t.Errorf("completions: flag %q is missing", f)
		}
	}
	for _, f := range comp.Flags {
		if !slices.Contains(want, f) {
			t.Errorf("completions: flag %q is not a dbc flag", f)
		}
	}
}

// TestCatsManifest_BinMatchesBuild pins the plugin's PATH link to the file the
// [[build]] step writes: a mismatch installs cleanly and then leaves
// ~/.cats/bin/dbc dangling.
func TestCatsManifest_BinMatchesBuild(t *testing.T) {
	m := loadCatsManifest(t)
	if m.ID != "rohanthewiz.dbc" {
		t.Errorf("id = %q, want rohanthewiz.dbc", m.ID)
	}
	if !slices.Equal(m.Bin, []string{"./bin/dbc"}) {
		t.Errorf("bin = %v, want [./bin/dbc] (what [[build]] produces)", m.Bin)
	}
}
