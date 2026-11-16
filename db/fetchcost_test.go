package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
)

// The fetch loop's cost per cell (N-282). readSet is what every query's
// rows go through, so what it spends on one cell it spends rows × columns
// times: nothing at the default 1,000 rows, a great deal once max_rows is
// raised for an export.
//
// cellsDriver stands in for the two shapes of value the real drivers hand
// back, without a server:
//
//	"bytes"  go-sql-driver/mysql over the text protocol: every non-NULL
//	         value is a []byte into the driver's own read buffer, which the
//	         next row overwrites (so a destination must copy it)
//	"typed"  pgx's stdlib adapter (and modernc's SQLite): Go values by
//	         column type — int64, float64, string, bool, time.Time, NULL
//
// The DSN is "<shape>:<rows>:<cols>".
type cellsDriver struct{}

func init() { sql.Register("dbc-cells", cellsDriver{}) }

func (cellsDriver) Open(dsn string) (driver.Conn, error) {
	parts := strings.Split(dsn, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("dsn %q: want shape:rows:cols", dsn)
	}
	rows, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, err
	}
	cols, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, err
	}
	return &cellsConn{shape: parts[0], rows: rows, cols: cols}, nil
}

type cellsConn struct {
	shape      string
	rows, cols int
}

func (c *cellsConn) Prepare(string) (driver.Stmt, error) { return &cellsStmt{c}, nil }
func (c *cellsConn) Close() error                        { return nil }
func (c *cellsConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

type cellsStmt struct{ c *cellsConn }

func (s *cellsStmt) Close() error                               { return nil }
func (s *cellsStmt) NumInput() int                              { return -1 }
func (s *cellsStmt) Exec([]driver.Value) (driver.Result, error) { return nil, driver.ErrSkip }
func (s *cellsStmt) Query([]driver.Value) (driver.Rows, error) {
	return &cellsRows{c: s.c, buf: make([]byte, 0, 64*s.c.cols)}, nil
}

type cellsRows struct {
	c   *cellsConn
	row int
	buf []byte // the "bytes" shape's read buffer, reused row after row
}

func (r *cellsRows) Columns() []string {
	cols := make([]string, r.c.cols)
	for i := range cols {
		cols[i] = "c" + strconv.Itoa(i)
	}
	return cols
}
func (r *cellsRows) Close() error { return nil }

// cellsT0 is the typed shape's timestamp base; fixed, so runs compare.
var cellsT0 = time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)

func (r *cellsRows) Next(dest []driver.Value) error {
	if r.row >= r.c.rows {
		return io.EOF
	}
	n := r.row
	r.row++
	if r.c.shape == "bytes" {
		// One buffer for the whole row, overwritten by the next, as the
		// MySQL driver's is: each value is a slice of it.
		r.buf = r.buf[:0]
		for i := range dest {
			if i%7 == 6 {
				dest[i] = nil
				continue
			}
			start := len(r.buf)
			switch i % 3 {
			case 0:
				r.buf = strconv.AppendInt(r.buf, int64(n*31+i), 10)
			case 1:
				r.buf = append(r.buf, "customer name "...)
				r.buf = strconv.AppendInt(r.buf, int64(n), 10)
			default:
				r.buf = append(r.buf, "2026-11-16 09:30:00"...)
			}
			dest[i] = r.buf[start:len(r.buf):len(r.buf)]
		}
		return nil
	}
	for i := range dest {
		switch i % 7 {
		case 0:
			dest[i] = int64(n*31 + i)
		case 1:
			dest[i] = "customer name " + strconv.Itoa(n)
		case 2:
			dest[i] = float64(n) / 8
		case 3:
			dest[i] = cellsT0.Add(time.Duration(n) * time.Second)
		case 4:
			dest[i] = n%2 == 0
		case 5:
			dest[i] = int64(n % 50) // small: strconv's table, no allocation
		default:
			dest[i] = nil
		}
	}
	return nil
}

// fetchCells runs the readSet loop over a cellsDriver result.
func fetchCells(tb testing.TB, shape string, rows, cols int) {
	tb.Helper()
	dbh, err := sql.Open("dbc-cells", fmt.Sprintf("%s:%d:%d", shape, rows, cols))
	if err != nil {
		tb.Fatal(err)
	}
	defer dbh.Close()
	m := NewManager(&config.Config{MaxRows: rows + 1})
	sqlRows, err := dbh.Query("SELECT *")
	if err != nil {
		tb.Fatal(err)
	}
	defer sqlRows.Close()
	res, err := m.readSet(context.Background(), sqlRows, "c", "SELECT *")
	if err != nil {
		tb.Fatal(err)
	}
	if len(res.Rows) != rows {
		tb.Fatalf("read %d rows, want %d", len(res.Rows), rows)
	}
}

// BenchmarkFetch reads 10,000 rows of 14 columns in each shape. The
// allocs/op and B/op over 140,000 cells are the per-cell cost.
func BenchmarkFetch(b *testing.B) {
	for _, shape := range []string{"bytes", "typed"} {
		b.Run(shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fetchCells(b, shape, 10_000, 14)
			}
		})
	}
}

// driveCells walks a cellsDriver result without scanning it: the driver's
// own cost (boxing each value into a driver.Value), which readSet cannot
// avoid and the budget below subtracts.
func driveCells(tb testing.TB, shape string, rows, cols int) {
	tb.Helper()
	dbh, err := sql.Open("dbc-cells", fmt.Sprintf("%s:%d:%d", shape, rows, cols))
	if err != nil {
		tb.Fatal(err)
	}
	defer dbh.Close()
	sqlRows, err := dbh.Query("SELECT *")
	if err != nil {
		tb.Fatal(err)
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
	}
}

// BenchmarkFetchDriver is BenchmarkFetch's floor: the same rows walked
// without a Scan.
func BenchmarkFetchDriver(b *testing.B) {
	for _, shape := range []string{"bytes", "typed"} {
		b.Run(shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				driveCells(b, shape, 10_000, 14)
			}
		})
	}
}

// The fetch loop's budget (N-282): on MySQL-shaped rows, at most 2.5
// allocations per non-NULL cell beyond the driver's own — one copy of the
// bytes, the box Raw's `any` needs for it, and a little for the rows'
// slabs and the result's growing row list. HEAD before N-282 spent 5.2:
// a clone in Scan, two string conversions, the box, and two slices a row.
func TestFetchCostsWhatItHolds(t *testing.T) {
	const rows, cols = 4000, 14
	// every 7th column is NULL (cellsRows.Next)
	nonNull := float64(rows * (cols - cols/7))
	base := testing.AllocsPerRun(3, func() { driveCells(t, "bytes", rows, cols) })
	got := testing.AllocsPerRun(3, func() { fetchCells(t, "bytes", rows, cols) })
	per := (got - base) / nonNull
	t.Logf("%.3f allocations per cell beyond the driver's", per)
	if per > 2.5 {
		t.Errorf("%.2f allocations per cell beyond the driver's (%.0f in all, the driver %.0f), want at most 2.5",
			per, got, base)
	}
}

// oldReadSet is readSet's row loop as it was before N-282: a *any per
// column, then renderVal and rawVal per cell. The new loop must hold the
// same Rows and Raw.
func oldReadSet(tb testing.TB, rows *sql.Rows) ([][]string, [][]any) {
	tb.Helper()
	cols, err := rows.Columns()
	if err != nil {
		tb.Fatal(err)
	}
	holders := make([]any, len(cols))
	for i := range holders {
		holders[i] = new(any)
	}
	var disp [][]string
	var raws [][]any
	for rows.Next() {
		if err = rows.Scan(holders...); err != nil {
			tb.Fatal(err)
		}
		d := make([]string, len(cols))
		r := make([]any, len(cols))
		for i, h := range holders {
			v := *(h.(*any))
			d[i] = oldRenderVal(v)
			r[i] = oldRawVal(v)
		}
		disp = append(disp, d)
		raws = append(raws, r)
	}
	return disp, raws
}

func oldRenderVal(v any) string {
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

func oldRawVal(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return v
	}
}

// The new loop holds what the old one held, cell for cell, in both
// shapes and over enough rows to span several slab chunks; and the rows
// do not share room — an append to one leaves the next alone.
func TestFetchMatchesOldLoop(t *testing.T) {
	for _, shape := range []string{"bytes", "typed"} {
		const rows, cols = 3000, 9
		dsn := fmt.Sprintf("%s:%d:%d", shape, rows, cols)
		dbh, err := sql.Open("dbc-cells", dsn)
		if err != nil {
			t.Fatal(err)
		}
		old, err := dbh.Query("SELECT *")
		if err != nil {
			t.Fatal(err)
		}
		wantRows, wantRaw := oldReadSet(t, old)
		old.Close()

		cur, err := dbh.Query("SELECT *")
		if err != nil {
			t.Fatal(err)
		}
		m := NewManager(&config.Config{MaxRows: rows})
		res, err := m.readSet(context.Background(), cur, "c", "SELECT *")
		cur.Close()
		dbh.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != rows || len(res.Raw) != rows || res.Truncated {
			t.Fatalf("%s: %d rows, %d raw, truncated %v", shape, len(res.Rows), len(res.Raw), res.Truncated)
		}
		for ri := range wantRows {
			for ci := range cols {
				if res.Rows[ri][ci] != wantRows[ri][ci] || res.Raw[ri][ci] != wantRaw[ri][ci] {
					t.Fatalf("%s row %d col %d: (%q, %#v), want (%q, %#v)", shape, ri, ci,
						res.Rows[ri][ci], res.Raw[ri][ci], wantRows[ri][ci], wantRaw[ri][ci])
				}
			}
		}
		// rows cut from one slab chunk are capped at their own length
		if r0 := res.Rows[0]; cap(r0) != len(r0) || cap(res.Raw[0]) != len(res.Raw[0]) {
			t.Errorf("%s: row 0 has cap %d/%d for %d cells", shape, cap(r0), cap(res.Raw[0]), len(r0))
		}
		before := res.Rows[1][0]
		_ = append(res.Rows[0], "spill")
		if res.Rows[1][0] != before {
			t.Errorf("%s: an append to row 0 wrote over row 1", shape)
		}
	}
}

// cellValue's text is fmt.Sprint's, for every type a driver hands back and
// at the edges of each: the fast paths are the formatting %v uses.
func TestCellValueMatchesSprint(t *testing.T) {
	vals := []any{
		nil, "", "text", []byte(nil), []byte("bytes"), true, false,
		int64(0), int64(-1), int64(99), int64(100), int64(math.MaxInt64), int64(math.MinInt64),
		int32(-7), int32(math.MaxInt32), uint64(0), uint64(math.MaxUint64),
		float32(0.1), float32(-3.4e38), float32(1e-45), float32(math.Inf(1)), float32(math.NaN()),
		0.0, math.Copysign(0, -1), 0.1, 1e21, 1e20, 123456789.0, 1e-7, 5e-324, math.MaxFloat64,
		math.Inf(1), math.Inf(-1), math.NaN(),
		time.Date(2026, 11, 16, 9, 30, 0, 123, time.UTC),
		time.Date(1999, 1, 2, 3, 4, 5, 0, time.FixedZone("x", -5*3600)),
		int(42), uint8(7), []int{1, 2}, struct{ A int }{3},
	}
	// and a spread of floats by their bits, the hardest case for a
	// shortest-repr formatter to agree on
	for i := range 2000 {
		bits := uint64(i) * 0x9E3779B97F4A7C15
		vals = append(vals, math.Float64frombits(bits), math.Float32frombits(uint32(bits>>32)))
	}
	for _, v := range vals {
		disp, raw := cellValue(v)
		if want := oldRenderVal(v); disp != want {
			t.Errorf("cellValue(%#v) text = %q, want %q", v, disp, want)
		}
		want := oldRawVal(v)
		if f, ok := want.(float64); ok && math.IsNaN(f) {
			if g, ok := raw.(float64); !ok || !math.IsNaN(g) {
				t.Errorf("cellValue(%#v) raw = %#v, want NaN", v, raw)
			}
			continue
		}
		if f, ok := want.(float32); ok && f != f {
			if g, ok := raw.(float32); !ok || g == g {
				t.Errorf("cellValue(%#v) raw = %#v, want NaN", v, raw)
			}
			continue
		}
		if !reflect.DeepEqual(raw, want) {
			t.Errorf("cellValue(%#v) raw = %#v, want %#v", v, raw, want)
		}
	}
}

// recSink is a RowSink that keeps what it is handed, to check the contract.
type recSink struct {
	sets [][]string
	rows [][]string
	raws [][]any
	fail error // returned from Row once failAt rows are in
	at   int
}

func (s *recSink) Set(cols []string) error {
	s.sets = append(s.sets, cols)
	return nil
}

func (s *recSink) Row(disp []string, raw []any) error {
	if s.fail != nil && len(s.rows) == s.at {
		return s.fail
	}
	// copied: the sink's slices are reused for the next row
	s.rows = append(s.rows, append([]string(nil), disp...))
	s.raws = append(s.raws, append([]any(nil), raw...))
	return nil
}

// StreamRows: the rows go to the sink, not the Result; max_rows still
// cuts them; a sink's error ends the statement with that error; a set
// with neither columns nor rows does not reach the sink.
func TestStreamRows(t *testing.T) {
	run := func(dsn string, maxRows int, sink *recSink) (*Manager, error, int) {
		dbh, err := sql.Open("dbc-cells", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer dbh.Close()
		m := NewManager(&config.Config{MaxRows: maxRows})
		rows, err := dbh.Query("SELECT *")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		res, err := m.readSets(StreamRows(context.Background(), sink), rows, "c", "SELECT *", nil)
		if res != nil {
			if len(res.Rows) != 0 || len(res.Raw) != 0 {
				t.Errorf("%s: the Result held %d rows under a sink", dsn, len(res.Rows))
			}
			if res.Truncated {
				return m, err, -1
			}
		}
		return m, err, 0
	}

	s := &recSink{}
	if _, err, trunc := run("typed:50:3", 1000, s); err != nil || trunc != 0 {
		t.Fatalf("typed:50:3: %v, truncated %v", err, trunc != 0)
	}
	if len(s.sets) != 1 || strings.Join(s.sets[0], ",") != "c0,c1,c2" || len(s.rows) != 50 {
		t.Fatalf("sets %v, %d rows", s.sets, len(s.rows))
	}
	if s.rows[49][1] != "customer name 49" || s.raws[49][0] != int64(49*31) {
		t.Errorf("row 49 = %v / %#v", s.rows[49], s.raws[49])
	}

	s = &recSink{}
	if _, err, trunc := run("typed:50:3", 20, s); err != nil || trunc != -1 || len(s.rows) != 20 {
		t.Errorf("max_rows 20: %v, truncated %v, %d rows", err, trunc != 0, len(s.rows))
	}

	full := errors.New("disk full")
	s = &recSink{fail: full, at: 7}
	if _, err, _ := run("bytes:50:3", 1000, s); !errors.Is(err, full) || len(s.rows) != 7 {
		t.Errorf("a failing sink: %v after %d rows, want disk full after 7", err, len(s.rows))
	}

	// no columns and no rows: what runMaybe reads as "it wrote"
	s = &recSink{}
	if _, err, _ := run("typed:0:0", 1000, s); err != nil || len(s.sets) != 0 {
		t.Errorf("an empty column-less set reached the sink: %v, %v", err, s.sets)
	}
	// columns and no rows: the header still goes out
	s = &recSink{}
	if _, err, _ := run("typed:0:2", 1000, s); err != nil || len(s.sets) != 1 || len(s.rows) != 0 {
		t.Errorf("an empty set with columns: %v, sets %v, %d rows", err, s.sets, len(s.rows))
	}
	// rows and no columns (Postgres's SELECT FROM t): each row, empty
	s = &recSink{}
	if _, err, _ := run("typed:3:0", 1000, s); err != nil || len(s.sets) != 1 || len(s.rows) != 3 {
		t.Errorf("rows with no columns: %v, sets %v, %d rows", err, s.sets, len(s.rows))
	}
}

// Every set of a statement that returns several (a MySQL CALL) reaches
// the sink, in order.
func TestStreamRowsSets(t *testing.T) {
	dbh, err := sql.Open("dbc-sets", "3")
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	rows, err := dbh.Query("CALL p()")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := NewManager(&config.Config{MaxRows: 100})
	s := &recSink{}
	res, err := m.readSets(StreamRows(context.Background(), s), rows, "c", "CALL p()", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.sets) != 3 || s.sets[2][0] != "c3" || len(s.rows) != 6 || s.rows[5][0] != "31" {
		t.Errorf("sets %v, rows %v", s.sets, s.rows)
	}
	if len(res.More) != 2 || res.More[1].Columns[0] != "c3" {
		t.Errorf("the Result's sets: %+v", res)
	}
}
