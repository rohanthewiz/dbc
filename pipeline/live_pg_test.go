package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/rohanthewiz/dbc/etl"
)

// Live Postgres tests for the stream path's source and sink, opt-in on
// DBC_LIVE_PG_DSN like etl's:
//
//	DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:5432/dbc?sslmode=disable' \
//	go test ./pipeline -run LivePG -v
//
// livePGHost is newTestHost plus "pg" and "pg2": two pools on the one
// live database, as two config names for one server would be, so the
// same-database lookup (etl.SameDatabase) is the real one and not the
// shared-pool shortcut. The tests work in a pipeline_live schema, dropped
// first and on cleanup.
func livePGHost(t *testing.T) *testHost {
	t.Helper()
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	h := newTestHost(t)
	for _, name := range []string{"pg", "pg2"} {
		dbh, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dbh.Close() })
		h.conns[name] = etl.Conn{Name: name, DB: dbh, Engine: etl.Postgres}
	}
	h.exec(t, "pg", "DROP SCHEMA IF EXISTS pipeline_live CASCADE")
	h.exec(t, "pg", "CREATE SCHEMA pipeline_live")
	t.Cleanup(func() { _, _ = h.conns["pg"].DB.Exec("DROP SCHEMA IF EXISTS pipeline_live CASCADE") })
	return h
}

// count is SELECT count(*) of table on conn.
func (h *testHost) count(t *testing.T, conn, table string) string {
	t.Helper()
	return h.rows(t, conn, "SELECT count(*) FROM "+table)[0][0]
}

// TestLivePGMoveKeepsRowsWhenTheSinkFails is N-251: a sql.read that moves
// rows (DELETE … RETURNING) commits only once its sinks have. Read unheld,
// a result under one batch committed as its rows ran out, before the sink
// had even opened, so a sink that failed lost the rows. And a move that
// lands commits: the rows are in the sink and gone from the source.
func TestLivePGMoveKeepsRowsWhenTheSinkFails(t *testing.T) {
	h := livePGHost(t)
	h.exec(t, "pg", "CREATE TABLE pipeline_live.moving (id int PRIMARY KEY, v text)")
	h.exec(t, "pg", "INSERT INTO pipeline_live.moving SELECT g, 'v' || g FROM generate_series(1, 10) g")

	p := New("move")
	f := p.Fragment("f")
	f.Node("sql.read", Config{"conn": "pg", "query": "DELETE FROM pipeline_live.moving RETURNING id, v"})
	f.ThenFunc(func(*Env, *Batch) error { return errors.New("sink down") })
	if _, err := Run(context.Background(), h, p.Spec(), Options{}); err == nil || !strings.Contains(err.Error(), "sink down") {
		t.Fatalf("run: %v, want the sink's failure", err)
	}
	if n := h.count(t, "pg", "pipeline_live.moving"); n != "10" {
		t.Fatalf("after a failed move the source has %s rows, want all 10 back", n)
	}

	p = New("move")
	f = p.Fragment("f")
	f.Node("sql.read", Config{"conn": "pg", "query": "DELETE FROM pipeline_live.moving RETURNING id, v"})
	f.Then("sql.write", Config{"conn": "b", "table": "moved", "create": "true"})
	if _, err := Run(context.Background(), h, p.Spec(), Options{}); err != nil {
		t.Fatal(err)
	}
	if src, dst := h.count(t, "pg", "pipeline_live.moving"), h.count(t, "b", "moved"); src != "0" || dst != "10" {
		t.Errorf("after the move: source %s rows, sink %s; want 0 and 10", src, dst)
	}
}
