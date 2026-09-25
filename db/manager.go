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
	"github.com/jackc/pgx/v5"
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

// The cap is a default for the TUI's shape, not a law: dbc web pins a session
// per browser tab, and raises it with SetMemoryPool.

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
	// memMaxOpen is the pool cap of a shared in-memory SQLite database;
	// 0 means memSQLiteMaxOpen. Guarded by mu.
	memMaxOpen int
	closed     bool
	cfg        *config.Config
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
				// open set the default cap; a raised one is applied here,
				// under mu, so a SetMemoryPool racing this open is not
				// lost between the two
				dbh.SetMaxOpenConns(m.memMaxOpenLocked())
			}
		}
		m.mu.Unlock()

		call.dbh, call.err = dbh, err
		close(call.done)
		return dbh, err
	}
}

// SetMemoryPool sets the pool cap of every shared in-memory SQLite database,
// open now or opened later. n counts every connection: the anchor, one per
// pinned session, and the pool's own. A UI that pins more than one session
// at a time (dbc web: one per browser tab) raises it; n < 3 is ignored, since
// the anchor and one session alone would leave nothing for the pool.
//
// A session that finds the pool full waits for a connection under its run's
// context, so the cost of too small a cap is a run that waits (and can be
// canceled), not a failure.
func (m *Manager) SetMemoryPool(n int) {
	if n < memSQLiteMaxOpen {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memMaxOpen = n
	for name := range m.anchors {
		if dbh := m.conns[name]; dbh != nil {
			dbh.SetMaxOpenConns(n)
		}
	}
}

func (m *Manager) memMaxOpenLocked() int {
	if m.memMaxOpen > 0 {
		return m.memMaxOpen
	}
	return memSQLiteMaxOpen
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
	dbh, err = openPool(drv, dsn)
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
	// A built-in demo is seeded here, on its first open, rather than up front
	// for every demo at launch: a headless run then touches only the demo it
	// uses, and the bytdb demo's file is not opened (and rewritten) by a run
	// that never asked for it. Seeding before the pool is cached means every
	// caller — including ones waiting on this open — sees a seeded database,
	// and since open runs once per name per Manager, a demo is never reseeded
	// under the user's edits mid-session. The caller's ctx, not pctx, bounds
	// it: connect_timeout is a limit on dialing, not on running statements.
	if cc.Demo {
		if err = seedDB(ctx, dbh, name); err != nil {
			closeOpened(dbh, anchor)
			return nil, nil, err
		}
	}
	return dbh, anchor, nil
}

// openPool is sql.Open, except for Postgres. sql.Open("pgx", dsn) builds
// pgx's bare connector, which has no way to take pgx options, so Postgres
// parses the DSN here and opens through pgxstdlib.OpenDB with pgShouldPing.
// The DSN is now parsed once per pool, not once per new connection, so a
// .pgpass or PG* environment change is picked up when dbc reopens the
// connection, not by the next pooled dial. A bad DSN fails here, not at the
// first ping. pgconn's parse error leaves the password out, as it did when
// the error came from the ping.
func openPool(drv, dsn string) (*sql.DB, error) {
	if drv != "pgx" {
		return sql.Open(drv, dsn)
	}
	pcfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxstdlib.OpenDB(*pcfg, pgxstdlib.OptionShouldPing(pgShouldPing)), nil
}

// pgShouldPing decides whether pgx pings a pooled connection before handing it
// out again. On its own, pgx pings only when the connection has sat idle more
// than a second since its last checkout (stdlib Conn.ResetSession, v5.10.0).
// A connection the server cut inside that second is handed out unchecked.
// Its statement is written into the dead socket, and the reply is the
// server's FATAL or EOF. pgx cannot tell whether the statement ran, so it
// does not return driver.ErrBadConn, database/sql does not retry, and the
// statement fails once.
//
// The go-sql-driver/mysql check runs on every checkout, and so does this one.
// config.DefaultConnIdleTimeout promises this on both engines: a connection
// the server cuts is replaced without the user seeing it. The cheap signal
// is the socket itself. A server that ends a session (pg_terminate_backend,
// idle_session_timeout, a restart) sends FATAL and closes its end. A proxy
// that drops an idle client closes its end too. Either way bytes or a FIN
// sit in the kernel's buffer, and sockQuiet finds them with no round trip.
// Then this asks for the ping, the ping fails, ResetSession returns
// ErrBadConn, and database/sql dials a fresh connection.
//
//	checkout ─► idle > 1s? ──yes──► ping ─ok─► reuse
//	               │no                 └fail─► ErrBadConn ─► new connection
//	               ▼
//	          sockQuiet? ──no───► ping (as above)
//	               │yes
//	               ▼
//	             reuse (no round trip)
//
// Pinging on every checkout (always true) would also cover this, but it
// costs a round trip per pooled statement. A script looping over a remote
// server would pay that on every Query. The peek costs one syscall.
//
// What the peek cannot see, pgx's 1s ping still covers: a path that died
// silently (no FIN, no RST) and a FIN swallowed by a pgconn background read
// left over from a slow write.
func pgShouldPing(_ context.Context, p pgxstdlib.ShouldPingParams) bool {
	return p.IdleDuration > time.Second || !sockQuiet(p.Conn.PgConn().Conn())
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

// queryVerbs are the leading verbs of a statement that returns rows. "with"
// is here only as the fallback for a WITH whose main verb sqlsplit.Verbs could
// not find — a statement malformed enough to fail either way — which keeps it
// on the Query path it always took; a well-formed WITH is judged by the verb
// after its CTE list instead.
var queryVerbs = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true,
	"describe": true, "desc": true, "pragma": true, "values": true, "table": true,
}

// returningVerbs are the writes that can carry a RETURNING clause and so
// hand back rows: INSERT/UPDATE/DELETE on Postgres, SQLite, and bytdb;
// INSERT/DELETE/REPLACE on MariaDB and SQLite; MERGE on Postgres 17+.
// Only these are checked, so DDL that merely mentions the word — a rule or
// function body outside a dollar quote, say — stays an Exec. They double as
// the list of writes a CTE body can be (see isRead).
var returningVerbs = map[string]bool{
	"insert": true, "update": true, "delete": true, "replace": true, "merge": true,
}

// isRead reports whether a statement is a plain read: its main verb is one
// that only fetches, and — for a WITH — none of its CTEs is a write. Leading
// comments are skipped, so an annotated SELECT is still recognized.
//
// The main verb of a WITH is the one after its CTE list, so `WITH x AS (…)
// UPDATE …` is a write. A CTE body is checked too because Postgres lets one
// write: `WITH d AS (DELETE … RETURNING *) SELECT count(*) FROM d` returns
// rows like a SELECT, yet it deleted them.
func isRead(stmt string) bool {
	v := sqlsplit.Verbs(stmt)
	if !queryVerbs[v.Main] {
		return false
	}
	for _, cte := range v.CTEs {
		if returningVerbs[cte] {
			return false
		}
	}
	return true
}

// IsRead is isRead for other packages: whether running stmt could change
// anything. The TUI asks it before an EXPLAIN ANALYZE, to say what will
// happen before it does.
func IsRead(stmt string) bool { return isRead(stmt) }

// isQuery reports whether a statement returns rows, and so must run as a
// Query rather than an Exec. That is every statement whose main verb is a
// read, plus a write with a RETURNING clause: run as an Exec, `INSERT …
// RETURNING id` would report rows_affected and drop the ids it was written to
// fetch. For a WITH the main verb is the one after the CTE list
// (sqlsplit.Verbs), so `WITH x AS (…) DELETE …` is an Exec that reports rows
// affected, where going by its leading "with" made it a Query with an empty
// result.
//
// RETURNING is found lexically (sqlsplit.HasKeyword), so the word inside a
// string literal, a quoted identifier, or a comment does not count — and it
// is looked for in the main statement only, from Verbs' MainAt, so a CTE body
// that returns rows to the statement (`WITH d AS (DELETE … RETURNING id)
// INSERT INTO log SELECT id FROM d`) does not make the statement itself one
// that returns rows. The alternative — try Exec and fall back to Query when
// the driver objects — was rejected: drivers do not object (they run the
// write and discard the rows), and retrying a write that already ran would
// run it twice.
func isQuery(stmt string) bool {
	v := sqlsplit.Verbs(stmt)
	if queryVerbs[v.Main] {
		return true
	}
	return returningVerbs[v.Main] && sqlsplit.HasKeyword(stmt[v.MainAt:], "returning")
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

	// stateful is set once a statement other than a plain read succeeds on
	// the session. Only those can leave session state behind — BEGIN, SET,
	// CREATE TEMP, LOCK TABLES — so a session that has only ever run SELECTs can be
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
// the side of yes: any successful statement that is not a plain read counts
// (a write with RETURNING included), even ones (an UPDATE outside a
// transaction) that leave nothing behind.
func (s *Session) Stateful() bool { return s.stateful }

// Run executes one statement on the pinned connection, as RunContext does on
// the pool.
func (s *Session) Run(ctx context.Context, stmt string, args ...any) (*model.Result, error) {
	res, err := s.m.run(ctx, s.conn, s.name, stmt, args...)
	// Keyed off the statement, not res.IsExec: an INSERT … RETURNING comes
	// back as rows, yet it is a write like any other Exec — inside a BEGIN it
	// is part of the transaction the session must not silently lose.
	if err == nil && !isRead(stmt) {
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
