package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

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
	}
	return "", serr.New("unknown driver (use postgres, mysql, or sqlite)", "driver", d)
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
	dbh, err := sql.Open(drv, cc.DSN)
	if err != nil {
		return nil, serr.Wrap(err, "conn", name, "driver", drv)
	}
	if drv == "sqlite" && strings.Contains(cc.DSN, "mode=memory") {
		// A shared in-memory DB vanishes when its last conn closes; hold one open
		dbh.SetMaxOpenConns(1)
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
func (m *Manager) RunContext(ctx context.Context, name, stmt string, args ...any) (*model.Result, error) {
	dbh, err := m.DB(name)
	if err != nil {
		return nil, err
	}
	stmt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if stmt == "" {
		return nil, serr.New("empty statement")
	}
	start := time.Now()

	if !isQuery(stmt) {
		res, err := dbh.ExecContext(ctx, stmt, args...)
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

	rows, err := dbh.QueryContext(ctx, stmt, args...)
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
