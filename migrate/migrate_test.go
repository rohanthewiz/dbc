package migrate

import (
	"bufio"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"
	_ "modernc.org/sqlite"
)

func mustParse(t *testing.T, src string) Migration {
	t.Helper()
	m, err := parse(bufio.NewReader(strings.NewReader(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestParseSections(t *testing.T) {
	m := mustParse(t, `
-- +goose Up
-- SQL in section 'Up' is executed when this migration is applied
CREATE TABLE a (id int);
CREATE INDEX idx_a ON a (id);

-- +goose Down
-- SQL section 'Down' is executed when this migration is rolled back
DROP TABLE a;
`)
	if len(m.Up) != 2 || len(m.Down) != 1 {
		t.Fatalf("up=%d down=%d, want 2/1: %q %q", len(m.Up), len(m.Down), m.Up, m.Down)
	}
	if !strings.HasSuffix(m.Up[0], "CREATE TABLE a (id int)") {
		t.Errorf("up[0] = %q", m.Up[0])
	}
	if m.Down[0] != "-- SQL section 'Down' is executed when this migration is rolled back\nDROP TABLE a" {
		t.Errorf("down[0] = %q", m.Down[0])
	}
	if m.NoTx {
		t.Error("NoTx set without annotation")
	}
}

func TestParseStatementBlockAndNoTx(t *testing.T) {
	m := mustParse(t, `-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE TRIGGER t BEFORE INSERT ON a BEGIN
  SELECT 1;
  SELECT 2;
END;
-- +goose StatementEnd
SELECT 3;
-- +goose Down
DROP TRIGGER t;
`)
	if !m.NoTx {
		t.Error("NoTx not set")
	}
	if len(m.Up) != 2 {
		t.Fatalf("up = %q, want the trigger as one statement plus SELECT 3", m.Up)
	}
	if !strings.Contains(m.Up[0], "SELECT 1;\n  SELECT 2;\nEND;") {
		t.Errorf("verbatim block was split: %q", m.Up[0])
	}
	if m.Up[1] != "SELECT 3" {
		t.Errorf("up[1] = %q", m.Up[1])
	}
}

func TestParseDollarQuotedBodyNeedsNoBlock(t *testing.T) {
	m := mustParse(t, `-- +goose Up
CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql;
SELECT f();
-- +goose Down
DROP FUNCTION f;
`)
	if len(m.Up) != 2 {
		t.Fatalf("up = %q, want 2", m.Up)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"no up":            "CREATE TABLE a (id int);",
		"down before up":   "-- +goose Down\nDROP TABLE a;",
		"sql before up":    "CREATE TABLE a (id int);\n-- +goose Up\n",
		"unterminated blk": "-- +goose Up\n-- +goose StatementBegin\nSELECT 1;\n",
		"unknown ann":      "-- +goose Up\n-- +goose ENVSUB ON\nSELECT 1;",
		"duplicate up":     "-- +goose Up\nSELECT 1;\n-- +goose Up\n",
	}
	for name, src := range cases {
		if _, err := parse(bufio.NewReader(strings.NewReader(src))); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAnnotation(t *testing.T) {
	for line, want := range map[string]string{
		"-- +goose Up":                     "Up",
		"--+goose Down":                    "Down",
		"  --   +goose  NO   TRANSACTION ": "NO TRANSACTION",
	} {
		got, ok := annotation(line)
		if !ok || got != want {
			t.Errorf("%q: got %q/%v, want %q", line, got, ok, want)
		}
	}
	for _, line := range []string{"-- goose Up", "SELECT 1", "-- comment", "/* +goose Up */"} {
		if _, ok := annotation(line); ok {
			t.Errorf("%q recognized as an annotation", line)
		}
	}
}

// writeMigrations lays out a migrations directory with the given files and
// a couple of decoys that Load must skip.
func writeMigrations(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "queries_stash"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not sql"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const (
	migA = "-- +goose Up\nCREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT);\n-- +goose Down\nDROP TABLE a;\n"
	migB = "-- +goose Up\nCREATE TABLE b (id INTEGER PRIMARY KEY);\nINSERT INTO a (id, name) VALUES (1, 'x');\n-- +goose Down\nDROP TABLE b;\nDELETE FROM a;\n"
	migC = "-- +goose Up\nCREATE TABLE c (id INTEGER PRIMARY KEY);\n-- +goose Down\nDROP TABLE c;\n"
)

func TestLoadOrdersAndSkipsDecoys(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"20_b.sql": migB, "10_a.sql": migA, "5_no_name_ok.sql": migC, "notes.txt": "x",
	})
	migs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 3 || migs[0].Version != 5 || migs[1].Version != 10 || migs[2].Version != 20 {
		t.Fatalf("got %+v", migs)
	}
}

func TestLoadRejectsDuplicateVersion(t *testing.T) {
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA, "10_b.sql": migB})
	if _, err := Load(dir); err == nil {
		t.Fatal("expected duplicate version error")
	}
}

// engines returns an open handle per embedded engine so the runner is
// exercised on both placeholder styles and both transaction policies.
func engines(t *testing.T) map[string]*sql.DB {
	t.Helper()
	out := map[string]*sql.DB{}

	sq, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	out["sqlite"] = sq

	by, err := sql.Open(bytdbdrv.DriverName, filepath.Join(t.TempDir(), "m.bytdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = by.Close() })
	out["bytdb"] = by
	return out
}

func tableExists(t *testing.T, dbh *sql.DB, name string) bool {
	t.Helper()
	_, err := dbh.Exec("SELECT count(*) FROM " + name)
	return err == nil
}

func TestUpDownStatusAcrossEngines(t *testing.T) {
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA, "20_b.sql": migB, "30_c.sql": migC})

	for name, dbh := range engines(t) {
		t.Run(name, func(t *testing.T) {
			d, err := DialectFor(name)
			if err != nil {
				t.Fatal(err)
			}
			var log []string
			m, err := New(dbh, d, dir)
			if err != nil {
				t.Fatal(err)
			}
			m.Log = func(s string) { log = append(log, s) }

			// fresh database: version 0, nothing applied, table auto-created
			if v, err := m.Version(ctx); err != nil || v != 0 {
				t.Fatalf("initial version = %d, %v", v, err)
			}
			if !tableExists(t, dbh, VersionTable) {
				t.Fatal("version table not created")
			}

			// up-to stops short
			done, err := m.UpTo(ctx, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(done) != 2 || !tableExists(t, dbh, "b") || tableExists(t, dbh, "c") {
				t.Fatalf("up-to 20 applied %d, tables a/b/c exist: %v/%v/%v", len(done),
					tableExists(t, dbh, "a"), tableExists(t, dbh, "b"), tableExists(t, dbh, "c"))
			}
			sts, err := m.Statuses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !sts[0].Applied || !sts[1].Applied || sts[2].Applied || sts[0].AppliedAt == "" || sts[2].AppliedAt != "" {
				t.Fatalf("statuses = %+v", sts)
			}

			// up applies the rest; a second up is a no-op
			if done, err = m.Up(ctx); err != nil || len(done) != 1 || done[0].Version != 30 {
				t.Fatalf("up: %v %+v", err, done)
			}
			if done, err = m.Up(ctx); err != nil || len(done) != 0 {
				t.Fatalf("second up: %v %+v", err, done)
			}
			if v, _ := m.Version(ctx); v != 30 {
				t.Fatalf("version = %d, want 30", v)
			}

			// down one, then redo, then down-to 0 empties everything
			if done, err = m.Down(ctx); err != nil || len(done) != 1 || done[0].Version != 30 || tableExists(t, dbh, "c") {
				t.Fatalf("down: %v %+v", err, done)
			}
			if err = m.Redo(ctx); err != nil {
				t.Fatal(err)
			}
			if v, _ := m.Version(ctx); v != 20 {
				t.Fatalf("after redo version = %d, want 20", v)
			}
			if done, err = m.DownTo(ctx, 0); err != nil || len(done) != 2 {
				t.Fatalf("down-to 0: %v %+v", err, done)
			}
			if tableExists(t, dbh, "a") || tableExists(t, dbh, "b") {
				t.Fatal("tables survived down-to 0")
			}
			if v, _ := m.Version(ctx); v != 0 {
				t.Fatalf("final version = %d", v)
			}
			if len(log) == 0 || !strings.HasPrefix(log[0], "OK   10_a.sql") {
				t.Errorf("log = %q", log)
			}
		})
	}
}

func TestUpRefusesMissingUnlessAllowed(t *testing.T) {
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA, "30_c.sql": migC})
	dbh := engines(t)["sqlite"]

	m, err := New(dbh, SQLite, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	// a migration from another branch lands with a lower version than head
	if err = os.WriteFile(filepath.Join(dir, "20_b.sql"), []byte(migB), 0644); err != nil {
		t.Fatal(err)
	}
	m, _ = New(dbh, SQLite, dir)
	if _, err = m.Up(ctx); err == nil || !strings.Contains(err.Error(), "allow-missing") {
		t.Fatalf("expected missing-migration refusal, got %v", err)
	}
	m.AllowMissing = true
	if done, err := m.Up(ctx); err != nil || len(done) != 1 || done[0].Version != 20 {
		t.Fatalf("allow-missing up: %v %+v", err, done)
	}
}

func TestFailedMigrationIsRolledBack(t *testing.T) {
	ctx := context.Background()
	bad := "-- +goose Up\nCREATE TABLE d (id INTEGER PRIMARY KEY);\nSELECT * FROM does_not_exist;\n-- +goose Down\nDROP TABLE d;\n"
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA, "20_bad.sql": bad})
	dbh := engines(t)["sqlite"]

	m, err := New(dbh, SQLite, dir)
	if err != nil {
		t.Fatal(err)
	}
	done, err := m.Up(ctx)
	if err == nil {
		t.Fatal("expected the bad migration to fail")
	}
	if len(done) != 1 || done[0].Version != 10 {
		t.Fatalf("earlier migration should have been applied: %+v", done)
	}
	if tableExists(t, dbh, "d") {
		t.Error("table d survived a failed transaction")
	}
	if v, _ := m.Version(ctx); v != 10 {
		t.Errorf("version = %d after failure, want 10", v)
	}
	// the statement position rides in the error's fields, not its message
	if !strings.Contains(serr.StringFromErr(err), "2/2") {
		t.Errorf("error should name the failing statement position: %s", serr.StringFromErr(err))
	}
}

// A database goose itself migrated has its rows; we must read them the same
// way, including the legacy is_applied=false rollback marker.
func TestReadsGooseHistory(t *testing.T) {
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA, "20_b.sql": migB})
	dbh := engines(t)["sqlite"]
	for _, s := range []string{
		SQLite.createTable,
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, 1)",
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (10, 1)",
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (20, 1)",
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (20, 0)", // old-style rollback
		"CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT)",
	} {
		if _, err := dbh.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(dbh, SQLite, dir)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Version(ctx); v != 10 {
		t.Fatalf("version = %d, want 10 (20 was rolled back)", v)
	}
	done, err := m.Up(ctx)
	if err != nil || len(done) != 1 || done[0].Version != 20 {
		t.Fatalf("up: %v %+v", err, done)
	}
}

func TestCreate(t *testing.T) {
	dir := t.TempDir()
	path, err := Create(dir, "add users table!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "_add_users_table_.sql") {
		t.Errorf("path = %s", path)
	}
	m, err := ParseFile(path)
	if err != nil {
		t.Fatalf("new file does not parse: %v", err)
	}
	if len(m.Up) != 0 || len(m.Down) != 0 {
		t.Errorf("template should hold no statements: %+v", m)
	}
	if _, err = Create(dir, ""); err == nil {
		t.Error("empty name accepted")
	}
}
