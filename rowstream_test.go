package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/logger"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
)

// N-282: a lone CSV/TSV statement streams its rows from the driver to the
// output (rowStreams), never holding them. What it writes must be what the
// collected export of the same result would have been; a failure part way
// must leave an -o file as it was.

// liteCfg is a config with one SQLite connection in a fresh file.
func liteCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{MaxRows: 1000, DefaultConnection: "lite", Connections: []config.Connection{{
		Name: "lite", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "x.db")}}}
}

// streamed runs stmt headless to an -o file in format f and returns the
// file and what went to stderr.
func streamed(t *testing.T, cfg *config.Config, mgr *db.Manager, stmt string, f export.Format) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	setFlag(t, &flagOut, out)
	notes := captureStderr(t, func() { runQueryHeadless(cfg, mgr, stmt, f) })
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), notes
}

// captureStderr runs fn with os.Stderr going to a file, and returns what
// was written there.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = old }()
	fn()
	_, _ = f.Seek(0, io.SeekStart)
	b, _ := io.ReadAll(f)
	_ = f.Close()
	return string(b)
}

// A streamed statement writes the bytes the collected export of its result
// writes, in both delimited formats: rows with NULLs, quotes, separators
// and line breaks; an empty result (its header alone); a cut one; a write,
// which has no rows to stream and comes out as its count.
func TestRowStreamMatchesCollected(t *testing.T) {
	resetFlags(t)
	cfg := liteCfg(t)
	mgr := db.NewManager(cfg)
	defer mgr.Close()
	setup := []string{
		`CREATE TABLE t (id INTEGER, name TEXT, note TEXT, score REAL)`,
		`INSERT INTO t VALUES (1, 'Ann', NULL, 1.5), (2, 'Bo, "B"', 'tab	here', 2), (3, 'line
break', '', -0.25)`,
	}
	for _, s := range setup {
		if _, err := mgr.Run("lite", s); err != nil {
			t.Fatal(err)
		}
	}
	nums := `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1500) SELECT i, 'r' || i AS s FROM n`
	for _, stmt := range []string{
		`SELECT * FROM t ORDER BY id`,
		`SELECT * FROM t WHERE id > 99`,
		nums,
		`UPDATE t SET score = score + 1 WHERE id < 3`,
	} {
		for _, f := range []export.Format{export.CSV, export.TSV} {
			if !rowStreams(f, 1) {
				t.Fatalf("%s does not stream", f)
			}
			res, err := mgr.Run("lite", stmt)
			if err != nil {
				t.Fatal(err)
			}
			want, err := export.Render(res, f)
			if err != nil {
				t.Fatal(err)
			}
			// (an UPDATE runs twice, collected then streamed; its count is
			// the same both times)
			got, notes := streamed(t, cfg, mgr, stmt, f)
			if got != want {
				t.Errorf("%s as %s:\n got %.300q\nwant %.300q", stmt, f, got, want)
			}
			if stmt == nums {
				// cut at the config's 1,000: the note counts the rows that
				// went out, which the Result no longer holds
				if !strings.Contains(notes, "result truncated at 1000 rows") ||
					!strings.Contains(notes, "wrote 1000 rows to ") {
					t.Errorf("notes for the cut result: %q", notes)
				}
			}
		}
	}
}

// The child processes below call runQueryHeadless, which ends in os.Exit
// on any failure.
func TestRowStreamFailures(t *testing.T) {
	switch os.Getenv("DBC_TEST_ROWSTREAM") {
	case "":
	case "fails-midway":
		logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"})
		cfg := &config.Config{MaxRows: 0, DefaultConnection: "lite", Connections: []config.Connection{{
			Name: "lite", Driver: "sqlite", DSN: os.Getenv("DBC_TEST_DB")}}}
		cfg.MaxRows = 1 << 30
		flagOut = os.Getenv("DBC_TEST_OUT")
		// abs() of the smallest integer overflows: row 500 fails the
		// statement after 499 rows have gone to the file's twin
		runQueryHeadless(cfg, db.NewManager(cfg), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
			SELECT CASE WHEN i < 500 THEN i ELSE abs(-9223372036854775807 - 1) END FROM n`, export.CSV)
		os.Exit(0)
	case "stdout-fails":
		logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"})
		f, err := os.Open(os.DevNull)
		if err != nil {
			os.Exit(3)
		}
		os.Stdout = f
		cfg := &config.Config{MaxRows: 1 << 30, DefaultConnection: "lite", Connections: []config.Connection{{
			Name: "lite", Driver: "sqlite", DSN: os.Getenv("DBC_TEST_DB")}}}
		runQueryHeadless(cfg, db.NewManager(cfg), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100000)
			SELECT i, 'some text to fill the buffer' FROM n`, export.CSV)
		os.Exit(0) // the write failure was not seen
	}

	child := func(mode string, env ...string) (int, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRowStreamFailures$")
		cmd.Env = append(append(os.Environ(), "DBC_TEST_ROWSTREAM="+mode), env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), stderr.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, stderr.String()
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	out := filepath.Join(dir, "old.csv")
	if err := os.WriteFile(out, []byte("the last good export\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := child("fails-midway", "DBC_TEST_DB="+dbPath, "DBC_TEST_OUT="+out)
	if code != 1 || !strings.Contains(stderr, "query failed") {
		t.Errorf("a statement failing part way: exit %d, stderr %q; want 1 and query failed", code, stderr)
	}
	if b, _ := os.ReadFile(out); string(b) != "the last good export\n" {
		t.Errorf("the -o file holds %.80q, want the old export kept", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".old.csv*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	code, stderr = child("stdout-fails", "DBC_TEST_DB="+filepath.Join(dir, "y.db"))
	if code != 1 || !strings.Contains(stderr, "write failed") {
		t.Errorf("a stdout that refuses writes: exit %d, stderr %q; want 1 and write failed", code, stderr)
	}
}
