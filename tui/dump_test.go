package tui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pgdump"
)

// withPGConn adds a Postgres connection "pg" to the model's config, at a
// port nothing listens on, and returns the model with HOME at a temp dir
// (so the form's suggested path is predictable and nothing is written to
// the real home).
func withPGConn(t *testing.T) *Model {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := newTestModel(t)
	m.cfg.Connections = append(m.cfg.Connections,
		config.Connection{Name: "pg", Driver: "postgres", DSN: "host=127.0.0.1 port=1 dbname=app user=me"})
	m.refreshConns()
	m.render()
	return m
}

// The connections menu offers "Dump database…" on a Postgres connection,
// and on another engine dims it with the reason.
func TestDumpMenuRow(t *testing.T) {
	m := withPGConn(t)
	x, y := connRowAt(t, m, config.DemoSQLite)
	rightClick(t, m, x, y)
	if it := menuRow(t, m, "⇩ Dump database"); !strings.Contains(it.why, "Postgres") {
		t.Fatalf("SQLite row: why %q", it.why)
	}
	m.menu = nil

	x, y = connRowAt(t, m, "pg")
	rightClick(t, m, x, y)
	if it := menuRow(t, m, "⇩ Dump database"); it.why != "" {
		t.Fatalf("pg row disabled: %q", it.why)
	}
	pickMenu(t, m, "Dump database")
	if _, ok := m.modal.(*dumpForm); !ok {
		t.Fatalf("no dump form; modal %T", m.modal)
	}

	// while a dump runs: one at a time, and Stop is offered
	m.modal, m.dumpBusy = nil, "pg"
	rightClick(t, m, x, y)
	if it := menuRow(t, m, "⇩ Dump database"); !strings.Contains(it.why, "still running") {
		t.Fatalf("busy: why %q", it.why)
	}
	menuRow(t, m, "■ Stop dump of pg")
	m.menu, m.dumpBusy = nil, ""
}

// The form suggests a file named for the connection, follows the format
// with its suffix and its controls, and refuses what does not read.
func TestDumpFormFieldsAndRefusals(t *testing.T) {
	m := withPGConn(t)
	m.openDumpForm("pg")
	df := m.modal.(*dumpForm)
	out := df.fields[dfOut].Text()
	if !strings.HasPrefix(out, filepath.Join(os.Getenv("HOME"), "pg-")) || !strings.HasSuffix(out, ".sql") {
		t.Fatalf("suggested %q", out)
	}
	c := frame(m)
	findText(t, c, "Dump pg")
	findText(t, c, "no owners")

	df.focus = dfFormat
	key(t, m, "right") // custom
	if df.format != pgdump.Custom || !strings.HasSuffix(df.fields[dfOut].Text(), ".dump") {
		t.Fatalf("custom: %s %q", df.format, df.fields[dfOut].Text())
	}
	// an archive takes owner/create/clean at restore: the boxes go
	if strings.Contains(frame(m).String(), "no owners") {
		t.Fatal("no-owner shown for an archive")
	}
	key(t, m, "right") // directory
	if !strings.HasSuffix(df.fields[dfOut].Text(), "-dir") {
		t.Fatalf("directory: %q", df.fields[dfOut].Text())
	}
	findText(t, frame(m), "Jobs")

	df.fields[dfJobs].SetText("lots")
	key(t, m, "enter")
	if !strings.Contains(df.msg, "jobs") || df.msgKind != logErr || m.modal != df {
		t.Fatalf("bad jobs: %q", df.msg)
	}

	// a server that is not there: said in the form, which stays open
	df.fields[dfJobs].SetText("")
	key(t, m, "enter")
	if m.modal != df || df.msgKind != logErr || df.busy {
		t.Fatalf("unreachable server: modal %T, msg %q", m.modal, df.msg)
	}
	if m.dumpBusy != "" {
		t.Fatal("a dump started")
	}
}

// A prepared dump closes the form, runs, and logs each step and the
// outcome; the running mark is cleared when it ends.
func TestDumpRunsAndLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake pg_dump is a shell script")
	}
	m := withPGConn(t)
	dir := t.TempDir()
	tool := filepath.Join(dir, "pg_dump")
	script := `#!/bin/sh
for a in "$@"; do case "$a" in --file=*) out="${a#--file=}";; esac; done
echo "pg_dump: warning: something mild" >&2
echo "-- dumped" > "$out"
`
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "app.sql")

	m.openDumpForm("pg")
	df := m.modal.(*dumpForm)
	df.seq = 7
	run := &pgdump.Run{Name: "pg", ServerMajor: 17, Tools: pgdump.Tools{Dump: tool, Version: "17.2"},
		Opts: pgdump.Options{Format: pgdump.Plain, Out: out}}
	drive(t, m, dumpPreparedMsg{seq: 7, run: run})

	if m.modal != nil {
		t.Fatalf("form still open: %T", m.modal)
	}
	if m.dumpBusy != "" || m.dumpCancel != nil {
		t.Fatalf("still marked running: %q", m.dumpBusy)
	}
	log := logText(m)
	for _, want := range []string{"dumping pg (PostgreSQL 17) as plain to " + out, "pg_dump: warning: something mild",
		"dumped pg to " + out, "psql -X -d TARGET -f " + out} {
		if !strings.Contains(log, want) {
			t.Fatalf("log lacks %q:\n%s", want, log)
		}
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "-- dumped\n" {
		t.Fatalf("out: %q %v", b, err)
	}
	if m.dumpLastDir != dir {
		t.Fatalf("last dir %q", m.dumpLastDir)
	}
	// an answer for a press that is not the latest is dropped
	m.openDumpForm("pg")
	m.modal.(*dumpForm).seq = 9
	drive(t, m, dumpPreparedMsg{seq: 8, run: run})
	if _, ok := m.modal.(*dumpForm); !ok || m.dumpBusy != "" {
		t.Fatal("a stale answer started a dump")
	}
}
