package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	run, err := runStatements(context.Background(), sess, stmts, runHooks{})
	results := run.results
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
	run, err := runStatements(context.Background(), sess, stmts, runHooks{})
	results := run.results
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
	run, err := runStatements(context.Background(), sess, stmts, runHooks{})
	results := run.results
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

// --keep-going passes over a failed statement: the run goes on, the failure
// is reported with its position, and each result keeps its statement's
// place, so the output can say "3/3" rather than "2/2".
func TestRunStatementsKeepGoing(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT * FROM no_such_table; SELECT 3 AS n`)
	var failedAt []string
	h := runHooks{keepGoing: true, onFail: func(_ int, err error) {
		var se *serr.SErr
		if errors.As(err, &se) {
			failedAt = append(failedAt, se.FieldsMap()["statement"])
		}
	}}

	run, err := runStatements(context.Background(), sess, stmts, h)
	if err != nil {
		t.Fatalf("a passed-over failure stopped the run: %v", err)
	}
	if run.failed != 1 || !reflect.DeepEqual(failedAt, []string{"2/3"}) {
		t.Errorf("failed = %d, reported at %v; want 1, at [2/3]", run.failed, failedAt)
	}
	if !reflect.DeepEqual(run.at, []int{1, 3}) {
		t.Errorf("positions = %v, want [1 3]", run.at)
	}
	if len(run.results) != 2 || run.results[1].Rows[0][0] != "3" {
		t.Errorf("results = %v, want statements 1 and 3", run.results)
	}
}

// Ctrl+C means stop, --keep-going or not: a canceled run must not go on to
// the next statement.
func TestRunStatementsKeepGoingStopsOnCancel(t *testing.T) {
	sess := newTestSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	run, err := runStatements(ctx, sess, sqlsplit.Split("SELECT 1; SELECT 2"), runHooks{keepGoing: true})
	if !errors.Is(err, db.ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if run.failed != 0 {
		t.Errorf("a cancel was counted as a passed-over failure")
	}
}

// With a gap in the run, streamed and collected output still agree: both
// number each result by its statement.
func TestBlockStreamMatchesCollectedWithGap(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT * FROM no_such_table; SELECT 3 AS n`)
	var out, notes strings.Builder
	st := &blockStream{out: &out, notes: &notes, f: export.Text, total: len(stmts)}

	run, err := runStatements(context.Background(), sess, stmts,
		runHooks{onResult: st.add, keepGoing: true})
	if err != nil {
		t.Fatalf("runStatements: %v", err)
	}
	want, err := export.RenderRun(run.results, run.at, len(stmts), export.Text)
	if err != nil {
		t.Fatalf("RenderRun: %v", err)
	}
	if out.String() != want {
		t.Errorf("streamed output differs from collected:\n%s\n---\n%s", out.String(), want)
	}
	if !strings.Contains(want, "-- 3/3 │") {
		t.Errorf("the last result lost its statement position:\n%s", want)
	}
}

// txControl is what keeps --tx from being fought over: every statement that
// begins or ends a transaction is caught, and the ones that work inside one
// are let through.
func TestTxControl(t *testing.T) {
	cases := map[string]bool{
		"BEGIN":                          true,
		"begin immediate":                true,
		"START TRANSACTION":              true,
		"COMMIT":                         true,
		"END":                            true,
		"ROLLBACK":                       true,
		"ABORT":                          true,
		"PREPARE TRANSACTION 'x'":        true,
		"/* why */ commit":               true,
		"ROLLBACK TO SAVEPOINT a":        false,
		"ROLLBACK TO a":                  false,
		"SAVEPOINT a":                    false,
		"RELEASE SAVEPOINT a":            false,
		"PREPARE q AS SELECT 1":          false,
		"START REPLICA":                  false,
		"SELECT 'begin'":                 false,
		"UPDATE t SET commit = 1":        false,
		"INSERT INTO t VALUES ('abort')": false,
	}
	for stmt, want := range cases {
		if got := txControl(stmt); got != want {
			t.Errorf("txControl(%q) = %v, want %v", stmt, got, want)
		}
	}
}

// --tx refuses what it could not honor, before anything runs; without --tx
// a buffer may manage its own transaction, --keep-going or not.
func TestCheckRunFlags(t *testing.T) {
	plain := sqlsplit.Split("INSERT INTO t VALUES (1); SELECT 1")
	ownTx := sqlsplit.Split("BEGIN; INSERT INTO t VALUES (1); COMMIT")
	cases := []struct {
		name    string
		tx      bool
		keep    bool
		stmts   []sqlsplit.Stmt
		wantErr string
	}{
		{"no flags", false, false, ownTx, ""},
		{"--tx on plain statements", true, false, plain, ""},
		{"--keep-going on its own transaction", false, true, ownTx, ""},
		{"--tx with --keep-going", true, true, plain, "do not mix"},
		{"--tx over BEGIN", true, false, ownTx, "statement 1/3 (BEGIN)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setBoolFlag(t, &flagTx, c.tx)
			setBoolFlag(t, &flagKeep, c.keep)
			err := checkRunFlags(c.stmts)
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Errorf("err = %v, want one mentioning %q", err, c.wantErr)
			}
		})
	}
}

// endTx keeps the work of a run that succeeded and drops that of one that
// did not — including one stopped by Ctrl+C, whose context is already dead
// when the rollback has to run.
func TestEndTx(t *testing.T) {
	count := func(t *testing.T, sess *db.Session) string {
		t.Helper()
		res, err := sess.Run(context.Background(), "SELECT count(*) FROM cats WHERE name = 'Tx'")
		if err != nil {
			t.Fatal(err)
		}
		return res.Rows[0][0]
	}
	insert := func(t *testing.T, sess *db.Session) {
		t.Helper()
		for _, stmt := range []string{"BEGIN", "INSERT INTO cats (name, breed, age) VALUES ('Tx', 'Tabby', 1)"} {
			if _, err := sess.Run(context.Background(), stmt); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("commit", func(t *testing.T) {
		sess := newTestSession(t)
		insert(t, sess)
		if err := endTx(context.Background(), sess, true); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if n := count(t, sess); n != "1" {
			t.Errorf("found %s committed rows, want 1", n)
		}
	})
	t.Run("rollback after cancel", func(t *testing.T) {
		sess := newTestSession(t)
		insert(t, sess)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := endTx(ctx, sess, false); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		if n := count(t, sess); n != "0" {
			t.Errorf("found %s rows after the rollback, want 0", n)
		}
	})
}

// A result that hit max_rows must announce itself on stderr — a truncated
// export must not look complete.
func TestWarnTruncated(t *testing.T) {
	full := &model.Result{Rows: [][]string{{"a"}}}
	cut := &model.Result{Rows: [][]string{{"a"}, {"b"}}, Truncated: true}

	var sb strings.Builder
	warnTruncated(&sb, []*model.Result{full}, []int{1}, 1)
	if sb.Len() != 0 {
		t.Errorf("untruncated result warned: %q", sb.String())
	}

	sb.Reset()
	warnTruncated(&sb, []*model.Result{cut}, []int{1}, 1)
	if got := sb.String(); !strings.Contains(got, "truncated at 2 rows") {
		t.Errorf("single-result note = %q, want the row count", got)
	}

	sb.Reset()
	warnTruncated(&sb, []*model.Result{full, cut}, []int{1, 2}, 2)
	if got := sb.String(); !strings.Contains(got, "statement 2/2") {
		t.Errorf("multi-result note = %q, want the statement position", got)
	}

	// a run that passed over statements 2 and 3 names the statement, not the
	// result's place among the survivors
	sb.Reset()
	warnTruncated(&sb, []*model.Result{full, cut}, []int{1, 4}, 5)
	if got := sb.String(); !strings.Contains(got, "statement 4/5") {
		t.Errorf("note after a gap = %q, want statement 4/5", got)
	}
}

// A streamed run must write exactly the document the collected shape would
// have rendered, in every block format — streaming changes when the output
// appears, never what it says.
func TestBlockStreamMatchesCollected(t *testing.T) {
	for _, f := range []export.Format{export.Text, export.Markdown, export.CSV, export.TSV} {
		t.Run(string(f), func(t *testing.T) {
			sess := newTestSession(t)
			stmts := sqlsplit.Split(`
				SELECT name FROM cats ORDER BY name LIMIT 2;
				UPDATE cats SET age = age WHERE name = 'Luna';
				SELECT count(*) AS n FROM cats;
			`)
			var out, notes strings.Builder
			st := &blockStream{out: &out, notes: &notes, f: f, total: len(stmts)}
			run, err := runStatements(context.Background(), sess, stmts, runHooks{onResult: st.add})
			results := run.results
			if err != nil {
				t.Fatalf("runStatements: %v", err)
			}
			want, err := export.RenderAll(results, f)
			if err != nil {
				t.Fatalf("RenderAll: %v", err)
			}
			if out.String() != want {
				t.Errorf("streamed output differs from collected:\n%s\n---\n%s", out.String(), want)
			}
		})
	}
}

// When a statement fails, the blocks before it are already out, and their
// banners count against the whole run — the stream could not know it would
// stop early. The failing statement writes nothing.
func TestBlockStreamStopsAtFailure(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT 2 AS n; SELECT * FROM no_such_table`)
	var out, notes strings.Builder
	st := &blockStream{out: &out, notes: &notes, f: export.Text, total: len(stmts)}

	_, err := runStatements(context.Background(), sess, stmts, runHooks{onResult: st.add})
	if err == nil || !strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("err = %v, want the missing table", err)
	}
	if st.err != nil {
		t.Errorf("a statement failure was recorded as a stream failure: %v", st.err)
	}
	got := out.String()
	for _, want := range []string{"-- 1/3 │", "-- 2/3 │"} {
		if !strings.Contains(got, want) {
			t.Errorf("streamed output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "3/3") {
		t.Errorf("the failed statement wrote a block:\n%s", got)
	}
}

// A render failure stops the run before the next statement and is kept on
// the stream, so the caller reports it as a render failure, not a query one.
func TestBlockStreamRenderFailureStopsRun(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT 2 AS n`)
	var out, notes strings.Builder
	// HTML is not a block format, so RenderBlock refuses it
	st := &blockStream{out: &out, notes: &notes, f: export.HTML, total: len(stmts)}

	run, err := runStatements(context.Background(), sess, stmts, runHooks{onResult: st.add})
	results := run.results
	if err == nil || st.err == nil || err != st.err {
		t.Fatalf("err = %v, stream err = %v, want the same render failure", err, st.err)
	}
	if len(results) != 1 {
		t.Errorf("ran %d statements, want the run stopped after the first", len(results))
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q after a render failure", out.String())
	}
}

// The truncation note for a streamed block goes out with that block, naming
// its position in the whole run.
func TestBlockStreamNotesTruncation(t *testing.T) {
	var out, notes strings.Builder
	st := &blockStream{out: &out, notes: &notes, f: export.Text, total: 3}
	full := &model.Result{Columns: []string{"n"}, Rows: [][]string{{"1"}}}
	cut := &model.Result{Columns: []string{"n"}, Rows: [][]string{{"1"}, {"2"}}, Truncated: true}

	if err := st.add(0, full); err != nil {
		t.Fatal(err)
	}
	if notes.Len() != 0 {
		t.Errorf("untruncated block warned: %q", notes.String())
	}
	if err := st.add(1, cut); err != nil {
		t.Fatal(err)
	}
	if got := notes.String(); !strings.Contains(got, "statement 2/3: result truncated at 2 rows") {
		t.Errorf("note = %q, want the block's position in the run", got)
	}
}

// Streaming is for a multi-statement run in a block format on stdout; every
// other shape still collects.
func TestStreamsResults(t *testing.T) {
	cases := []struct {
		name    string
		outFile string
		format  export.Format
		stmts   int
		want    bool
	}{
		{"text, many, stdout", "", export.Text, 3, true},
		{"csv, many, stdout", "", export.CSV, 2, true},
		{"markdown, many, stdout", "", export.Markdown, 2, true},
		{"one statement renders bare", "", export.Text, 1, false},
		{"-o writes one file", "out.txt", export.Text, 3, false},
		{"json is one array", "", export.JSON, 3, false},
		{"html is one document", "", export.HTML, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setFlag(t, &flagOut, c.outFile)
			if got := streamsResults(c.format, c.stmts); got != c.want {
				t.Errorf("streamsResults = %v, want %v", got, c.want)
			}
		})
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

// A streamed script writes each result the moment it is shown, so the
// s.Print line before a Show lands before that result's block, not ahead of
// every block. The banners are open-ended ("#2"), and the whole of stdout is
// the log lines around export.RenderOpen's blocks.
func TestScriptHeadlessStreamsInOrder(t *testing.T) {
	mgr := newTestManager(t)
	setFlag(t, &flagOut, "")

	got := captureStdout(t, func() {
		runScriptHeadless(mgr, "testdata/show_two.go", export.Text)
	})

	tabby := strings.Index(got, "breed Tabby")
	first := strings.Index(got, "-- #1 │ demo │")
	siamese := strings.Index(got, "breed Siamese")
	second := strings.Index(got, "-- #2 │ demo │")
	if tabby < 0 || first < 0 || siamese < 0 || second < 0 {
		t.Fatalf("output is missing a log line or a banner:\n%s", got)
	}
	if !(tabby < first && first < siamese && siamese < second) {
		t.Errorf("results did not stream between the log lines:\n%s", got)
	}
	if strings.Contains(got, "/2 │") {
		t.Errorf("a script banner counted against a total:\n%s", got)
	}
}

// -o in a block format writes what the stream would have: "#i" banners, so a
// script's `> file` and `-o file` hold the same results.
func TestScriptHeadlessOutfileMatchesStream(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cats.txt")
	setFlag(t, &flagOut, out)
	runScriptHeadless(newTestManager(t), "testdata/show_two.go", export.Text)
	bs, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read -o file: %v", err)
	}

	// the stream, without the log lines that shared stdout with it
	setFlag(t, &flagOut, "")
	streamed := captureStdout(t, func() {
		runScriptHeadless(newTestManager(t), "testdata/show_two.go", export.Text)
	})
	var kept []string
	for _, line := range strings.SplitAfter(streamed, "\n") {
		if !strings.HasPrefix(line, "breed ") {
			kept = append(kept, line)
		}
	}
	// Durations differ run to run; everything else must agree.
	norm := func(s string) string {
		return regexp.MustCompile(` in [0-9.]+[µnm]?s`).ReplaceAllString(s, " in D")
	}
	if a, b := norm(string(bs)), norm(strings.Join(kept, "")); a != b {
		t.Errorf("-o file differs from the stream:\n%s\n---\n%s", a, b)
	}
}

// A script's stream numbers its blocks "#i" and names them "result #i" in
// notes and errors. After a failed write it stops the script once and drops
// every later result.
func TestBlockStreamOpenEnded(t *testing.T) {
	var out, notes strings.Builder
	st := &blockStream{out: &out, notes: &notes, f: export.Text}
	full := &model.Result{Conn: "demo", Columns: []string{"n"}, Rows: [][]string{{"1"}}}
	cut := &model.Result{Conn: "demo", Columns: []string{"n"}, Rows: [][]string{{"1"}, {"2"}}, Truncated: true}

	st.show(full, nil)
	st.show(cut, nil)
	want, err := export.RenderOpen([]*model.Result{full, cut}, export.Text)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Errorf("streamed output differs from RenderOpen:\n%s\n---\n%s", out.String(), want)
	}
	if got := notes.String(); !strings.Contains(got, "note: result #2: result truncated at 2 rows") {
		t.Errorf("note = %q, want the result's place among the script's", got)
	}

	// a render failure (HTML is no block format) stops the script, once
	stops := 0
	bad := &blockStream{out: &out, notes: &notes, f: export.HTML}
	bad.show(full, func() { stops++ })
	bad.show(full, func() { stops++ })
	var se *serr.SErr
	if !errors.As(bad.err, &se) || se.FieldsMap()["result"] != "#1" {
		t.Errorf("stream err = %v, want a failure naming result #1", bad.err)
	}
	if stops != 1 {
		t.Errorf("stop called %d times, want once", stops)
	}
}

// A script streams in any block format on stdout; -o and the document
// formats collect. There is no count to check.
func TestScriptStreams(t *testing.T) {
	cases := []struct {
		name    string
		outFile string
		format  export.Format
		want    bool
	}{
		{"text, stdout", "", export.Text, true},
		{"csv, stdout", "", export.CSV, true},
		{"markdown, stdout", "", export.Markdown, true},
		{"-o writes one file", "out.txt", export.Text, false},
		{"json is one array", "", export.JSON, false},
		{"html is one document", "", export.HTML, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setFlag(t, &flagOut, c.outFile)
			if got := scriptStreams(c.format); got != c.want {
				t.Errorf("scriptStreams = %v, want %v", got, c.want)
			}
		})
	}
}

// captureStdout runs fn with os.Stdout on a pipe and returns what it wrote.
// The pipe is drained as fn writes, so output past the pipe buffer cannot
// block it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		bs, _ := io.ReadAll(r)
		done <- string(bs)
	}()
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	return <-done
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

// setBoolFlag overrides a bool flag for one test and restores it after.
func setBoolFlag(t *testing.T, f *bool, v bool) {
	t.Helper()
	old := *f
	*f = v
	t.Cleanup(func() { *f = old })
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

	_, err := runStatements(ctx, sess, sqlsplit.Split("SELECT 1; SELECT 2"), runHooks{})
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
