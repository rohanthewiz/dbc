package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// VersionTable is the bookkeeping table. The name and columns are goose's,
// so a database goose has been migrating carries straight over: its history
// is read as-is, and goose could even be pointed back at it later.
const VersionTable = "goose_db_version"

// Dialect is the little that differs per engine: how to spell the version
// table, how to spell a placeholder, whether DDL may run in a transaction,
// where a string ends when a file is split into statements, and how two runs
// are kept apart (lock.go). The zero Dialect has no version table, but it
// still splits — as Generic does.
type Dialect struct {
	Name        string
	createTable string
	placeholder func(i int) string // 1-based
	// noTx forces every migration outside a transaction. bytdb executes DDL
	// outside transaction blocks, so wrapping would only make the run fail.
	noTx bool
	// split is the engine's reading of strings, for cutting a file's free
	// parts into statements (Load): on Postgres a seed row's 'C:\' ends at
	// its quote, and the statement after it is applied on its own.
	split sqlsplit.Dialect
	// lock is the session lock a writing run holds from before it reads the
	// version table until it ends (lock.go); nil when the engine has none.
	lock *sessionLock
	// immediate opens each transactional migration with BEGIN IMMEDIATE and
	// re-reads its version row under that write lock: SQLite's guard, since
	// it has no lock that outlives a transaction (runImmediate).
	immediate bool
	// commits reports a statement the engine commits implicitly around, for
	// an engine whose DDL is not transactional (MySQL): a failed migration's
	// rollback cannot undo it, and the error says so (commit.go). Nil when
	// DDL rolls back with the rest.
	commits func(stmt string) bool
}

var (
	pgPlaceholder = func(i int) string { return fmt.Sprintf("$%d", i) }
	qPlaceholder  = func(int) string { return "?" }

	// Postgres: the exact DDL goose uses, minus the now() default which is
	// not needed since tstamp is always written explicitly.
	Postgres = Dialect{Name: "postgres", placeholder: pgPlaceholder, split: sqlsplit.Postgres, lock: pgLock,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id serial NOT NULL PRIMARY KEY,
	version_id bigint NOT NULL,
	is_applied boolean NOT NULL,
	tstamp timestamp NULL DEFAULT now()
)`}
	MySQL = Dialect{Name: "mysql", placeholder: qPlaceholder, split: sqlsplit.MySQL, lock: myLock, commits: mysqlCommits,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id serial NOT NULL,
	version_id bigint NOT NULL,
	is_applied boolean NOT NULL,
	tstamp timestamp NULL DEFAULT now(),
	PRIMARY KEY (id)
)`}
	SQLite = Dialect{Name: "sqlite", placeholder: qPlaceholder, split: sqlsplit.SQLite, immediate: true,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	version_id INTEGER NOT NULL,
	is_applied INTEGER NOT NULL,
	tstamp TIMESTAMP DEFAULT (datetime('now'))
)`}
	// bytdb speaks the Postgres dialect but runs DDL outside transactions.
	// It needs no lock: the engine holds an OS lock on its file while it is
	// open, so no second process can open the database to migrate it.
	BytDB = Dialect{Name: "bytdb", placeholder: pgPlaceholder, noTx: true, split: sqlsplit.Bytdb,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id bigserial PRIMARY KEY,
	version_id bigint NOT NULL,
	is_applied boolean NOT NULL,
	tstamp timestamptz
)`}
)

// DialectFor maps a driver name — a config `driver` value or the canonical
// database/sql driver name the manager resolves it to — to its Dialect.
func DialectFor(driver string) (Dialect, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pg", "pgx":
		return Postgres, nil
	case "mysql", "mariadb":
		return MySQL, nil
	case "sqlite", "sqlite3":
		return SQLite, nil
	case "bytdb":
		return BytDB, nil
	}
	return Dialect{}, serr.New("no migration dialect for driver", "driver", driver)
}

// Status is one migration's state as reported by `status`.
type Status struct {
	Migration
	Applied   bool
	AppliedAt string // as the database rendered it; empty when pending
}

// Migrator runs a set of migrations against one database.
type Migrator struct {
	db   *sql.DB
	d    Dialect
	migs []Migration

	// AllowMissing lets `up` apply a migration whose version is lower than
	// one already applied. That happens when two branches each add a
	// migration and merge; goose refuses by default because the lower one was
	// not tested on top of the higher one, and so do we.
	AllowMissing bool

	// Log receives a line per migration applied or rolled back. Nil is silent.
	Log func(string)
}

// New builds a Migrator over dbh for the migrations in dir, split as d's
// engine reads them.
func New(dbh *sql.DB, d Dialect, dir string) (*Migrator, error) {
	migs, err := Load(dir, d)
	if err != nil {
		return nil, err
	}
	return NewWith(dbh, d, migs), nil
}

// NewWith builds a Migrator from already-parsed migrations; they are sorted
// by version here so callers need not.
func NewWith(dbh *sql.DB, d Dialect, migs []Migration) *Migrator {
	sorted := append([]Migration(nil), migs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	return &Migrator{db: dbh, d: d, migs: sorted}
}

// Migrations returns the loaded migrations in version order.
func (m *Migrator) Migrations() []Migration { return m.migs }

func (m *Migrator) logf(format string, a ...any) {
	if m.Log != nil {
		m.Log(fmt.Sprintf(format, a...))
	}
}

// execer is what recording needs from either a *sql.DB or a *sql.Tx, so the
// version row can go in the same transaction as the migration's statements.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// querier is what reading the version table needs. The read-only commands
// (status, version) pass the *sql.DB and take no lock; a writing run passes
// the *sql.Conn it holds the lock on (Migrator.locked).
type querier interface {
	execer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ensureTable creates the version table on first use and seeds it with
// goose's version-0 row. Existence is probed with a SELECT rather than
// CREATE TABLE IF NOT EXISTS, which not every engine spells the same way.
func (m *Migrator) ensureTable(ctx context.Context, q querier) error {
	if m.hasTable(ctx, q) {
		return nil
	}
	if _, err := q.ExecContext(ctx, m.d.createTable); err != nil {
		// Someone created it between the probe and the CREATE: a `status`,
		// which takes no lock, or a SQLite run, whose guard starts only at
		// its first migration. The table is there, which is all this wanted.
		if m.hasTable(ctx, q) {
			return nil
		}
		return serr.Wrap(err, "op", "create version table")
	}
	// version 0 marks "table initialized"; goose writes the same row
	return m.record(ctx, q, 0)
}

func (m *Migrator) hasTable(ctx context.Context, q querier) bool {
	var n int
	return q.QueryRowContext(ctx, "SELECT count(*) FROM "+VersionTable).Scan(&n) == nil
}

func (m *Migrator) record(ctx context.Context, ex execer, version int64) error {
	q := fmt.Sprintf("INSERT INTO %s (version_id, is_applied, tstamp) VALUES (%s, %s, %s)",
		VersionTable, m.d.placeholder(1), m.d.placeholder(2), m.d.placeholder(3))
	if _, err := ex.ExecContext(ctx, q, version, true, time.Now()); err != nil {
		return serr.Wrap(err, "op", "record version", "version", fmt.Sprint(version))
	}
	return nil
}

func (m *Migrator) unrecord(ctx context.Context, ex execer, version int64) error {
	q := fmt.Sprintf("DELETE FROM %s WHERE version_id = %s", VersionTable, m.d.placeholder(1))
	if _, err := ex.ExecContext(ctx, q, version); err != nil {
		return serr.Wrap(err, "op", "delete version", "version", fmt.Sprint(version))
	}
	return nil
}

// applied returns, for every version the table knows, whether it is
// currently applied and when. The table is a log, newest row last: older
// goose versions recorded a rollback as an is_applied=false row rather than
// deleting, so the newest row per version is the truth.
func (m *Migrator) applied(ctx context.Context, q querier) (map[int64]Status, error) {
	if err := m.ensureTable(ctx, q); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx,
		"SELECT version_id, is_applied, tstamp FROM "+VersionTable+" ORDER BY id DESC")
	if err != nil {
		return nil, serr.Wrap(err, "op", "read versions")
	}
	defer rows.Close()

	out := map[int64]Status{}
	for rows.Next() {
		var v int64
		var app bool
		var ts any
		if err = rows.Scan(&v, &app, &ts); err != nil {
			return nil, serr.Wrap(err, "op", "scan version")
		}
		if _, seen := out[v]; seen {
			continue // an older row for a version we already have the latest of
		}
		st := Status{Applied: app}
		if app {
			st.AppliedAt = renderTime(ts)
		}
		out[v] = st
	}
	return out, rows.Err()
}

// highest is the highest applied version in app, 0 when none is.
func highest(app map[int64]Status) int64 {
	var max int64
	for v, st := range app {
		if st.Applied && v > max {
			max = v
		}
	}
	return max
}

// stillToDo re-reads one migration's newest version row: an up is still to
// do when the version is not applied, a down when it is. It is how a SQLite
// runner that read the table before another runner committed finds out
// (runImmediate); the session-locked engines read the table under the lock
// in the first place and never need it.
func (m *Migrator) stillToDo(ctx context.Context, q querier, mig Migration, up bool) (bool, error) {
	var app bool
	err := q.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT is_applied FROM %s WHERE version_id = %s ORDER BY id DESC LIMIT 1",
		VersionTable, m.d.placeholder(1)), mig.Version).Scan(&app)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, serr.Wrap(err, "migration", mig.Name, "op", "re-read version")
	}
	if app != up {
		return true, nil
	}
	what := "applied"
	if !up {
		what = "rolled back"
	}
	m.logf("SKIP %s (another run %s it)", mig.Name, what)
	return false, nil
}

func renderTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.Local().Format("2006-01-02 15:04:05")
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

// Statuses reports every loaded migration with its applied state, in version
// order — the `status` command. It takes no lock: a status during a run
// shows what the run has committed so far, which is what one asks it for.
func (m *Migrator) Statuses(ctx context.Context) ([]Status, error) {
	app, err := m.applied(ctx, m.db)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(m.migs))
	for _, mig := range m.migs {
		st := app[mig.Version]
		st.Migration = mig
		out = append(out, st)
	}
	return out, nil
}

// Version returns the highest applied version, 0 when none is. Like
// Statuses it takes no lock.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	app, err := m.applied(ctx, m.db)
	if err != nil {
		return 0, err
	}
	return highest(app), nil
}

// The writing commands below each hold the run lock (lock.go) for their
// whole length — Redo across both its halves — and do their work in the
// lower-case forms, which take the held connection.

// Up applies every pending migration in version order and returns the ones
// it applied.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) {
	return m.UpTo(ctx, -1)
}

// UpTo applies pending migrations with version <= target; a negative target
// means all of them. It stops at the first failure, leaving that migration's
// transaction rolled back and the earlier ones committed.
func (m *Migrator) UpTo(ctx context.Context, target int64) (done []Migration, err error) {
	err = m.locked(ctx, func(c *sql.Conn) (err error) {
		done, err = m.upTo(ctx, c, target)
		return err
	})
	return done, err
}

func (m *Migrator) upTo(ctx context.Context, c *sql.Conn, target int64) ([]Migration, error) {
	app, err := m.applied(ctx, c)
	if err != nil {
		return nil, err
	}
	current := highest(app)
	var pending, missing []Migration
	for _, mig := range m.migs {
		if app[mig.Version].Applied {
			continue
		}
		if target >= 0 && mig.Version > target {
			break
		}
		if mig.Version < current {
			missing = append(missing, mig)
		}
		pending = append(pending, mig)
	}
	if len(missing) > 0 && !m.AllowMissing {
		names := make([]string, len(missing))
		for i, mg := range missing {
			names[i] = mg.Name
		}
		return nil, serr.New("found unapplied migrations older than the current version; "+
			"re-run with -allow-missing to apply them anyway",
			"current", fmt.Sprint(current), "missing", strings.Join(names, ", "))
	}

	var done []Migration
	for _, mig := range pending {
		ran, err := m.runOne(ctx, c, mig, mig.Up, true)
		if err != nil {
			return done, err
		}
		if !ran {
			continue // another run applied it first (SQLite only)
		}
		done = append(done, mig)
		m.logf("OK   %s", mig.Name)
	}
	if len(done) == 0 {
		m.logf("no migrations to run. current version: %d", current)
	}
	return done, nil
}

// UpByOne applies just the next pending migration.
func (m *Migrator) UpByOne(ctx context.Context) (done []Migration, err error) {
	err = m.locked(ctx, func(c *sql.Conn) error {
		app, err := m.applied(ctx, c)
		if err != nil {
			return err
		}
		for _, mig := range m.migs {
			if !app[mig.Version].Applied {
				done, err = m.upTo(ctx, c, mig.Version)
				return err
			}
		}
		m.logf("no migrations to run")
		return nil
	})
	return done, err
}

// Down rolls back the most recently applied migration (by version), and
// returns it.
func (m *Migrator) Down(ctx context.Context) (done []Migration, err error) {
	err = m.locked(ctx, func(c *sql.Conn) (err error) {
		done, err = m.down(ctx, c)
		return err
	})
	return done, err
}

func (m *Migrator) down(ctx context.Context, c *sql.Conn) ([]Migration, error) {
	app, err := m.applied(ctx, c)
	if err != nil {
		return nil, err
	}
	v := highest(app)
	if v == 0 {
		m.logf("no migrations to roll back")
		return nil, nil
	}
	return m.downFrom(ctx, c, v, v)
}

// DownTo rolls back every applied migration with version > target, newest
// first. DownTo(0) undoes everything.
func (m *Migrator) DownTo(ctx context.Context, target int64) (done []Migration, err error) {
	err = m.locked(ctx, func(c *sql.Conn) error {
		app, err := m.applied(ctx, c)
		if err != nil {
			return err
		}
		done, err = m.downFrom(ctx, c, highest(app), target+1)
		return err
	})
	return done, err
}

// downFrom rolls back applied migrations with floor <= version <= top, newest
// first. Rollback needs the file, so a migration the table knows but the
// directory no longer holds is an error: we cannot undo what we cannot read.
func (m *Migrator) downFrom(ctx context.Context, c *sql.Conn, top, floor int64) ([]Migration, error) {
	app, err := m.applied(ctx, c)
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]Migration{}
	for _, mig := range m.migs {
		byVersion[mig.Version] = mig
	}
	var versions []int64
	for v, st := range app {
		if st.Applied && v >= floor && v <= top && v != 0 {
			versions = append(versions, v)
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] > versions[j] })

	var done []Migration
	for _, v := range versions {
		mig, ok := byVersion[v]
		if !ok {
			return done, serr.New("applied migration has no file to roll back with",
				"version", fmt.Sprint(v))
		}
		ran, err := m.runOne(ctx, c, mig, mig.Down, false)
		if err != nil {
			return done, err
		}
		if !ran {
			continue // another run rolled it back first (SQLite only)
		}
		done = append(done, mig)
		m.logf("OK   %s (rolled back)", mig.Name)
	}
	if len(done) == 0 {
		m.logf("no migrations to roll back")
	}
	return done, nil
}

// Redo rolls back the latest migration and applies it again — the quickest
// way to iterate on a migration being written. Both halves run under one
// lock, so no other run slips in between the down and the up.
func (m *Migrator) Redo(ctx context.Context) error {
	return m.locked(ctx, func(c *sql.Conn) error {
		app, err := m.applied(ctx, c)
		if err != nil {
			return err
		}
		v := highest(app)
		if v == 0 {
			m.logf("no migrations to redo")
			return nil
		}
		if _, err = m.down(ctx, c); err != nil {
			return err
		}
		_, err = m.upTo(ctx, c, v)
		return err
	})
}

// runOne executes a migration's statements and updates the version table in
// the same transaction, so a failure half-way leaves neither the schema
// change nor the version row behind. Outside a transaction (NO TRANSACTION,
// or an engine that cannot) the statements run one by one and the version
// row is written only if all of them succeeded — a failure then leaves a
// partially applied migration that must be repaired by hand, which is why
// the transaction is the default. MySQL breaks the transaction's promise for
// DDL, and a failure there says which statements stayed (commit.go).
//
// ran is false when a SQLite run finds another run already did this one
// (stillToDo); the session-locked engines always run what they are given.
func (m *Migrator) runOne(ctx context.Context, c *sql.Conn, mig Migration, stmts []string, up bool) (ran bool, err error) {
	dir := "down"
	if up {
		dir = "up"
	}
	// fail wraps statement i's error. kept is how many statements before it
	// stay applied despite the failure, with why: the error carries the
	// note as a field, and the log says it plainly, since the run's error
	// reaches a terminal as a structured log line.
	fail := func(err error, i, kept int, why string) error {
		fields := []string{"migration", mig.Name, "direction", dir,
			"statement", fmt.Sprintf("%d/%d", i+1, len(stmts))}
		if kept > 0 {
			note := partlyApplied(kept, len(stmts), why, dir)
			m.logf("PARTIAL %s: %s", mig.Name, note)
			fields = append(fields, "partly_applied", note)
		}
		return serr.Wrap(err, fields...)
	}
	bookkeep := func(ex execer) error {
		if up {
			return m.record(ctx, ex, mig.Version)
		}
		return m.unrecord(ctx, ex, mig.Version)
	}

	if mig.NoTx || m.d.noTx {
		if m.d.immediate {
			// SQLite cannot hold a lock outside a transaction, so this is
			// the best it can do: look again just before running
			if todo, err := m.stillToDo(ctx, c, mig, up); err != nil || !todo {
				return false, err
			}
		}
		why := "the migration runs outside a transaction"
		if !mig.NoTx {
			why = m.d.Name + " runs DDL outside transactions"
		}
		for i, s := range stmts {
			if _, err := c.ExecContext(ctx, s); err != nil {
				return false, fail(err, i, i, why)
			}
		}
		if err := bookkeep(c); err != nil {
			if len(stmts) > 0 {
				m.logf("PARTIAL %s: %s", mig.Name, partlyApplied(len(stmts), len(stmts), why, dir))
			}
			return false, err
		}
		return true, nil
	}

	if m.d.immediate {
		return m.runImmediate(ctx, c, mig, stmts, up, fail, bookkeep)
	}

	const ddlWhy = "MySQL commits DDL as it runs, so the rollback could not undo them"
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return false, serr.Wrap(err, "migration", mig.Name, "op", "begin")
	}
	for i, s := range stmts {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			_ = tx.Rollback()
			return false, fail(err, i, m.d.keptAfterFailure(stmts, i, err), ddlWhy)
		}
	}
	if err = bookkeep(tx); err != nil {
		_ = tx.Rollback()
		if kept := m.d.keptAfterFailure(stmts, len(stmts), err); kept > 0 {
			m.logf("PARTIAL %s: %s", mig.Name, partlyApplied(kept, len(stmts), ddlWhy, dir))
		}
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, serr.Wrap(err, "migration", mig.Name, "op", "commit")
	}
	return true, nil
}

// runImmediate is runOne's transaction on SQLite. BEGIN IMMEDIATE takes
// the database's write lock before the first statement — a plain BEGIN
// takes it only at the first write, after the re-read, so two runners could
// both read "pending" — and so the re-read and the migration happen under
// one lock: a runner that read the version table before another committed
// finds the row there now and skips the migration.
//
//	runner A                         runner B
//	read table: 10 pending           read table: 10 pending
//	BEGIN IMMEDIATE (lock)           BEGIN IMMEDIATE … waits (busy_timeout)
//	re-read: 10 pending → run it
//	record 10, COMMIT (unlock)  ───▶ … lock
//	                                 re-read: 10 applied → SKIP, ROLLBACK
//
// database/sql's BeginTx cannot ask for IMMEDIATE (modernc's driver takes
// that only from the DSN, for every transaction), so the transaction is
// driven by hand on the held connection. A ROLLBACK that fails may leave
// the connection inside the transaction, holding the write lock, so it is
// discarded rather than pooled.
func (m *Migrator) runImmediate(ctx context.Context, c *sql.Conn, mig Migration, stmts []string, up bool,
	fail func(err error, i, kept int, why string) error, bookkeep func(ex execer) error) (bool, error) {
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, serr.Wrap(err, "migration", mig.Name, "op", "begin")
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// a fresh context: ctx may be what ended the run
		rctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if _, err := c.ExecContext(rctx, "ROLLBACK"); err != nil {
			discard(c)
		}
	}()

	todo, err := m.stillToDo(ctx, c, mig, up)
	if err != nil || !todo {
		return false, err
	}
	for i, s := range stmts {
		if _, err = c.ExecContext(ctx, s); err != nil {
			return false, fail(err, i, 0, "")
		}
	}
	if err = bookkeep(c); err != nil {
		return false, err
	}
	if _, err = c.ExecContext(ctx, "COMMIT"); err != nil {
		return false, serr.Wrap(err, "migration", mig.Name, "op", "commit")
	}
	committed = true
	return true, nil
}
