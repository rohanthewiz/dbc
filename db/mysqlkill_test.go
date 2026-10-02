package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"
)

// killFake is a driver.Connector standing in for go-sql-driver/mysql under
// mysqlKillConnector: each connection has an id, answers CONNECTION_ID(),
// and, like the real driver, closes itself when a context it is running a
// statement under is canceled. Every statement any connection receives is
// recorded, so a test can see the KILL go out.
type killFake struct {
	mu    sync.Mutex
	next  int64
	stmts []string
}

func (f *killFake) Connect(context.Context) (driver.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return &killFakeConn{f: f, id: f.next}, nil
}

func (f *killFake) Driver() driver.Driver { return nil }

func (f *killFake) record(q string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmts = append(f.stmts, q)
}

func (f *killFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stmts)
}

type killFakeConn struct {
	f      *killFake
	id     int64
	mu     sync.Mutex
	closed bool
}

func (c *killFakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *killFakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }
func (c *killFakeConn) Close() error                        { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *killFakeConn) IsValid() bool                       { c.mu.Lock(); defer c.mu.Unlock(); return !c.closed }

func (c *killFakeConn) PrepareContext(_ context.Context, q string) (driver.Stmt, error) {
	return c.Prepare(q)
}

func (c *killFakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

func (c *killFakeConn) Ping(context.Context) error               { return nil }
func (c *killFakeConn) ResetSession(context.Context) error       { return nil }
func (c *killFakeConn) CheckNamedValue(*driver.NamedValue) error { return nil }

// ExecContext: "SLEEP" runs until ctx is canceled, then closes the
// connection, as go-sql-driver/mysql's watcher does; anything else is done
// at once.
func (c *killFakeConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.f.record(q)
	if q == "SLEEP" {
		<-ctx.Done()
		_ = c.Close()
		return nil, ctx.Err()
	}
	return driver.RowsAffected(0), nil
}

func (c *killFakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	if q != `SELECT CONNECTION_ID()` {
		c.f.record(q)
	}
	return &killFakeRows{id: c.id}, nil
}

type killFakeRows struct {
	id   int64
	done bool
}

func (r *killFakeRows) Columns() []string { return []string{"id"} }
func (r *killFakeRows) Close() error      { return nil }
func (r *killFakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done, dest[0] = true, r.id
	return nil
}

// A pooled statement whose context is canceled while it runs is killed on
// the server by its connection's id, over a connection of its own; one that
// finished, or a connection closed while healthy, is not (N-089). The pool
// is held to one connection, so a KILL that waited for the pool would hang.
func TestMySQLKillConnKillsACanceledPooledStatement(t *testing.T) {
	f := &killFake{}
	dbh := sql.OpenDB(&mysqlKillConnector{inner: f})
	defer dbh.Close()
	dbh.SetMaxOpenConns(1)

	if _, err := dbh.ExecContext(context.Background(), "UPDATE t SET n = 1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := dbh.ExecContext(ctx, "SLEEP")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("canceled statement: err = %v", err)
		}
	case <-time.After(killTimeout + time.Second):
		t.Fatal("the canceled statement's release hung: the KILL waited on the pool")
	}
	if got := f.seen(); !slices.Contains(got, "KILL 1") {
		t.Fatalf("statements = %q, want KILL 1 for the canceled one's connection", got)
	}

	// the pool goes on, on a new connection, and a healthy close kills nothing
	if _, err := dbh.ExecContext(context.Background(), "UPDATE t SET n = 2"); err != nil {
		t.Fatal(err)
	}
	n := len(f.seen())
	dbh.Close()
	if got := f.seen(); len(got) != n {
		t.Errorf("closing healthy connections sent %q", got[n:])
	}
}

// The driver's own connection is reached through the wrapper.
func TestDriverConnUnwraps(t *testing.T) {
	inner := &killFakeConn{}
	if driverConn(&mysqlKillConn{inner: inner}) != any(inner) {
		t.Error("driverConn did not unwrap mysqlKillConn")
	}
	if driverConn(inner) != any(inner) {
		t.Error("driverConn changed a bare connection")
	}
}
