package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"sync"
)

// STOP REACHES THE SERVER FOR POOLED MYSQL STATEMENTS TOO.
//
// go-sql-driver/mysql stops a canceled statement only on its own side: it
// closes the socket, and the server, which does not read the socket while a
// statement runs, goes on executing it — a COUNT(*) on a big table, a
// catalog query, a script's statement — holding its locks until done. A
// pinned Session already sends the KILL the driver does not
// (Session.reapCanceled); this does the same for every connection of a
// MySQL pool (N-089), the ones nothing pins: a script's statements, the
// sidebar's row counts (each bounded by countTimeout, so a slow count was
// abandoned and left running), the catalog and completion loads.
//
// Every MySQL pool is opened through mysqlKillConnector (openPool):
//
//	Connect ──► driver's Connect ──► SELECT CONNECTION_ID() ──► mysqlKillConn{id}
//	statement ──► lastCtx = its ctx ──► driver
//	database/sql finds the conn bad and closes it ──► Close:
//	    lastCtx canceled AND the driver had already closed the conn?
//	      yes ──► KILL <id> over a fresh connection from the same connector
//
// Why at Close. The driver closes its connection exactly when a context it
// is watching is canceled mid-statement (or the socket fails), and
// database/sql then closes the driver conn instead of pooling it — after a
// statement's error and after a canceled result's rows are closed alike. So
// Close sees every cancel without wrapping Rows and Stmts, and only a
// connection that is finished anyway is killed. A healthy connection whose
// last context ended later (a request done, a counting superseded) is still
// valid at Close and is left alone.
//
// Why a fresh connection for the KILL, not the pool. Close runs inside
// database/sql's release of the dead connection, before the pool counts it
// gone: with the pool at its limit (SetMaxOpenConns), a KILL that waited for
// a pooled connection would wait on the very one being closed.

// mysqlKillConnector is a go-sql-driver/mysql connector whose connections
// know their server id and kill their statement when it is canceled.
type mysqlKillConnector struct {
	inner driver.Connector
}

// Connect opens a driver connection and learns its id. Failing to learn it
// is no reason to refuse the connection: it is then only abandoned on a
// cancel, as before.
func (k *mysqlKillConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := k.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &mysqlKillConn{inner: c, k: k, id: connectionID(ctx, c)}, nil
}

func (k *mysqlKillConnector) Driver() driver.Driver { return k.inner.Driver() }

// kill ends connection id on the server, best effort, within killTimeout. A
// KILL that fails (the connection already gone: "Unknown thread id") changes
// nothing anyone can act on.
func (k *mysqlKillConnector) kill(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	c, err := k.inner.Connect(ctx)
	if err != nil {
		return
	}
	defer c.Close()
	if ex, ok := c.(driver.ExecerContext); ok {
		_, _ = ex.ExecContext(ctx, "KILL "+id, nil)
	}
}

// connectionID reads CONNECTION_ID() on a fresh driver connection; "" when
// it cannot. The id is spliced into a KILL statement, so only a number is
// kept; CONNECTION_ID() never returns anything else.
func connectionID(ctx context.Context, c driver.Conn) string {
	q, ok := c.(driver.QueryerContext)
	if !ok {
		return ""
	}
	rows, err := q.QueryContext(ctx, `SELECT CONNECTION_ID()`, nil)
	if err != nil {
		return ""
	}
	defer rows.Close()
	dest := make([]driver.Value, 1)
	if err = rows.Next(dest); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	var id string
	switch v := dest[0].(type) {
	case int64:
		id = strconv.FormatInt(v, 10)
	case []byte:
		id = string(v)
	case string:
		id = v
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return ""
	}
	return id
}

// mysqlKillConn is a go-sql-driver/mysql connection plus its server id and
// the context of its last statement. It forwards every interface the
// driver's connection implements that database/sql asks for, so the pool
// treats it exactly as the bare connection. sql.Conn.Raw hands it out
// instead of the driver's; code that wants the driver's (mysqlNetConn)
// unwraps it with driverConn.
//
// database/sql never uses one driver connection from two goroutines at
// once, and calls Close only when no statement is running, so lastCtx needs
// no lock of its own; reaped is a sync.Once because a Session's own reap and
// the pool's Close may both ask.
type mysqlKillConn struct {
	inner   driver.Conn
	k       *mysqlKillConnector
	id      string
	lastCtx context.Context
	reaped  sync.Once
}

// the interfaces database/sql looks for on a driver connection, all of
// which go-sql-driver/mysql's implements
var (
	_ driver.ConnPrepareContext = (*mysqlKillConn)(nil)
	_ driver.ConnBeginTx        = (*mysqlKillConn)(nil)
	_ driver.ExecerContext      = (*mysqlKillConn)(nil)
	_ driver.QueryerContext     = (*mysqlKillConn)(nil)
	_ driver.Pinger             = (*mysqlKillConn)(nil)
	_ driver.SessionResetter    = (*mysqlKillConn)(nil)
	_ driver.Validator          = (*mysqlKillConn)(nil)
	_ driver.NamedValueChecker  = (*mysqlKillConn)(nil)
)

func (c *mysqlKillConn) Prepare(q string) (driver.Stmt, error) { return c.inner.Prepare(q) }
func (c *mysqlKillConn) Begin() (driver.Tx, error)             { return c.inner.Begin() }

// PrepareContext records ctx too: a statement with arguments is run by
// database/sql as prepare-then-execute under the same context, since the
// driver skips (ErrSkip) its direct path when it would have to interpolate.
func (c *mysqlKillConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	c.lastCtx = ctx
	return c.inner.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (c *mysqlKillConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.lastCtx = ctx
	return c.inner.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *mysqlKillConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.lastCtx = ctx
	return c.inner.(driver.ExecerContext).ExecContext(ctx, q, args)
}

func (c *mysqlKillConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.lastCtx = ctx
	return c.inner.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *mysqlKillConn) Ping(ctx context.Context) error {
	return c.inner.(driver.Pinger).Ping(ctx)
}

func (c *mysqlKillConn) ResetSession(ctx context.Context) error {
	return c.inner.(driver.SessionResetter).ResetSession(ctx)
}

func (c *mysqlKillConn) IsValid() bool { return c.inner.(driver.Validator).IsValid() }

func (c *mysqlKillConn) CheckNamedValue(nv *driver.NamedValue) error {
	return c.inner.(driver.NamedValueChecker).CheckNamedValue(nv)
}

// Close closes the driver's connection, and first kills its statement on
// the server when it was canceled (see the top of this file).
func (c *mysqlKillConn) Close() error {
	c.reap()
	return c.inner.Close()
}

// reap sends the KILL when the last statement's context was canceled and
// the driver has closed the connection over it. Once per connection.
func (c *mysqlKillConn) reap() {
	if c.id == "" || c.lastCtx == nil || c.lastCtx.Err() == nil || c.IsValid() {
		return
	}
	c.reaped.Do(func() { c.k.kill(c.id) })
}

// markReaped records that the connection's statement was already killed
// (Session.reapCanceled), so Close does not send a second KILL.
func (c *mysqlKillConn) markReaped() { c.reaped.Do(func() {}) }

// driverConn unwraps what sql.Conn.Raw hands out to the driver's own
// connection: mysqlKillConn's inner one, anything else as it is.
func driverConn(dc any) any {
	if kc, ok := dc.(*mysqlKillConn); ok {
		return kc.inner
	}
	return dc
}
