// Package erd draws a connection's schema as an entity-relationship diagram:
// its tables, their columns and keys, and the foreign keys between them.
//
// It renders the same three ways the explain package renders a plan, for
// the same reasons: a PNG or JPEG for a chat, a ticket or a slide (drawn in
// Go, picture.go), and Mermaid erDiagram source (mermaid.go) for a pull
// request or a wiki page that draws ```mermaid blocks.
//
//	db.Manager.Schema ──► *erd.Schema ──┬─► Mermaid()      text
//	(catalog queries,       (this file)  ├─► PNG(opt)       bytes
//	 one per engine)                     └─► JPEG(opt)      bytes
//	                        Around(names, depth) narrows it to one
//	                        table's neighbourhood first
//
// The package is a leaf — it knows nothing of drivers or connections. The db
// package reads each engine's catalog and builds a Schema; everything here
// works on that model alone, so it is testable without a database.
package erd

import (
	"slices"
	"sort"
	"strings"
)

// Schema is the diagram's subject: a set of tables and the foreign keys
// among them. Rels only ever point at tables in Tables — a foreign key to a
// table outside the set (another MySQL database, a schema the user cannot
// see) is dropped when the Schema is built, as the diagram has no box to
// draw it to.
type Schema struct {
	Conn   string // connection name, for the title and file names
	Driver string // engine, for the title
	Tables []*Table
	Rels   []*Rel
}

// Table is one entity: a table, or a view when asked for.
type Table struct {
	Schema string
	Name   string
	// Label is how the table is named on the diagram, and how a caller
	// names it to Around: bare when the connection has one schema,
	// schema.name when it has several — the sidebar's rule, so the name a
	// user clicks is the name that resolves.
	Label   string
	View    bool
	Cols    []*Column
	PK      []string   // primary key columns, in key order; nil when none
	Uniques [][]string // each unique key's columns, in key order
}

// Column is one column and the key roles it plays.
type Column struct {
	Name     string
	Type     string
	Nullable bool
	PK       bool // part of the primary key
	FK       bool // part of some foreign key
	Unique   bool // part of some unique key (not the primary key)
}

// Rel is one foreign key: Child's ChildCols reference Parent's ParentCols,
// pairwise in order. A self-reference (an employee's manager) has Child ==
// Parent.
type Rel struct {
	Name       string
	Child      *Table
	Parent     *Table
	ChildCols  []string
	ParentCols []string
}

// Col finds a column by name, or nil.
func (t *Table) Col(name string) *Column {
	for _, c := range t.Cols {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// Optional reports whether a child row may have no parent: any of the
// foreign key's columns is nullable. A composite key with one NULL column is
// not checked at all under the default MATCH SIMPLE, so one is enough.
// It decides the parent end's notation: zero-or-one rather than exactly one.
func (r *Rel) Optional() bool {
	for _, n := range r.ChildCols {
		if c := r.Child.Col(n); c == nil || c.Nullable {
			return true
		}
	}
	return false
}

// OneToOne reports whether at most one child row can reference a parent row:
// the foreign key's columns are exactly the child's primary key or one of
// its unique keys (as a set — key order does not matter for uniqueness). It
// decides the child end's notation: zero-or-one rather than zero-or-many.
func (r *Rel) OneToOne() bool {
	if sameSet(r.ChildCols, r.Child.PK) {
		return true
	}
	for _, u := range r.Child.Uniques {
		if sameSet(r.ChildCols, u) {
			return true
		}
	}
	return false
}

// Identifying reports whether the foreign key is part of the child's
// identity: every one of its columns is in the child's primary key (an
// order line keyed by (order_id, line_no)). Mermaid draws these solid and
// the rest dashed.
func (r *Rel) Identifying() bool {
	if len(r.Child.PK) == 0 {
		return false
	}
	for _, n := range r.ChildCols {
		if !slices.Contains(r.Child.PK, n) {
			return false
		}
	}
	return true
}

// sameSet reports whether a and b hold the same names, ignoring order. Keys
// never repeat a column, so comparing sorted copies is exact.
func sameSet(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// MarkKeys sets each column's PK, FK and Unique flags from the tables' keys
// and the relationships. The builder calls it once the Schema is complete;
// it is idempotent, so a caller that edits keys can call it again.
func (s *Schema) MarkKeys() {
	for _, t := range s.Tables {
		for _, c := range t.Cols {
			c.PK, c.FK, c.Unique = false, false, false
		}
		for _, n := range t.PK {
			if c := t.Col(n); c != nil {
				c.PK = true
				// a primary key column is NOT NULL whatever the catalog's
				// nullable column says (SQLite reports an INTEGER PRIMARY
				// KEY as nullable, since it is the rowid alias)
				c.Nullable = false
			}
		}
		for _, u := range t.Uniques {
			for _, n := range u {
				if c := t.Col(n); c != nil {
					c.Unique = true
				}
			}
		}
	}
	for _, r := range s.Rels {
		for _, n := range r.ChildCols {
			if c := r.Child.Col(n); c != nil {
				c.FK = true
			}
		}
	}
}

// Find resolves a name as a caller spells it — the table's Label, or
// schema.name, or the bare name when that is unique — to a table. An exact
// match wins over a case-insensitive one, and a name that fits several
// tables (a bare name in two schemas) is not guessed at.
func (s *Schema) Find(name string) (*Table, bool) {
	var exact, fold []*Table
	for _, t := range s.Tables {
		for _, n := range []string{t.Label, t.Schema + "." + t.Name, t.Name} {
			if n == name {
				exact = append(exact, t)
				break
			}
			if strings.EqualFold(n, name) {
				fold = append(fold, t)
				break
			}
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], true
	case len(exact) == 0 && len(fold) == 1:
		return fold[0], true
	}
	return nil, false
}

// Around narrows the schema to the named tables and every table within depth
// foreign-key hops of them, in either direction — the tables a person asking
// about orders wants to see: what orders references, and what references
// orders. depth 0 is the named tables alone; a negative depth is unlimited
// (the named tables' whole connected component).
//
// A name that does not resolve (see Find) is returned in missing rather than
// failing the whole call, so a caller can say which one was wrong.
func (s *Schema) Around(names []string, depth int) (out *Schema, missing []string) {
	// adjacency over tables, ignoring direction: an ERD neighbourhood is
	// "connected by a key", whichever side holds it
	adj := map[*Table][]*Table{}
	for _, r := range s.Rels {
		if r.Child != r.Parent {
			adj[r.Child] = append(adj[r.Child], r.Parent)
			adj[r.Parent] = append(adj[r.Parent], r.Child)
		}
	}
	// breadth-first from all the named tables at once, so a table two hops
	// from one name and one hop from another is at distance one
	dist := map[*Table]int{}
	var queue []*Table
	for _, n := range names {
		t, ok := s.Find(n)
		if !ok {
			missing = append(missing, n)
			continue
		}
		if _, seen := dist[t]; !seen {
			dist[t] = 0
			queue = append(queue, t)
		}
	}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		if depth >= 0 && dist[t] >= depth {
			continue
		}
		for _, n := range adj[t] {
			if _, seen := dist[n]; !seen {
				dist[n] = dist[t] + 1
				queue = append(queue, n)
			}
		}
	}
	return s.keep(func(t *Table) bool { _, ok := dist[t]; return ok }), missing
}

// Selection is what a caller asks a diagram to show. The zero value is every
// table (no views) on the connection.
type Selection struct {
	// Tables, when set, centres the diagram on these tables (as Find
	// resolves names) and their neighbours within Depth hops.
	Tables []string
	// Depth is Around's depth; ignored without Tables.
	Depth int
	// Views keeps the views. A view named in Tables is kept regardless —
	// asking for a diagram of a view is asking to see it.
	Views bool
	// Schema, when set and Tables is not, keeps only that schema's tables:
	// "diagram everything" from a sidebar that shows one schema at a time
	// means everything in that schema, not every table in the database.
	// With Tables it is ignored, since a neighbourhood follows its keys
	// into whatever schema they lead.
	Schema string
}

// Select narrows the schema to sel. Names that do not resolve come back in
// missing, and the rest of the selection still applies.
func (s *Schema) Select(sel Selection) (out *Schema, missing []string) {
	out = s
	named := map[*Table]bool{}
	if len(sel.Tables) > 0 {
		out, missing = s.Around(sel.Tables, sel.Depth)
		for _, n := range sel.Tables {
			if t, ok := s.Find(n); ok {
				named[t] = true
			}
		}
	}
	if len(sel.Tables) == 0 && sel.Schema != "" {
		out = out.keep(func(t *Table) bool { return t.Schema == sel.Schema })
	}
	if !sel.Views {
		out = out.keep(func(t *Table) bool { return !t.View || named[t] })
	}
	return out, missing
}

// WithoutViews drops the views. Most diagrams want only tables: a view has
// no keys, so it would be a box with no lines, and a schema with many views
// would be mostly those.
func (s *Schema) WithoutViews() *Schema {
	return s.keep(func(t *Table) bool { return !t.View })
}

// keep is the schema narrowed to the tables in, with the relationships
// between kept tables. Tables and columns are shared with s, not copied:
// a Schema is never modified once built, so sharing is safe.
func (s *Schema) keep(in func(*Table) bool) *Schema {
	out := &Schema{Conn: s.Conn, Driver: s.Driver}
	for _, t := range s.Tables {
		if in(t) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, r := range s.Rels {
		if in(r.Child) && in(r.Parent) {
			out.Rels = append(out.Rels, r)
		}
	}
	return out
}

// Sort puts the tables in Label order and the relationships in (child,
// name) order, so every rendering of the same schema is byte-for-byte the
// same whatever order the catalog returned rows in.
func (s *Schema) Sort() {
	sort.SliceStable(s.Tables, func(i, j int) bool { return s.Tables[i].Label < s.Tables[j].Label })
	sort.SliceStable(s.Rels, func(i, j int) bool {
		a, b := s.Rels[i], s.Rels[j]
		if a.Child.Label != b.Child.Label {
			return a.Child.Label < b.Child.Label
		}
		return a.Name < b.Name
	})
}
