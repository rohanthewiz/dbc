package db

import (
	"path/filepath"
	"strconv"
	"sync"
	"testing"

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
