package designer

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/erd"
)

// Catalog is what the designer knows of the connection: its schema (tables,
// columns, keys, foreign keys), the indexes of the tables on the canvas, and
// the planner's row estimates.
//
// EVIDENCE OR SILENCE (the explain package's rule, kept here): a rule about
// indexes speaks only when IndexesKnown, and a rule about size only when the
// table's estimate is known. On bytdb, which lists no indexes, and SQLite,
// which keeps no estimates, those rules are quiet rather than guessing — the
// live plan (plan.go) is what speaks for those engines.
type Catalog struct {
	Driver string
	Schema *erd.Schema
	// Indexes per table. Primary and unique KEYS are indexed on every
	// engine dbc speaks, so they count as indexes even when this is empty.
	Indexes      map[TableKey][]Index
	IndexesKnown bool
	// Rows is each table's estimated row count; a table not in it is of
	// unknown size.
	Rows map[TableKey]int64
}

// TableKey names a table of the catalog.
type TableKey struct{ Schema, Name string }

// Index is one index: its key columns in order ("" for an expression).
type Index struct {
	Name    string   `json:"name"`
	Cols    []string `json:"cols"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
	Partial bool     `json:"partial,omitempty"`
	Method  string   `json:"method,omitempty"`
}

// Size thresholds, in rows, matching the explain package's: below bigTable a
// full scan is the right plan and not worth a word; past hugeTable even a
// careful plan should not read the table whole without a reason.
const (
	bigTable  = 10_000
	hugeTable = 100_000
)

// erdTable finds the catalog's table for a canvas table, or nil (dropped,
// renamed, or another connection's design).
func (c *Catalog) erdTable(t *Table) *erd.Table {
	if c == nil || c.Schema == nil {
		return nil
	}
	for _, et := range c.Schema.Tables {
		if et.Name == t.Name && (et.Schema == t.Schema || t.Schema == "") {
			return et
		}
	}
	return nil
}

// rows is a table's estimated size, and whether it is known.
func (c *Catalog) rows(et *erd.Table) (int64, bool) {
	if c == nil || et == nil {
		return 0, false
	}
	n, ok := c.Rows[TableKey{et.Schema, et.Name}]
	return n, ok
}

// big reports whether a table is known to be at least n rows.
func (c *Catalog) big(et *erd.Table, n int64) bool {
	r, ok := c.rows(et)
	return ok && r >= n
}

// indexes is every index of a table, the keys included: the primary key and
// each unique key are indexed on every engine (SQLite's INTEGER PRIMARY KEY
// is the rowid itself), so they are added when the listing lacks them — as
// it always does on an engine whose indexes are not listed.
func (c *Catalog) indexes(et *erd.Table) []Index {
	if et == nil {
		return nil
	}
	var out []Index
	if c != nil {
		out = append(out, c.Indexes[TableKey{et.Schema, et.Name}]...)
	}
	has := func(cols []string) bool {
		return slices.ContainsFunc(out, func(x Index) bool { return slices.Equal(x.Cols, cols) })
	}
	if len(et.PK) > 0 && !has(et.PK) {
		out = append(out, Index{Name: et.Name + " primary key", Cols: et.PK, Unique: true, Primary: true})
	}
	for _, u := range et.Uniques {
		if !has(u) {
			out = append(out, Index{Name: et.Name + " unique key", Cols: u, Unique: true})
		}
	}
	return out
}

// leads reports whether some usable index of the table starts with one of
// cols — the test of "can an index find these rows without reading them
// all". A partial index is not counted (it serves only queries whose filter
// implies its own), nor is a non-btree one for an equality or range (a GIN
// index on a column serves @> and full-text, not =).
func (c *Catalog) leads(et *erd.Table, cols ...string) (Index, bool) {
	for _, x := range c.indexes(et) {
		if x.Partial || (x.Method != "" && x.Method != "btree" && x.Method != "hash") || len(x.Cols) == 0 {
			continue
		}
		if slices.Contains(cols, x.Cols[0]) && x.Cols[0] != "" {
			return x, true
		}
	}
	return Index{}, false
}

// unique reports whether cols identify at most one row of the table: they
// cover its primary key or one of its unique keys (or unique indexes). A
// superset of a key is a key too — (id, name) is unique if id is.
//
// known is false for a table with no keys at all (a view, or a table
// without any): its uniqueness cannot be told, which the rules treat as
// "may be many" but say "may".
func (c *Catalog) unique(et *erd.Table, cols []string) (unique, known bool) {
	if et == nil {
		return false, false
	}
	covers := func(key []string) bool {
		if len(key) == 0 {
			return false
		}
		for _, k := range key {
			if !slices.Contains(cols, k) {
				return false
			}
		}
		return true
	}
	known = len(et.PK) > 0 || len(et.Uniques) > 0
	if covers(et.PK) {
		return true, true
	}
	for _, u := range et.Uniques {
		if covers(u) {
			return true, true
		}
	}
	for _, x := range c.indexes(et) {
		if x.Unique && !x.Partial && !slices.Contains(x.Cols, "") {
			known = true
			if covers(x.Cols) {
				return true, true
			}
		}
	}
	return false, known
}

// ── column types ───────────────────────────────────────────────────────

// family is what kind of value a column holds, from its declared type,
// coarsely: enough to write a literal the right way, to check that a typed
// value fits, and to see that two joined columns are comparable without a
// cast. famUnknown (SQLite's typeless columns, an unusual type) turns the
// checks off rather than guessing.
type family string

const (
	famInt     family = "int"
	famNum     family = "num"
	famText    family = "text"
	famBool    family = "bool"
	famDate    family = "date" // a calendar date, no time of day
	famTime    family = "time" // a timestamp / datetime
	famUUID    family = "uuid"
	famJSON    family = "json"
	famBin     family = "bin"
	famUnknown family = ""
)

// familyOf reads a declared type: "character varying(80)", "int unsigned",
// "timestamp with time zone", SQLite's free-form "VARCHAR(20)" or "".
func familyOf(typ string) family {
	t := strings.ToLower(strings.TrimSpace(typ))
	switch {
	case t == "":
		return famUnknown
	case strings.HasPrefix(t, "interval"):
		return famUnknown // contains "int", but is a duration
	case strings.HasPrefix(t, "timestamp"), strings.HasPrefix(t, "datetime"):
		return famTime
	case t == "date":
		return famDate
	case strings.HasPrefix(t, "time"), t == "year":
		return famUnknown // a time of day; the date operators do not apply
	case strings.Contains(t, "int"), strings.Contains(t, "serial"):
		return famInt
	case strings.Contains(t, "numeric"), strings.Contains(t, "decimal"), strings.Contains(t, "real"),
		strings.Contains(t, "double"), strings.Contains(t, "float"), strings.Contains(t, "money"):
		return famNum
	case strings.HasPrefix(t, "bool"), t == "bit(1)":
		return famBool
	case t == "uuid":
		return famUUID
	case strings.HasPrefix(t, "json"):
		return famJSON
	case strings.Contains(t, "char"), strings.Contains(t, "text"), strings.Contains(t, "clob"),
		strings.HasPrefix(t, "enum"), t == "citext", t == "name", strings.HasPrefix(t, "set("):
		return famText
	case strings.Contains(t, "bytea"), strings.Contains(t, "blob"), strings.Contains(t, "binary"):
		return famBin
	}
	return famUnknown
}

// comparable reports whether columns of the two families can be compared
// without the engine casting one side on every row. Integers and other
// numbers compare natively; anything against an unknown is given the
// benefit of the doubt.
func comparable(a, b family) bool {
	if a == famUnknown || b == famUnknown || a == b {
		return true
	}
	num := func(f family) bool { return f == famInt || f == famNum }
	tm := func(f family) bool { return f == famDate || f == famTime }
	return (num(a) && num(b)) || (tm(a) && tm(b))
}

// ── dialects ───────────────────────────────────────────────────────────

// dialect is how an engine spells what the generator writes.
type dialect struct {
	name     string // postgres, mysql, sqlite, bytdb
	backtick bool   // MySQL quotes names with `…`
	fold     bool   // Postgres folds an unquoted name to lower case
	// backslash: MySQL (by default) reads a backslash in a string literal
	// as an escape, so a literal's backslashes are doubled there
	backslash bool
	fullJoin  bool // FULL OUTER JOIN is spoken (not by MySQL)
}

// dialectOf maps a configured driver to its dialect. An unknown driver gets
// Postgres's, as completion does.
func dialectOf(driver string) dialect {
	switch strings.ToLower(driver) {
	case "mysql", "mariadb":
		return dialect{name: "mysql", backtick: true, backslash: true}
	case "sqlite", "sqlite3":
		return dialect{name: "sqlite", fullJoin: true}
	case "bytdb":
		return dialect{name: "bytdb", fold: true, fullJoin: true}
	}
	return dialect{name: "postgres", fold: true, fullJoin: true}
}

// reserved are words a name must be quoted to be read as a name rather than
// as SQL: the clause words an alias may never be (sqlcomplete's list) and
// the ones tables and columns are commonly named after ("user", "order").
var reserved = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`on where join inner left right full outer cross natural using
		group order limit offset having union intersect except window set values select
		returning for fetch lateral as and or when then else end default partition
		tablesample do into from with not is in force ignore straight_join use
		by asc desc null all any some case
		user table column check primary references unique constraint grant only
		analyse analyze collate current_user session_user current_date current_time
		current_timestamp key to at if of no between like ilike exists true false
		create drop alter index view distinct some both leading trailing
		array cast localtime localtimestamp placing similar symmetric verbose
		natural freeze notnull isnull variadic window`) {
		m[w] = true
	}
	return m
}()

// ident writes a name as the dialect needs it: bare when the engine reads it
// back unchanged, quoted otherwise — a capital on Postgres (which folds it
// to lower case), a space or symbol anywhere, a leading digit, a reserved
// word. The same rule as sqlcomplete's dialect.quote.
func (d dialect) ident(name string) string {
	plain := name != "" && !(name[0] >= '0' && name[0] <= '9')
	for i := 0; i < len(name) && plain; i++ {
		b := name[i]
		switch {
		case b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z'):
		case b >= 'A' && b <= 'Z':
			plain = !d.fold
		default:
			plain = false
		}
	}
	if plain && !reserved[strings.ToLower(name)] {
		return name
	}
	if d.backtick {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// lit writes s as a string literal.
func (d dialect) lit(s string) string {
	if d.backslash {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
