package db

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
)

func TestSqliteDSN(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cats.db", "cats.db?_pragma=busy_timeout(5000)"},
		{"file:cats.db?cache=shared", "file:cats.db?cache=shared&_pragma=busy_timeout(5000)"},
		{"file:cats.db?_pragma=busy_timeout(100)", "file:cats.db?_pragma=busy_timeout(100)"},
	}
	for _, c := range cases {
		if got := sqliteDSN(c.in); got != c.want {
			t.Errorf("sqliteDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Concurrent writers on a file-backed SQLite must wait for the write lock,
// not fail instantly with "database is locked".
func TestSqliteConcurrentWrites(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "file", Driver: "sqlite",
			DSN: filepath.Join(t.TempDir(), "busy.db"),
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()

	if _, err := mgr.Run("file", "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	const writers, writes = 4, 25
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range writes {
				if _, err := mgr.Run("file", "INSERT INTO t (n) VALUES (1)"); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatalf("concurrent write failed: %v", err)
	}

	res, err := mgr.Run("file", "SELECT count(*) FROM t")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := strconv.Itoa(writers * writes); res.Rows[0][0] != want {
		t.Errorf("count = %s, want %s", res.Rows[0][0], want)
	}
}

// SetMemoryPool raises the cap of an in-memory SQLite pool that is already
// open, and of one opened later; it never lowers it below the TUI's shape.
func TestSetMemoryPool(t *testing.T) {
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "a", Driver: "sqlite", DSN: "file:" + memName(t) + "a?mode=memory&cache=shared"},
		{Name: "b", Driver: "sqlite", DSN: "file:" + memName(t) + "b?mode=memory&cache=shared"},
	}}
	m := NewManager(cfg)
	defer m.Close()
	a, err := m.DB("a")
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Stats().MaxOpenConnections; got != memSQLiteMaxOpen {
		t.Fatalf("default cap = %d, want %d", got, memSQLiteMaxOpen)
	}
	m.SetMemoryPool(2) // below the minimum: ignored
	if got := a.Stats().MaxOpenConnections; got != memSQLiteMaxOpen {
		t.Fatalf("cap after SetMemoryPool(2) = %d, want %d", got, memSQLiteMaxOpen)
	}
	m.SetMemoryPool(16)
	if got := a.Stats().MaxOpenConnections; got != 16 {
		t.Fatalf("open pool's cap = %d, want 16", got)
	}
	b, err := m.DB("b")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Stats().MaxOpenConnections; got != 16 {
		t.Fatalf("later pool's cap = %d, want 16", got)
	}
}

// Disconnect closes a pool — a later call opens a fresh one — but leaves a
// shared in-memory database's open, since closing it would destroy it.
func TestDisconnectSparesInMemory(t *testing.T) {
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "file", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "d.db")},
		{Name: "mem", Driver: "sqlite", DSN: "file:" + memName(t) + "?mode=memory&cache=shared"},
	}}
	m := NewManager(cfg)
	defer m.Close()
	if m.Disconnect("file") {
		t.Error("disconnecting a pool never opened reported closing one")
	}
	for _, name := range []string{"file", "mem"} {
		if _, err := m.Run(name, "CREATE TABLE t (n INTEGER)"); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
	}
	before, _ := m.DB("file")
	if !m.Disconnect("file") {
		t.Error("an open file database's pool was not closed")
	}
	if after, _ := m.DB("file"); after == before {
		t.Error("the closed pool was handed out again")
	}
	if _, err := m.Run("file", "SELECT count(*) FROM t"); err != nil {
		t.Errorf("the file database after a reopen: %v", err)
	}
	if m.Disconnect("mem") {
		t.Error("an in-memory database's pool was closed")
	}
	if _, err := m.Run("mem", "SELECT count(*) FROM t"); err != nil {
		t.Errorf("the in-memory database lost its table: %v", err)
	}
}

// SQLite's rows affected is the last write's until another write: a DDL
// statement after an INSERT of 4 rows reports 0, not 4 (N-207). The writes
// keep their own counts, a REPLACE and a WITH … DELETE included. On one
// pinned session, as a headless multi-statement run has.
func TestSqliteDDLRowsAffected(t *testing.T) {
	mgr := NewManager(&config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "lite", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "aff.db")},
	}})
	defer mgr.Close()
	s, err := mgr.Session(context.Background(), "lite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []struct {
		stmt string
		want int64
	}{
		{"CREATE TABLE a (x INTEGER PRIMARY KEY)", 0},
		{"INSERT INTO a VALUES (1), (2), (3), (4)", 4},
		{"CREATE TABLE b (y INTEGER)", 0},
		{"CREATE INDEX bi ON b (y)", 0},
		{"DROP TABLE b", 0},
		{"REPLACE INTO a VALUES (1), (2)", 2},
		{"ALTER TABLE a ADD COLUMN z TEXT", 0},
		{"UPDATE a SET z = 'k' WHERE x > 1", 3},
		{"WITH d AS (SELECT 4 AS x) DELETE FROM a WHERE x IN (SELECT x FROM d)", 1},
		{"CREATE VIEW v AS SELECT x FROM a", 0},
	} {
		r, err := s.Run(context.Background(), c.stmt)
		if err != nil {
			t.Fatalf("%s: %v", c.stmt, err)
		}
		if r.Affected != c.want {
			t.Errorf("%s: %d rows affected, want %d", c.stmt, r.Affected, c.want)
		}
	}
}

// A schema summary remembered as slow (MarkSummarySlow) outlives a
// Disconnect — the database is the same one when the user connects again,
// and forgetting would charge the summary's wait on every reconnect — and
// goes with a Drop, with what derived connections remembered: an edit may
// have pointed the name at another database. A configured connection that
// merely has a derived-looking name keeps its own.
func TestSummarySlowMemory(t *testing.T) {
	cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{
		{Name: "pg", Driver: "postgres", DSN: "postgres://localhost/app"},
		{Name: "pg/own", Driver: "postgres", DSN: "postgres://localhost/own"},
		{Name: "other", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "o.db")},
	}}
	m := NewManager(cfg)
	defer m.Close()
	if m.SummarySlow("pg") {
		t.Fatal("slow before anything was marked")
	}
	for _, name := range []string{"pg", "pg/analytics", "pg/own", "other"} {
		m.MarkSummarySlow(name)
	}

	m.Disconnect("pg")
	if !m.SummarySlow("pg") || !m.SummarySlow("pg/analytics") {
		t.Error("a Disconnect forgot the slow summary")
	}

	m.Drop("pg")
	if m.SummarySlow("pg") || m.SummarySlow("pg/analytics") {
		t.Error("a Drop kept the slow summary of pg or its derived connection")
	}
	if !m.SummarySlow("pg/own") || !m.SummarySlow("other") {
		t.Error("a Drop of pg forgot another configured connection's")
	}
}

// The slow-summary memory lapses after slowSummaryTTL (N-314), so one
// transient timeout does not cost a long-running process its counts for
// good; a second mark restarts the window. The entry's time is backdated
// rather than the test sleeping, so the window can stay at its real size.
func TestSummarySlowLapses(t *testing.T) {
	m := NewManager(&config.Config{MaxRows: 1000})
	defer m.Close()
	backdate := func(name string, by time.Duration) {
		m.mu.Lock()
		m.slowSummary[name] = m.slowSummary[name].Add(-by)
		m.mu.Unlock()
	}

	m.MarkSummarySlow("pg")
	backdate("pg", slowSummaryTTL-time.Minute)
	if !m.SummarySlow("pg") {
		t.Fatal("forgotten inside its window")
	}

	backdate("pg", 2*time.Minute) // now a minute past the window
	if m.SummarySlow("pg") {
		t.Fatal("still remembered after its window")
	}
	m.mu.Lock()
	_, kept := m.slowSummary["pg"]
	m.mu.Unlock()
	if kept {
		t.Error("the lapsed entry was left in the map")
	}

	// tried again and slow again: a fresh window, not the old one's end
	m.MarkSummarySlow("pg")
	backdate("pg", slowSummaryTTL/2)
	m.MarkSummarySlow("pg")
	backdate("pg", slowSummaryTTL/2+time.Minute)
	if !m.SummarySlow("pg") {
		t.Error("a second mark did not restart the window")
	}
}

// A connection that cannot be opened is marked as such (N-287), under its
// serr wrap so the fields still show; a statement's failure is not.
func TestConnectErrMarked(t *testing.T) {
	cfg := &config.Config{MaxRows: 10, ConnectTimeout: 2 * time.Second, Connections: []config.Connection{
		{Name: "down", Driver: "postgres", DSN: "postgres://u:p@127.0.0.1:1/x?sslmode=disable"},
		{Name: "lite", Driver: "sqlite", DSN: "file:connerr?mode=memory&cache=shared"},
	}}
	mgr := NewManager(cfg)
	defer mgr.Close()
	_, err := mgr.DB("down")
	if err == nil || !IsConnectErr(err) {
		t.Fatalf("an unreachable server: %v, IsConnectErr %v", err, IsConnectErr(err))
	}
	if f := serr.SErrFromErr(err).FieldsMap(); f["conn"] != "down" {
		t.Errorf("the serr fields are hidden: %v (%T)", f, err)
	}
	_, err = mgr.Run("lite", "SELEC 1")
	if err == nil || IsConnectErr(err) {
		t.Errorf("a syntax error is not a connect failure: %v", err)
	}
}
