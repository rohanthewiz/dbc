package sdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// A script that reads a table with s.Reader and loads it back with a
// truncating s.Writer on the same Postgres database (N-309): the Reader's
// transaction holds ACCESS SHARE until its rows run out, and a TRUNCATE
// opened before the loop would wait on it for good, on the script's own
// goroutine. The Writer takes the same-database rules (a DELETE) while a
// Reader on its database is open, so the rewrite completes. Opt-in:
// DBC_LIVE_PG_DSN.
func TestLiveReaderWriterSameTable(t *testing.T) {
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "live", Driver: "postgres", DSN: dsn},
		{Name: "live2", Driver: "postgres", DSN: dsn}, // another pool, the same database
	}}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	for _, q := range []string{`DROP TABLE IF EXISTS dbc_live_rw`,
		`CREATE TABLE dbc_live_rw (id int PRIMARY KEY, n int)`,
		`INSERT INTO dbc_live_rw SELECT g, g FROM generate_series(1, 500) g`} {
		if _, err := mgr.Run("live", q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = mgr.Run("live", `DROP TABLE IF EXISTS dbc_live_rw`) })

	for _, writerConn := range []string{"live", "live2"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s := New(mgr, nil, nil).WithContext(ctx)
		rd, err := s.Reader("live", `SELECT id, n FROM dbc_live_rw ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		w, err := s.Writer(writerConn, "dbc_live_rw", []string{"id", "n"}, WriteOpts{Truncate: true})
		if err != nil {
			cancel()
			t.Fatalf("writer on %s: %v", writerConn, err)
		}
		for rd.Next() {
			row := rd.Row()
			if err := w.Write([]any{row[0], row[1].(int64) * 10}); err != nil {
				t.Fatal(err)
			}
		}
		if err := rd.Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Close(); err != nil {
			cancel()
			t.Fatalf("writer on %s: %v", writerConn, err)
		}
		s.Release()
		cancel()
	}
	res, err := mgr.Run("live", `SELECT count(*), sum(n) FROM dbc_live_rw`)
	if err != nil {
		t.Fatal(err)
	}
	// two passes, each n × 10, over 1..500 (sum 125250)
	if res.Rows[0][0] != "500" || res.Rows[0][1] != "12525000" {
		t.Errorf("after the rewrites: %v", res.Rows[0])
	}
}
