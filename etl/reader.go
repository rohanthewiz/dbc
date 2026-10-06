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
type Reader struct {
	ctx     context.Context
	conn    string
	rows    *sql.Rows
	cols    []string
	dbTypes []string // driver type names, upper case ("INT4", "VARCHAR", "")
	binary  []bool   // column holds raw bytes: keep []byte as []byte
	holders []any    // *any per column, reused across Scan calls
	row     []any
	n       int64
	err     error
	closed  bool
}

// Read runs query on c and returns a Reader positioned before the first row.
// The query runs under ctx; canceling it stops the fetch.
func Read(ctx context.Context, c Conn, query string, args ...any) (*Reader, error) {
	rows, err := c.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "read", "query", clip(query))
	}
	cols, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, serr.Wrap(err, "conn", c.Name, "op", "read columns")
	}
	rd := &Reader{ctx: ctx, conn: c.Name, rows: rows, cols: cols,
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
		_ = r.Close()
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

// Close releases the query's connection. It is safe to call more than once,
// and a deferred Close after reading to the end costs nothing.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.rows.Close()
}
