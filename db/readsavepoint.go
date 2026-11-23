package db

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"time"

	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/stmtcut"
)

// A plain read cut at max_rows inside the user's own Postgres transaction
// (N-324).
//
// N-276 stops a cut read on the server instead of reading the rest of its
// result. On Postgres the stop is a cancel, and a canceled statement aborts
// the transaction it ran in, so inside a BEGIN the user opened the read was
// left to stream to its end: `BEGIN; UPDATE …; SELECT * FROM events` over
// 100M rows kept 1,000 and ran for minutes. Now such a read runs inside a
// savepoint of dbc's, and a cut that ends in an aborted transaction is
// rolled back to it:
//
//	SAVEPOINT dbc_read ─► the read ─┬─ transaction fine ('T') ──────────► RELEASE
//	                                ├─ aborted ('E'), dbc left the rows ─► ROLLBACK TO, RELEASE
//	                                │    (a cut at max_rows, or a sink that
//	                                │    stopped taking rows)
//	                                └─ aborted, anything else ───────────► ROLLBACK TO, RELEASE,
//	                                                                       then abort it again at the
//	                                                                       transaction's level
//
// The last branch is the reason the savepoint is not simply rolled back to
// after every failure. A read's own error (a typo, a division by zero) or
// the user's Stop aborts the transaction exactly as it did without the
// savepoint, and as psql's would: its work is undone and its locks freed at
// once, and the next statement gets "current transaction is aborted" until
// ROLLBACK (readSavepoint.end says why that takes a second failure). Only
// an abort dbc caused, by stopping a read it no longer wanted, is undone. Rows the user never saw cannot abort their transaction either:
// an error in the part of a cut read that was not read is rolled back too,
// as it is outside a transaction, where the cut's stop ends the statement
// before the error is reached.
//
// What it costs, and where it does not apply:
//
//   - A read that fails inside a transaction is followed by three more
//     statements (ROLLBACK TO, RELEASE, a failing DO), and the server's log
//     shows the second failure after the read's own.
//   - Two round trips (SAVEPOINT, RELEASE) on every plain read run inside a
//     transaction on a pinned session, cut or not, since whether it will be
//     cut is not known until max_rows rows have arrived. A read that cannot
//     return more than max_rows rows — a LIMIT n at the statement's own
//     level with n ≤ max_rows — skips them (readBound). A read outside a
//     transaction never pays: the pool's and an idle session's reads are
//     stopped with a plain cancel, as N-276 made them.
//   - ROLLBACK TO undoes what the read did inside the savepoint, which for
//     a plain read (isRead) is its locks: the share lock on each table it
//     read is let go at the cut. Today the read held it to COMMIT, which
//     kept a concurrent ALTER or DROP of the table waiting.
//   - A read that takes row locks or transaction-level advisory locks is
//     not wrapped, and is read to its end as before (locksRows): rolling
//     back to the savepoint would let go of every lock it took, where the
//     user ran it to hold them until COMMIT.
//
// Why not a cursor (DECLARE … CURSOR, FETCH max_rows+1, CLOSE), which
// stops without a cancel: Postgres never runs a cursor's query in parallel,
// and plans it for its first rows (cursor_tuple_fraction), so a large
// aggregate inside a transaction could take several times as long as it
// does outside one. The savepoint leaves the plan alone.

// readSavepointName is the savepoint a read runs inside. A user savepoint
// of the same name is shadowed while the read runs and is the one in effect
// again after RELEASE: Postgres releases and rolls back to the most recent
// savepoint of a name.
const readSavepointName = "dbc_read"

// savepointUndoTimeout bounds the RELEASE and ROLLBACK TO after a read.
// They run under a context of their own, as explain's rollback does: after
// the user's Stop the read's context is dead, and a RELEASE canceled part
// way would itself abort the transaction.
const savepointUndoTimeout = 10 * time.Second

// readSavepoint is one read's savepoint: taken by saveRead before the read,
// ended by end after it. A nil *readSavepoint is a read without one, and
// its end returns the read's error as it is.
type readSavepoint struct {
	conn *sql.Conn
	name string // the connection's name, for errors
}

// saveRead takes a savepoint before a plain read (query's cut) when the
// read runs inside the user's Postgres transaction and could be cut at
// max_rows, so that a cut can stop it without aborting that transaction
// (see the comment at the top of this file). It returns nil, and sends
// nothing, when the read needs none:
//
//	ex not a pinned connection (*sql.Conn) ─► nil: the pool's connections
//	                                           hold no transaction of the user's
//	not Postgres, or no transaction open ───► nil: a cancel aborts nothing
//	  (or a failed one: the read fails anyway)  (MySQL's KILL QUERY never does)
//	the read takes row or xact locks ───────► nil: read to the end, as before
//	a LIMIT ≤ max_rows at its own level ────► nil: it cannot be cut
//	else ───────────────────────────────────► SAVEPOINT dbc_read
//
// The transaction status is pgconn's, from the last ReadyForQuery, so the
// checks cost no round trip; only the SAVEPOINT does.
func (m *Manager) saveRead(ctx context.Context, ex execQuerier, name, stmt string) (*readSavepoint, error) {
	c, ok := ex.(*sql.Conn)
	if !ok || pgTxStatusOf(c) != 'T' {
		return nil, nil
	}
	words := sqlsplit.Postgres.Words(stmt)
	if locksRows(stmt, words) {
		return nil, nil
	}
	if n, ok := readBound(words); ok && n <= int64(m.cfg.MaxRows) {
		return nil, nil
	}
	if _, err := c.ExecContext(ctx, "SAVEPOINT "+readSavepointName); err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "read savepoint")
	}
	return &readSavepoint{conn: c, name: name}, nil
}

// end finishes the savepoint once the read's rows are closed, and returns
// the read's error — or, when the read succeeded but the savepoint could not
// be ended, that failure. pt is the read's stmtcut.Point: whether dbc left
// its rows part way (Left) decides whether an aborted transaction is dbc's
// doing. readErr is the read's error, nil when it succeeded; ctx is the
// caller's, whose end means the user stopped the read.
//
// The transaction status after the read says what happened, so no error
// text is parsed:
//
//	'T'                                   the read left the transaction usable,
//	                                      whatever it returned ─► RELEASE
//	'E', dbc left the rows, ctx alive     the cut's stop (or an error past the
//	                                      rows kept) ─► ROLLBACK TO, RELEASE
//	'E' otherwise                         the read's own error, or the user's
//	                                      Stop ─► ROLLBACK TO, RELEASE, reabort
//	anything else                         the connection is gone, and the
//	                                      transaction with it: nothing to end
//
// Why the read's own failure is not simply left where it happened: inside
// the savepoint it aborts only the subtransaction. The block then refuses
// statements until ROLLBACK, as before, but keeps the transaction's earlier
// work and its locks, which a failure at the transaction's own level lets go
// of at once — so an UPDATE's row lock would hold other sessions back until
// the user typed ROLLBACK. Rolling back to the savepoint, releasing it and
// failing a statement at the transaction's level (reabortStmt) leaves the
// state the read's error would have left without the savepoint: aborted,
// work undone, locks freed, open until ROLLBACK. The caller still sees the
// read's own error; the second one shows only in the server's log.
func (sp *readSavepoint) end(ctx context.Context, pt *stmtcut.Point, readErr error) error {
	if sp == nil {
		return readErr
	}
	var undo []string
	reabort := false
	switch pgTxStatusOf(sp.conn) {
	case 'T':
		undo = []string{"RELEASE SAVEPOINT " + readSavepointName}
	case 'E':
		undo = []string{"ROLLBACK TO SAVEPOINT " + readSavepointName, "RELEASE SAVEPOINT " + readSavepointName}
		reabort = !pt.Left() || ctx.Err() != nil
	default:
		return readErr
	}
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), savepointUndoTimeout)
	defer cancel()
	for _, q := range undo {
		if _, err := sp.conn.ExecContext(uctx, q); err != nil {
			if readErr != nil {
				// The read failed and so does its cleanup: the read's error
				// is the one to see, and the transaction is aborted either
				// way (perhaps still holding its locks until ROLLBACK).
				return readErr
			}
			// The rows are lost with the error, on purpose: a transaction
			// left aborted, or a connection gone with it, must not look like
			// a read that went well. Session.Classify finds which.
			return serr.Wrap(err, "conn", sp.name, "op", "read savepoint", "query", q,
				"detail", "the read's savepoint could not be ended; if the session is still open, its transaction may be aborted — ROLLBACK to go on")
		}
	}
	if reabort {
		// Fails by design: that failure is the abort. Its error is not the
		// caller's to see, and any failure would serve.
		_, _ = sp.conn.ExecContext(uctx, reabortStmt)
	}
	return readErr
}

// reabortStmt fails, so that the transaction it runs in is aborted at its
// own level (see end). A RAISE, so the server's log names dbc and why; were
// plpgsql missing or not granted, DO would fail anyway, which is all that is
// needed.
const reabortStmt = `DO $$BEGIN RAISE EXCEPTION 'dbc: the read failed inside the transaction; aborting it as the failure would have'; END$$`

// pgTxStatusOf returns pgconn's transaction status for a pinned Postgres
// connection — 'I' idle, 'T' in a transaction, 'E' in a failed one — from
// the last ReadyForQuery, without a round trip; 0 when the connection is not
// pgx's or is closed. No rows may be open on c.
func pgTxStatusOf(c *sql.Conn) byte {
	var st byte
	_ = c.Raw(func(dc any) error {
		if pc, ok := dc.(*pgxstdlib.Conn); ok {
			st = pc.Conn().PgConn().TxStatus()
		}
		return nil
	})
	return st
}

// xactLockFuncs take advisory locks held until the transaction ends. Inside
// a savepoint that is rolled back to they are let go at once, so a read
// that calls one is not wrapped (locksRows). The session-level ones are not
// listed: effectOf already makes their read no plain read, so it is never
// cut.
var xactLockFuncs = map[string]bool{
	"pg_advisory_xact_lock":            true,
	"pg_advisory_xact_lock_shared":     true,
	"pg_try_advisory_xact_lock":        true,
	"pg_try_advisory_xact_lock_shared": true,
}

// rowLockWords are the words after FOR that make a row-locking clause:
// FOR UPDATE, FOR NO KEY UPDATE, FOR SHARE, FOR KEY SHARE.
var rowLockWords = map[string]bool{"update": true, "no": true, "share": true, "key": true}

// locksRows reports whether a Postgres read takes locks the user may mean to
// hold until COMMIT, which a ROLLBACK TO SAVEPOINT would let go of: a
// row-locking clause at any depth (a subquery's FOR UPDATE locks too), or a
// call to a transaction-level advisory lock function. words is
// sqlsplit.Postgres.Words(stmt).
//
// It errs toward yes, which costs only the old behavior (the read is read
// to its end): FOR followed by one of the words anywhere counts, though
// nothing else in Postgres's grammar puts one there.
func locksRows(stmt string, words []sqlsplit.Word) bool {
	for i := 0; i+1 < len(words); i++ {
		if words[i].Text == "for" && rowLockWords[words[i+1].Text] {
			return true
		}
	}
	return slices.ContainsFunc(sqlsplit.Postgres.Calls(stmt), func(f string) bool { return xactLockFuncs[f] })
}

// readBound returns the most rows a Postgres read can return, when its text
// says so: LIMIT n, or FETCH FIRST|NEXT n ROW|ROWS ONLY, at the statement's
// own level (depth 0) — a subquery's LIMIT bounds only the subquery. FETCH
// FIRST ROW ONLY is 1. words is sqlsplit.Postgres.Words(stmt).
//
// A wrong answer here costs little either way, so the reading is plain:
// a bound missed means a savepoint taken for nothing (two round trips); a
// bound read where there is none (LIMIT 10 + 5000 reads as 10) means a cut
// read is read to its end, as before N-324. LIMIT ALL, LIMIT $1 and LIMIT
// (10) give no bound.
func readBound(words []sqlsplit.Word) (n int64, ok bool) {
	num := func(i int) (int64, bool) {
		if i >= len(words) || words[i].Depth != 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(words[i].Text, 10, 64)
		return v, err == nil && v >= 0
	}
	for i, w := range words {
		if w.Depth != 0 {
			continue
		}
		switch w.Text {
		case "limit":
			if v, ok := num(i + 1); ok {
				return v, true
			}
		case "fetch":
			if i+1 >= len(words) || (words[i+1].Text != "first" && words[i+1].Text != "next") {
				continue
			}
			if v, ok := num(i + 2); ok {
				return v, true
			}
			if i+2 < len(words) && (words[i+2].Text == "row" || words[i+2].Text == "rows") {
				return 1, true
			}
		}
	}
	return 0, false
}
