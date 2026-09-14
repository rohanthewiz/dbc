package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// VersionTable is the bookkeeping table. The name and columns are goose's,
// so a database goose has been migrating carries straight over: its history
// is read as-is, and goose could even be pointed back at it later.
const VersionTable = "goose_db_version"

// Dialect is the little that differs per engine: how to spell the version
// table, how to spell a placeholder, and whether DDL may run in a transaction.
type Dialect struct {
	Name        string
	createTable string
	placeholder func(i int) string // 1-based
	// noTx forces every migration outside a transaction. bytdb executes DDL
	// outside transaction blocks, so wrapping would only make the run fail.
	noTx bool
}

var (
	pgPlaceholder = func(i int) string { return fmt.Sprintf("$%d", i) }
	qPlaceholder  = func(int) string { return "?" }

	// Postgres: the exact DDL goose uses, minus the now() default which is
	// not needed since tstamp is always written explicitly.
	Postgres = Dialect{Name: "postgres", placeholder: pgPlaceholder,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id serial NOT NULL PRIMARY KEY,
	version_id bigint NOT NULL,
	is_applied boolean NOT NULL,
	tstamp timestamp NULL DEFAULT now()
)`}
	MySQL = Dialect{Name: "mysql", placeholder: qPlaceholder,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id serial NOT NULL,
	version_id bigint NOT NULL,
	is_applied boolean NOT NULL,
	tstamp timestamp NULL DEFAULT now(),
	PRIMARY KEY (id)
)`}
	SQLite = Dialect{Name: "sqlite", placeholder: qPlaceholder,
		createTable: `CREATE TABLE ` + VersionTable + ` (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	version_id INTEGER NOT NULL,
	is_applied INTEGER NOT NULL,
	tstamp TIMESTAMP DEFAULT (datetime('now'))
)`}
	// bytdb speaks the Postgres dialect but runs DDL outside transactions.
	BytDB = Dialect{Name: "bytdb", placeholder: pgPlaceholder, noTx: true,
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

// New builds a Migrator over dbh for the migrations in dir.
func New(dbh *sql.DB, d Dialect, dir string) (*Migrator, error) {
	migs, err := Load(dir)
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

// ensureTable creates the version table on first use and seeds it with
// goose's version-0 row. Existence is probed with a SELECT rather than
// CREATE TABLE IF NOT EXISTS, which not every engine spells the same way.
func (m *Migrator) ensureTable(ctx context.Context) error {
	var n int
	err := m.db.QueryRowContext(ctx, "SELECT count(*) FROM "+VersionTable).Scan(&n)
	if err == nil {
		return nil
	}
	if _, err = m.db.ExecContext(ctx, m.d.createTable); err != nil {
		return serr.Wrap(err, "op", "create version table")
	}
	// version 0 marks "table initialized"; goose writes the same row
	return m.record(ctx, m.db, 0)
}

// execer is what recording needs from either a *sql.DB or a *sql.Tx, so the
// version row can go in the same transaction as the migration's statements.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
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
func (m *Migrator) applied(ctx context.Context) (map[int64]Status, error) {
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx,
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
// order — the `status` command.
func (m *Migrator) Statuses(ctx context.Context) ([]Status, error) {
	app, err := m.applied(ctx)
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

// Version returns the highest applied version, 0 when none is.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	app, err := m.applied(ctx)
	if err != nil {
		return 0, err
	}
	var max int64
	for v, st := range app {
		if st.Applied && v > max {
			max = v
		}
	}
	return max, nil
}

// Up applies every pending migration in version order and returns the ones
// it applied.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) {
	return m.UpTo(ctx, -1)
}

// UpTo applies pending migrations with version <= target; a negative target
// means all of them. It stops at the first failure, leaving that migration's
// transaction rolled back and the earlier ones committed.
func (m *Migrator) UpTo(ctx context.Context, target int64) ([]Migration, error) {
	app, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	current := int64(0)
	for v, st := range app {
		if st.Applied && v > current {
			current = v
		}
	}
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
		if err = m.runOne(ctx, mig, mig.Up, true); err != nil {
			return done, err
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
func (m *Migrator) UpByOne(ctx context.Context) ([]Migration, error) {
	app, err := m.applied(ctx)
	if err != nil {
		return nil, err
	}
	for _, mig := range m.migs {
		if !app[mig.Version].Applied {
			return m.UpTo(ctx, mig.Version)
		}
	}
	m.logf("no migrations to run")
	return nil, nil
}

// Down rolls back the most recently applied migration (by version), and
// returns it.
func (m *Migrator) Down(ctx context.Context) ([]Migration, error) {
	v, err := m.Version(ctx)
	if err != nil {
		return nil, err
	}
	if v == 0 {
		m.logf("no migrations to roll back")
		return nil, nil
	}
	return m.downFrom(ctx, v, v)
}

// DownTo rolls back every applied migration with version > target, newest
// first. DownTo(0) undoes everything.
func (m *Migrator) DownTo(ctx context.Context, target int64) ([]Migration, error) {
	v, err := m.Version(ctx)
	if err != nil {
		return nil, err
	}
	return m.downFrom(ctx, v, target+1)
}

// downFrom rolls back applied migrations with floor <= version <= top, newest
// first. Rollback needs the file, so a migration the table knows but the
// directory no longer holds is an error: we cannot undo what we cannot read.
func (m *Migrator) downFrom(ctx context.Context, top, floor int64) ([]Migration, error) {
	app, err := m.applied(ctx)
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
		if err = m.runOne(ctx, mig, mig.Down, false); err != nil {
			return done, err
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
// way to iterate on a migration being written.
func (m *Migrator) Redo(ctx context.Context) error {
	v, err := m.Version(ctx)
	if err != nil {
		return err
	}
	if v == 0 {
		m.logf("no migrations to redo")
		return nil
	}
	if _, err = m.Down(ctx); err != nil {
		return err
	}
	_, err = m.UpTo(ctx, v)
	return err
}

// runOne executes a migration's statements and updates the version table in
// the same transaction, so a failure half-way leaves neither the schema
// change nor the version row behind. Outside a transaction (NO TRANSACTION,
// or an engine that cannot) the statements run one by one and the version
// row is written only if all of them succeeded — a failure then leaves a
// partially applied migration that must be repaired by hand, which is why
// the transaction is the default.
func (m *Migrator) runOne(ctx context.Context, mig Migration, stmts []string, up bool) error {
	dir := "down"
	if up {
		dir = "up"
	}
	fail := func(err error, i int) error {
		return serr.Wrap(err, "migration", mig.Name, "direction", dir,
			"statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
	}
	bookkeep := func(ex execer) error {
		if up {
			return m.record(ctx, ex, mig.Version)
		}
		return m.unrecord(ctx, ex, mig.Version)
	}

	if mig.NoTx || m.d.noTx {
		for i, s := range stmts {
			if _, err := m.db.ExecContext(ctx, s); err != nil {
				return fail(err, i)
			}
		}
		return bookkeep(m.db)
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return serr.Wrap(err, "migration", mig.Name, "op", "begin")
	}
	for i, s := range stmts {
		if _, err = tx.ExecContext(ctx, s); err != nil {
			_ = tx.Rollback()
			return fail(err, i)
		}
	}
	if err = bookkeep(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return serr.Wrap(err, "migration", mig.Name, "op", "commit")
	}
	return nil
}
