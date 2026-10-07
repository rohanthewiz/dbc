package script

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sdb"
)

// runScript interprets src as a script against a Manager with two SQLite
// file connections, "a" (seeded with the demo cats) and "b" (empty), and
// returns its printed lines.
func runScript(t *testing.T, src string) (*db.Manager, []string, error) {
	t.Helper()
	dir := t.TempDir()
	mgr := db.NewManager(&config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(dir, "a.db")},
		{Name: "b", Driver: "sqlite", DSN: filepath.Join(dir, "b.db")},
	}})
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "a"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	var printed []string
	s := sdb.New(mgr, func(*model.Result) {}, func(line string) { printed = append(printed, line) })
	return mgr, printed, Run(path, s)
}

func query(t *testing.T, mgr *db.Manager, conn, q string) [][]string {
	t.Helper()
	r, err := mgr.Run(conn, q)
	if err != nil {
		t.Fatal(err)
	}
	return r.Rows
}

// The ETL API from inside the interpreter: a CopyOpts literal with an
// interpreted closure as its Transform, a Reader feeding a Writer across
// connections, and a Writer the script never closes — which Release must
// roll back, not commit.
func TestScriptETL(t *testing.T) {
	mgr, printed, err := runScript(t, `package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	st, err := s.Copy("a", "b", "cats", sdb.CopyOpts{
		Create: true, ProgressEvery: 4,
		Transform: func(row []any) ([]any, error) {
			if row[3].(int64) > 5 {
				return nil, nil
			}
			row[1] = strings.ToUpper(row[1].(string))
			return row, nil
		},
	})
	if err != nil {
		return err
	}
	s.Print("rows=%d skipped=%d", st.Rows, st.Skipped)

	rd, err := s.Reader("a", "SELECT id, breed FROM cats WHERE age >= ? ORDER BY id", 4)
	if err != nil {
		return err
	}
	defer rd.Close()
	w, err := s.Writer("b", "breeds", rd.Columns(), sdb.WriteOpts{
		Setup: []string{"CREATE TABLE breeds (id INTEGER PRIMARY KEY, breed TEXT)"},
	})
	if err != nil {
		return err
	}
	defer w.Abort()
	for rd.Next() {
		row := rd.Row()
		row[1] = strings.ToLower(row[1].(string))
		if err := w.Write(row); err != nil {
			return err
		}
	}
	if err := rd.Err(); err != nil {
		return err
	}
	n, err := w.Close()
	if err != nil {
		return err
	}
	s.Print("breeds=%d", n)

	// Left open on purpose: Release must roll this back.
	dangling, err := s.Writer("b", "breeds", []string{"id", "breed"}, sdb.WriteOpts{Truncate: true})
	if err != nil {
		return err
	}
	return dangling.Write([]any{100, "dangling"})
}
`)
	if err != nil {
		t.Fatal(err)
	}
	// The DDL log: Copy's Create and the Writer's Setup, each before the
	// rows it readies. The dangling Writer's Truncate is a DELETE on SQLite
	// (data, not DDL), so it adds no line. The CREATE's column list is
	// createDDL's business, so only its head is checked.
	if len(printed) != 5 || !strings.HasPrefix(printed[0], `DDL b: CREATE TABLE IF NOT EXISTS "cats" (`) {
		t.Fatalf("printed %q", printed)
	}
	wantLog := "  a → b: 4 rows|rows=6 skipped=2|DDL b: CREATE TABLE breeds (id INTEGER PRIMARY KEY, breed TEXT)|breeds=4"
	if got := strings.Join(printed[1:], "|"); got != wantLog {
		t.Errorf("printed %q, want %q after the Copy's CREATE", got, wantLog)
	}
	if got := query(t, mgr, "b", "SELECT name FROM cats ORDER BY id"); len(got) != 6 || got[0][0] != "WHISKERS" {
		t.Errorf("b.cats = %v", got)
	}
	got := query(t, mgr, "b", "SELECT id, breed FROM breeds ORDER BY id")
	want := "[[3 maine coon] [5 bengal] [6 siamese] [8 maine coon]]"
	if s := fmtRows(got); s != want {
		t.Errorf("b.breeds = %s, want %s (the dangling truncate must have rolled back)", s, want)
	}
}

// Every DDL statement a script runs through Query or Exec is logged before
// it runs, and only DDL: a SELECT or INSERT adds no line, a CREATE behind a
// comment or after an INSERT in one Exec does, and a DROP that fails gets a
// "failed" line even though the script carries on past the error.
func TestScriptLogsDDL(t *testing.T) {
	mgr, printed, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	if _, err := s.Exec("a", "-- scratch\nCREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)"); err != nil {
		return err
	}
	if _, err := s.Query("a", "SELECT count(*) FROM cats"); err != nil {
		return err
	}
	if _, err := s.Exec("a", "INSERT INTO notes VALUES (1, 'x'); ALTER TABLE notes ADD COLUMN at TEXT"); err != nil {
		return err
	}
	if _, err := s.Exec("a", "DROP TABLE nope"); err == nil {
		s.Print("dropped a table that does not exist")
	}
	s.Print("done")
	return nil
}
`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DDL a: -- scratch\nCREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)",
		"DDL a: ALTER TABLE notes ADD COLUMN at TEXT",
		"DDL a: DROP TABLE nope",
	}
	if len(printed) != 5 || strings.Join(printed[:3], "|") != strings.Join(want, "|") {
		t.Fatalf("printed %q, want %q then a failed line and done", printed, want)
	}
	if !strings.HasPrefix(printed[3], "DDL a failed: ") || !strings.Contains(printed[3], "nope") || printed[4] != "done" {
		t.Errorf("after the DROP: %q", printed[3:])
	}
	// both halves of the two-statement Exec ran
	if got := query(t, mgr, "a", "SELECT id, body, at FROM notes"); fmtRows(got) != "[[1 x NULL]]" {
		t.Errorf("notes = %v", got)
	}

	// The log belongs to running a script: a session a host drives itself,
	// as dbc copy does, has it off.
	var direct []string
	s := sdb.New(mgr, nil, func(line string) { direct = append(direct, line) })
	if _, err = s.Exec("a", "CREATE TABLE quiet (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 0 {
		t.Errorf("a session outside script.Run logged %q", direct)
	}
}

func fmtRows(rows [][]string) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		parts[i] = "[" + strings.Join(r, " ") + "]"
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// A script that panics with a Writer open still has it rolled back.
func TestScriptPanicReleasesWriter(t *testing.T) {
	mgr, _, err := runScript(t, `package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	if _, err := s.Copy("a", "b", "cats", sdb.CopyOpts{Create: true}); err != nil {
		return err
	}
	w, err := s.Writer("b", "cats", []string{"id", "name", "breed", "age", "adopted"}, sdb.WriteOpts{Truncate: true})
	if err != nil {
		return err
	}
	_ = w.Write([]any{50, "x", "y", 1, true})
	panic("boom")
}
`)
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want the panic", err)
	}
	if got := query(t, mgr, "b", "SELECT count(*) FROM cats"); got[0][0] != "8" {
		t.Errorf("b.cats has %s rows after the panic, want the 8 copied before it", got[0][0])
	}
}

// RunSource runs text with no file behind it (a built-in example), past
// the //go:build ignore line every sample carries, and fails to compile as
// a file would. (Its errors carry name in the serr "script" field, which
// the headless log prints.)
func TestRunSource(t *testing.T) {
	mgr := db.NewManager(&config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "a.db")},
	}})
	t.Cleanup(mgr.Close)
	var printed []string
	s := sdb.New(mgr, func(*model.Result) {}, func(line string) { printed = append(printed, line) })
	const hdr = "//go:build ignore\n\npackage main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n"
	if err := RunSource("example:hi.go", hdr+"func Run(s *sdb.S) error { s.Print(\"hi\"); return nil }\n", s); err != nil {
		t.Fatal(err)
	}
	if len(printed) != 1 || printed[0] != "hi" {
		t.Errorf("printed %q, want [hi]", printed)
	}
	err := RunSource("example:bad.go", hdr+"func Run(s *sdb.S) error { return s.Nope() }\n", s)
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Errorf("err = %v, want the compile error for s.Nope", err)
	}
}
