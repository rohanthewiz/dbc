package etl

import (
	"context"
	"testing"
)

// Empty is a load of no rows: Setup, then the truncate, committed; a table
// that is not there is no error and nothing done (N-296).
func TestEmpty(t *testing.T) {
	ctx := context.Background()
	for _, e := range []Engine{SQLite, Bytdb} {
		t.Run(e.String(), func(t *testing.T) {
			c := fileConn(t, e, "dst")
			mustExec(t, c, "CREATE TABLE t (i INTEGER PRIMARY KEY)")
			mustExec(t, c, "CREATE TABLE log (msg TEXT PRIMARY KEY)")
			mustExec(t, c, "INSERT INTO t VALUES (1)")
			did, err := Empty(ctx, c, "t", WriteOptions{Truncate: true, Setup: []string{"INSERT INTO log VALUES ('setup ran')"}})
			if err != nil || !did {
				t.Fatalf("Empty = %v, %v", did, err)
			}
			if got := dump(t, c, "SELECT count(*) FROM t")[0][0]; got != "0" {
				t.Errorf("t holds %s rows", got)
			}
			if got := dump(t, c, "SELECT count(*) FROM log")[0][0]; got != "1" {
				t.Errorf("setup ran %s times", got)
			}
			if did, err := Empty(ctx, c, "not_there", WriteOptions{Truncate: true}); err != nil || did {
				t.Errorf("a missing table: %v, %v", did, err)
			}
		})
	}
}
