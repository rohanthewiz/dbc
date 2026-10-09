package etl

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Upsert on SQLite runs in the normal suite; Postgres's staging-table
// path needs a server (TestLivePGUpsert, below). Both must agree on what an
// upsert means: a row whose key is there updates it, any other is
// inserted, and a key that comes twice ends with its last row.

// upsertLoad writes rows into table matched on key, in batches of batch
// rows, and commits.
func upsertLoad(t *testing.T, c Conn, table string, cols, key []string, batch int, rows ...[]any) (int64, error) {
	t.Helper()
	w, err := NewWriter(context.Background(), c, table, cols, WriteOptions{Upsert: key, BatchSize: batch})
	if err != nil {
		return 0, err
	}
	defer w.Abort()
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return 0, err
		}
	}
	return w.Close()
}

func TestWriterUpsertSQLite(t *testing.T) {
	c := fileConn(t, SQLite, "up.db")
	mustExec(t, c, `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT, age INTEGER)`,
		`INSERT INTO cats VALUES (1, 'Tom', 3), (2, 'Kit', 1)`)
	cols, key := []string{"id", "name", "age"}, []string{"id"}

	// a batch of 2 with 5 rows: the duplicate key 3 straddles two batches
	// (rows 3 and 5), and key 1 is repeated inside one (rows 1 and 2)
	n, err := upsertLoad(t, c, "cats", cols, key, 2,
		[]any{int64(1), "Tom", int64(4)},
		[]any{int64(1), "Thomas", int64(5)},
		[]any{int64(3), "Mog", int64(2)},
		[]any{int64(4), "Felix", int64(7)},
		[]any{int64(3), "Mog II", int64(9)})
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("count = %d, want 5 (rows written, not rows changed)", n)
	}
	want := [][]string{{"1", "Thomas", "5"}, {"2", "Kit", "1"}, {"3", "Mog II", "9"}, {"4", "Felix", "7"}}
	if got := dump(t, c, "SELECT id, name, age FROM cats ORDER BY id"); !reflect.DeepEqual(got, want) {
		t.Errorf("after the upsert:\n got %q\nwant %q", got, want)
	}

	// every column a key: nothing to update, so an existing row is left
	// alone (DO NOTHING) and a new one is inserted
	mustExec(t, c, `CREATE TABLE tags (cat INTEGER, tag TEXT, PRIMARY KEY (cat, tag))`,
		`INSERT INTO tags VALUES (1, 'grey')`)
	if _, err = upsertLoad(t, c, "tags", []string{"cat", "tag"}, []string{"cat", "tag"}, 0,
		[]any{int64(1), "grey"}, []any{int64(1), "fluffy"}); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, c, "SELECT cat, tag FROM tags ORDER BY tag"); !reflect.DeepEqual(got, [][]string{{"1", "fluffy"}, {"1", "grey"}}) {
		t.Errorf("all-key upsert: %q", got)
	}

	// the statement says what it does
	w, err := NewWriter(context.Background(), c, "cats", cols, WriteOptions{Upsert: key})
	if err != nil {
		t.Fatal(err)
	}
	sql := w.impl.(*inserter).insertSQL(1)
	_ = w.Abort()
	if !strings.HasSuffix(sql, `ON CONFLICT ("id") DO UPDATE SET "name" = excluded."name", "age" = excluded."age"`) {
		t.Errorf("insert = %s", sql)
	}
}

// An upsert whose load fails rolls back like any load: the rows it had
// already updated are as they were.
func TestWriterUpsertSQLiteAbort(t *testing.T) {
	c := fileConn(t, SQLite, "abort.db")
	mustExec(t, c, `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`, `INSERT INTO cats VALUES (1, 'Tom')`)
	_, err := upsertLoad(t, c, "cats", []string{"id", "name"}, []string{"id"}, 1,
		[]any{int64(1), "Thomas"}, []any{int64(2), nil})
	if err == nil || !strings.Contains(err.Error(), "NOT NULL") {
		t.Fatalf("err = %v, want a NOT NULL failure", err)
	}
	if got := dump(t, c, "SELECT id, name FROM cats ORDER BY id"); !reflect.DeepEqual(got, [][]string{{"1", "Tom"}}) {
		t.Errorf("after a failed upsert: %q", got)
	}
}

func TestWriterUpsertRefused(t *testing.T) {
	c := fileConn(t, SQLite, "refuse.db")
	mustExec(t, c, `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT)`)
	_, err := NewWriter(context.Background(), c, "cats", []string{"id", "name"}, WriteOptions{Upsert: []string{"uid"}})
	if err == nil || !strings.Contains(err.Error(), "not among the columns") {
		t.Errorf("key outside the columns: err = %v", err)
	}

	k := fileConn(t, Bytdb, "refuse.bytdb")
	mustExec(t, k, `CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT)`)
	_, err = NewWriter(context.Background(), k, "cats", []string{"id", "name"}, WriteOptions{Upsert: []string{"id"}})
	if err == nil || !strings.Contains(err.Error(), "upsert is for Postgres and SQLite") {
		t.Errorf("bytdb: err = %v", err)
	}
}

// The Postgres statements, checked as text here so a change to their
// shape shows without a server; TestLivePGUpsert runs them.
func TestPGUpsertStatements(t *testing.T) {
	stage, merge := pgUpsert("sales.orders", []string{"id", "total", "note"}, []string{"id"})
	if stage != `CREATE TEMP TABLE "_dbc_upsert" ON COMMIT DROP AS SELECT "id", "total", "note" FROM "sales"."orders" WITH NO DATA` {
		t.Errorf("stage = %s", stage)
	}
	want := `INSERT INTO "sales"."orders" ("id", "total", "note") SELECT DISTINCT ON ("id") "id", "total", "note" FROM "_dbc_upsert" ` +
		`ORDER BY "id", ctid DESC ON CONFLICT ("id") DO UPDATE SET "total" = excluded."total", "note" = excluded."note"`
	if merge != want {
		t.Errorf("merge:\n got %s\nwant %s", merge, want)
	}
	if _, merge = pgUpsert("t", []string{"a", "b"}, []string{"a", "b"}); !strings.HasSuffix(merge, `ON CONFLICT ("a", "b") DO NOTHING`) {
		t.Errorf("all-key merge = %s", merge)
	}
}

// TestLivePGUpsert runs the staging-table upsert on a real Postgres
// (DBC_LIVE_PG_DSN, see live_test.go): updates and inserts, a key twice
// in one load (the last wins, and the merge does not trip over "cannot
// affect row a second time"), a bytea column through the staging table,
// and an aborted or failed upsert that leaves the table and the session
// clean.
func TestLivePGUpsert(t *testing.T) {
	_, dst := livePG(t)
	mustExec(t, dst, `CREATE TABLE etl_live.up (id int PRIMARY KEY, name text NOT NULL, blob bytea, n int DEFAULT 7)`,
		`INSERT INTO etl_live.up (id, name, blob) VALUES (1, 'Tom', '\x01'), (2, 'Kit', NULL)`)
	cols, key := []string{"id", "name", "blob"}, []string{"id"}
	var traced []string
	dst.Trace = func(s string) { traced = append(traced, s) }

	n, err := upsertLoad(t, dst, "etl_live.up", cols, key, 0,
		[]any{int64(1), "Thomas", []byte{0xde, 0xad}},
		[]any{int64(3), "Mog", nil},
		[]any{int64(3), "Mog II", []byte{0x00}},
		[]any{int64(4), "tab\there", []byte{}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("count = %d, want 4 (rows written)", n)
	}
	want := [][]string{{"1", "Thomas", `\xdead`, "7"}, {"2", "Kit", "NULL", "7"}, {"3", "Mog II", `\x00`, "7"}, {"4", "tab\there", `\x`, "7"}}
	if got := dump(t, dst, `SELECT id::text, name, coalesce(blob::text, 'NULL'), n::text FROM etl_live.up ORDER BY id`); !reflect.DeepEqual(got, want) {
		t.Errorf("after the upsert:\n got %q\nwant %q", got, want)
	}
	if len(traced) != 2 || !strings.HasPrefix(traced[0], "CREATE TEMP TABLE") || !strings.HasPrefix(traced[1], "INSERT INTO") {
		t.Errorf("traced = %q, want the staging CREATE and the merge", traced)
	}

	// the staging table went with the transaction: a second load on the
	// same pool makes its own
	if _, err = upsertLoad(t, dst, "etl_live.up", cols, key, 0, []any{int64(2), "Kitty", nil}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got := dump(t, dst, `SELECT name FROM etl_live.up WHERE id = 2`); got[0][0] != "Kitty" {
		t.Errorf("second upsert: %q", got)
	}

	// abort: nothing changes
	w, err := NewWriter(context.Background(), dst, "etl_live.up", cols, WriteOptions{Upsert: key})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Write([]any{int64(1), "Gone", nil}); err != nil {
		t.Fatal(err)
	}
	_ = w.Abort()
	if got := dump(t, dst, `SELECT name FROM etl_live.up WHERE id = 1`); got[0][0] != "Thomas" {
		t.Errorf("after abort: %q", got)
	}
	// a merge the target refuses (a NULL into NOT NULL — the staging table
	// took it) rolls the whole load back, the rows before it included
	_, err = upsertLoad(t, dst, "etl_live.up", cols, key, 0, []any{int64(1), "Tom again", nil}, []any{int64(9), nil, nil})
	if err == nil || !strings.Contains(err.Error(), "null value") {
		t.Fatalf("err = %v, want a NOT NULL failure at the merge", err)
	}
	if got := dump(t, dst, `SELECT name FROM etl_live.up WHERE id = 1`); got[0][0] != "Thomas" {
		t.Errorf("after a failed merge: %q", got)
	}
	// a table the load's Setup creates (sql.write's create + upsert): the
	// staging table is made after Setup, from the table just created
	ddl, err := CreateStmt(Postgres, Postgres, "etl_live.fresh", cols, []string{"INT4", "TEXT", "BYTEA"}, key)
	if err != nil {
		t.Fatal(err)
	}
	w, err = NewWriter(context.Background(), dst, "etl_live.fresh", cols, WriteOptions{Setup: []string{ddl}, Upsert: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range [][]any{{int64(1), "a", nil}, {int64(1), "b", []byte{0xff}}} {
		if err = w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = w.Close(); err != nil {
		t.Fatalf("create + upsert: %v", err)
	}
	if got := dump(t, dst, `SELECT id::text, name, blob::text FROM etl_live.fresh`); !reflect.DeepEqual(got, [][]string{{"1", "b", `\xff`}}) {
		t.Errorf("create + upsert: %q", got)
	}
	// a key with no unique constraint is the server's own error
	mustExec(t, dst, `CREATE TABLE etl_live.nokey (id int, name text)`)
	if _, err = upsertLoad(t, dst, "etl_live.nokey", []string{"id", "name"}, key, 0, []any{int64(1), "x"}); err == nil ||
		!strings.Contains(err.Error(), "no unique or exclusion constraint") {
		t.Errorf("no unique key: err = %v", err)
	}
	assertNoOpenTx(t, dst)
}
