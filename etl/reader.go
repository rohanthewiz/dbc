package etl

import (
	"context"
	"database/sql"
	"strings"

	"github.com/rohanthewiz/serr"
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
	ctx     context.Context
	conn    string
	tx      *sql.Tx // Postgres: the read's transaction, which scopes pgPinOutput
	rows    *sql.Rows
	cols    []string
	dbTypes []string // driver type names, upper case ("INT4", "VARCHAR", "")
	binary  []bool   // column holds raw bytes: keep []byte as []byte
	holders []any    // *any per column, reused across Scan calls
	row     []any
	n       int64
	err     error
	closed  bool
	// holdTx keeps the transaction open past the last row, for Copy: it
	// commits the read (Close) only after its load has, or rolls it back
	// (rollback). Next would otherwise commit as the rows run out.
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

// read is Read with setup: statements run in the read's transaction after
// pgPinOutput and before the query (Postgres only, where there is one) —
// a copy's lock_timeout (sourceSetup).
func read(ctx context.Context, c Conn, query string, setup []string, args ...any) (*Reader, error) {
	var (
		tx   *sql.Tx
		rows *sql.Rows
		err  error
	)
	if c.Engine == Postgres {
		if tx, err = c.DB.BeginTx(ctx, nil); err != nil {
			return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "read", "query", clip(query))
		}
		for _, s := range append([]string{pgPinOutput}, setup...) {
			if _, err = tx.ExecContext(ctx, s); err != nil {
				break
			}
		}
		if err == nil {
			rows, err = tx.QueryContext(ctx, query, args...)
		}
	} else {
		rows, err = c.DB.QueryContext(ctx, query, args...)
	}
	if err != nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "read", "query", clip(query))
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		if tx != nil {
			_ = tx.Rollback()
		}
		return nil, serr.Wrap(err, "conn", c.Name, "op", "read columns")
	}
	rd := &Reader{ctx: ctx, conn: c.Name, tx: tx, rows: rows, cols: cols,
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
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.rows.Close()
	if r.tx != nil {
		if err == nil && r.err == nil {
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
func (r *Reader) rollback() {
	if r.closed {
		return
	}
	r.closed = true
	_ = r.rows.Close()
	if r.tx != nil {
		_ = r.tx.Rollback()
	}
}
