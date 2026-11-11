package main

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/logger"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
)

// N-267: a headless export can go past max_rows from the command line, a
// failed write to stdout fails the run, and a run that lands on the demo
// for want of a config file says so.

// --max-rows N is the run's max_rows; 0 lifts the cap; a negative number
// is a usage error; not given, the config's stands.
func TestMaxRowsFlag(t *testing.T) {
	parse := func(argv ...string) error {
		t.Helper()
		app := newCLI()
		app.Action = func(context.Context, *cli.Command) error { return nil }
		return app.Run(context.Background(), append([]string{"dbc"}, argv...))
	}
	for _, c := range []struct {
		argv []string
		want int
	}{
		{[]string{"SELECT 1"}, 1000},
		{[]string{"--max-rows", "5000", "SELECT 1"}, 5000},
		{[]string{"SELECT 1", "--max-rows", "0"}, math.MaxInt},
		{[]string{"--max-rows=1", "SELECT 1"}, 1},
	} {
		resetFlags(t)
		if err := parse(c.argv...); err != nil {
			t.Fatalf("%v: %v", c.argv, err)
		}
		cfg := &config.Config{MaxRows: 1000}
		applyMaxRows(cfg)
		if cfg.MaxRows != c.want {
			t.Errorf("%v: max rows %d, want %d", c.argv, cfg.MaxRows, c.want)
		}
	}
	resetFlags(t)
	if err := parse("--max-rows", "-1", "SELECT 1"); err == nil || !strings.Contains(err.Error(), "0 (no cap) or more") {
		t.Errorf("--max-rows -1: %v, want a usage error", err)
	}
}

// A headless CSV export of 1,500 rows: cut at the config's 1,000, whole
// with --max-rows 0.
func TestMaxRowsExport(t *testing.T) {
	const sql = `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500) SELECT i FROM n`
	export1500 := func(maxRows string) int {
		t.Helper()
		resetFlags(t)
		if maxRows != "" {
			flagMaxRows, flagMaxRowsSet = 0, true
		}
		cfg := &config.Config{MaxRows: 1000, DefaultConnection: "lite", Connections: []config.Connection{{
			Name: "lite", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "x.db")}}}
		applyMaxRows(cfg)
		mgr := db.NewManager(cfg)
		defer mgr.Close()
		out := filepath.Join(t.TempDir(), "n.csv")
		setFlag(t, &flagOut, out)
		runQueryHeadless(cfg, mgr, sql, export.CSV)
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(b), "\n") - 1 // the header
	}
	if n := export1500(""); n != 1000 {
		t.Errorf("with the config's cap: %d rows, want 1000", n)
	}
	if n := export1500("0"); n != 1500 {
		t.Errorf("with --max-rows 0: %d rows, want all 1500", n)
	}
}

// A write to stdout that fails ends the run with status 1 and says why;
// it used to exit 0 with a short file (`> /full/disk/x.csv`). Run in a
// child process, since fail exits: there stdout is a file opened
// read-only, so every write to it fails.
func TestStdoutWriteFailureFails(t *testing.T) {
	if os.Getenv("DBC_TEST_STDOUT_FAILS") == "1" {
		logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"}) // as main does; fail closes it
		f, err := os.Open(os.DevNull)
		if err != nil {
			os.Exit(3)
		}
		os.Stdout = f
		writeOut("i\n1\n", nil)
		os.Exit(0) // the write was not checked
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdoutWriteFailureFails$")
	cmd.Env = append(os.Environ(), "DBC_TEST_STDOUT_FAILS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("child: %v (stderr %q), want exit status 1", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "write failed") {
		t.Errorf("stderr %q does not say the write failed", stderr.String())
	}
}

// demoNote speaks only when a run lands on a demo for want of a config
// file, and names where dbc looked.
func TestDemoNote(t *testing.T) {
	demo := &config.Config{Demo: true, Searched: []string{"/w/dbc.toml", "/h/.config/dbc/config.toml"},
		Connections: []config.Connection{{Name: "demo", Demo: true}, {Name: "saved"}}}
	resetFlags(t)
	got := demoNote(demo, "demo")
	for _, want := range []string{"no config file found", "/w/dbc.toml, /h/.config/dbc/config.toml", `"demo"`, "--config"} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q lacks %q", got, want)
		}
	}
	if s := demoNote(demo, "saved"); s != "" {
		t.Errorf("a saved (non-demo) connection: %q, want nothing", s)
	}
	if s := demoNote(&config.Config{Connections: demo.Connections}, "demo"); s != "" {
		t.Errorf("with a config file: %q, want nothing", s)
	}
	for _, f := range []*string{&flagDemo, &flagConn, &flagDSN} {
		resetFlags(t)
		*f = "x"
		if s := demoNote(demo, "demo"); s != "" {
			t.Errorf("the demo asked for (a flag set): %q, want nothing", s)
		}
	}
}

// With no config file, Load records where it looked, absolute.
func TestLoadRecordsWhereItLooked(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", dir)
	cfg, err := config.LoadDemo("", config.DemoDefault)
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd() // the temp dir as the OS spells it (/private/var on macOS)
	want := []string{filepath.Join(wd, "dbc.toml"), filepath.Join(dir, ".config", "dbc", "config.toml")}
	if !cfg.Demo || strings.Join(cfg.Searched, "|") != strings.Join(want, "|") {
		t.Errorf("searched %q (demo %v), want %q", cfg.Searched, cfg.Demo, want)
	}
}
