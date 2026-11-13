package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/rohanthewiz/serr"
)

// Migrations whose second run would show: 20 and 30 are data migrations,
// so a version applied twice leaves a second row in counter.
const (
	migCounter = "-- +goose Up\nCREATE TABLE counter (n INTEGER);\n-- +goose Down\nDROP TABLE counter;\n"
	migBump20  = "-- +goose Up\nINSERT INTO counter (n) VALUES (20);\n-- +goose Down\nDELETE FROM counter WHERE n = 20;\n"
	migBump30  = "-- +goose Up\nINSERT INTO counter (n) VALUES (30);\n-- +goose Down\nDELETE FROM counter WHERE n = 30;\n"
)

// openSQLite opens its own handle on path, as a second process would. The
// busy_timeout is what dbc's manager gives every SQLite DSN (db/manager.go):
// a runner waiting on another's BEGIN IMMEDIATE waits instead of failing.
func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	dbh, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return dbh
}

func countRows(t *testing.T, dbh *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := dbh.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestSQLiteSkipsWhatAnotherRunDid is N-270's SQLite race made
// deterministic: a runner that read the version table before another runner
// committed goes on to run the same migration. Under BEGIN IMMEDIATE it
// re-reads the row, finds it applied, and skips — the data migration's row
// is not inserted twice, nor is its version row. The same holds for a
// rollback another run already did.
func TestSQLiteSkipsWhatAnotherRunDid(t *testing.T) {
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{"10_counter.sql": migCounter, "20_bump.sql": migBump20})
	path := filepath.Join(t.TempDir(), "m.db")
	a, b := openSQLite(t, path), openSQLite(t, path)

	ma, err := New(a, SQLite, dir)
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	mb, _ := New(b, SQLite, dir)
	mb.Log = func(s string) { log = append(log, s) }

	// b reads the table first: everything is pending
	if _, err = mb.applied(ctx, b); err != nil {
		t.Fatal(err)
	}
	// a applies both
	if done, err := ma.Up(ctx); err != nil || len(done) != 2 {
		t.Fatalf("a up: %v %+v", err, done)
	}
	// b goes on to run 20 from its stale reading
	cb, err := b.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	mig20 := mb.Migrations()[1]
	ran, err := mb.runOne(ctx, cb, mig20, mig20.Up, true)
	if err != nil || ran {
		t.Fatalf("stale up of 20: ran=%v err=%v, want a skip", ran, err)
	}
	if n := countRows(t, a, "SELECT count(*) FROM counter WHERE n = 20"); n != 1 {
		t.Errorf("counter holds %d rows for 20, want 1", n)
	}
	if n := countRows(t, a, "SELECT count(*) FROM "+VersionTable+" WHERE version_id = 20"); n != 1 {
		t.Errorf("version table holds %d rows for 20, want 1", n)
	}
	if len(log) != 1 || log[0] != "SKIP 20_bump.sql (another run applied it)" {
		t.Errorf("log = %q", log)
	}

	// and down: a rolls 20 back; b, still believing it applied, skips
	if _, err = ma.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if ran, err = mb.runOne(ctx, cb, mig20, mig20.Down, false); err != nil || ran {
		t.Fatalf("stale down of 20: ran=%v err=%v, want a skip", ran, err)
	}

	// the skip rolled its transaction back: the connection is not left
	// holding the write lock, so a's next write does not wait on it
	if _, err = a.Exec("INSERT INTO counter (n) VALUES (99)"); err != nil {
		t.Errorf("write after the skip: %v", err)
	}
}

// TestSQLiteConcurrentRunsApplyOnce runs several `up`s at once on one file,
// each through its own handle as separate processes would. Every one
// succeeds, and each migration — two of them data migrations — lands once.
// Before N-270 this failed: runners that read the table at the same time
// each ran the CREATE (the second failing on "table exists") or each ran a
// data migration.
func TestSQLiteConcurrentRunsApplyOnce(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"10_counter.sql": migCounter, "20_bump.sql": migBump20, "30_bump.sql": migBump30,
	})
	// fresh: the runners also race to create the version table. table
	// exists: the race is the migrations' alone.
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			concurrentRuns(t, dir, fresh)
		})
	}
}

func concurrentRuns(t *testing.T, dir string, fresh bool) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	if !fresh {
		m, _ := New(openSQLite(t, path), SQLite, dir)
		if _, err := m.Version(ctx); err != nil {
			t.Fatal(err)
		}
	}

	const runners = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, runners)
	for i := 0; i < runners; i++ {
		dbh := openSQLite(t, path)
		m, err := New(dbh, SQLite, dir)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = m.Up(ctx)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("runner %d: %s", i, serr.StringFromErr(err))
		}
	}

	check := openSQLite(t, path)
	if n := countRows(t, check, "SELECT count(*) FROM counter"); n != 2 {
		t.Errorf("counter holds %d rows, want 2 (20 and 30 once each)", n)
	}
	for _, v := range []int{0, 10, 20, 30} {
		if n := countRows(t, check, "SELECT count(*) FROM "+VersionTable+" WHERE version_id = ?", v); n != 1 {
			t.Errorf("version %d has %d rows, want 1", v, n)
		}
	}
}

// TestSQLiteFailedMigrationFreesTheLock: a migration that fails inside its
// BEGIN IMMEDIATE is rolled back on the held connection, so neither the pool
// (one connection, the same one reused) nor another process inherits the
// open transaction or its write lock.
func TestSQLiteFailedMigrationFreesTheLock(t *testing.T) {
	ctx := context.Background()
	bad := "-- +goose Up\nCREATE TABLE d (id INTEGER);\nINSERT INTO nope VALUES (1);\n-- +goose Down\nDROP TABLE d;\n"
	dir := writeMigrations(t, map[string]string{"10_bad.sql": bad})
	path := filepath.Join(t.TempDir(), "m.db")
	a, other := openSQLite(t, path), openSQLite(t, path)
	a.SetMaxOpenConns(1)

	m, _ := New(a, SQLite, dir)
	if _, err := m.Up(ctx); err == nil {
		t.Fatal("expected the bad migration to fail")
	}
	if tableExists(t, a, "d") {
		t.Error("table d survived the rollback")
	}
	// were the connection still inside the transaction, this CREATE would
	// join it, and other's insert would wait out its busy_timeout
	if _, err := a.Exec("CREATE TABLE z (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := other.ExecContext(cctx, "INSERT INTO z VALUES (1)"); err != nil {
		t.Fatalf("another handle could not write after the failed run: %v", err)
	}
}

// fakeLock is a session lock that is held by "another run" for the first
// busy tries, for testing the waiting loop without a server.
type fakeLock struct {
	busy       int
	tries      int
	releases   int
	releaseErr error
}

func (f *fakeLock) lock() *sessionLock {
	return &sessionLock{
		try: func(context.Context, *sql.Conn) (bool, error) {
			f.tries++
			return f.tries > f.busy, nil
		},
		holder: func(context.Context, *sql.Conn) string { return "pid 4242" },
		release: func(context.Context, *sql.Conn) error {
			f.releases++
			return f.releaseErr
		},
	}
}

func withFastPoll(t *testing.T) {
	t.Helper()
	old := lockPoll
	lockPoll = 5 * time.Millisecond
	t.Cleanup(func() { lockPoll = old })
}

// TestSessionLockWaitsAndReleases: a run that finds the lock held says who
// holds it, once, waits, runs once it has it, and releases it after.
func TestSessionLockWaitsAndReleases(t *testing.T) {
	withFastPoll(t)
	ctx := context.Background()
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA})
	f := &fakeLock{busy: 3}
	d := SQLite
	d.lock = f.lock()
	dbh := engines(t)["sqlite"]
	m, _ := New(dbh, d, dir)
	var log []string
	m.Log = func(s string) { log = append(log, s) }

	if done, err := m.Up(ctx); err != nil || len(done) != 1 {
		t.Fatalf("up: %v %+v", err, done)
	}
	if f.tries != 4 || f.releases != 1 {
		t.Errorf("tries=%d releases=%d, want 4 and 1", f.tries, f.releases)
	}
	want := []string{
		"waiting for the migration lock: another run holds it (pid 4242)",
		"migration lock taken",
		"OK   10_a.sql",
	}
	if strings.Join(log, "\n") != strings.Join(want, "\n") {
		t.Errorf("log = %q, want %q", log, want)
	}

	// read-only commands take no lock
	if _, err := m.Statuses(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Version(ctx); err != nil {
		t.Fatal(err)
	}
	if f.tries != 4 {
		t.Errorf("status/version tried the lock: tries=%d", f.tries)
	}

	// Redo holds it once across its down and its up
	if err := m.Redo(ctx); err != nil {
		t.Fatal(err)
	}
	if f.tries != 5 || f.releases != 2 {
		t.Errorf("redo: tries=%d releases=%d, want 5 and 2", f.tries, f.releases)
	}
}

// TestSessionLockWaitEndsWithContext: a run waiting for the lock stops when
// its context does — the CLI's Ctrl-C — with the context's error, so the
// command reports a cancel rather than a failure; it never held the lock,
// so it releases nothing.
func TestSessionLockWaitEndsWithContext(t *testing.T) {
	withFastPoll(t)
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA})
	f := &fakeLock{busy: 1 << 30}
	d := SQLite
	d.lock = f.lock()
	m, _ := New(engines(t)["sqlite"], d, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := m.Up(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if f.releases != 0 {
		t.Errorf("released a lock it never took: %d", f.releases)
	}
}

// TestSessionLockReleaseFailureDiscardsConn: when the release fails, the
// session may still hold the lock, so its connection is closed rather than
// pooled — the server frees a session's locks when it ends.
func TestSessionLockReleaseFailureDiscardsConn(t *testing.T) {
	dir := writeMigrations(t, map[string]string{"10_a.sql": migA})
	f := &fakeLock{releaseErr: fmt.Errorf("connection reset")}
	d := SQLite
	d.lock = f.lock()
	dbh := engines(t)["sqlite"]
	m, _ := New(dbh, d, dir)
	if _, err := m.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := dbh.Stats(); st.OpenConnections != 0 || st.MaxIdleClosed+st.MaxLifetimeClosed+st.MaxIdleTimeClosed != 0 {
		t.Errorf("stats = %+v: the run's connection went back to the pool", st)
	}
}

// TestMySQLCommits is the lexical half of MySQL's implicit-commit rule.
func TestMySQLCommits(t *testing.T) {
	for stmt, want := range map[string]bool{
		"CREATE TABLE a (id int)":                  true,
		"  /* lead */ alter table a add b int":     true,
		"-- note\nDROP INDEX i ON a":               true,
		"RENAME TABLE a TO b":                      true,
		"TRUNCATE a":                               true,
		"GRANT SELECT ON a TO u":                   true,
		"LOCK TABLES a WRITE":                      true,
		"CREATE TEMPORARY TABLE t (id int)":        false,
		"create  temporary table t (id int)":       false,
		"DROP TEMPORARY TABLE t":                   false,
		"INSERT INTO a VALUES (1)":                 false,
		"UPDATE a SET b = 1":                       false,
		"WITH x AS (SELECT 1) SELECT * FROM x":     false,
		"SET @v = 1":                               false,
		"CALL p()":                                 false,
		"SELECT 'CREATE TABLE in a string' FROM a": false,
	} {
		if got := mysqlCommits(stmt); got != want {
			t.Errorf("%q: commits = %v, want %v", stmt, got, want)
		}
	}
}

// TestKeptAfterFailure is the rule in commit.go, case by case (each was
// checked against MySQL 8.4 by hand: see the live test for the run).
func TestKeptAfterFailure(t *testing.T) {
	syntax := &mysql.MySQLError{Number: 1064, Message: "You have an error in your SQL syntax"}
	exists := &mysql.MySQLError{Number: 1050, Message: "Table 'a' already exists"}
	cases := []struct {
		name  string
		stmts []string
		i     int
		err   error
		want  int
	}{
		{"ddl then failing dml", []string{"CREATE TABLE a (id int)", "INSERT INTO a VALUES (1)", "INSERT INTO nope VALUES (1)"}, 2, exists, 2},
		{"dml then ddl failing to run", []string{"INSERT INTO a VALUES (1)", "ALTER TABLE a ADD id int"}, 1, exists, 1},
		{"dml then ddl syntax error", []string{"INSERT INTO a VALUES (1)", "ALTER TABLE a ADD (("}, 1, syntax, 0},
		{"dml only", []string{"INSERT INTO a VALUES (1)", "UPDATE nope SET x = 1"}, 1, exists, 0},
		{"temporary does not commit", []string{"CREATE TEMPORARY TABLE t (id int)", "UPDATE nope SET x = 1"}, 1, exists, 0},
		{"first statement fails", []string{"CREATE TABLE a (id int)"}, 0, exists, 0},
		{"version row fails after ddl", []string{"CREATE TABLE a (id int)", "INSERT INTO a VALUES (1)"}, 2, exists, 2},
	}
	for _, c := range cases {
		if got := MySQL.keptAfterFailure(c.stmts, c.i, c.err); got != c.want {
			t.Errorf("%s: kept = %d, want %d", c.name, got, c.want)
		}
		// on an engine with transactional DDL nothing is ever kept
		if got := Postgres.keptAfterFailure(c.stmts, c.i, c.err); got != 0 {
			t.Errorf("%s on postgres: kept = %d, want 0", c.name, got)
		}
	}
}

// TestNoTxFailureSaysWhatStayed: outside a transaction every statement
// before the failing one has committed, on any engine, and the error and
// the log say so.
func TestNoTxFailureSaysWhatStayed(t *testing.T) {
	bad := "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE TABLE d (id INTEGER);\nCREATE TABLE e (id INTEGER);\nINSERT INTO nope VALUES (1);\n-- +goose Down\nDROP TABLE e;\nDROP TABLE d;\n"
	dir := writeMigrations(t, map[string]string{"10_bad.sql": bad})
	dbh := engines(t)["sqlite"]
	m, _ := New(dbh, SQLite, dir)
	var log []string
	m.Log = func(s string) { log = append(log, s) }

	_, err := m.Up(context.Background())
	if err == nil {
		t.Fatal("expected a failure")
	}
	const note = "statements 1-2 of 3 stay applied (the migration runs outside a transaction); " +
		"the version table still lists the migration as pending, so undo or finish them by hand before migrating up again"
	if s := serr.StringFromErr(err); !strings.Contains(s, note) {
		t.Errorf("error lacks the note: %s", s)
	}
	if len(log) != 1 || log[0] != "PARTIAL 10_bad.sql: "+note {
		t.Errorf("log = %q", log)
	}
	if !tableExists(t, dbh, "d") || !tableExists(t, dbh, "e") {
		t.Error("the statements that ran should have stayed")
	}
}
