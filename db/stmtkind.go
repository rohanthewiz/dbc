package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/stmtcut"
)

// A statement has to be sent as a Query or as an Exec before anything is
// known about what it returns, and database/sql gives each only half an
// answer: a Query hands back rows but no rows-affected count, an Exec the
// count but none of the rows (the drivers read them off the wire and drop
// them). Going by the leading verb settles most statements — SELECT is a
// Query, UPDATE an Exec — but not the ones whose verb says nothing about
// their result:
//
//	CALL monthly_report()        rows, when the procedure SELECTs (MySQL) or
//	                             has OUT parameters (Postgres); else a count
//	EXECUTE stmt                 whatever the prepared statement returns
//	FETCH 10 FROM cur            rows
//	CHECK / ANALYZE / OPTIMIZE / REPAIR / CHECKSUM TABLE t     rows, on MySQL
//	HANDLER t READ …, HELP 'x', XA RECOVER                     rows, on MySQL
//
// Run as an Exec — as everything outside queryVerbs was — each of these
// reported "0 affected" and lost its rows.
//
// So a statement is one of three kinds, and the third is run as a Query
// whose result decides what it was:
//
//	             ┌─ isQuery ───────────────► kindRows   Query, rows
//	statement ───┼─ execVerbs ─────────────► kindExec   Exec, rows affected
//	             └─ anything else ─────────► kindMaybe  Query ─┬─ columns ──► rows
//	                (Postgres, MySQL)                          └─ none ─────► rows affected,
//	                                                                          asked of the driver
//	                                                                          another way (below)
//
// The third kind is everything that is not known to be an Exec, rather than
// a list of the verbs above: a statement missing from a list of row-returning
// verbs would go on losing its rows without a sign, where one missing from
// execVerbs costs a round trip and still reports the right count.
type stmtKind int

const (
	kindExec  stmtKind = iota // returns no rows: Exec, report rows affected
	kindRows                  // returns rows: Query
	kindMaybe                 // may do either: Query, and rows affected when no columns came back
)

// execVerbs are the leading verbs of statements that return no rows on any
// engine, so they run as an Exec and report rows affected the way they
// always did. The writes are here for their count, which only an Exec
// reports on every driver; a write with RETURNING never gets this far
// (isQuery finds it first). The rest are here so that the everyday
// statements of a script — DDL, transaction control, SET — do not pay
// kindMaybe's extra round trip to learn what their verb already says.
//
// A verb is left out when any engine answers it with rows: ANALYZE is
// rowless on Postgres but a result set on MySQL (ANALYZE TABLE), so it is
// a kindMaybe on both.
var execVerbs = map[string]bool{
	// writes
	"insert": true, "update": true, "delete": true, "replace": true, "merge": true,
	// DDL and maintenance
	"create": true, "alter": true, "drop": true, "truncate": true, "rename": true,
	"comment": true, "refresh": true, "reindex": true, "cluster": true, "vacuum": true,
	// transactions
	"begin": true, "start": true, "commit": true, "rollback": true, "savepoint": true,
	"release": true, "end": true, "abort": true,
	// session state
	"set": true, "reset": true, "use": true, "lock": true, "unlock": true,
	"discard": true, "listen": true, "unlisten": true, "notify": true,
	"declare": true, "close": true, "move": true, "prepare": true, "deallocate": true,
	// access and administration
	"grant": true, "revoke": true, "copy": true, "load": true, "do": true,
	"flush": true, "kill": true, "attach": true, "detach": true, "checkpoint": true,
	"install": true, "uninstall": true, "reassign": true, "security": true, "import": true,
}

// kindOf says how run sends stmt on an engine whose driver is drv (the
// canonical name: pgx, mysql, sqlite, bytdb) and which reads SQL as d.
//
// kindMaybe is Postgres's and MySQL's only. SQLite and bytdb have no
// statement outside queryVerbs that returns rows, and their rows-affected
// count comes from an Exec alone (see sqliteCounts), so there every
// statement that is not a Query stays the Exec it was.
//
// A text holding more than one statement stays an Exec too. Postgres runs
// one as a simple query, which takes several commands; kindMaybe's Query
// goes through the extended protocol, which refuses them ("cannot insert
// multiple commands into a prepared statement"). dbc splits what the user
// types, so only a script's s.Exec("CALL a(); CALL b()") gets here, and it
// keeps working as it did.
func kindOf(stmt string, d sqlsplit.Dialect, drv string) stmtKind {
	if isQuery(stmt, d) {
		return kindRows
	}
	if drv != "pgx" && drv != "mysql" {
		return kindExec
	}
	if execVerbs[d.Verbs(stmt).Main] {
		return kindExec
	}
	if len(d.Split(stmt)) > 1 {
		return kindExec
	}
	return kindMaybe
}

// maxMoreSets caps the result sets kept past a statement's first
// (model.Result.More). Each set is already cut at max_rows; this bounds how
// many of them there can be, for the procedure that SELECTs inside a loop
// and so answers with a set per iteration. It is far more than a procedure
// written to report several things returns, and far fewer than a runaway
// loop does.
const maxMoreSets = 50

// query runs stmt as a Query on ex and reads every result set it returns
// (readSets). The rows are closed before it returns, not only by a defer:
// on Postgres the command tag reaches pgTagTracer when the rows close, and
// runMaybe reads it as soon as query is back.
//
// cut says the statement may be stopped on the server once its result is
// cut at max_rows, rather than read to the end by rows.Close (N-276; see
// package stmtcut). run passes it for a plain read only (isRead). A write
// returns its rows as or after it does its work, and stopping it there
// could undo the work: a DELETE … RETURNING cut at 1,000 rows must still
// delete every row it matched. (Postgres runs such a write to its end
// before it sends a row, and does not act on a cancel while it sends them;
// the rule does not lean on that.) A read that leaves state row by row —
// SELECT set_config(…) on each row, MySQL's @n := @n + 1 — is no plain
// read either, and is read to its end so the state is the whole
// statement's.
func (m *Manager) query(ctx context.Context, ex execQuerier, name, stmt string, cut bool, args ...any) (*model.Result, error) {
	// The statement runs under a context of its own when it may be cut, so
	// the stop (on Postgres, a cancel of that context) reaches it and
	// nothing else. Errors are still judged against ctx, the caller's: the
	// cut's own cancel is never reported as the user's Stop.
	qctx := ctx
	var pt *stmtcut.Point
	if cut {
		qctx, pt = stmtcut.Watch(ctx)
		defer pt.Release()
	}
	rows, err := ex.QueryContext(qctx, stmt, args...)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name)
	}
	defer rows.Close()
	res, err := m.readSets(ctx, rows, name, stmt, pt)
	if err != nil {
		return nil, err
	}
	// Close's error is the iteration's, which readSets has already
	// reported or found nil.
	_ = rows.Close()
	return res, nil
}

// readSets reads rows into a Result: its first result set, and under
// Result.More each further one the driver has (database/sql's
// NextResultSet). Only go-sql-driver/mysql has any, for a CALL whose
// procedure runs more than one SELECT; on the other drivers NextResultSet
// is false at once and the Result is the one set, as before.
//
// The driver skips a set with no columns — the OK packet that ends a CALL
// is one — so every set here has columns, except the first of a statement
// that returned none at all, which is how runMaybe knows it was not a
// query.
//
// Sets past maxMoreSets are not read into memory: Result.MoreCut is set
// and the loop stops, leaving rows.Close to discard them.
//
// pt, when not nil, is the statement's stmtcut.Point (query's cut). A first
// set cut at max_rows is then closed through it: the statement is stopped
// on the server if the rest is still coming after stmtcut.Grace, instead of
// rows.Close reading every remaining row — minutes, for a SELECT * over a
// large table. A plain read has the one set, so nothing past it is lost.
//
//	           ┌─ not cut ─────────────────► further sets, rows.Err (as before)
//	first set ─┤
//	           └─ cut, pt ─► pt.Close ─┬─ caller's ctx ended ─► its cancel error
//	                                   └─ else ──────────────► the set, Truncated
//
// keepTx is passed to pt.Close: on Postgres a stop is a cancel, which aborts
// the transaction the statement ran in, and a user's BEGIN must not end
// because a grid holds only max_rows. Inside one, the rest is read as
// before; MySQL's KILL QUERY ends only the statement and applies anywhere.
func (m *Manager) readSets(ctx context.Context, rows *sql.Rows, name, stmt string, pt *stmtcut.Point) (*model.Result, error) {
	first, err := m.readSet(ctx, rows, name, stmt)
	if err != nil {
		return nil, err
	}
	if first.Truncated && pt != nil {
		pt.Close(rows, true)
		// The rows' error is the stop's own, so it is not looked at; but a
		// Stop the user pressed meanwhile is theirs to see, as it was when
		// the drain ended in it — and for a session, the cue for the KILL
		// its guard sends when the stop cost the connection.
		if err := ctx.Err(); err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "fetch")
		}
		return first, nil
	}
	// NextResultSet also drops what a set cut at max_rows left unread.
	for rows.NextResultSet() {
		if len(first.More) >= maxMoreSets {
			first.MoreCut = true
			break
		}
		set, err := m.readSet(ctx, rows, name, stmt)
		if err != nil {
			return nil, err
		}
		first.More = append(first.More, set)
	}
	// NextResultSet reports its failure here: a procedure that raised
	// after its first SELECT fails the statement, rather than passing for
	// one that returned a single set.
	if err = rows.Err(); err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "next result set")
	}
	return first, nil
}

// readSet reads the result set rows is positioned on, up to max_rows of it.
func (m *Manager) readSet(ctx context.Context, rows *sql.Rows, name, stmt string) (*model.Result, error) {
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
	return result, nil
}

// stamp gives every set of res the statement's wall time: the sets came
// from one statement, and a result shown alone should still say how long
// its statement took.
func stamp(res *model.Result, d time.Duration) *model.Result {
	for _, set := range res.Sets() {
		set.Duration = d
	}
	return res
}

// affectedResult is the Result of a statement that returned no rows: its
// rows-affected count, as a one-cell table for whatever renders it as one.
func affectedResult(name, stmt string, aff int64, start time.Time) *model.Result {
	return &model.Result{
		Conn: name, Query: stmt, IsExec: true, Affected: aff,
		Columns:  []string{"rows_affected"},
		Rows:     [][]string{{strconv.FormatInt(aff, 10)}},
		Raw:      [][]any{{aff}},
		Duration: time.Since(start),
	}
}

// runMaybe runs a kindMaybe statement: as a Query, so that its rows are
// kept when it has any. When it has none — no result set came back at all —
// it was a write after all, and its rows-affected count is what to report.
// A Query does not carry one through database/sql, so each engine is asked
// in the way it offers:
//
//	Postgres   the command tag ("INSERT 0 2", "FETCH 10"), which
//	           pgTagTracer hands over when the rows close
//	MySQL      SELECT ROW_COUNT(), on the same connection (mysqlMaybe)
//
// With that, an EXECUTE of a prepared INSERT or a CALL of a procedure that
// only writes reports the count it reported as an Exec.
//
// On Postgres the Query goes out in pgx's QueryExecModeDescribeExec, not
// the pool's default, which prepares each distinct text once and reuses
// the columns it described then. That is sound for a SELECT, whose columns
// the server checks against the prepared ones ("cached plan must not
// change result type", which pgx answers by preparing again), but not for
// these statements: the columns of `FETCH 2 FROM cur` or `EXECUTE q` are
// those of whatever cur or q names NOW, the server checks nothing, and
// after the cursor is declared again over another query the rows arrive
// under the first one's columns ("convert field 0 failed: invalid length
// for int4"). DescribeExec describes the statement on every run. It costs
// a second round trip, on statements that are typed, not looped.
func (m *Manager) runMaybe(ctx context.Context, ex execQuerier, name, drv, stmt string, start time.Time, args ...any) (*model.Result, error) {
	if drv == "mysql" {
		return m.mysqlMaybe(ctx, ex, name, stmt, start, args...)
	}
	// pgx reads a QueryExecMode ahead of the arguments as an option; its
	// database/sql adapter passes arguments through as they are.
	var tag atomic.Int64
	res, err := m.query(context.WithValue(ctx, pgTagKey{}, &tag), ex, name, stmt, false,
		append([]any{pgx.QueryExecModeDescribeExec}, args...)...)
	if err != nil {
		return nil, err
	}
	if len(res.Columns) == 0 {
		return affectedResult(name, stmt, tag.Load(), start), nil
	}
	return stamp(res, time.Since(start)), nil
}

// pgTagKey is the context key under which runMaybe leaves an *atomic.Int64
// for pgTagTracer to fill.
type pgTagKey struct{}

// pgTagTracer hands a Postgres statement's rows-affected count to a caller
// that ran it as a Query. pgx knows the count — it is in the command tag
// that ends every statement — but database/sql's Rows has no place for it;
// only an Exec's Result does. pgx's query tracer is the one hook that sees
// the tag of a Query, and it is called with the statement's own context,
// so the caller that wants the count puts a holder in that context
// (pgTagKey) and every other statement costs one failed context lookup.
//
// The holder is atomic because the rows can be closed — and so the tag
// stored — by database/sql's own goroutine when the context is canceled,
// while the caller is still reading.
//
// It has a second job, at the start of a statement: one whose context
// carries a stmtcut.Point (a plain read that may be cut at max_rows) learns
// there that canceling it stops it and keeps the connection — every dbc
// pool has pgCancelHandler — and whether it went out inside a transaction,
// which that cancel would abort. The status is pgconn's, from the last
// ReadyForQuery, so it is read without a round trip.
type pgTagTracer struct{}

// TraceQueryStart leaves the context as it is: the holder is already in it.
// It tells a stmtcut.Point, when there is one, how the statement can be
// stopped (see above).
func (pgTagTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if p := stmtcut.From(ctx); p != nil {
		p.SetCancels(conn.PgConn().TxStatus() != 'I')
	}
	return ctx
}

// TraceQueryEnd runs when a Query's rows close, or an Exec returns.
func (pgTagTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	if h, ok := ctx.Value(pgTagKey{}).(*atomic.Int64); ok {
		h.Store(d.CommandTag.RowsAffected())
	}
}

// mysqlMaybe is runMaybe on MySQL. The count of a statement that returned
// no rows is read with SELECT ROW_COUNT(), which is the server's name for
// the affected-rows value an Exec would have been handed: for a CALL, the
// count of the last statement the procedure ran.
//
// ROW_COUNT() answers for the connection's last statement, so the two must
// run on one connection. A Session's already do. A pooled run (ex is the
// *sql.DB) checks a connection out for the pair and hands it back after:
//
//	dbh.Conn ─► Query(stmt) ─► no columns? ─► SELECT ROW_COUNT() ─► Conn.Close (back to the pool)
//
// What the pool's own Query adds over a checked-out connection is one retry
// when the driver says driver.ErrBadConn — the statement never left — so
// that is done here, once, on a fresh checkout (as RunNotices does for
// Postgres).
func (m *Manager) mysqlMaybe(ctx context.Context, ex execQuerier, name, stmt string, start time.Time, args ...any) (*model.Result, error) {
	dbh, pooled := ex.(*sql.DB)
	if !pooled {
		return m.mysqlMaybeOn(ctx, ex, name, stmt, start, args...)
	}
	for retried := false; ; retried = true {
		c, err := dbh.Conn(ctx)
		if err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "conn")
		}
		res, err := m.mysqlMaybeOn(ctx, c, name, stmt, start, args...)
		_ = c.Close()
		if err != nil && !retried && errors.Is(err, driver.ErrBadConn) {
			continue
		}
		return res, err
	}
}

// mysqlMaybeOn is one attempt of mysqlMaybe, on a connection that will also
// run the ROW_COUNT().
func (m *Manager) mysqlMaybeOn(ctx context.Context, ex execQuerier, name, stmt string, start time.Time, args ...any) (*model.Result, error) {
	res, err := m.query(ctx, ex, name, stmt, false, args...)
	if err != nil {
		return nil, err
	}
	if len(res.Columns) > 0 {
		return stamp(res, time.Since(start)), nil
	}
	// The statement ran; a count that cannot be read (the context ended
	// just now, the statement was a SHUTDOWN) is reported as 0, not as the
	// statement's failure. ROW_COUNT() is -1 after a statement that has no
	// count to give, which is 0 here as it was from an Exec.
	var aff sql.NullInt64
	if rows, qerr := ex.QueryContext(ctx, "SELECT ROW_COUNT()"); qerr == nil {
		if rows.Next() {
			_ = rows.Scan(&aff)
		}
		_ = rows.Close()
	}
	return affectedResult(name, stmt, max(aff.Int64, 0), start), nil
}
