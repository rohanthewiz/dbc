package etl

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/serr"
)

// A batched INSERT that fails says which rows the batch held, not the row
// that filled it (N-295): with batches of 3, a duplicate key at row 5 is
// in rows 4-6, and there is no "row" field naming row 6.
func TestInsertFailureNamesTheBatchRows(t *testing.T) {
	ctx := context.Background()
	dst := fileConn(t, SQLite, "dst")
	mustExec(t, dst, "CREATE TABLE n (i INTEGER PRIMARY KEY)")
	w, err := NewWriter(ctx, dst, "n", []string{"i"}, WriteOptions{BatchSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	var werr error
	for _, i := range []int64{1, 2, 3, 4, 2, 6, 7} { // row 5 repeats key 2, after it was committed in batch 1
		if werr = w.Write([]any{i}); werr != nil {
			break
		}
	}
	if werr == nil {
		_, werr = w.Close()
	}
	if werr == nil {
		t.Fatal("a duplicate key loaded")
	}
	f := serr.SErrFromErr(werr).FieldsMap()
	if f["rows"] != "4-6" || f["batch_rows"] != "3" {
		t.Errorf("fields = %v, want rows 4-6 of a 3-row batch", f)
	}
	if _, ok := f["row"]; ok {
		t.Errorf("a row field (%q) names the row that filled the batch, not the one that failed", f["row"])
	}
	if !strings.Contains(werr.Error(), "UNIQUE") {
		t.Errorf("err = %v", werr)
	}
}

// A COPY into Postgres that fails carries the server's DETAIL and its
// CONTEXT — the key that clashed, and the COPY's line, which is the
// failing row (N-295). Opt-in: DBC_LIVE_PG_DSN.
func TestLiveCopyFailureCarriesDetail(t *testing.T) {
	c := managerConn(t, "DBC_LIVE_PG_DSN", "postgres", Postgres)
	mustExec(t, c, "DROP TABLE IF EXISTS dbc_live_loaderr")
	mustExec(t, c, "CREATE TABLE dbc_live_loaderr (id int PRIMARY KEY, v int CHECK (v < 100))")
	t.Cleanup(func() { mustExec(t, c, "DROP TABLE IF EXISTS dbc_live_loaderr") })
	ctx := context.Background()

	load := func(rows [][]any) error {
		w, err := NewWriter(ctx, c, "dbc_live_loaderr", []string{"id", "v"}, WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Abort()
		for _, r := range rows {
			if err = w.Write(r); err != nil {
				return err
			}
		}
		_, err = w.Close()
		return err
	}
	var rows [][]any
	for i := range 50 {
		rows = append(rows, []any{int64(i), int64(1)})
	}
	rows = append(rows, []any{int64(42), int64(1)}) // row 51 repeats key 42
	err := load(rows)
	if err == nil {
		t.Fatal("a duplicate key loaded")
	}
	msg := err.Error()
	t.Logf("duplicate: %s", msg)
	if !strings.Contains(msg, "DETAIL: Key (id)=(42) already exists") || !strings.Contains(msg, "COPY dbc_live_loaderr, line 51") {
		t.Errorf("the duplicate's error lacks the server's DETAIL or CONTEXT: %s", msg)
	}
	err = load([][]any{{int64(1), int64(1)}, {int64(2), int64(500)}})
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("a CHECK failure should name its COPY line: %v", err)
	}
}
