package etl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	_ "modernc.org/sqlite"
)

// The embedded engines need no server, so these run in the normal suite and
// cover everything but the Postgres paths (live_test.go): the row path,
// batching, Create's type mapping, Transform, and rollback.

// fileConn opens a fresh SQLite or bytdb database in a temp dir.
func fileConn(t *testing.T, e Engine, name string) Conn {
	t.Helper()
	drv := map[Engine]string{SQLite: "sqlite", Bytdb: bytdbdrv.DriverName}[e]
	dbh, err := sql.Open(drv, filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	return Conn{Name: name, DB: dbh, Engine: e}
}

func mustExec(t *testing.T, c Conn, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := c.DB.Exec(s); err != nil {
			t.Fatalf("%s: %s: %v", c.Name, s, err)
		}
	}
}

// dump reads a whole query back as strings, NULL as "NULL", for comparing.
func dump(t *testing.T, c Conn, query string) [][]string {
	t.Helper()
	rd, err := Read(context.Background(), c, query)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var out [][]string
	for rd.Next() {
		row := make([]string, len(rd.Row()))
		for i, v := range rd.Row() {
			switch x := v.(type) {
			case nil:
				row[i] = "NULL"
			case []byte:
				row[i] = fmt.Sprintf("%x", x)
			default:
				row[i] = fmt.Sprint(x)
			}
		}
		out = append(out, row)
	}
	if err := rd.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func seedCats(t *testing.T, c Conn) {
	t.Helper()
	mustExec(t, c,
		`CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER, weight REAL, chip BLOB)`,
		"INSERT INTO cats VALUES (1, 'Whiskers', 3, 4.5, x'00ff'), (2, 'Luna', 1, NULL, NULL), "+
			"(3, 'tab\there', 5, 6.25, NULL), (4, 'new\nline', 7, 3.0, x'0a'), (5, 'Leo', 2, 5.5, NULL)")
}

func TestCopySQLiteToBytdbCreates(t *testing.T) {
	ctx := context.Background()
	src, dst := fileConn(t, SQLite, "src.db"), fileConn(t, Bytdb, "dst.bytdb")
	seedCats(t, src)

	st, err := Copy(ctx, src, "cats", dst, CopyOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 5 || st.Direct {
		t.Errorf("stats = %+v, want 5 rows, row path", st)
	}
	want := dump(t, src, "SELECT id, name, age, weight, chip FROM cats ORDER BY id")
	got := dump(t, dst, "SELECT id, name, age, weight, chip FROM cats ORDER BY id")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("destination rows\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(st.String(), "copied 5 rows src.db:cats → dst.bytdb:cats") {
		t.Errorf("String() = %q", st.String())
	}

	// Again with Truncate: the same 5 rows, not 10 — and Create is a no-op
	// on a table that is there.
	if st, err = Copy(ctx, src, "cats", dst, CopyOptions{Create: true, Truncate: true}); err != nil || st.Rows != 5 {
		t.Fatalf("second copy: %+v, %v", st, err)
	}
	if n := dump(t, dst, "SELECT count(*) FROM cats")[0][0]; n != "5" {
		t.Errorf("after truncating copy: %s rows, want 5", n)
	}
}

func TestCopyTransformSkipsAndRewrites(t *testing.T) {
	src, dst := fileConn(t, SQLite, "src.db"), fileConn(t, SQLite, "dst.db")
	seedCats(t, src)
	var progress []int64
	st, err := Copy(context.Background(), src, "cats", dst, CopyOptions{
		To: "grown", Create: true, Columns: []string{"id", "name"},
		Where: "age >= ?", Args: []any{2},
		Transform: func(row []any) ([]any, error) {
			if row[0].(int64) == 3 {
				return nil, nil // skip
			}
			row[1] = strings.ToUpper(row[1].(string))
			return row, nil
		},
		Progress: func(n int64) { progress = append(progress, n) }, ProgressEvery: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 3 || st.Skipped != 1 {
		t.Errorf("stats = %+v, want 3 rows, 1 skipped", st)
	}
	got := dump(t, dst, "SELECT * FROM grown ORDER BY id")
	want := [][]string{{"1", "WHISKERS"}, {"4", "NEW\nLINE"}, {"5", "LEO"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if !reflect.DeepEqual(progress, []int64{2}) {
		t.Errorf("progress calls = %v, want [2]", progress)
	}
}

// A failure part way leaves the destination exactly as it was — including
// the rows a Truncate would have removed and the batches already sent.
func TestCopyFailureRollsBack(t *testing.T) {
	for _, e := range []Engine{SQLite, Bytdb} {
		t.Run(e.String(), func(t *testing.T) {
			src, dst := fileConn(t, SQLite, "src.db"), fileConn(t, e, "dst")
			seedCats(t, src)
			mustExec(t, dst, "CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER, weight REAL, chip BYTEA)")
			if e == SQLite {
				// SQLite would take BYTEA as a type name, but keep it idiomatic
				mustExec(t, dst, "DROP TABLE cats",
					"CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER, weight REAL, chip BLOB)")
			}
			mustExec(t, dst, "INSERT INTO cats (id, name) VALUES (99, 'keep me')")

			boom := errors.New("boom")
			_, err := Copy(context.Background(), src, "cats", dst, CopyOptions{
				Truncate: true, BatchSize: 2, // two batches go out before the failure
				Transform: func(row []any) ([]any, error) {
					if row[0].(int64) == 5 {
						return nil, boom
					}
					return row, nil
				},
			})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want boom", err)
			}
			got := dump(t, dst, "SELECT id, name FROM cats")
			if want := [][]string{{"99", "keep me"}}; !reflect.DeepEqual(got, want) {
				t.Errorf("destination after failed copy = %q, want %q", got, want)
			}

			// A server-side failure (duplicate key against row 99's table
			// once it is not truncated) rolls back the same way.
			mustExec(t, src, "UPDATE cats SET id = 99 WHERE id = 5")
			if _, err = Copy(context.Background(), src, "cats", dst, CopyOptions{BatchSize: 2}); err == nil {
				t.Fatal("copy over a duplicate key succeeded")
			}
			if got := dump(t, dst, "SELECT count(*) FROM cats")[0][0]; got != "1" {
				t.Errorf("rows after duplicate-key failure = %s, want 1", got)
			}
		})
	}
}

func TestWriterBatchesAndAbort(t *testing.T) {
	ctx := context.Background()
	for _, e := range []Engine{SQLite, Bytdb} {
		t.Run(e.String(), func(t *testing.T) {
			dst := fileConn(t, e, "dst")
			mustExec(t, dst, "CREATE TABLE n (i INTEGER PRIMARY KEY, s TEXT)")

			w, err := NewWriter(ctx, dst, "n", []string{"i", "s"}, WriteOptions{BatchSize: 3})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 10 { // 3 full batches through the prepared stmt + 1 short
				if err = w.Write([]any{int64(i), fmt.Sprint("v", i)}); err != nil {
					t.Fatal(err)
				}
			}
			if err = w.Write([]any{1}); err == nil {
				t.Fatal("short row accepted")
			}
			// the short row poisoned the load: Close reports it, commits nothing
			if _, err = w.Close(); err == nil {
				t.Fatal("Close after a failed Write succeeded")
			}
			if got := dump(t, dst, "SELECT count(*) FROM n")[0][0]; got != "0" {
				t.Errorf("rows after failed load = %s, want 0", got)
			}

			w, err = NewWriter(ctx, dst, "n", []string{"i", "s"}, WriteOptions{BatchSize: 3})
			if err != nil {
				t.Fatal(err)
			}
			for i := range 10 {
				if err = w.Write([]any{int64(i), fmt.Sprint("v", i)}); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := w.Close(); err != nil || n != 10 {
				t.Fatalf("Close = %d, %v; want 10", n, err)
			}
			_ = w.Abort() // after Close: a no-op, not a rollback
			if got := dump(t, dst, "SELECT count(*), sum(i) FROM n")[0]; got[0] != "10" || got[1] != "45" {
				t.Errorf("count, sum = %v, want 10, 45", got)
			}
		})
	}
}

func TestCopyOptionErrors(t *testing.T) {
	src, dst := fileConn(t, SQLite, "src.db"), fileConn(t, SQLite, "dst.db")
	for _, c := range []struct {
		table string
		opt   CopyOptions
		want  string
	}{
		{"", CopyOptions{Query: "SELECT 1"}, "To (the destination table) is required"},
		{"", CopyOptions{Query: "SELECT 1", To: "x", Where: "1=1"}, "Query replaces Where"},
		{"", CopyOptions{}, "no source table"},
	} {
		if _, err := Copy(context.Background(), src, c.table, dst, c.opt); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err = %v, want %q", c.opt, err, c.want)
		}
	}
}
