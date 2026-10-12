// Package tutorial is the interactive companion to
// docs/scripting-and-etl-tutorial.md: the lessons as data, the workspace
// files they write, and the checks that say whether a lesson worked.
// `dbc tutorial` (tutorialcmd.go) is its command line.
//
//	tutorial/
//	    tutorial.go     ◄─ types, the embedded workspace, init, detection
//	    lessons.go      ◄─ the lessons, one per doc section (§0 … §12)
//	    checks.go       ◄─ check helpers: row counts, files, clean runs
//	    flow.go         ◄─ show / run / check a lesson, the status list
//	    runner.go       ◄─ Runner: how a lesson runs dbc (a child process)
//	    workspace/      ◄─ the files, byte for byte as the doc shows them
//
// Everything a lesson does goes through dbc itself, run as a child process
// in the workspace (see Runner). So `--run` does exactly what the learner
// would type, and a check reads state the same way a learner would
// (`dbc -t json "SELECT …"`), not through dbc's internals.
package tutorial

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rohanthewiz/serr"
)

// The workspace files. Each .go file in it carries //go:build ignore, so
// `go build ./...` never compiles them; dbc interprets them.
// TestWorkspaceMatchesDoc keeps every file the doc shows equal to its code
// block there.
//
//go:embed workspace
var files embed.FS

// DocPath is the tutorial's Markdown, relative to the repo root.
const DocPath = "docs/scripting-and-etl-tutorial.md"

// DocURL is the doc on GitHub, for a binary installed without a checkout.
const DocURL = "https://github.com/rohanthewiz/dbc/blob/main/" + DocPath

// Lesson is one section of the tutorial.
type Lesson struct {
	N        int       // the doc's section number (§N)
	Title    string    // the doc's heading after "N. ", exactly
	Summary  string    // a few lines: what the lesson teaches
	Files    []string  // workspace files the lesson adds, slash-separated
	Commands []Command // what to type, in order
	Checks   []Check   // the state the lesson leaves; none = nothing to check
}

// Command is one dbc invocation a lesson runs: Args after "dbc".
type Command struct {
	Args []string
	// ExpectFail marks a command that fails on purpose to show something
	// (§4's copy into a full table shows the one-transaction load). Its
	// non-zero exit does not stop the lesson; a zero exit is reported.
	ExpectFail bool
	// Note says why the command is there; printed after it.
	Note string
}

// Check is one thing a lesson must leave behind.
type Check struct {
	What string             // "warehouse has 6 orders"
	Hint string             // what to do when it fails
	Test func(e *Env) error // nil = passed; the error says what was found
}

// Env is what a check or a run works in.
type Env struct {
	Ctx context.Context // canceled on Ctrl+C
	Dir string          // the workspace
	Dbc Runner          // runs dbc there
}

// Anchor is the lesson's heading anchor on GitHub ("6-pipelines-etl-as-data"):
// lower case, punctuation dropped, spaces to hyphens.
func (l Lesson) Anchor() string {
	var b strings.Builder
	for _, r := range strings.ToLower(fmt.Sprintf("%d. %s", l.N, l.Title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
		// everything else (".", ":", "`", ",") is dropped, as GitHub does
	}
	return b.String()
}

// Find returns lesson n.
func Find(n int) (Lesson, bool) {
	for _, l := range Lessons() {
		if l.N == n {
			return l, true
		}
	}
	return Lesson{}, false
}

// File returns an embedded workspace file by its slash path.
func File(name string) ([]byte, error) {
	bs, err := files.ReadFile("workspace/" + name)
	if err != nil {
		return nil, serr.Wrap(err, "op", "read tutorial file", "file", name)
	}
	return bs, nil
}

// FileNames lists every embedded workspace file, slash-separated.
func FileNames() []string {
	var out []string
	_ = fs.WalkDir(files, "workspace", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, strings.TrimPrefix(p, "workspace/"))
		}
		return nil
	})
	return out
}

// Written says what WriteFile did.
type Written int

const (
	Created  Written = iota // the file was not there
	Kept                    // it was there and stays (no force)
	Replaced                // it was there and force replaced it
)

// WriteFile writes the embedded file name into dir. A file already there
// is the learner's and is kept, unless force, so their edits survive a
// second --run.
func WriteFile(dir, name string, force bool) (Written, error) {
	bs, err := File(name)
	if err != nil {
		return Kept, err
	}
	dst := filepath.Join(dir, filepath.FromSlash(name))
	was := Created
	if _, err := os.Stat(dst); err == nil {
		if !force {
			return Kept, nil
		}
		was = Replaced
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return Kept, serr.Wrap(err, "op", "make directory", "file", dst)
	}
	if err := os.WriteFile(dst, bs, 0o644); err != nil {
		return Kept, serr.Wrap(err, "op", "write tutorial file", "file", dst)
	}
	return was, nil
}

// Dirs are the workspace's directories, made by Init so dbc's listings
// (`dbc scripts`, `dbc pipelines`) have somewhere to look from the start.
// They match the *_dir settings in workspace/dbc.toml.
var Dirs = []string{"scripts", "pipelines", "jobs", "plugins", "data", "runs"}

// Init makes dir a tutorial workspace: lesson 0's files and the empty
// directories. It reports each file on w.
func Init(dir string, force bool, w io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return serr.Wrap(err, "op", "make workspace", "dir", dir)
	}
	for _, d := range Dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return serr.Wrap(err, "op", "make workspace", "dir", d)
		}
	}
	l, _ := Find(0)
	for _, name := range l.Files {
		was, err := WriteFile(dir, name, force)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, writtenLine(was, name))
	}
	return nil
}

// writtenLine reports one WriteFile: + written, ↻ replaced, ✓ kept.
func writtenLine(was Written, name string) string {
	switch was {
	case Created:
		return "  + " + name
	case Replaced:
		return "  ↻ " + name + "   (replaced: --force)"
	}
	return "  ✓ " + name + "   (there already: kept)"
}

// ErrNotWorkspace says dir is not a tutorial workspace.
var ErrNotWorkspace = errors.New("not a tutorial workspace")

// CheckWorkspace says whether dir is a tutorial workspace: a dbc.toml that
// defines the two connections every lesson uses. Any other dbc.toml is
// someone's real config, and a lesson must not run against it. A plain
// error wrapping ErrNotWorkspace, since its text is shown to the learner;
// a config that can't be read is a real failure and gets serr's fields.
func CheckWorkspace(dir string) error {
	p := filepath.Join(dir, "dbc.toml")
	var cfg struct {
		Connection []struct{ Name string } `toml:"connection"`
	}
	if _, err := toml.DecodeFile(p, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s has no dbc.toml", ErrNotWorkspace, dir)
		}
		return serr.Wrap(err, "op", "read workspace config", "file", p)
	}
	have := map[string]bool{}
	for _, c := range cfg.Connection {
		have[c.Name] = true
	}
	for _, want := range []string{"shop", "warehouse"} {
		if !have[want] {
			return fmt.Errorf("%w: %s's dbc.toml has no %s connection", ErrNotWorkspace, dir, want)
		}
	}
	return nil
}
