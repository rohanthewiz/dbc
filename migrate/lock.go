package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/rohanthewiz/serr"
)

// Two runs at once — two deploys, a deploy and a hand at a terminal — must
// not both apply a migration. Each reads the version table, each finds the
// same versions pending, and each runs them: a CREATE fails on the second
// (harmlessly, inside its transaction), but a data migration runs twice, and
// the version table, which has no unique key (goose's log-shaped history
// keeps several rows per version), takes a second row without complaint.
//
// So every command that writes holds a lock for the database while it reads
// the table and runs. How depends on what the engine offers:
//
//	engine    lock                                     held
//	───────── ──────────────────────────────────────── ─────────────────────
//	postgres  pg_try_advisory_lock(goose's key)        the whole run
//	mysql     GET_LOCK('goose_db_version:<db>', 0)     the whole run
//	sqlite    BEGIN IMMEDIATE, then re-read the row    each migration's tx
//	bytdb     none needed: the engine holds an OS lock on its file while it
//	          is open, so a second process cannot open the database at all
//
// Postgres and MySQL locks belong to a session, so a run holds one
// connection (*sql.Conn) from before the lock to after the release, and
// every statement of the run goes through it. Sending them through the pool
// instead would also work for the lock, but a pool capped at one connection
// would then deadlock between the lock's connection and the work's.
//
// SQLite has no lock that outlives a transaction, so its guard is per
// migration: see runImmediate. A NO TRANSACTION migration on SQLite re-reads
// its row just before it runs, which narrows the window but cannot close
// it — nothing on SQLite can be held across statements outside a
// transaction.

// sessionLock is a named lock the server ties to the session that took it,
// and frees when that session ends — so a run that dies holding it leaves no
// stale lock behind.
type sessionLock struct {
	// try takes the lock if it is free, without waiting, and reports
	// whether it did.
	try func(ctx context.Context, c *sql.Conn) (bool, error)
	// holder names the session holding the lock, for the "waiting" line —
	// a pid to pg_terminate_backend, a connection id to KILL. "" when it
	// cannot be told; it is best effort and never fails the run.
	holder func(ctx context.Context, c *sql.Conn) string
	// release gives the lock up.
	release func(ctx context.Context, c *sql.Conn) error
}

// pgLockKey is goose's lock id (its DefaultLockID: the CRC-64/ECMA of
// "goose"), so a `dbc migrate` and a goose run with its session locker
// exclude each other on a database the two share while a project switches
// tools. Advisory locks are scoped to the database, as the version table is.
const pgLockKey int64 = 5887940537704921958

// pgLock waits by polling pg_try_advisory_lock, not by blocking in
// pg_advisory_lock. A blocked SELECT pg_advisory_lock(…) is a statement in
// progress, holding a snapshot for as long as it waits; CREATE INDEX
// CONCURRENTLY in the holder's run waits for every older snapshot to go
// away, so the holder would wait for the waiter that waits for the holder
// (Postgres would break that cycle by failing one of them). A try returns at
// once and holds nothing between attempts. goose's session locker polls for
// the same reason.
//
// A transaction-mode pooler (PgBouncer pool_mode=transaction) hands each
// statement to whichever server session is free, so a session lock taken
// through one does not hold; migrate through a session-mode pool or
// directly.
var pgLock = &sessionLock{
	try: func(ctx context.Context, c *sql.Conn) (bool, error) {
		var ok bool
		err := c.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", pgLockKey).Scan(&ok)
		return ok, err
	},
	holder: func(ctx context.Context, c *sql.Conn) string {
		// pg_locks stores a bigint key split in two: classid holds the high
		// 32 bits, objid the low 32, and objsubid 1 marks the one-key form
		// (2 is the two-int4 form, a different lock).
		var pid int64
		err := c.QueryRowContext(ctx, `SELECT pid FROM pg_locks
WHERE locktype = 'advisory' AND granted AND objsubid = 1
  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND classid::bigint = $1 AND objid::bigint = $2
LIMIT 1`,
			int64(uint64(pgLockKey)>>32), int64(uint64(pgLockKey)&0xffffffff)).Scan(&pid)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("pid %d", pid)
	},
	release: func(ctx context.Context, c *sql.Conn) error {
		_, err := c.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgLockKey)
		return err
	},
}

// myLockName is the MySQL lock's name, as SQL. MySQL's lock names are
// server-wide, not per database, so the name carries the database the
// version table lives in; GET_LOCK refuses a name over 64 characters, so it
// is cut there (two long database names that agree for that long would then
// share a lock, which only makes their runs take turns).
const myLockName = "LEFT(CONCAT('" + VersionTable + ":', IFNULL(DATABASE(), '')), 64)"

// myLock polls GET_LOCK with a zero timeout rather than waiting inside it.
// MySQL has no snapshot problem like Postgres's, but a wait inside GET_LOCK
// is a statement the driver can only abandon by closing the connection, and
// the server may not notice that until the wait ends; a poll keeps every
// statement short and the wait in Go, where ctx ends it cleanly.
var myLock = &sessionLock{
	try: func(ctx context.Context, c *sql.Conn) (bool, error) {
		// 1: taken; 0: someone else holds it; NULL: an error (the
		// session was killed, say), reported as one
		var got sql.NullInt64
		if err := c.QueryRowContext(ctx, "SELECT GET_LOCK("+myLockName+", 0)").Scan(&got); err != nil {
			return false, err
		}
		if !got.Valid {
			return false, serr.New("GET_LOCK returned NULL")
		}
		return got.Int64 == 1, nil
	},
	holder: func(ctx context.Context, c *sql.Conn) string {
		var id sql.NullInt64
		if err := c.QueryRowContext(ctx, "SELECT IS_USED_LOCK("+myLockName+")").Scan(&id); err != nil || !id.Valid {
			return ""
		}
		return fmt.Sprintf("connection %d", id.Int64)
	},
	release: func(ctx context.Context, c *sql.Conn) error {
		_, err := c.ExecContext(ctx, "SELECT RELEASE_LOCK("+myLockName+")")
		return err
	},
}

// lockPoll is how long a run waiting for the lock sleeps between tries. A
// second is nothing next to a migration, and keeps the waiter's load on the
// server negligible. A variable so tests can shorten it.
var lockPoll = time.Second

// releaseTimeout bounds the cleanup statements (unlock, rollback) that run
// on a fresh context after ctx may already have ended.
const releaseTimeout = 5 * time.Second

// locked runs fn on one connection held for the whole run, under the
// dialect's session lock when it has one. The lock is taken before fn reads
// the version table and released after fn's last statement, so a second run
// reads the table only once the first is done with it.
func (m *Migrator) locked(ctx context.Context, fn func(c *sql.Conn) error) error {
	c, err := m.db.Conn(ctx)
	if err != nil {
		return serr.Wrap(err, "op", "reserve a connection for the run")
	}
	defer c.Close()

	lk := m.d.lock
	if lk == nil {
		return fn(c)
	}
	if err = m.acquire(ctx, c, lk); err != nil {
		return err
	}
	defer func() {
		// A fresh context: ctx may be what ended the run, and a lock left
		// held on a pooled connection would shut every later run out until
		// that connection happened to close. If the release fails anyway,
		// the connection is closed rather than pooled — the server frees a
		// session's locks when the session ends.
		rctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if rerr := lk.release(rctx, c); rerr != nil {
			discard(c)
		}
	}()
	return fn(c)
}

// acquire tries lk until it is taken or ctx ends. The first miss logs who
// holds it, so a deploy that seems to hang says what it is waiting for.
func (m *Migrator) acquire(ctx context.Context, c *sql.Conn, lk *sessionLock) error {
	waited := false
	for {
		ok, err := lk.try(ctx, c)
		if err != nil {
			return serr.Wrap(err, "op", "take the migration lock")
		}
		if ok {
			if waited {
				m.logf("migration lock taken")
			}
			return nil
		}
		if !waited {
			waited = true
			who := ""
			if h := lk.holder(ctx, c); h != "" {
				who = " (" + h + ")"
			}
			m.logf("waiting for the migration lock: another run holds it%s", who)
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return serr.Wrap(ctx.Err(), "op", "wait for the migration lock")
		case <-t.C:
		}
	}
}

// discard closes c's underlying connection instead of returning it to the
// pool: database/sql drops a connection whose Raw callback reports
// ErrBadConn. It is for a connection that may still hold the lock or sit
// inside a transaction, which the next user of the pool must not inherit.
// c is unusable afterwards.
func discard(c *sql.Conn) {
	_ = c.Raw(func(any) error { return driver.ErrBadConn })
}
