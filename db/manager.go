package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// memSQLiteMaxOpen caps the pool of a shared in-memory SQLite database:
// one anchor connection (see Manager.anchors) + one pinned Session (the
// TUI's) + one for the pool, so neither the anchor nor the session can starve
// a script or a catalog refresh.
const memSQLiteMaxOpen = 3

// Manager owns the pool of named database connections. Connections are
// opened lazily on first use and cached for the life of the process.
//
// Opening a connection means a network dial and a ping, which against an
// unreachable host takes the full ping timeout. mu is never held across that:
// it guards only the maps, and a connection that is being opened is tracked in
// opening, so concurrent callers for the same name wait for the one open in
// progress (and can give up on it with their own ctx) while callers for any
// other name are not held up at all.
//
//	DBContext("a") ──lock── cached? ─yes─► return
//	                         │no
//	                  opening["a"]? ─yes─► unlock, wait on call.done (or ctx)
//	                         │no
//	              opening["a"] = call, unlock
//	              sql.Open + ping (no lock held)
//	              lock, conns["a"] = dbh, delete opening["a"], unlock
//	              close(call.done) ── wakes the waiters
type Manager struct {
	mu      sync.Mutex
	conns   map[string]*sql.DB
	opening map[string]*openCall
	// anchors holds one connection open on each shared in-memory SQLite
	// database. Such a database lives exactly as long as its last connection,
	// so without an anchor it would vanish the moment the pool closed its
	// last one — which a Session does on purpose when it is released (see
	// Session.Close). The anchor is checked out and never used, so the pool
	// can never close it.
	anchors map[string]*sql.Conn
	closed  bool
	cfg     *config.Config
}

// openCall is one open-and-ping in progress. dbh and err are written before
// done is closed and only read after, so they need no lock.
type openCall struct {
	done chan struct{}
	dbh  *sql.DB
	err  error
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		conns:   map[string]*sql.DB{},
		opening: map[string]*openCall{},
		anchors: map[string]*sql.Conn{},
		cfg:     cfg,
	}
}

// Names returns the configured connection names in config order.
func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.cfg.Connections))
	for _, c := range m.cfg.Connections {
		names = append(names, c.Name)
	}
	return names
}

func driverFor(d string) (string, error) {
	switch strings.ToLower(d) {
	case "postgres", "postgresql", "pg", "pgx":
		return "pgx", nil
	case "mysql", "mariadb":
		return "mysql", nil
	case "sqlite", "sqlite3":
		return "sqlite", nil
	case "bytdb":
		return bytdbdrv.DriverName, nil
	}
	return "", serr.New("unknown driver (use postgres, mysql, sqlite, or bytdb)", "driver", d)
}

// Driver resolves a config driver name (postgres, pg, mysql, sqlite3, ...)
// to the canonical database/sql driver name the manager opens it with, so
// other packages can branch on engine without re-listing the aliases.
func Driver(d string) (string, error) {
	return driverFor(d)
}

// DB returns the live *sql.DB for a named connection, opening and pinging
// it on first use.
func (m *Manager) DB(name string) (*sql.DB, error) {
	return m.DBContext(context.Background(), name)
}

// DBContext is DB with a context: canceling ctx abandons the wait for a
// connection being opened, and aborts the ping when this caller is the one
// opening it. The open is additionally capped at connect_timeout
// (config.ConnectTimeout).
func (m *Manager) DBContext(ctx context.Context, name string) (*sql.DB, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, serr.New("connection manager is closed", "conn", name)
		}
		if dbh, ok := m.conns[name]; ok {
			m.mu.Unlock()
			return dbh, nil
		}
		if call, ok := m.opening[name]; ok {
			// Someone else is already opening this connection: wait for them
			// rather than dial a second pool, which one of the two would only
			// have to close again — and against a dead host, rather than pay
			// a second ping timeout for the same answer.
			m.mu.Unlock()
			select {
			case <-call.done:
			case <-ctx.Done():
				return nil, wrapRunErr(ctx, ctx.Err(), name, "op", "open")
			}
			// The opener's caller gave up, which says nothing about the
			// database — this caller still wants it, so go round and open it.
			if call.err != nil && errors.Is(call.err, ErrCanceled) && ctx.Err() == nil {
				continue
			}
			return call.dbh, call.err
		}
		call := &openCall{done: make(chan struct{})}
		m.opening[name] = call
		m.mu.Unlock()

		dbh, anchor, err := m.open(ctx, name)

		m.mu.Lock()
		delete(m.opening, name)
		if err == nil && m.closed {
			// Close ran while this was dialing: nobody will close this pool
			// if it is cached now, so do not cache it.
			closeOpened(dbh, anchor)
			dbh, anchor = nil, nil
			err = serr.New("connection manager is closed", "conn", name)
		}
		if err == nil {
			m.conns[name] = dbh
			if anchor != nil {
				m.anchors[name] = anchor
			}
		}
		m.mu.Unlock()

		call.dbh, call.err = dbh, err
		close(call.done)
		return dbh, err
	}
}

// open does the slow part of DBContext — sql.Open, pool settings, ping — with
// no lock held. anchor is non-nil only for a shared in-memory SQLite database.
func (m *Manager) open(ctx context.Context, name string) (dbh *sql.DB, anchor *sql.Conn, err error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, nil, serr.New("unknown connection", "name", name)
	}
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return nil, nil, err
	}
	dsn := cc.DSN
	if drv == "sqlite" {
		dsn = sqliteDSN(dsn)
	}
	dbh, err = sql.Open(drv, dsn)
	if err != nil {
		return nil, nil, serr.Wrap(err, "conn", name, "driver", drv)
	}
	// conn_idle_timeout: see config.DefaultConnIdleTimeout for why an idle
	// pooled connection is closed at all. 0 means never, which is also
	// database/sql's own meaning for it, so the value passes straight through.
	dbh.SetConnMaxIdleTime(m.cfg.ConnIdleTimeout)
	memory := drv == "sqlite" && strings.Contains(cc.DSN, "mode=memory")
	if memory {
		// A shared in-memory DB vanishes when its last conn closes; the
		// anchor below holds one open. The cap leaves room for a pinned
		// Session (the TUI's) and a pooled run beside it, so neither starves
		// a script running on the pool.
		dbh.SetMaxOpenConns(memSQLiteMaxOpen)
		dbh.SetConnMaxIdleTime(0)
		dbh.SetConnMaxLifetime(0)
	}
	// sql.Open only validates the DSN; the ping is where the dial and the
	// handshake happen, so it is what connect_timeout bounds. 0 is "no limit
	// of dbc's own": the ping then runs under the caller's ctx alone.
	pctx, cancel := ctx, context.CancelFunc(func() {})
	if t := m.cfg.ConnectTimeout; t > 0 {
		pctx, cancel = context.WithTimeout(ctx, t)
	}
	defer cancel()
	if err = dbh.PingContext(pctx); err != nil {
		_ = dbh.Close()
		// wrapRunErr sees the caller's ctx, not pctx: only the caller giving
		// up counts as a cancel. A ping timeout is a plain connect failure.
		return nil, nil, wrapRunErr(ctx, err, name, "op", "ping")
	}
	if memory {
		if anchor, err = dbh.Conn(pctx); err != nil {
			_ = dbh.Close()
			return nil, nil, wrapRunErr(ctx, err, name, "op", "anchor")
		}
	}
	return dbh, anchor, nil
}

// closeOpened closes a pool and its anchor. The anchor goes first: DB.Close
// only closes idle connections, and a checked-out one — which the anchor
// always is — would otherwise stay open until it was returned, i.e. never.
func closeOpened(dbh *sql.DB, anchor *sql.Conn) {
	if anchor != nil {
		_ = anchor.Close()
	}
	if dbh != nil {
		_ = dbh.Close()
	}
}

// sqliteDSN gives a sqlite DSN a busy_timeout unless it already sets one.
// Without it a locked database fails writers instantly ("database is
// locked"); with it they wait for the lock. modernc's driver applies _pragma
// DSN params to every new connection, busy_timeout before the others, and
// accepts them with or without a file: prefix.
func sqliteDSN(dsn string) string {
	if strings.Contains(dsn, "busy_timeout") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(5000)"
}

// Drop closes a single cached connection and forgets it, so a later DB call
// opens a fresh one. Used when a connection is known to be unusable — an
// embedded engine whose file could not be initialized, say — where leaving the
// handle in the pool would only hand it back to the next caller.
func (m *Manager) Drop(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dbh, ok := m.conns[name]; ok {
		closeOpened(dbh, m.anchors[name])
		delete(m.conns, name)
		delete(m.anchors, name)
	}
}

// Close closes all open connections. A connection still being opened when
// Close runs is closed by its opener instead of being cached (see closed).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for name, dbh := range m.conns {
		closeOpened(dbh, m.anchors[name])
		delete(m.conns, name)
		delete(m.anchors, name)
	}
}

var queryVerbs = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true,
	"describe": true, "desc": true, "pragma": true, "values": true, "table": true,
}

// isQuery reports whether a statement returns rows. Leading comments are
// skipped, so an annotated SELECT is still recognized.
func isQuery(stmt string) bool {
	return queryVerbs[sqlsplit.FirstKeyword(stmt)]
}

// Run executes a statement on the named connection, without cancellation.
func (m *Manager) Run(name, stmt string, args ...any) (*model.Result, error) {
	return m.RunContext(context.Background(), name, stmt, args...)
}

// RunContext executes a statement on the named connection. SELECT-like
// statements return their rows; anything else runs as Exec and returns rows
// affected. Canceling ctx aborts the statement on the server where the driver
// supports it, and always stops row collection.
//
// The statement takes whichever pooled connection is free. Statements that
// depend on session state — BEGIN/COMMIT, SET, temp tables — need a Session.
func (m *Manager) RunContext(ctx context.Context, name, stmt string, args ...any) (*model.Result, error) {
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, dbh, name, stmt, args...)
}

// execQuerier is the part of *sql.DB and *sql.Conn that running a statement
// needs, so the same code serves a pooled run and a pinned session.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (m *Manager) run(ctx context.Context, ex execQuerier, name, stmt string, args ...any) (*model.Result, error) {
	stmt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if stmt == "" {
		return nil, serr.New("empty statement")
	}
	start := time.Now()

	if !isQuery(stmt) {
		res, err := ex.ExecContext(ctx, stmt, args...)
		if err != nil {
			return nil, wrapRunErr(ctx, err, name)
		}
		aff, _ := res.RowsAffected()
		return &model.Result{
			Conn: name, Query: stmt, IsExec: true, Affected: aff,
			Columns:  []string{"rows_affected"},
			Rows:     [][]string{{strconv.FormatInt(aff, 10)}},
			Raw:      [][]any{{aff}},
			Duration: time.Since(start),
		}, nil
	}

	rows, err := ex.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "columns")
	}

	result := &model.Result{Conn: name, Query: stmt, Columns: cols}
	holders := make([]any, len(cols))
	for i := range holders {
		holders[i] = new(any)
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "fetch")
		}
		if len(result.Rows) >= m.cfg.MaxRows {
			result.Truncated = true
			break
		}
		if err = rows.Scan(holders...); err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "scan")
		}
		disp := make([]string, len(cols))
		raw := make([]any, len(cols))
		for i, h := range holders {
			v := *(h.(*any))
			disp[i] = renderVal(v)
			raw[i] = rawVal(v)
		}
		result.Rows = append(result.Rows, disp)
		result.Raw = append(result.Raw, raw)
	}
	if err = rows.Err(); err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "iterate")
	}
	result.Duration = time.Since(start)
	return result, nil
}

// Session is a run pinned to one connection from the pool, so a sequence of
// statements shares a database session: BEGIN/COMMIT bracket the statements
// between them, SET and temp tables outlive the statement that made them.
// Close discards the connection rather than returning it to the pool, so none
// of that state can leak to the pool's next user.
//
// A Session is not safe for concurrent use; its holder serializes access.
type Session struct {
	m    *Manager
	name string
	conn *sql.Conn

	// stateful is set once a non-query statement succeeds on the session.
	// Only those can leave session state behind — BEGIN, SET, CREATE TEMP,
	// LOCK TABLES — so a session that has only ever run SELECTs can be
	// swapped for a fresh one without anyone losing anything.
	stateful bool
}

// Session pins a connection on the named database. The caller must Close it.
func (m *Manager) Session(ctx context.Context, name string) (*Session, error) {
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	c, err := dbh.Conn(ctx)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "session")
	}
	return &Session{m: m, name: name, conn: c}, nil
}

// Name is the connection the session is pinned to.
func (s *Session) Name() string { return s.name }

// Stateful reports whether the session may hold state a replacement session
// would not have — an open transaction, SET values, temp tables. It errs on
// the side of yes: any successful non-query statement counts, including
// ones (an UPDATE outside a transaction) that leave nothing behind.
func (s *Session) Stateful() bool { return s.stateful }

// Run executes one statement on the pinned connection, as RunContext does on
// the pool.
func (s *Session) Run(ctx context.Context, stmt string, args ...any) (*model.Result, error) {
	res, err := s.m.run(ctx, s.conn, s.name, stmt, args...)
	if err == nil && res.IsExec {
		s.stateful = true
	}
	return res, err
}

// Fault says what a failed Run means for the session it ran on, so every
// holder of a Session applies the same rule when a statement fails.
type Fault int

const (
	// FaultNone: the statement failed (or did not), the session is fine.
	FaultNone Fault = iota
	// FaultRetry: the connection was dead before the statement reached the
	// server (the driver said so with driver.ErrBadConn), and the session
	// held no state. Drop it and run the statement again on a new one.
	FaultRetry
	// FaultDrop: the connection died, possibly with the statement already
	// sent — it may have run. Drop the session but report the error as it
	// stands; replaying a statement that may have run is not safe.
	FaultDrop
	// FaultLost: the connection died with session state on it. Drop the
	// session and report SessionLost: whatever comes next must not run as if
	// the transaction or settings were still there.
	FaultLost
)

// Classify reads a Run error against the session's state:
//
//	err == nil or connection still alive ─────────────► FaultNone
//	connection dead, session stateful ────────────────► FaultLost
//	connection dead, stateless, driver.ErrBadConn ────► FaultRetry
//	connection dead, stateless, anything else ────────► FaultDrop
//
// "Dead" is not only driver.ErrBadConn. A driver reports that only when it
// knows the statement was never sent; a connection cut while the statement
// was in flight comes back as the driver's own network error ("unexpected
// EOF", "connection reset by peer"), and the connection is closed behind it.
// So after any error the driver is asked directly whether the connection is
// still usable — see alive.
func (s *Session) Classify(err error) Fault {
	if err == nil {
		return FaultNone
	}
	badConn := BadConn(err)
	if !badConn && s.alive() {
		return FaultNone
	}
	switch {
	case s.stateful:
		return FaultLost
	case badConn:
		return FaultRetry
	}
	return FaultDrop
}

// alive asks the driver whether the session's connection can still be used.
// Raw hands over the driver connection without touching the server; its
// callback returns nil either way, so this check never closes anything.
func (s *Session) alive() bool {
	ok := true
	err := s.conn.Raw(func(dc any) error {
		ok = driverConnAlive(dc)
		return nil
	})
	return err == nil && ok // ErrConnDone: the sql.Conn itself is closed
}

// driverConnAlive reports whether a driver connection is still usable.
//
// The MySQL, SQLite and bytdb drivers answer through driver.Validator, the
// interface database/sql itself asks before pooling a connection. pgx's
// stdlib adapter does not implement it, so pgx is asked directly: pgconn
// closes the connection on any network error, and IsClosed says so. A driver
// that offers neither is assumed alive, which is what database/sql assumes
// too.
func driverConnAlive(dc any) bool {
	if pc, ok := dc.(*pgxstdlib.Conn); ok {
		return !pc.Conn().IsClosed()
	}
	if v, ok := dc.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

// Close releases the session by closing its connection outright, so whatever
// the session left open is gone on every engine: the server rolls back an
// unfinished transaction when its connection ends, and SET values and temp
// tables end with it.
//
// Handing the connection back to the pool instead would leave that to the
// driver's session reset, and the drivers do not agree: pgx discards a
// connection that is mid-transaction and bytdb rolls it back, but the MySQL
// and SQLite drivers return it as-is — so the pool's next user (a catalog
// refresh, a script's Query) would run inside the user's open transaction.
// SET values and temp tables survive the reset on every driver. Sessions are
// opened rarely, so the reconnect this costs is nothing.
//
// Raw with a callback that returns driver.ErrBadConn is database/sql's one
// supported way to get a checked-out connection closed instead of pooled.
// It also closes the sql.Conn, so there is no Conn.Close after it.
func (s *Session) Close() error {
	err := s.conn.Raw(func(any) error { return driver.ErrBadConn })
	if err == nil || errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return nil // discarded as asked, or already closed
	}
	return serr.Wrap(err, "conn", s.name, "op", "close session")
}

// ErrSessionLost is returned when a pinned session's connection died after
// the session had taken on state (see FaultLost). The statement is not retried on a fresh
// session: a COMMIT replayed there would find no transaction and "succeed",
// and a statement after a lost SET would silently run under the defaults.
var ErrSessionLost = errors.New("session lost")

// SessionLost wraps err (a BadConn error from a stateful session) as
// ErrSessionLost, in words that tell the user what is gone and what to do.
func SessionLost(name string, err error) error {
	return serr.Wrap(fmt.Errorf("%w: %w", ErrSessionLost, err), "conn", name,
		"detail", "connection lost; its open transaction, SET values and temp tables are gone"+
			" — run again to continue on a new session")
}

// BadConn reports whether err means the connection the statement ran on is no
// longer usable — a caller holding a Session should discard it and open a
// fresh one.
func BadConn(err error) bool {
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone)
}

// ErrCanceled is returned when a statement was stopped by the user. Callers
// test for it with errors.Is; the underlying context error stays wrapped
// inside for anyone who cares which one it was.
var ErrCanceled = errors.New("statement canceled")

// wrapRunErr adds connection context to a statement error, and marks it as a
// cancellation when the context was the cause. Drivers report an aborted
// statement in their own words ("pq: canceling statement due to user
// request", "interrupted"), so the context is the reliable signal.
func wrapRunErr(ctx context.Context, err error, name string, kv ...string) error {
	fields := append([]string{"conn", name}, kv...)
	cerr := ctx.Err()
	if cerr == nil {
		return serr.Wrap(err, fields...)
	}
	cause := "user"
	if errors.Is(cerr, context.DeadlineExceeded) {
		cause = "timeout"
	}
	return serr.Wrap(fmt.Errorf("%w: %w", ErrCanceled, cerr),
		append(fields, "cause", cause, "driver_err", err.Error())...)
}

func renderVal(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

func rawVal(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return v
	}
}
