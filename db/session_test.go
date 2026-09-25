package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
)

var memSeq atomic.Int64

// fileSQLiteMgr gives a Manager with one file-backed SQLite connection whose
// pool is capped at a single connection, so whatever connection a Session
// hands back is the very one the next pooled statement would get.
func fileSQLiteMgr(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "f", Driver: "sqlite",
			DSN: filepath.Join(t.TempDir(), "sess.db"),
		}},
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	dbh, err := mgr.DB("f")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dbh.SetMaxOpenConns(1)
	return mgr
}

// The modernc SQLite driver does not reset a pooled connection, so a Session
// returned to the pool used to carry its open transaction and temp tables
// into the next pooled statement. Closing a Session must end both.
func TestSessionCloseLeavesNothingInPool(t *testing.T) {
	mgr := fileSQLiteMgr(t)
	ctx := context.Background()
	if _, err := mgr.Run("f", "CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	sess, err := mgr.Session(ctx, "f")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	for _, stmt := range []string{
		"CREATE TEMP TABLE scratch (n INTEGER)",
		"BEGIN",
		"INSERT INTO t VALUES (1)",
	} {
		if _, err = sess.Run(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err = sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// with MaxOpenConns(1), a pooled-back connection would be this one
	res, err := mgr.Run("f", "SELECT count(*) FROM t")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if res.Rows[0][0] != "0" {
		t.Errorf("rows in t = %s, want 0: the open transaction leaked into the pool", res.Rows[0][0])
	}
	res, err = mgr.Run("f", "SELECT count(*) FROM sqlite_temp_master WHERE name = 'scratch'")
	if err != nil {
		t.Fatalf("temp lookup: %v", err)
	}
	if res.Rows[0][0] != "0" {
		t.Error("the session's temp table leaked into the pool")
	}
	// and nothing is left holding the write lock
	if _, err = mgr.Run("f", "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("pooled write after session close: %v", err)
	}
}

// A shared in-memory SQLite database lives only as long as its last
// connection. Discarding a Session's connection must not take the database
// with it, even when the session held the only connection the pool had made.
func TestInMemoryDBSurvivesSessionClose(t *testing.T) {
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{
			Name: "m", Driver: "sqlite",
			DSN: fmt.Sprintf("file:sesstest%d?mode=memory&cache=shared", memSeq.Add(1)),
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	ctx := context.Background()

	for i := range 3 {
		sess, err := mgr.Session(ctx, "m")
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		if i == 0 {
			if _, err = sess.Run(ctx, "CREATE TABLE keep (n INTEGER)"); err != nil {
				t.Fatalf("create: %v", err)
			}
		}
		if _, err = sess.Run(ctx, "SELECT count(*) FROM keep"); err != nil {
			t.Fatalf("session %d lost the database: %v", i, err)
		}
		if err = sess.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if _, err := mgr.Run("m", "SELECT count(*) FROM keep"); err != nil {
		t.Fatalf("pool lost the database: %v", err)
	}
}

// Stateful is what decides whether a dead session may be silently replaced,
// so it must flip on the first statement that can leave state behind.
func TestSessionStateful(t *testing.T) {
	mgr := fileSQLiteMgr(t)
	ctx := context.Background()
	sess, err := mgr.Session(ctx, "f")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	if _, err = sess.Run(ctx, "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if sess.Stateful() {
		t.Error("stateful after only a SELECT")
	}
	if _, err = sess.Run(ctx, "no such statement"); err == nil {
		t.Fatal("bad SQL succeeded")
	}
	if sess.Stateful() {
		t.Error("stateful after a failed statement")
	}
	if _, err = sess.Run(ctx, "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !sess.Stateful() {
		t.Error("not stateful after BEGIN")
	}
}

// A write with RETURNING comes back as rows, not as an exec, but it is still
// a write: the session must count as stateful after it, or a dead connection
// mid-transaction would be retried as if nothing were lost.
func TestSessionStatefulAfterReturning(t *testing.T) {
	mgr := fileSQLiteMgr(t)
	ctx := context.Background()
	if _, err := mgr.Run("f", "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	sess, err := mgr.Session(ctx, "f")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	res, err := sess.Run(ctx, "INSERT INTO t (v) VALUES ('a') RETURNING id")
	if err != nil {
		t.Fatalf("insert returning: %v", err)
	}
	if res.IsExec || len(res.Rows) != 1 || res.Rows[0][0] != "1" {
		t.Errorf("exec=%v rows=%v, want [[1]] as rows", res.IsExec, res.Rows)
	}
	if !sess.Stateful() {
		t.Error("not stateful after INSERT … RETURNING")
	}
}

// blackhole listens on loopback and accepts connections without ever
// answering, the way a firewalled or wedged server looks to a client: the TCP
// dial succeeds and the postgres startup handshake then waits forever.
// accepted receives once per connection taken.
func blackhole(t *testing.T) (addr string, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ch := make(chan struct{}, 16)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			ch <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return ln.Addr().String(), ch
}

// A connection stuck in its first ping must hold up nobody but the callers
// waiting on that same connection — and each of them only until its own ctx
// says stop.
func TestSlowOpenDoesNotBlockOtherConnections(t *testing.T) {
	addr, accepted := blackhole(t)
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{
			{Name: "slow", Driver: "postgres", DSN: "postgres://u:p@" + addr + "/x?sslmode=disable"},
			{Name: "fast", Driver: "sqlite",
				DSN: fmt.Sprintf("file:slowtest%d?mode=memory&cache=shared", memSeq.Add(1))},
		},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()

	openerCtx, cancelOpener := context.WithCancel(context.Background())
	defer cancelOpener()
	openerErr := make(chan error, 1)
	go func() {
		_, err := mgr.DBContext(openerCtx, "slow")
		openerErr <- err
	}()
	select {
	case <-accepted: // the open is now parked in the handshake
	case <-time.After(3 * time.Second):
		t.Fatal("the slow connection never dialed")
	}

	start := time.Now()
	if _, err := mgr.DB("fast"); err != nil {
		t.Fatalf("fast: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("opening another connection took %s while one was stuck pinging", d)
	}

	// a second caller for the stuck connection waits on the first open, and
	// can give up on it independently
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelWait()
	start = time.Now()
	_, err := mgr.DBContext(waitCtx, "slow")
	if !errors.Is(err, ErrCanceled) {
		t.Errorf("waiter err = %v, want ErrCanceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("waiter took %s to honor its ctx", d)
	}

	// and the opener's own cancel aborts the ping (this config sets no
	// connect_timeout, so nothing else would)
	cancelOpener()
	select {
	case err = <-openerErr:
		if !errors.Is(err, ErrCanceled) {
			t.Errorf("opener err = %v, want ErrCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceling the opener did not stop its ping")
	}
}

// conn_idle_timeout reaches the pool: an idle pooled connection is closed once
// it has sat unused that long. database/sql's cleaner wakes at most once a
// second, so this polls rather than sleeping a fixed time.
func TestConnIdleTimeoutClosesIdleConns(t *testing.T) {
	cfg := &config.Config{
		MaxRows:         1000,
		ConnIdleTimeout: 10 * time.Millisecond,
		Connections: []config.Connection{{
			Name: "f", Driver: "sqlite",
			DSN: filepath.Join(t.TempDir(), "idle.db"),
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()
	dbh, err := mgr.DB("f")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err = mgr.Run("f", "SELECT 1"); err != nil {
		t.Fatalf("select: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for dbh.Stats().MaxIdleTimeClosed == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("idle connection never closed; stats: %+v", dbh.Stats())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := dbh.Stats().Idle; n != 0 {
		t.Errorf("idle connections = %d after the timeout, want 0", n)
	}
	// and the pool reopens on demand
	if _, err = mgr.Run("f", "SELECT 1"); err != nil {
		t.Fatalf("select after idle close: %v", err)
	}
}

// connect_timeout bounds the open: a host that accepts the TCP connection but
// never answers fails once it has elapsed, as a connect failure rather than
// as a cancel — the user did not stop anything.
func TestConnectTimeoutBoundsOpen(t *testing.T) {
	addr, _ := blackhole(t)
	cfg := &config.Config{
		MaxRows:        1000,
		ConnectTimeout: 300 * time.Millisecond,
		Connections: []config.Connection{{
			Name: "slow", Driver: "postgres", DSN: "postgres://u:p@" + addr + "/x?sslmode=disable",
		}},
	}
	mgr := NewManager(cfg)
	defer mgr.Close()

	start := time.Now()
	_, err := mgr.DB("slow")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("opened a connection to a server that never answers")
	}
	if errors.Is(err, ErrCanceled) {
		t.Errorf("err = %v, want a plain connect failure, not ErrCanceled", err)
	}
	if elapsed < 250*time.Millisecond || elapsed > 3*time.Second {
		t.Errorf("open failed after %s, want about the 300ms connect_timeout", elapsed)
	}
}

// A WITH is judged by the verb after its CTE list, and by what its CTEs do:
// whether it runs as a Query (isQuery) and whether it leaves the session
// stateful (isRead negated).
func TestIsQueryIsReadWith(t *testing.T) {
	cases := []struct {
		stmt        string
		query, read bool
	}{
		{"WITH x AS (SELECT 1) SELECT * FROM x", true, true},
		{"WITH x AS (SELECT 1) UPDATE t SET a = 1", false, false},
		{"WITH x AS (SELECT 1) DELETE FROM t WHERE id IN (SELECT * FROM x)", false, false},
		{"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", false, false},
		{"WITH x AS (SELECT 1) DELETE FROM t RETURNING id", true, false},
		// a write in a CTE returns rows to the statement, not to the caller
		{"WITH d AS (DELETE FROM t RETURNING id) INSERT INTO log SELECT id FROM d", false, false},
		// a SELECT that writes: rows back, but not a read
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT count(*) FROM d", true, false},
		// unchanged for everything that is not a WITH
		{"SELECT 1", true, true},
		{"INSERT INTO t VALUES (1) RETURNING id", true, false},
		{"UPDATE t SET a = 1", false, false},
	}
	for _, c := range cases {
		if got := isQuery(c.stmt); got != c.query {
			t.Errorf("isQuery(%q) = %v, want %v", c.stmt, got, c.query)
		}
		if got := isRead(c.stmt); got != c.read {
			t.Errorf("isRead(%q) = %v, want %v", c.stmt, got, c.read)
		}
	}
}

// A write behind a CTE list reports rows affected — it used to run as a
// Query and show an empty result — and leaves the session stateful, as any
// other write does. With RETURNING it hands back its rows.
func TestSessionWithWrite(t *testing.T) {
	mgr := fileSQLiteMgr(t)
	ctx := context.Background()
	for _, s := range []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)",
		"INSERT INTO t (v) VALUES ('a'), ('b'), ('c')",
	} {
		if _, err := mgr.Run("f", s); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	sess, err := mgr.Session(ctx, "f")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()

	res, err := sess.Run(ctx, "WITH old AS (SELECT id FROM t WHERE id < 3) UPDATE t SET v = 'x' WHERE id IN (SELECT id FROM old)")
	if err != nil {
		t.Fatalf("with update: %v", err)
	}
	if !res.IsExec || res.Affected != 2 {
		t.Errorf("exec=%v affected=%d rows=%v, want 2 rows affected", res.IsExec, res.Affected, res.Rows)
	}
	if !sess.Stateful() {
		t.Error("not stateful after WITH … UPDATE")
	}

	res, err = sess.Run(ctx, "WITH gone AS (SELECT 3 AS id) DELETE FROM t WHERE id IN (SELECT id FROM gone) RETURNING v")
	if err != nil {
		t.Fatalf("with delete returning: %v", err)
	}
	if res.IsExec || len(res.Rows) != 1 || res.Rows[0][0] != "c" {
		t.Errorf("exec=%v rows=%v, want [[c]] as rows", res.IsExec, res.Rows)
	}
}
