package db

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// What one cell costs (N-282).
//
// Every row a query returns passes through readSet, which keeps each value
// twice: once as display text (Result.Rows) and once typed (Result.Raw, for
// JSON export, NULL tests and numeric alignment). It used to build the two
// separately, from a value database/sql had already copied:
//
//	MySQL, text protocol (every value a []byte into the driver's buffer)
//	  Scan into *any ──► bytes.Clone          1 alloc, a copy
//	  renderVal       ──► string(b)            1 alloc, a copy
//	  rawVal          ──► string(b)            1 alloc, a copy
//	  raw[i] = s      ──► boxed string header  1 alloc
//	  + a []string and a []any per row
//
//	pgx / SQLite (typed values)
//	  renderVal ──► fmt.Sprint(v) for every number, and for every string
//
// Nothing at the default 1,000 rows; with max_rows raised for an export it
// is three copies of the data held or thrown away, and five allocations a
// cell (measured: 5.3 a cell, 148 B, on a 14-column MySQL-shaped result).
//
// Now the Scan destination is a cell (a sql.Scanner), so database/sql hands
// it the driver's value as it is — for a []byte, before any clone — and it
// converts once:
//
//	[]byte     string(b), and that one string is both the display text and
//	           the raw value: 1 copy, 2 allocs (the copy, and the box Raw's
//	           `any` needs for a string)
//	string     the same string both ways, and the driver's own box reused
//	int64 …    strconv instead of fmt.Sprint; small ints come from
//	           strconv's table with no allocation; the raw value is the
//	           driver's box, reused
//	time.Time  formatted once (RFC3339Nano; a DATE column's as a date),
//	           the text both ways
//	nil        "NULL" and nil
//
// and the rows' []string and []any are cut from slabs (rowSlab) rather than
// made one by one.
//
// What the text says (N-288). It was fmt.Sprint's for every type, which
// loses detail a database kept:
//
//	value                    fmt.Sprint              now
//	timestamp 12:00:00.25    2026-11-10T12:00:00Z    2026-11-10T12:00:00.25Z
//	date (pgx: a midnight)   2026-11-10T00:00:00Z    2026-11-10
//	float8 1234567           1.234567e+06            1234567
//	float4 0.1 (pgx widens)  0.10000000149011612     0.1
//	float8 'NaN', '-Inf'     NaN, -Inf               NaN, -Infinity
//	bytea \xff00             "\xff\x00" raw bytes    \xff00
//
// A float is written in plain decimal between 1e-7 and 1e21 and with an
// exponent outside it (JavaScript's rule, and encoding/json's for a JSON
// number), so 1e300 is not three hundred digits. Infinities take
// Postgres's spelling. Bytes that are not UTF-8 — a bytea, a MySQL BLOB —
// are hex in Postgres's bytea output form; bytes that are (every value of
// MySQL's text protocol) stay text, as before.

// colKind is what a column's database type says about how its values read,
// beyond the Go type the driver hands over. Only two types need it.
type colKind uint8

const (
	kindPlain colKind = iota
	// kindDate: a DATE column. pgx (and MySQL with parseTime) hand a date
	// over as a time.Time at midnight UTC, which read as a timestamp.
	kindDate
	// kindFloat32: a single-precision column (Postgres float4/real, MySQL
	// FLOAT). pgx's database/sql adapter widens it to float64, whose
	// shortest form shows the widening's noise; read as 32 bits it is the
	// value that was stored.
	kindFloat32
)

// columnKinds is each column's colKind, from its database type name; nil
// when the driver cannot say (every column then reads plain).
func columnKinds(rows *sql.Rows) []colKind {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil
	}
	kinds := make([]colKind, len(cts))
	for i, ct := range cts {
		kinds[i] = colKindOf(ct.DatabaseTypeName())
	}
	return kinds
}

// colKindOf maps a database type name to its colKind. The names are the
// drivers': pgx says DATE and FLOAT4, MySQL DATE and FLOAT, SQLite the
// declared type as written (so "date", or "DATE NOT NULL"'s first word is
// all it keeps).
func colKindOf(typeName string) colKind {
	switch strings.ToUpper(strings.TrimSpace(typeName)) {
	case "DATE":
		return kindDate
	case "FLOAT4", "REAL4", "FLOAT":
		// MySQL's FLOAT is single precision; its DOUBLE is DOUBLE.
		// Postgres says FLOAT8 for double precision and FLOAT4 for real.
		// SQLite's REAL is 8 bytes and reaches here as "REAL": plain.
		return kindFloat32
	}
	return kindPlain
}

// cellValue is a driver value's display text and raw value, as a result
// keeps them: NULL is "NULL" and nil; bytes and times are text both ways
// (a []byte is valid only until the next row, and a time's text is what
// every export writes); everything else keeps its Go value as raw, so a
// JSON export writes numbers as numbers.
//
// The text is fmt.Sprint's for every type but floats, times and non-UTF-8
// bytes (TestCellValueMatchesSprint), whose forms are above.
func cellValue(v any) (disp string, raw any) { return cellValueAs(v, kindPlain) }

// cellValueAs is cellValue for a column of kind k.
func cellValueAs(v any, k colKind) (disp string, raw any) {
	switch t := v.(type) {
	case nil:
		return "NULL", nil
	case []byte:
		s := bytesText(t)
		return s, s
	case string:
		// v, not t: re-boxing t would allocate a header the driver's
		// interface already holds
		return t, v
	case int64:
		return strconv.FormatInt(t, 10), v
	case float64:
		if k == kindFloat32 && float64(float32(t)) == t {
			// the widened float4: its 32-bit value, raw too, so JSON
			// writes 0.1 rather than 0.10000000149011612. Only a value
			// that fits 32 bits exactly is narrowed; anything else (a
			// driver that did not widen) is left as it came.
			f := float32(t)
			return FloatText(float64(f), 32), f
		}
		return FloatText(t, 64), v
	case bool:
		return strconv.FormatBool(t), v
	case time.Time:
		s := timeText(t, k)
		return s, s
	case int32:
		return strconv.FormatInt(int64(t), 10), v
	case uint64:
		return strconv.FormatUint(t, 10), v
	case float32:
		return FloatText(float64(t), 32), v
	}
	return fmt.Sprint(v), v
}

// FloatText is a float's display text: plain decimal, shortest round-trip
// digits for bits (32 or 64), between 1e-7 and 1e21 in magnitude (and 0);
// an exponent outside that; NaN, Infinity and -Infinity as Postgres spells
// them. Exported for the pipelines' and the grid's own float text, so a
// value reads the same everywhere.
func FloatText(f float64, bits int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	if a := math.Abs(f); a == 0 || (a >= 1e-7 && a < 1e21) {
		return strconv.FormatFloat(f, 'f', -1, bits)
	}
	return strconv.FormatFloat(f, 'g', -1, bits)
}

// timeText is a time's display text: a DATE column's midnight as the date
// alone, anything else RFC3339 with its fraction of a second (RFC3339Nano
// drops trailing zeros, so a whole second still reads as before).
func timeText(t time.Time, k colKind) string {
	if k == kindDate && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return t.Format(time.DateOnly)
	}
	return t.Format(time.RFC3339Nano)
}

// bytesText is a []byte's display text: the bytes as they are when they are
// UTF-8 (text, as MySQL sends every value), else hex in Postgres's bytea
// output form, \x then two digits a byte — what psql shows, and text that
// survives CSV and JSON, where raw bytes became invalid UTF-8 or U+FFFD.
func bytesText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 2+hex.EncodedLen(len(b)))
	out[0], out[1] = '\\', 'x'
	hex.Encode(out[2:], b)
	return string(out)
}

// cell is readSet's Scan destination. As a sql.Scanner it is handed the
// driver's value unconverted: database/sql's convertAssign clones a []byte
// only for the destination types it knows (*any, *[]byte), and a Scanner
// is not one of them. The value is converted before Scan returns, as the
// contract asks of a Scanner given driver memory. kind is its column's
// (columnKinds), set once per result set.
type cell struct {
	disp string
	raw  any
	kind colKind
}

func (c *cell) Scan(src any) error {
	c.disp, c.raw = cellValueAs(src, c.kind)
	return nil
}

// Rows per slab chunk: the first chunk is small, so a one-row result does
// not reserve room for hundreds; each next one doubles, to at most
// slabMaxCells cells (at 16 bytes a slot, 256 KB per array).
const (
	slabFirstRows = 8
	slabMaxCells  = 16 << 10
)

// rowSlab hands out the []string and []any of each row from shared chunks,
// so a result of n rows makes about 2·log(n) slices instead of 2n.
//
// Each row is cut with its capacity set to its length (a full slice
// expression), so an append to one row reallocates rather than writing over
// the next row's cells. The rows of a chunk keep the whole chunk alive;
// a result's rows are kept or dropped together, so that costs nothing.
type rowSlab struct {
	cols int
	next int // rows in the next chunk
	disp []string
	raw  []any
}

func newRowSlab(cols int) *rowSlab { return &rowSlab{cols: cols, next: slabFirstRows} }

// row returns the next row's two slices, each cols long. room is how many
// more rows the caller may still take (max_rows less what it holds), so a
// chunk is never cut for rows the cap will refuse.
func (s *rowSlab) row(room int) ([]string, []any) {
	n := s.cols
	if n == 0 {
		// a result with no columns (Postgres's `SELECT FROM t`) still has
		// rows; they are empty but not nil, as make gave them — a nil row
		// encodes as JSON null
		return []string{}, []any{}
	}
	if len(s.disp) < n {
		rows := max(min(s.next, room, slabMaxCells/n), 1)
		s.next = min(s.next*2, max(slabMaxCells/n, 1))
		s.disp, s.raw = make([]string, rows*n), make([]any, rows*n)
	}
	d, r := s.disp[:n:n], s.raw[:n:n]
	s.disp, s.raw = s.disp[n:], s.raw[n:]
	return d, r
}

// RowSink takes a statement's rows as they are read, in place of its Result
// holding them: a headless CSV export of a whole table writes each row as it
// arrives instead of holding every row first (StreamRows).
type RowSink interface {
	// Set starts a result set: its column names. It is called once for
	// every set that has columns or rows, before the set's first row —
	// not for the empty first set of a statement that returned no rows,
	// which comes back as a rows-affected Result as it always has.
	Set(cols []string) error
	// Row is one row of the current set. The slices are reused for the
	// next row, so a sink that keeps a row copies the slices; the strings
	// and values in them are its to keep.
	Row(disp []string, raw []any) error
}

// rowSinkKey is the context key StreamRows puts a RowSink under.
type rowSinkKey struct{}

// StreamRows returns ctx carrying sink: a statement run under it (Run,
// Session.Run) hands every row to sink as it is read. Its Result comes back
// with each set's Columns and Truncated as usual, and no Rows or Raw. A
// statement that returned no rows (an INSERT, a CALL that only writes)
// reaches the sink not at all and comes back as its usual rows-affected
// Result, which the caller then renders as it would any other.
//
// max_rows still applies: the sink is handed at most that many rows of a
// set, and the set is marked Truncated, as a held result is.
//
// A sink's error ends the statement with that error. When the statement is
// a plain read it is also stopped on the server rather than read to the end
// (stmtcut), so a full disk does not wait out the rest of a large table.
//
// Only the statement's own rows go to the sink: nothing else a run sends
// (the ROW_COUNT() after a MySQL CALL, a liveness ping) reads through
// readSet.
func StreamRows(ctx context.Context, sink RowSink) context.Context {
	return context.WithValue(ctx, rowSinkKey{}, sink)
}

// sinkOf is the RowSink ctx carries, or nil.
func sinkOf(ctx context.Context) RowSink {
	s, _ := ctx.Value(rowSinkKey{}).(RowSink)
	return s
}

// sinkErr marks a RowSink's error on its way out of readSet, so readSets
// can tell it from the driver's and stop the statement on the server
// rather than drain it. It is unwrapped before the caller sees it.
type sinkErr struct{ err error }

func (e sinkErr) Error() string { return e.err.Error() }
func (e sinkErr) Unwrap() error { return e.err }
