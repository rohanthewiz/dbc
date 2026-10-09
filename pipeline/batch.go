// Package pipeline is dbc's data-flow layer: typed rows move in batches
// from a source, through transforms, into sinks — a fragment — and
// fragments run in order to make a pipeline. Jobs (several pipelines in a
// dependency graph) build on it in package jobs.
//
//	fragment "orders"                              fragment "merge"
//	┌─────────┐   ┌───────────┐   ┌───────────┐    ┌─────────────────┐
//	│ source  │──►│ transform │─┬►│ sink      │    │ action          │
//	│sql.read │   │go.transf. │ │ │sql.write  │    │sql.exec         │
//	└─────────┘   └───────────┘ │ └───────────┘    └─────────────────┘
//	                            └►│ sink      │
//	                              │preview    │
//	                              └───────────┘
//
// Inside a fragment rows move as Batches of up to Fragment.Batch rows: the
// source yields one, each transform reshapes it, each sink loads it, and
// the sinks commit together at the end — so a fragment is all-or-nothing
// per sink. Nothing passes between fragments but what the sinks committed
// and a few named values (Stats.Vars), which is what makes a fragment
// restartable on its own and a pipeline readable from any SQL console.
//
// The pieces, and the file each is in:
//
//	Batch, Col                 batch.go    the rows between nodes
//	Host, Env, Source,
//	Transform, Sink, Action    node.go     what a node is handed and must do
//	Plugin, Field, Register    plugin.go   kinds of node, self-described
//	Spec, Fragment, Node       spec.go     a pipeline as data (JSON)
//	Check                      check.go    what is wrong with a spec, before a run
//	Run, Options, RunStats     run.go      the fragment and pipeline runners
//	Builder                    builder.go  a Spec built from Go, for scripts
//	Gen                        gen.go      a Spec written out as a script
//	sql.*, cols.*, rows.*,
//	preview                    builtin_*.go  the compiled-in plugins
//
// The package does not import sdb: sdb imports it, aliases its types and
// has S satisfy Host, so scripts see one API (sdb.Batch, s.RunPipeline)
// and nothing here depends on how a host builds a session. The Go-code
// plugins (go.transform and kin) need the script interpreter, which needs
// sdb, so package script registers them.
package pipeline

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/model"
)

// Col is one column of a Batch: its name and, when known, the source
// driver's type name (upper case, as etl.Reader reports it: "INT4",
// "VARCHAR", "DATETIME"). A sink that creates a table maps DBType to the
// destination's own types; "" (a column a transform added, say) becomes
// text, which every engine can hold.
type Col struct {
	Name   string `json:"name"`
	DBType string `json:"type,omitempty"`
}

// Batch is a slice of rows that share one column list. Values are what
// etl.Reader yields — int64, float64, bool, string, time.Time, []byte and
// nil for NULL — and a transform may put in anything the destination's
// Writer accepts: into Postgres also slices (arrays), maps and structs
// (JSON), time.Duration (an interval) and driver.Valuers; into MySQL,
// SQLite and bytdb only what database/sql converts (see etl/pgtext.go).
//
// Rows are positional: Rows[i][j] is column Cols[j] of row i. The helpers
// below look a column up by name so a transform written for one query
// still works when a column is added before it; they are conveniences,
// and indexing Rows directly is fine for a hot loop (resolve the column
// once with Col).
//
// A Batch is owned by whoever holds it: a transform may change it in place
// and hand it on. The runner clones a batch before giving it to a second
// consumer (a fan-out), so no node ever sees another's edits.
type Batch struct {
	Cols []Col
	Rows [][]any
}

// NewBatch makes a batch of cols with rows (which may be nil).
func NewBatch(cols []Col, rows [][]any) *Batch {
	return &Batch{Cols: cols, Rows: rows}
}

// ColsOf pairs column names with driver type names (either may be shorter;
// a missing type is "").
func ColsOf(names, dbTypes []string) []Col {
	out := make([]Col, len(names))
	for i, n := range names {
		out[i].Name = n
		if i < len(dbTypes) {
			out[i].DBType = dbTypes[i]
		}
	}
	return out
}

// Len is the number of rows; 0 for a nil batch.
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return len(b.Rows)
}

// Names lists the column names, in order.
func (b *Batch) Names() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.Cols))
	for i, c := range b.Cols {
		out[i] = c.Name
	}
	return out
}

// DBTypes lists the columns' driver type names, in order ("" where unknown).
func (b *Batch) DBTypes() []string {
	if b == nil {
		return nil
	}
	out := make([]string, len(b.Cols))
	for i, c := range b.Cols {
		out[i] = c.DBType
	}
	return out
}

// Col is the index of the column called name, or -1. Names are matched
// exactly first, then without regard to case, so "ID" finds "id" when
// there is no "ID".
func (b *Batch) Col(name string) int {
	if b == nil {
		return -1
	}
	for i, c := range b.Cols {
		if c.Name == name {
			return i
		}
	}
	for i, c := range b.Cols {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// Get is row's value in the column called name; nil when there is no such
// column or row.
func (b *Batch) Get(row int, name string) any {
	j := b.Col(name)
	if j < 0 || row < 0 || row >= b.Len() {
		return nil
	}
	return b.Rows[row][j]
}

// Set puts v in row's column called name, reporting whether there was
// such a column and row.
func (b *Batch) Set(row int, name string, v any) bool {
	j := b.Col(name)
	if j < 0 || row < 0 || row >= b.Len() {
		return false
	}
	b.Rows[row][j] = v
	return true
}

// AddCol appends a column, filled by calling fill for each row (nil fill
// leaves every value NULL). dbType is the driver type name a sink's
// CREATE TABLE should map, or "" for text. A column of that name already
// there is replaced in place instead, so AddCol is also "set a whole
// column".
func (b *Batch) AddCol(name, dbType string, fill func(row int) any) {
	if b == nil {
		return
	}
	j := slices.IndexFunc(b.Cols, func(c Col) bool { return c.Name == name })
	if j < 0 {
		j = len(b.Cols)
		b.Cols = append(b.Cols, Col{Name: name, DBType: dbType})
		for i := range b.Rows {
			b.Rows[i] = append(b.Rows[i], nil)
		}
	} else {
		b.Cols[j].DBType = dbType
	}
	if fill == nil {
		return
	}
	for i := range b.Rows {
		b.Rows[i][j] = fill(i)
	}
}

// Drop removes the named columns (unknown names are ignored).
func (b *Batch) Drop(names ...string) {
	if b == nil {
		return
	}
	var keep []string
	for _, c := range b.Cols {
		if !slices.ContainsFunc(names, func(n string) bool { return n == c.Name }) {
			keep = append(keep, c.Name)
		}
	}
	b.Keep(keep...)
}

// Keep reduces the batch to the named columns, in the order given — a
// select and a reorder in one. A name the batch does not have is ignored.
func (b *Batch) Keep(names ...string) {
	if b == nil {
		return
	}
	var idx []int
	var cols []Col
	for _, n := range names {
		if j := b.Col(n); j >= 0 && !slices.Contains(idx, j) {
			idx = append(idx, j)
			cols = append(cols, b.Cols[j])
		}
	}
	for i, row := range b.Rows {
		out := make([]any, len(idx))
		for k, j := range idx {
			out[k] = row[j]
		}
		b.Rows[i] = out
	}
	b.Cols = cols
}

// Rename changes a column's name, reporting whether it was there.
func (b *Batch) Rename(from, to string) bool {
	j := b.Col(from)
	if j < 0 {
		return false
	}
	b.Cols[j].Name = to
	return true
}

// Filter keeps the rows keep says to, in order.
func (b *Batch) Filter(keep func(row int) bool) {
	if b == nil {
		return
	}
	out := b.Rows[:0]
	for i, row := range b.Rows {
		if keep(i) {
			out = append(out, row)
		}
	}
	// the dropped tail is cleared so the rows do not linger behind the slice
	for i := len(out); i < len(b.Rows); i++ {
		b.Rows[i] = nil
	}
	b.Rows = out
}

// Clone copies the batch: the column list and every row slice, so the
// copy can be changed without the original seeing it. Values themselves
// are shared ([]byte included), as a transform is not expected to edit a
// value in place.
func (b *Batch) Clone() *Batch {
	if b == nil {
		return nil
	}
	c := &Batch{Cols: slices.Clone(b.Cols), Rows: make([][]any, len(b.Rows))}
	for i, row := range b.Rows {
		c.Rows[i] = slices.Clone(row)
	}
	return c
}

// Result renders the batch as a result set, for the grid (preview) or an
// export: display strings as db.Manager makes them (NULL, RFC 3339 times,
// bytes in hex) and the typed values as Raw.
func (b *Batch) Result(conn, query string) *model.Result {
	r := &model.Result{Conn: conn, Query: query, Columns: b.Names()}
	for _, row := range b.Rows {
		strs := make([]string, len(row))
		for j, v := range row {
			strs[j] = FormatValue(v)
		}
		r.Rows = append(r.Rows, strs)
		r.Raw = append(r.Raw, slices.Clone(row))
	}
	return r
}

// FormatValue is a value as the grid shows it.
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return x
	case []byte:
		return fmt.Sprintf("\\x%x", x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case float64:
		return fmt.Sprintf("%g", x)
	}
	return fmt.Sprint(v)
}
