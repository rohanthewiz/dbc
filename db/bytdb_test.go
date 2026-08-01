package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// bytdbMgr gives a Manager with one bytdb connection on a fresh file.
func bytdbMgr(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "bd", Driver: "bytdb",
			DSN: filepath.Join(t.TempDir(), "test.bytdb"),
		}},
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	return mgr
}

func TestBytdbDriverAlias(t *testing.T) {
	drv, err := driverFor("bytdb")
	if err != nil {
		t.Fatalf("driverFor(bytdb): %v", err)
	}
	if drv != "bytdb" {
		t.Errorf("driverFor(bytdb) = %q", drv)
	}
	// the error text should name every driver a user may configure
	_, err = driverFor("cassandra")
	if err == nil || !strings.Contains(err.Error(), "bytdb") {
		t.Errorf("unknown-driver error should mention bytdb: %v", err)
	}
}

// A bytdb connection has to work through the ordinary Manager paths: open on
// first use, ping, DDL and DML as Exec, SELECT as a query.
func TestBytdbThroughManager(t *testing.T) {
	mgr := bytdbMgr(t)

	res, err := mgr.Run("bd", "CREATE TABLE cats (id int PRIMARY KEY, name text, age int)")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.IsExec {
		t.Error("CREATE TABLE should report as an exec")
	}

	res, err = mgr.Run("bd", "INSERT INTO cats VALUES (1, 'mia', 4), (2, 'otto', 7)")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if res.Affected != 2 {
		t.Errorf("affected = %d, want 2", res.Affected)
	}

	res, err = mgr.Run("bd", "SELECT name, age FROM cats WHERE age > $1 ORDER BY name", 5)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "otto" || res.Rows[0][1] != "7" {
		t.Errorf("rows = %v, want [[otto 7]]", res.Rows)
	}
	if got := res.Columns; len(got) != 2 || got[0] != "name" || got[1] != "age" {
		t.Errorf("columns = %v", got)
	}
}

// bytdb stores timestamps, dates, and UUIDs as int64s and raw bytes so they
// sort in the key encoding. The driver owes the results table the values a
// person would recognize, not the encoding.
func TestBytdbTypeRendering(t *testing.T) {
	mgr := bytdbMgr(t)

	if _, err := mgr.Run("bd", `CREATE TABLE ev (
		id int PRIMARY KEY, at timestamp, day date, uid uuid, tags text[], meta jsonb)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := mgr.Run("bd", `INSERT INTO ev VALUES (1,
		'2024-03-05 10:30:00', '1815-12-10',
		'11111111-2222-3333-4444-555555555555', '{a,b}', '{"x":1}')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := mgr.Run("bd", "SELECT at, day, uid, tags, meta FROM ev")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	want := []string{
		"2024-03-05T10:30:00Z",                 // renderVal's RFC3339, from a time.Time
		"1815-12-10",                           // a bare date, not a midnight instant
		"11111111-2222-3333-4444-555555555555", // hyphenated, not 16 raw bytes
		"{a,b}",
		`{"x":1}`,
	}
	for i, w := range want {
		if res.Rows[0][i] != w {
			t.Errorf("column %d = %q, want %q", i, res.Rows[0][i], w)
		}
	}
}

// A Session is what makes BEGIN/COMMIT mean anything: the statements between
// them must share one transaction, and a block left open must not survive the
// connection going back to the pool.
func TestBytdbSessionTransaction(t *testing.T) {
	mgr := bytdbMgr(t)
	ctx := context.Background()

	if _, err := mgr.Run("bd", "CREATE TABLE t (n int PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	sess, err := mgr.Session(ctx, "bd")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	for _, stmt := range []string{"BEGIN", "INSERT INTO t VALUES (1)", "COMMIT"} {
		if _, err = sess.Run(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// a block left open when the session closes rolls back
	for _, stmt := range []string{"BEGIN", "INSERT INTO t VALUES (2)"} {
		if _, err = sess.Run(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err = sess.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	res, err := mgr.Run("bd", "SELECT n FROM t ORDER BY n")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "1" {
		t.Errorf("rows = %v, want the committed row only", res.Rows)
	}
}

// Canceling must stop the statement and come back as ErrCanceled, the same as
// on the server-backed drivers — bytdb's row pumps poll the context.
func TestBytdbCancel(t *testing.T) {
	mgr := bytdbMgr(t)

	if _, err := mgr.Run("bd", "CREATE TABLE t (n int PRIMARY KEY)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead when the statement starts
	_, err := mgr.RunContext(ctx, "bd", "SELECT n FROM t")
	if err == nil {
		t.Fatal("a canceled statement should fail")
	}
	if !errors.Is(err, ErrCanceled) {
		t.Errorf("error = %v, want ErrCanceled", err)
	}
}

// The catalog query has to run on a real bytdb database and find both a table
// and a view — the kind of SQL that compiles in the head and not in the
// database.
func TestTablesQueryRunsOnBytdb(t *testing.T) {
	mgr := bytdbMgr(t)

	for _, stmt := range []string{
		"CREATE TABLE cats (id int PRIMARY KEY, age int)",
		"CREATE VIEW old_cats AS SELECT id FROM cats WHERE age > 4",
	} {
		if _, err := mgr.Run("bd", stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	q, err := TablesQuery("bytdb")
	if err != nil {
		t.Fatalf("TablesQuery: %v", err)
	}
	res, err := mgr.Run("bd", q)
	if err != nil {
		t.Fatalf("run catalog query: %v", err)
	}

	found := map[string]string{}
	for _, row := range res.Rows {
		found[row[1]] = row[2]
	}
	if found["cats"] != "BASE TABLE" {
		t.Errorf("cats listed as %q — got %v", found["cats"], res.Rows)
	}
	if found["old_cats"] != "VIEW" {
		t.Errorf("old_cats listed as %q — got %v", found["old_cats"], res.Rows)
	}
	for name := range found {
		if strings.HasPrefix(name, "pg_") {
			t.Errorf("catalog table %q should not be listed", name)
		}
	}
}
