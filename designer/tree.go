package designer

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/rohanthewiz/dbc/erd"
)

// Planning the FROM clause. The canvas is a graph — tables joined however
// the user drew them — but a FROM clause is a sequence, each JOIN able to
// name only tables already placed. So the joins are laid out as a TREE,
// breadth-first from the main table:
//
//	canvas                         tree from customers (main)
//	customers ─── orders           customers c                  root
//	    │            │               ├─ orders o        JOIN …   (via j1)
//	 addresses   order_lines         │    └─ order_lines ol JOIN … (via j3)
//	                                 └─ addresses a     LEFT JOIN … (via j2)
//
// A join between two tables already placed (a cycle: orders also joined to
// addresses as its shipping address) is not a second path into the tree;
// its condition is ANDed into the ON of whichever of the two was placed
// later (node.extra), where both are in scope.
//
// Tables no join reaches start a component of their own, CROSS JOINed —
// every row with every row, which the cartesian rule warns about.
//
// A table read in a subquery (exists, notexists, summary) is a leaf placed
// under the table it is tested against, but written into WHERE (or as a
// derived table) rather than as a JOIN; it must be joined to nothing else,
// since its subquery can see only itself and its parent.

// node is a canvas table as the analysis sees it.
type node struct {
	t     *Table
	et    *erd.Table // nil when the catalog does not have it
	alias string
	pos   int // index in Design.Tables

	parent   *node
	via      *Join // the join that placed it under parent; nil for a root
	fromLeft bool  // parent is via.Left (so this node is via.Right)
	kids     []*node
	extra    []*Join // cycle joins whose conditions go into this node's ON
	comp     int     // its component: 0 is the main table's
	order    int     // placement order, which is the FROM order
	placed   bool

	// optional: an outer join may leave this table's columns NULL in a
	// result row (it is on the non-kept side of a left join, or under a
	// full join, or below such a table). A WHERE filter on it then undoes
	// the outer join (ruleLeftFilter).
	optional bool
}

// role says how the node enters the statement.
func (n *node) role() string {
	switch {
	case n.parent == nil && n.comp == 0:
		return "main"
	case n.parent == nil:
		return "cross"
	case n.via.Kind.tests():
		return string(n.via.Kind)
	case n.via.Kind == JoinSummary:
		return "summary"
	}
	return "joined"
}

// inSubquery reports whether the node is read in a subquery of its own.
func (n *node) inSubquery() bool { return n.via != nil && n.via.Kind.subquery() }

// sideCols are the columns of a join on one side: Left's or Right's.
func sideCols(j *Join, left bool) []string {
	out := make([]string, 0, len(j.On))
	for _, p := range j.On {
		if left {
			out = append(out, p.L)
		} else {
			out = append(out, p.R)
		}
	}
	return out
}

// tree is the laid-out design.
type tree struct {
	d     *Design
	cat   *Catalog
	nodes map[string]*node // by table ID
	order []*node          // FROM order
	// dangling are joins naming a table that is not on the canvas, or
	// joining a table to itself under one ID; they are left out of the
	// SQL and reported (ruleBroken).
	dangling []*Join
	// misplaced are subquery joins whose tested table could not be placed
	// as a leaf under its Left: it is the main table, or reached first
	// through another join (ruleSubqueryLeaf).
	misplaced []*Join
	degree    map[string]int // joins touching each table ID (dangling excluded)
	// rightJoins are left joins read from their kept side's far end, so
	// written as RIGHT JOIN (ruleRightJoin suggests another main table).
	rightJoins []*Join
}

// normalize fills in what the page may leave out or get wrong: a table or
// join without a unique ID gets one, a table without a usable alias gets
// one, Main is pointed at a table that exists. It edits d in place and is
// idempotent, so the design Analyze returns can be saved and sent back.
func normalize(d *Design) {
	seen := map[string]bool{}
	for i := range d.Tables {
		t := &d.Tables[i]
		if t.ID == "" || seen[t.ID] {
			t.ID = freshID("t", seen)
		}
		seen[t.ID] = true
	}
	jseen := map[string]bool{}
	for i := range d.Joins {
		j := &d.Joins[i]
		if j.ID == "" || jseen[j.ID] {
			j.ID = freshID("j", jseen)
		}
		jseen[j.ID] = true
		if j.Kind == "" {
			j.Kind = JoinInner
		}
	}
	// aliases: keep each valid one the first time it is seen (case
	// blind — MySQL compares aliases that way, and two that differ only
	// by case would confuse a reader anyway), then name the rest
	taken := map[string]bool{}
	var unnamed []*Table
	for i := range d.Tables {
		t := &d.Tables[i]
		if validAlias(t.Alias) && !taken[strings.ToLower(t.Alias)] {
			taken[strings.ToLower(t.Alias)] = true
		} else {
			unnamed = append(unnamed, t)
		}
	}
	for _, t := range unnamed {
		t.Alias = makeAlias(t.Name, taken)
		taken[strings.ToLower(t.Alias)] = true
	}
	if d.Main != "" && d.table(d.Main) == nil {
		d.Main = ""
	}
	if d.Limit < 0 {
		d.Limit = 0
	}
	if d.Offset < 0 {
		d.Offset = 0
	}
}

// freshID is prefix followed by the smallest number not in seen.
func freshID(prefix string, seen map[string]bool) string {
	for n := 1; ; n++ {
		if id := prefix + strconv.Itoa(n); !seen[id] {
			return id
		}
	}
}

// validAlias reports whether a is a plain identifier that needs no quotes
// anywhere: ASCII letters, digits and _, not first a digit, not reserved.
func validAlias(a string) bool {
	if a == "" || len(a) > 30 || reserved[strings.ToLower(a)] {
		return false
	}
	for i, r := range a {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// makeAlias names a table by its words' initials — order_lines is ol,
// CustomerAddress is ca — numbered when that is taken (c, c2, c3). An
// initialism that is a SQL word (orders_new would be "on") is numbered too,
// rather than quoted, since an alias is typed and read far more often than
// a table's name.
func makeAlias(name string, taken map[string]bool) string {
	var b strings.Builder
	prev := rune(0)
	for i, r := range name {
		start := i == 0 || prev == '_' || prev == ' ' || prev == '.' ||
			(unicode.IsUpper(r) && unicode.IsLower(prev))
		if start && r < unicode.MaxASCII && (unicode.IsLetter(r)) {
			b.WriteRune(unicode.ToLower(r))
		}
		prev = r
	}
	base := b.String()
	if base == "" {
		base = "t"
	}
	if len(base) > 4 {
		base = base[:4]
	}
	if !taken[base] && !reserved[base] {
		return base
	}
	for n := 2; ; n++ {
		if a := base + strconv.Itoa(n); !taken[a] {
			return a
		}
	}
}

// plan lays out the (normalized) design as a tree. It never fails: what
// cannot be placed properly is recorded for the rules to report, and the
// rest is still laid out, so the user sees as much of the statement as can
// be written.
func plan(d *Design, cat *Catalog) *tree {
	tr := &tree{d: d, cat: cat, nodes: map[string]*node{}, degree: map[string]int{}}
	for i := range d.Tables {
		t := &d.Tables[i]
		tr.nodes[t.ID] = &node{t: t, et: cat.erdTable(t), alias: t.Alias, pos: i}
	}
	// adjacency, in the joins' order so the layout is stable
	adj := map[string][]*Join{}
	for i := range d.Joins {
		j := &d.Joins[i]
		if tr.nodes[j.Left] == nil || tr.nodes[j.Right] == nil || j.Left == j.Right {
			tr.dangling = append(tr.dangling, j)
			continue
		}
		adj[j.Left] = append(adj[j.Left], j)
		adj[j.Right] = append(adj[j.Right], j)
		tr.degree[j.Left]++
		tr.degree[j.Right]++
	}
	if len(d.Tables) == 0 {
		return tr
	}

	main := d.Main
	if main == "" {
		main = d.Tables[0].ID
	}
	used := map[*Join]bool{}
	place := func(n *node, comp int) {
		n.comp, n.placed = comp, true
		n.order = len(tr.order)
		tr.order = append(tr.order, n)
	}
	// bfs grows one component from its root
	bfs := func(root *node, comp int) {
		place(root, comp)
		queue := []*node{root}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			for _, j := range adj[x.t.ID] {
				if used[j] {
					continue
				}
				used[j] = true
				fromLeft := j.Left == x.t.ID
				otherID := j.Right
				if !fromLeft {
					otherID = j.Left
				}
				y := tr.nodes[otherID]
				if y.placed {
					// already placed: a cycle
					later := x
					if y.order > x.order {
						later = y
					}
					later.extra = append(later.extra, j)
					continue
				}
				if j.Kind.subquery() && !fromLeft {
					// the tested side reached first: it was the main table
					// or got here through another join
					tr.misplaced = append(tr.misplaced, j)
				}
				if j.Kind == JoinLeft && !fromLeft {
					tr.rightJoins = append(tr.rightJoins, j)
				}
				y.parent, y.via, y.fromLeft = x, j, fromLeft
				y.optional = x.optional || j.Kind == JoinFull || (j.Kind == JoinLeft && fromLeft)
				x.kids = append(x.kids, y)
				place(y, comp)
				queue = append(queue, y)
			}
		}
	}
	bfs(tr.nodes[main], 0)
	comp := 0
	for i := range d.Tables {
		if n := tr.nodes[d.Tables[i].ID]; !n.placed {
			comp++
			bfs(n, comp)
		}
	}
	return tr
}

// components is how many disconnected groups of tables the canvas holds.
func (tr *tree) components() int {
	max := 0
	for _, n := range tr.order {
		if n.comp > max {
			max = n.comp
		}
	}
	return max + 1
}

// path returns the nodes from the root of n's component down to n.
func (n *node) path() []*node {
	var out []*node
	for x := n; x != nil; x = x.parent {
		out = append([]*node{x}, out...)
	}
	return out
}

// under reports whether n is in the subtree of a (a itself included).
func (n *node) under(a *node) bool {
	for x := n; x != nil; x = x.parent {
		if x == a {
			return true
		}
	}
	return false
}
