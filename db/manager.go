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
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// Manager owns the pool of named database connections. Connections are
// opened lazily on first use and cached for the life of the process.
type Manager struct {
	mu    sync.Mutex
	conns map[string]*sql.DB
	cfg   *config.Config
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{conns: map[string]*sql.DB{}, cfg: cfg}
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

// DB returns the live *sql.DB for a named connection, opening and pinging
// it on first use.
func (m *Manager) DB(name string) (*sql.DB, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if dbh, ok := m.conns[name]; ok {
		return dbh, nil
	}
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	drv, err := driverFor(cc.Driver)
	if err != nil {
		return nil, err
	}
	dsn := cc.DSN
	if drv == "sqlite" {
		dsn = sqliteDSN(dsn)
	}
	dbh, err := sql.Open(drv, dsn)
	if err != nil {
		return nil, serr.Wrap(err, "conn", name, "driver", drv)
	}
	if drv == "sqlite" && strings.Contains(cc.DSN, "mode=memory") {
		// A shared in-memory DB vanishes when its last conn closes; hold one
		// open. Two are allowed so a pinned Session (the TUI's) cannot starve
		// a script running on the pool.
		dbh.SetMaxOpenConns(2)
		dbh.SetConnMaxIdleTime(0)
		dbh.SetConnMaxLifetime(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = dbh.PingContext(ctx); err != nil {
		_ = dbh.Close()
		return nil, serr.Wrap(err, "conn", name, "op", "ping")
	}
	m.conns[name] = dbh
	return dbh, nil
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
		_ = dbh.Close()
		delete(m.conns, name)
	}
}

// Close closes all open connections.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, dbh := range m.conns {
		_ = dbh.Close()
		delete(m.conns, name)
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
	dbh, err := m.DB(name)
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
// Close returns the connection to the pool.
type Session struct {
	m    *Manager
	name string
	conn *sql.Conn
}

// Session pins a connection on the named database. The caller must Close it.
func (m *Manager) Session(ctx context.Context, name string) (*Session, error) {
	dbh, err := m.DB(name)
	if err != nil {
		return nil, err
	}
	c, err := dbh.Conn(ctx)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "session")
	}
	return &Session{m: m, name: name, conn: c}, nil
}

// Run executes one statement on the pinned connection, as RunContext does on
// the pool.
func (s *Session) Run(ctx context.Context, stmt string, args ...any) (*model.Result, error) {
	return s.m.run(ctx, s.conn, s.name, stmt, args...)
}

// Close releases the connection. An unfinished transaction on it is rolled
// back by the driver when the connection is reset.
func (s *Session) Close() error {
	return s.conn.Close()
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
