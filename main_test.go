package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rohanthewiz/serr"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// newTestManager builds a manager over a private in-memory SQLite seeded with
// the demo data, so each test runs against its own cats table.
func newTestManager(t *testing.T) *db.Manager {
	t.Helper()

	cfg := &config.Config{
		MaxRows:           1000,
		DefaultConnection: "demo",
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) +
				"?mode=memory&cache=shared",
		}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "demo"); err != nil {
		t.Fatalf("seed demo: %v", err)
	}
	return mgr
}

// newTestSession pins a connection on the seeded demo database.
func newTestSession(t *testing.T) *db.Session {
	t.Helper()

	sess, err := newTestManager(t).Session(context.Background(), "demo")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestRunStatementsInOrder(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`
		INSERT INTO cats (name, breed, age) VALUES ('Zed', 'Tabby', 4);
		-- a comment between statements is not a statement
		SELECT name FROM cats WHERE name = 'Zed';
		SELECT count(*) AS n FROM cats;
	`)
	if len(stmts) != 3 {
		t.Fatalf("split gave %d statements, want 3", len(stmts))
	}
	results, err := runStatements(context.Background(), sess, stmts)
	if err != nil {
		t.Fatalf("runStatements: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if !results[0].IsExec || results[0].Affected != 1 {
		t.Errorf("insert result = %+v, want one row affected", results[0])
	}
	// the SELECT must see the INSERT that ran before it
	if len(results[1].Rows) != 1 || results[1].Rows[0][0] != "Zed" {
		t.Errorf("select saw %v, want the inserted cat", results[1].Rows)
	}
	if results[2].Rows[0][0] != "9" {
		t.Errorf("count = %s, want 9 (8 seeded + 1)", results[2].Rows[0][0])
	}
}

// The statements share one session, so a transaction spanning them holds —
// the rollback must undo the insert two statements back.
func TestRunStatementsShareOneTransaction(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`
		BEGIN;
		INSERT INTO cats (name, breed, age) VALUES ('Ghost', 'Tabby', 2);
		ROLLBACK;
		SELECT count(*) AS n FROM cats WHERE name = 'Ghost';
	`)
	results, err := runStatements(context.Background(), sess, stmts)
	if err != nil {
		t.Fatalf("runStatements: %v", err)
	}
	if n := results[len(results)-1].Rows[0][0]; n != "0" {
		t.Errorf("found %s rolled-back cats, want 0 — the statements did not share a session", n)
	}
}

// A failure stops the run there, but the results already collected come back
// so the caller can still render them.
func TestRunStatementsStopsAtFirstError(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT * FROM no_such_table; SELECT 3 AS n`)
	results, err := runStatements(context.Background(), sess, stmts)
	if err == nil {
		t.Fatal("expected an error from the missing table")
	}
	if !strings.Contains(err.Error(), "no_such_table") {
		t.Errorf("error lost its cause: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want the one that succeeded", len(results))
	}
	if results[0].Rows[0][0] != "1" {
		t.Errorf("kept the wrong result: %v", results[0].Rows)
	}
}

// A result that hit max_rows must announce itself on stderr — a truncated
// export must not look complete.
func TestWarnTruncated(t *testing.T) {
	full := &model.Result{Rows: [][]string{{"a"}}}
	cut := &model.Result{Rows: [][]string{{"a"}, {"b"}}, Truncated: true}

	var sb strings.Builder
	warnTruncated(&sb, []*model.Result{full})
	if sb.Len() != 0 {
		t.Errorf("untruncated result warned: %q", sb.String())
	}

	sb.Reset()
	warnTruncated(&sb, []*model.Result{cut})
	if got := sb.String(); !strings.Contains(got, "truncated at 2 rows") {
		t.Errorf("single-result note = %q, want the row count", got)
	}

	sb.Reset()
	warnTruncated(&sb, []*model.Result{full, cut})
	if got := sb.String(); !strings.Contains(got, "statement 2/2") {
		t.Errorf("multi-result note = %q, want the statement position", got)
	}
}

// A headless script must render through --format and land where -o says, not in the
// hardcoded text table it once always printed.
func TestScriptHeadlessHonorsFormatAndOutfile(t *testing.T) {
	mgr := newTestManager(t)
	out := filepath.Join(t.TempDir(), "cats.csv")
	setFlag(t, &flagOut, out)

	runScriptHeadless(mgr, "testdata/show_two.go", export.CSV)

	bs, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read -o file: %v", err)
	}
	got := string(bs)
	// two shown results, each its own CSV block with a header row
	if n := strings.Count(got, "name\n"); n != 2 {
		t.Errorf("got %d header rows, want one per shown result:\n%s", n, got)
	}
	for _, want := range []string{"Whiskers", "Oliver", "Luna", "Milo"} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
	// and the script's Print output stayed out of the data
	if strings.Contains(got, "->") {
		t.Errorf("script log output leaked into the data stream:\n%s", got)
	}
}

// scriptLog keeps s.Print off stdout only when it would corrupt what is
// already there: a machine-readable format writing to stdout.
func TestScriptLogDestination(t *testing.T) {
	cases := []struct {
		name    string
		outFile string
		format  export.Format
		want    *os.File
	}{
		{"text to stdout shares it", "", export.Text, os.Stdout},
		{"csv to stdout takes it", "", export.CSV, os.Stderr},
		{"csv to a file frees stdout", "out.csv", export.CSV, os.Stdout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setFlag(t, &flagOut, c.outFile)
			if got := scriptLog(c.format); got != c.want {
				t.Errorf("scriptLog = %v, want %v", got, c.want)
			}
		})
	}
}

// setFlag overrides a string flag for one test and restores it after.
func setFlag(t *testing.T, f *string, v string) {
	t.Helper()
	old := *f
	*f = v
	t.Cleanup(func() { *f = old })
}

// A canceled run stays recognizable as a cancellation, so main exits 130
// rather than reporting a query failure.
func TestRunStatementsCanceled(t *testing.T) {
	sess := newTestSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runStatements(ctx, sess, sqlsplit.Split("SELECT 1; SELECT 2"))
	if !errors.Is(err, db.ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	var se *serr.SErr
	if !errors.As(err, &se) || se.FieldsMap()["statement"] != "1/2" {
		t.Errorf("error should name the statement that stopped: %v", err)
	}
}

// sqlInput is the whole headless-or-TUI decision, so every row of its table
// is pinned here: argument, --file, "-", piped stdin, nothing, and the
// combinations that are refused.
func TestSQLInput(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "q.sql")
	if err := os.WriteFile(file, []byte("SELECT 1 FROM file"), 0644); err != nil {
		t.Fatal(err)
	}
	const piped = "SELECT 1 FROM stdin"

	cases := []struct {
		name         string
		args         []string
		file         string
		stdinPiped   bool
		wantSQL      string
		wantHeadless bool
		wantErr      string
	}{
		{"argument", []string{"SELECT 1"}, "", false, "SELECT 1", true, ""},
		{"argument wins over piped stdin", []string{"SELECT 1"}, "", true, "SELECT 1", true, ""},
		{"file", nil, file, false, "SELECT 1 FROM file", true, ""},
		{"dash reads stdin", nil, "-", false, piped, true, ""},
		{"piped stdin", nil, "", true, piped, true, ""},
		{"nothing opens the TUI", nil, "", false, "", false, ""},
		{"argument and file", []string{"SELECT 1"}, file, false, "", false, "not both"},
		{"unquoted SQL", []string{"SELECT", "*", "FROM", "t"}, "", false, "", false, "4 arguments"},
		{"missing file", nil, filepath.Join(dir, "nope.sql"), false, "", false, "no such file"},
		{"old -f csv habit", nil, "csv", false, "", false, "use -t csv"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, headless, err := sqlInput(c.args, c.file, strings.NewReader(piped), c.stdinPiped)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if sql != c.wantSQL || headless != c.wantHeadless {
				t.Errorf("got (%q, %v), want (%q, %v)", sql, headless, c.wantSQL, c.wantHeadless)
			}
		})
	}
}

// The cli wiring: short and long spellings land in the same variable, flags
// may follow the SQL, root flags reach the subcommands (persistent), and the
// migrate verbs arrive untouched. The actions are swapped for recorders so
// nothing connects or exits.
func TestCLIParsing(t *testing.T) {
	type got struct {
		action              string
		args                []string
		file, format, conn  string
		out, dir, dsn, demo string
		missing             bool
	}
	cases := []struct {
		name string
		argv []string
		want got
	}{
		{"short flags after the SQL", []string{"SELECT 1", "-t", "csv", "-c", "pg"},
			got{action: "root", args: []string{"SELECT 1"}, format: "csv", conn: "pg"}},
		{"long flags", []string{"--file", "q.sql", "--format", "json", "--conn", "pg", "--out", "r.json"},
			got{action: "root", file: "q.sql", format: "json", conn: "pg", out: "r.json"}},
		{"single-dash long names still parse", []string{"-dsn", "x", "-demo", "sqlite", "SELECT 1"},
			got{action: "root", args: []string{"SELECT 1"}, format: "text", dsn: "x", demo: "sqlite"}},
		{"-f - is stdin, not the end of flags", []string{"-f", "-", "-t", "tsv"},
			got{action: "root", file: "-", format: "tsv"}},
		{"goose word order for migrate", []string{"--dir", "db/migrate", "--allow-missing", "migrate", "up"},
			got{action: "migrate", args: []string{"up"}, format: "text", dir: "db/migrate", missing: true}},
		{"root flags after the subcommand", []string{"migrate", "down-to", "0", "-t", "json"},
			got{action: "migrate", args: []string{"down-to", "0"}, format: "json"}},
		{"script", []string{"-o", "r.csv", "script", "s.go"},
			got{action: "script", args: []string{"s.go"}, format: "text", out: "r.csv"}},
		{"-- keeps a leading comment as SQL", []string{"--", "-- note\nSELECT 1"},
			got{action: "root", args: []string{"-- note\nSELECT 1"}, format: "text"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetFlags(t)
			var g got
			record := func(name string) cli.ActionFunc {
				return func(_ context.Context, cmd *cli.Command) error {
					g = got{action: name, args: cmd.Args().Slice(), file: flagFile, format: flagFormat,
						conn: flagConn, out: flagOut, dir: flagDir, dsn: flagDSN, demo: flagDemo,
						missing: flagMissing}
					if len(g.args) == 0 {
						g.args = nil
					}
					return nil
				}
			}
			app := newCLI()
			app.Action = record("root")
			for _, sub := range app.Commands {
				sub.Action = record(sub.Name)
			}
			if err := app.Run(context.Background(), append([]string{"dbc"}, c.argv...)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if c.want.format == "" {
				c.want.format = "text"
			}
			if !reflect.DeepEqual(g, c.want) {
				t.Errorf("got  %+v\nwant %+v", g, c.want)
			}
		})
	}
}

// resetFlags zeroes the flag variables for one test and restores them after,
// since newCLI writes into package globals. DBC_DEMO is cleared so the
// environment cannot leak into --demo.
func resetFlags(t *testing.T) {
	t.Helper()
	t.Setenv("DBC_DEMO", "")
	ptrs := []*string{&flagConfig, &flagConn, &flagFile, &flagFormat, &flagOut,
		&flagDemo, &flagDriver, &flagDSN, &flagDir}
	for _, p := range ptrs {
		setFlag(t, p, "")
	}
	old := flagMissing
	flagMissing = false
	t.Cleanup(func() { flagMissing = old })
}
