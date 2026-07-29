package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// newTestManager builds a manager over a private in-memory SQLite seeded with
// the demo data, so each test runs against its own cats table.
func newTestManager(t *testing.T) *db.Manager {
	t.Helper()

	cfg := &config.Config{
		MaxRows:           1000,
		DefaultConnection: "demo",
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			DSN: "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) +
				"?mode=memory&cache=shared",
		}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, "demo"); err != nil {
		t.Fatalf("seed demo: %v", err)
	}
	return mgr
}

// newTestSession pins a connection on the seeded demo database.
func newTestSession(t *testing.T) *db.Session {
	t.Helper()

	sess, err := newTestManager(t).Session(context.Background(), "demo")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestRunStatementsInOrder(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`
		INSERT INTO cats (name, breed, age) VALUES ('Zed', 'Tabby', 4);
		-- a comment between statements is not a statement
		SELECT name FROM cats WHERE name = 'Zed';
		SELECT count(*) AS n FROM cats;
	`)
	if len(stmts) != 3 {
		t.Fatalf("split gave %d statements, want 3", len(stmts))
	}
	results, err := runStatements(context.Background(), sess, stmts)
	if err != nil {
		t.Fatalf("runStatements: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if !results[0].IsExec || results[0].Affected != 1 {
		t.Errorf("insert result = %+v, want one row affected", results[0])
	}
	// the SELECT must see the INSERT that ran before it
	if len(results[1].Rows) != 1 || results[1].Rows[0][0] != "Zed" {
		t.Errorf("select saw %v, want the inserted cat", results[1].Rows)
	}
	if results[2].Rows[0][0] != "9" {
		t.Errorf("count = %s, want 9 (8 seeded + 1)", results[2].Rows[0][0])
	}
}

// The statements share one session, so a transaction spanning them holds —
// the rollback must undo the insert two statements back.
func TestRunStatementsShareOneTransaction(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`
		BEGIN;
		INSERT INTO cats (name, breed, age) VALUES ('Ghost', 'Tabby', 2);
		ROLLBACK;
		SELECT count(*) AS n FROM cats WHERE name = 'Ghost';
	`)
	results, err := runStatements(context.Background(), sess, stmts)
	if err != nil {
		t.Fatalf("runStatements: %v", err)
	}
	if n := results[len(results)-1].Rows[0][0]; n != "0" {
		t.Errorf("found %s rolled-back cats, want 0 — the statements did not share a session", n)
	}
}

// A failure stops the run there, but the results already collected come back
// so the caller can still render them.
func TestRunStatementsStopsAtFirstError(t *testing.T) {
	sess := newTestSession(t)
	stmts := sqlsplit.Split(`SELECT 1 AS n; SELECT * FROM no_such_table; SELECT 3 AS n`)
	results, err := runStatements(context.Background(), sess, stmts)
	if err == nil {
		t.Fatal("expected an error from the missing table")
	}
	if !strings.Contains(err.Error(), "no_such_table") {
		t.Errorf("error lost its cause: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want the one that succeeded", len(results))
	}
	if results[0].Rows[0][0] != "1" {
		t.Errorf("kept the wrong result: %v", results[0].Rows)
	}
}

// A canceled run stays recognizable as a cancellation, so main exits 130
// rather than reporting a query failure.
func TestRunStatementsCanceled(t *testing.T) {
	sess := newTestSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runStatements(ctx, sess, sqlsplit.Split("SELECT 1; SELECT 2"))
	if !errors.Is(err, db.ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	var se *serr.SErr
	if !errors.As(err, &se) || se.FieldsMap()["statement"] != "1/2" {
		t.Errorf("error should name the statement that stopped: %v", err)
	}
}
