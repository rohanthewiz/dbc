package etl

import (
	"context"
	"database/sql"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/stmtcut"
)

// Reader streams a query's rows one at a time, with typed values. It is the
// unbounded sibling of a script's s.Query: no max_rows cap and no
// stringified copy of every value, so it suits a table of any size.
//
//	rd, err := Read(ctx, src, "SELECT id, email FROM users")
//	defer rd.Close()
//	for rd.Next() {
//		row := rd.Row() // []any{int64(1), "a@b.c"}
//	}
//	err = rd.Err()
//
// Values are what the driver returns — int64, float64, bool, string,
// time.Time, []byte, nil — with one normalization: a []byte from a column
// that is not a binary type becomes a string. MySQL's text protocol returns
// nearly every value as bytes, and without this a VARCHAR would load into
// Postgres as bytea hex or fail outright.
//
// On Postgres the values pgx does not decode — intervals, arrays, ranges,
// composites — arrive as the server's text for them, and the read pins the
// settings that shape that text (see pgPinOutput): an interval or a date[]
// reads the same from every server, whatever its DateStyle.
type Reader struct {
	ctx  context.Context
	conn string
	tx   *sql.Tx // Postgres: the read's transaction, which scopes pgPinOutput
	rows *sql.Rows
	// cut is the query's stmtcut.Point: the query runs under a context of
	// its own, so a read that ends before its last row can stop it on the
	// server rather than have rows.Close read the rest (N-276; see Close).
	cut *stmtcut.Point
	// plain: the query only reads (plainRead), so stopping it early loses
	// nothing a commit would have kept.
	plain bool
	// done: the rows ran out (Next saw their end, error or not); there is
	// nothing left to stop.
	done    bool
	cols    []string
	dbTypes []string // driver type names, upper case ("INT4", "VARCHAR", "")
	binary  []bool   // column holds raw bytes: keep []byte as []byte
	holders []any    // *any per column, reused across Scan calls
	row     []any
	n       int64
	err     error
	closed  bool
	// holdTx keeps the transaction open past the last row, for Copy and
	// ReadOptions.Hold: the read commits (Close) only after its load has,
	// or rolls back (rollback, Abort). Next would otherwise commit as the
	// rows run out.
	holdTx bool
}

// pgPinOutput fixes, for the current transaction only (set_config's
// is_local), the session settings that decide how Postgres writes a value
// as text. A database or role can set any of them (ALTER DATABASE … SET
// DateStyle …), and text written under one server's settings is read back
// under another's — the destination of a copy — so without this a copy
// between two differently configured servers changes values:
//
//	setting             a server may say      so the text reads    pinned to
//	DateStyle           SQL, DMY / German     "15/03/2024"         ISO: 2024-03-15
//	                    → the destination, MDY, refuses it — or, when the
//	                      day is ≤ 12, silently swaps day and month
//	IntervalStyle       sql_standard          "-1-2 …"             postgres: explicit
//	                    → a leading sign applies to every field     signs per field
//	extra_float_digits  0 (pre-12 default)    0.1+0.2 → "0.3"      3: exact
//
// Only the output half of DateStyle is set: "ISO" alone keeps the
// session's field order, so a literal in the caller's own query
// ('02/01/2024' in a Where) still means what it meant. Input under the
// pinned IntervalStyle and float digits is unchanged for every value a
// query would sensibly contain. TimeZone is left alone: ISO output carries
// the offset, so the instant survives any destination zone.
const pgPinOutput = `SELECT set_config('datestyle', 'ISO', true),
	set_config('intervalstyle', 'postgres', true),
	set_config('extra_float_digits', '3', true)`

// Read runs query on c and returns a Reader positioned before the first row.
// The query runs under ctx; canceling it stops the fetch.
//
// On Postgres the query runs in a transaction of its own, which pins the
// text output settings (pgPinOutput) for this read and no other: the
// connection goes back to the pool with the session's own settings. The
// transaction commits when the Reader closes, so a query with side effects
// (DELETE … RETURNING, nextval) keeps them just as it would have
// unwrapped; a read that fails or is canceled rolls them back.
func Read(ctx context.Context, c Conn, query string, args ...any) (*Reader, error) {
	return read(ctx, c, query, nil, args...)
}

// ReadOptions shape a Reader beyond what Read does.
type ReadOptions struct {
	// Hold keeps the read's transaction (Postgres) open past its last
	// row: Next no longer commits as the rows run out, and the caller ends
	// the read — Close commits it, Abort rolls it back. A caller that
	// loads the rows somewhere holds the read until that load has
	// committed, so a query that writes (DELETE … RETURNING, moving rows)
	// keeps its change only once the rows have arrived — Copy's order,
	// and a pipeline's sql.read's. Unheld, a short result committed in the
	// first batch, before the load that was to take it had even opened,
	// and a load that failed then lost the rows. Off Postgres a read has
	// no transaction and Hold changes nothing.
	Hold bool
	// SameDatabase says the rows feed a load on the same Postgres
	// database that is open while the read is (NewSameDatabaseWriter).
	// The read's lock waits are then bounded, as the load's are, so the
	// two waiting on each other through this process ends in an error,
	// not a hang (sameDatabaseSetup). For a query: a plain table read
	// never waits on the load.
	SameDatabase bool
}

// ReadWith is Read with opt.
func ReadWith(ctx context.Context, c Conn, query string, opt ReadOptions, args ...any) (*Reader, error) {
	var setup []string
	if opt.SameDatabase && c.Engine == Postgres {
		setup = []string{pgSameDBLockTimeout()}
	}
	rd, err := read(ctx, c, query, setup, args...)
	if err != nil {
		return nil, err
	}
	rd.holdTx = opt.Hold
	return rd, nil
}

// read is Read with setup: statements run in the read's transaction after
// pgPinOutput and before the query (Postgres only, where there is one) —
// a copy's lock_timeout (sourceSetup).
func read(ctx context.Context, c Conn, query string, setup []string, args ...any) (*Reader, error) {
	var (
		tx   *sql.Tx
		rows *sql.Rows
		err  error
	)
	// Only the query runs under the Point's context; the transaction and
	// the pins keep ctx. A stop then cancels the query alone, and the
	// transaction is ended by Close or rollback with a ROLLBACK on a live
	// connection. Had the transaction been begun under the Point's context,
	// database/sql would roll it back on the cancel by itself and, since
	// pgx's stdlib conn is no driver.Validator, discard the connection.
	qctx, cut := stmtcut.Watch(ctx)
	if c.Engine == Postgres {
		// A cancel stops a pgx statement on any pool: with dbc's handler by
		// a cancel request that keeps the connection, with pgx's default by
		// closing it, which a pooled read can spare. The transaction it
		// aborts is the read's own. So the Point needs no word from db's
		// tracer here (which says the same when the pool is dbc's).
		cut.SetCancels(true)
		if tx, err = c.DB.BeginTx(ctx, nil); err != nil {
			cut.Release()
			return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "read", "query", clip(query))
		}
		for _, s := range append([]string{pgPinOutput}, setup...) {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				break
			}
		}
		if err == nil {
			rows, err = tx.QueryContext(qctx, query, args...)
		}
	} else {
		rows, err = c.DB.QueryContext(qctx, query, args...)
	}
	if err != nil {
		cut.Release()
		if tx != nil {
			_ = tx.Rollback()
		}
		return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "read", "query", clip(query))
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		cut.Release()
		if tx != nil {
			_ = tx.Rollback()
		}
		return nil, serr.Wrap(err, "conn", c.Name, "op", "read columns")
	}
	rd := &Reader{ctx: ctx, conn: c.Name, tx: tx, rows: rows, cols: cols,
		cut: cut, plain: plainRead(query, c.Engine.Dialect()),
		dbTypes: make([]string, len(cols)), binary: make([]bool, len(cols)),
		holders: make([]any, len(cols))}
	// Column types are advisory: a driver that cannot report them leaves the
	// names empty, and the only cost is that []byte is read as a string.
	if cts, err := rows.ColumnTypes(); err == nil && len(cts) == len(cols) {
		for i, ct := range cts {
			rd.dbTypes[i] = strings.ToUpper(ct.DatabaseTypeName())
			rd.binary[i] = isBinaryType(rd.dbTypes[i])
		}
	}
	for i := range rd.holders {
		rd.holders[i] = new(any)
	}
	return rd, nil
}

// Columns returns the result's column names.
func (r *Reader) Columns() []string { return r.cols }

// DBTypes returns the driver's type name for each column, upper case
// ("INT4", "TEXT", "DATETIME"); empty where the driver does not say.
func (r *Reader) DBTypes() []string { return r.dbTypes }

// Next advances to the next row, reporting false at the end or on an error
// (see Err). The Reader closes itself when it runs out.
func (r *Reader) Next() bool {
	if r.closed || r.err != nil {
		return false
	}
	if !r.rows.Next() {
		r.done = true
		if err := r.rows.Err(); err != nil {
			r.err = serr.Wrap(canceled(r.ctx, err), "conn", r.conn, "op", "read", "after_rows", itoa(r.n))
		}
		// Held for its owner, who decides how it ends. database/sql has
		// already closed the rows on reaching the end; only the
		// transaction (and so the connection) waits.
		if r.holdTx && r.err == nil {
			return false
		}
		// Close commits the read's transaction on Postgres; a failed commit
		// is the read's failure (a DELETE … RETURNING whose rows were all
		// read but whose delete did not stick).
		if err := r.Close(); err != nil && r.err == nil {
			r.err = serr.Wrap(canceled(r.ctx, err), "conn", r.conn, "op", "end read")
		}
		return false
	}
	if err := r.rows.Scan(r.holders...); err != nil {
		r.err = serr.Wrap(err, "conn", r.conn, "op", "scan", "row", itoa(r.n+1))
		_ = r.Close()
		return false
	}
	// A fresh slice per row, so a caller may keep, modify, or hand the row
	// to a Writer that buffers it, without the next Scan overwriting it.
	// database/sql already copies []byte into a *any destination, so the
	// values themselves are safe to keep too.
	row := make([]any, len(r.cols))
	for i, h := range r.holders {
		v := *(h.(*any))
		if b, ok := v.([]byte); ok && !r.binary[i] {
			v = string(b)
		}
		row[i] = v
	}
	r.row = row
	r.n++
	return true
}

// Row returns the current row. Each call to Next makes a new slice.
func (r *Reader) Row() []any { return r.row }

// Count is the number of rows read so far.
func (r *Reader) Count() int64 { return r.n }

// Err returns the error that stopped Next early, if any.
func (r *Reader) Err() error { return r.err }

// Close releases the query's connection — on Postgres after ending the
// read's transaction: COMMIT when the read went well (to the end, or
// stopped early by the caller), ROLLBACK after a failure. It is safe to
// call more than once, and a deferred Close after reading to the end costs
// nothing.
//
// Closed before its last row, a read that only reads stops its query on
// the server instead of reading the rest; a query that writes is read to
// its end and committed, so its work is kept.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	defer r.cut.Release()
	// A read closed before its last row stops its query on the server when
	// the rest is still arriving after stmtcut.Grace — a script that breaks
	// out of its loop, a pipeline whose rows.limit was met — instead of
	// reading every remaining row first, which for a large table took as
	// long as reading it all (N-276):
	//
	//	rows ran out ─────────────────► as before (nothing left to read)
	//	failed (Err) ─────────────────► stop, ROLLBACK
	//	stopped early, plain read ────► stop; COMMIT if the rest arrived
	//	                                first, else ROLLBACK (Postgres
	//	                                aborted the transaction with it)
	//	stopped early, may write ─────► read to the end, COMMIT: the
	//	                                write's work completes and is
	//	                                kept, as Close always promised
	//
	// "Plain" is lexical (plainRead), so a SELECT of a function that writes
	// reads as one: stopped early, its calls so far are rolled back where
	// they used to run for every row and commit.
	var err error
	stopped := false
	if !r.done && (r.err != nil || r.plain) {
		stopped = r.cut.Close(r.rows, false)
	} else {
		err = r.rows.Close()
	}
	if r.tx != nil {
		if err == nil && r.err == nil && !stopped {
			err = r.tx.Commit()
		} else {
			_ = r.tx.Rollback()
		}
	}
	return err
}

// rollback ends the read as a failed one: it closes the rows and rolls the
// transaction back, whether or not the rows ran out cleanly. It is Copy's
// way out for a held read (holdTx) whose load did not commit, and a no-op
// once the Reader is closed.
//
// The query is stopped if its rest is still arriving after stmtcut.Grace,
// whatever it is: nothing it did is to be kept. On Postgres the
// transaction rolls back anyway. On MySQL the KILL QUERY makes InnoDB undo
// the statement as a whole, where reading it to the end let a write
// complete and commit. SQLite and bytdb have nothing to stop: their rows
// close without reading the rest.
func (r *Reader) rollback() {
	if r.closed {
		return
	}
	r.closed = true
	defer r.cut.Release()
	r.cut.Close(r.rows, false)
	if r.tx != nil {
		_ = r.tx.Rollback()
	}
}

// readVerbs are the main verbs of a query that only reads, for plainRead.
var readVerbs = map[string]bool{"select": true, "values": true, "table": true, "show": true}

// plainRead reports whether query only reads, so that stopping it part way
// — and rolling back its transaction — loses nothing a commit would have
// kept. It is narrower than db's isRead on purpose, for a package that
// does not import db: a read verb, no CTE that writes (Postgres's `WITH d
// AS (DELETE … RETURNING *) SELECT …`), and no INTO after the main verb
// (SELECT … INTO new_table). Anything else is read to its end on Close, as
// before. Strings and comments are read as d reads them, so a 'delete' in
// a literal is not a write.
func plainRead(query string, d sqlsplit.Dialect) bool {
	v := d.Verbs(query)
	if !readVerbs[v.Main] {
		return false
	}
	for _, cte := range v.CTEs {
		if writeVerbs[cte] {
			return false
		}
	}
	return !d.HasKeyword(query[v.MainAt:], "into")
}

// Abort ends the read as a failed one, whatever state it is in: the rows
// are closed and, on Postgres, the read's transaction is rolled back — so
// a DELETE … RETURNING whose rows went nowhere (the load that was to take
// them failed) keeps its rows. Close would commit a read that ran clean to
// the end; a caller that knows the rows were lost calls this instead. A
// no-op once the Reader is closed.
func (r *Reader) Abort() { r.rollback() }
