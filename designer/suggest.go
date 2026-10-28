package designer

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/erd"
)

// Suggestions: the designer knowing the schema so the user does not have
// to. A table dropped on the canvas is joined to what is there by its
// foreign key (AddTable); two tables nothing connects are offered the
// shortest path of foreign keys between them, bridge tables and all
// (connectFix); the palette lists the tables one key away (Related).

// rel is a foreign key between two canvas nodes a and b, its pairs oriented
// a→b (L is a's column), as a Join from a to b would hold them.
type rel struct {
	name     string
	pairs    []Pair
	label    string // "o.customer_id = c.id"
	aChild   bool   // a holds the key's columns (a's rows each reference one b)
	optional bool   // the key's columns may be NULL
}

// relsBetween lists the foreign keys between a and b, a's own (a → b) first.
// A self-reference between two copies of one table counts both ways: a
// row's manager (a.manager_id = b.id) and its reports (b.manager_id = a.id).
func relsBetween(cat *Catalog, a, b *node) []rel {
	if cat == nil || cat.Schema == nil || a.et == nil || b.et == nil {
		return nil
	}
	var mine, theirs []rel
	for _, r := range cat.Schema.Rels {
		if r.Child == a.et && r.Parent == b.et {
			mine = append(mine, makeRel(r, a, b, true))
		}
		if r.Child == b.et && r.Parent == a.et {
			theirs = append(theirs, makeRel(r, a, b, false))
		}
	}
	return append(mine, theirs...)
}

func makeRel(r *erd.Rel, a, b *node, aChild bool) rel {
	x := rel{name: r.Name, aChild: aChild, optional: r.Optional()}
	var conds []string
	for i := range r.ChildCols {
		p := Pair{L: r.ParentCols[i], R: r.ChildCols[i]}
		if aChild {
			p = Pair{L: r.ChildCols[i], R: r.ParentCols[i]}
		}
		x.pairs = append(x.pairs, p)
		conds = append(conds, a.alias+"."+p.L+" = "+b.alias+"."+p.R)
	}
	x.label = strings.Join(conds, " AND ")
	return x
}

// defaultKind is the join a foreign key suggests. Following an optional
// key from the child (an order's coupon, which most orders lack) is a LEFT
// join: an inner one would silently drop every child without one, and
// "orders with their coupon" means all the orders. Everything else is an
// inner join, which the story then explains ("customers with none are
// left out").
func defaultKind(r rel) JoinKind {
	if r.aChild && r.optional {
		return JoinLeft
	}
	return JoinInner
}

// ErrNoSuchTable is AddTable's answer for a table the catalog lacks.
var ErrNoSuchTable = errors.New("no such table on this connection")

// AddTable puts a table on the canvas at (x, y) and joins it to what is
// there, returning the new design and a line for each join made (or not).
//
// It joins the new table to EACH component of the canvas once at most —
// the first table of the component (in FROM order) that a foreign key
// links it to. Joining it to two tables of one component would close a
// cycle, and a cycle of foreign keys is a constraint the user did not ask
// for (an order's shipping address being its customer's home address);
// joining it to two components bridges them, which is exactly what adding
// order_lines between orders and products should do.
//
// No foreign key: a guess by name (orders.customer_id to customers.id),
// marked as one. Nothing at all: the table stays unjoined, and the
// cartesian rule offers a path.
func AddTable(d Design, cat *Catalog, schema, name string, x, y float64) (Design, []string, error) {
	if cat == nil {
		cat = &Catalog{}
	}
	d = d.clone()
	normalize(&d)
	probe := &Table{Schema: schema, Name: name}
	et := cat.erdTable(probe)
	if et == nil {
		return d, nil, ErrNoSuchTable
	}
	before := plan(&d, cat)

	ids := map[string]bool{}
	for _, t := range d.Tables {
		ids[t.ID] = true
	}
	t := Table{ID: freshID("t", ids), Schema: et.Schema, Name: et.Name, X: x, Y: y}
	taken := map[string]bool{}
	for _, o := range d.Tables {
		taken[strings.ToLower(o.Alias)] = true
	}
	t.Alias = makeAlias(et.Name, taken)
	d.Tables = append(d.Tables, t)
	nn := &node{t: &d.Tables[len(d.Tables)-1], et: et, alias: t.Alias}

	var notes []string
	jids := map[string]bool{}
	for _, j := range d.Joins {
		jids[j.ID] = true
	}
	for comp := 0; comp < before.components(); comp++ {
		var made bool
		for _, n := range before.order {
			if n.comp != comp || n.inSubquery() {
				continue
			}
			rels := relsBetween(cat, n, nn)
			if len(rels) == 0 {
				continue
			}
			r := rels[0]
			j := Join{ID: freshID("j", jids), Left: n.t.ID, Right: t.ID, Kind: defaultKind(r), On: r.pairs, FK: r.name}
			jids[j.ID] = true
			d.Joins = append(d.Joins, j)
			note := "joined " + t.Alias + " to " + n.alias + " on " + r.label + " (" + r.name + ")"
			if j.Kind == JoinLeft {
				note += " — a left join: " + n.alias + "'s key may be empty, and an inner join would drop those rows"
			}
			if len(rels) > 1 {
				note += "; " + strconv.Itoa(len(rels)) + " foreign keys link them — click the join to pick another"
			}
			notes = append(notes, note)
			made = true
			break
		}
		if made {
			continue
		}
		for _, n := range before.order {
			if n.comp != comp || n.inSubquery() {
				continue
			}
			if p, ok := guessPairs(n, nn); ok {
				j := Join{ID: freshID("j", jids), Left: n.t.ID, Right: t.ID, Kind: JoinInner, On: []Pair{p}, Guess: true}
				jids[j.ID] = true
				d.Joins = append(d.Joins, j)
				notes = append(notes, "joined "+t.Alias+" to "+n.alias+" on "+n.alias+"."+p.L+" = "+t.Alias+"."+p.R+
					" — guessed from the names: no foreign key links them")
				break
			}
		}
	}
	if len(d.Tables) > 1 && len(notes) == 0 {
		notes = append(notes, "no foreign key links "+et.Name+" to the other tables — drag a column onto another table's to join them")
	}
	if len(d.Tables) == 1 {
		d.Main = t.ID
	}
	return d, notes, nil
}

// guessPairs matches a and b by the naming convention most schemas follow:
// one side's <singular of the other's name>_id against the other's
// single-column primary key. A prefixed name (app_users, qd_orders) is
// tried by its last word too, which is what its keys are usually named
// after (user_id, order_id).
func guessPairs(a, b *node) (Pair, bool) {
	if a.et == nil || b.et == nil {
		return Pair{}, false
	}
	try := func(child, parent *node) (string, string, bool) {
		if len(parent.et.PK) != 1 {
			return "", "", false
		}
		name, pk := strings.ToLower(parent.et.Name), strings.ToLower(parent.et.PK[0])
		want := []string{singular(name) + "_" + pk}
		if i := strings.LastIndexByte(name, '_'); i > 0 && i < len(name)-1 {
			want = append(want, singular(name[i+1:])+"_"+pk)
		}
		for _, c := range child.et.Cols {
			if slices.Contains(want, strings.ToLower(c.Name)) {
				return c.Name, parent.et.PK[0], true
			}
		}
		return "", "", false
	}
	if c, p, ok := try(a, b); ok {
		return Pair{L: c, R: p}, true
	}
	if c, p, ok := try(b, a); ok {
		return Pair{L: p, R: c}, true
	}
	return Pair{}, false
}

// singular undoes the common English plurals table names use.
func singular(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return strings.TrimSuffix(s, "ies") + "y"
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "ches"):
		return strings.TrimSuffix(s, "es")
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// connectFix finds the shortest path of foreign keys from n's component to
// the main table's, through at most three bridge tables, and returns the
// design with the path's tables and joins added. Breadth-first over the
// whole schema's keys, from every table of n's component at once.
func connectFix(d *Design, cat *Catalog, tr *tree, n *node) (*Design, string) {
	if cat == nil || cat.Schema == nil {
		return nil, ""
	}
	from := map[*erd.Table]*node{}
	to := map[*erd.Table]*node{}
	for _, x := range tr.order {
		if x.et == nil || x.inSubquery() {
			continue
		}
		if x.comp == n.comp && from[x.et] == nil {
			from[x.et] = x
		}
		if x.comp == 0 && to[x.et] == nil {
			to[x.et] = x
		}
	}
	adj := map[*erd.Table][]*erd.Rel{}
	for _, r := range cat.Schema.Rels {
		adj[r.Child] = append(adj[r.Child], r)
		if r.Parent != r.Child {
			adj[r.Parent] = append(adj[r.Parent], r)
		}
	}
	type step struct {
		prev *erd.Table
		via  *erd.Rel
	}
	seen := map[*erd.Table]step{}
	var queue []*erd.Table
	for et := range from {
		seen[et] = step{}
		queue = append(queue, et)
	}
	slices.SortFunc(queue, func(a, b *erd.Table) int { return from[a].order - from[b].order })
	var end *erd.Table
	for depth := 0; depth < 4 && len(queue) > 0 && end == nil; depth++ {
		var next []*erd.Table
		for _, et := range queue {
			for _, r := range adj[et] {
				other := r.Parent
				if other == et {
					other = r.Child
				}
				if _, ok := seen[other]; ok {
					continue
				}
				seen[other] = step{prev: et, via: r}
				if to[other] != nil {
					end = other
					break
				}
				next = append(next, other)
			}
			if end != nil {
				break
			}
		}
		queue = next
	}
	if end == nil {
		return nil, ""
	}
	// walk back: end (in the main component) … start (in n's)
	var chain []*erd.Table
	var rels []*erd.Rel
	for et := end; ; {
		chain = append(chain, et)
		s := seen[et]
		if s.prev == nil {
			break
		}
		rels = append(rels, s.via)
		et = s.prev
	}
	// chain[0] is in the main component, chain[len-1] in n's; the tables
	// between are the bridges, laid out evenly between the two ends
	c := d.clone()
	ids, jids, taken := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, t := range c.Tables {
		ids[t.ID], taken[strings.ToLower(t.Alias)] = true, true
	}
	for _, j := range c.Joins {
		jids[j.ID] = true
	}
	a, b := to[chain[0]], from[chain[len(chain)-1]]
	nodes := make([]*node, len(chain))
	nodes[0], nodes[len(chain)-1] = a, b
	var bridges []string
	for i := 1; i < len(chain)-1; i++ {
		f := float64(i) / float64(len(chain)-1)
		t := Table{ID: freshID("t", ids), Schema: chain[i].Schema, Name: chain[i].Name,
			X: a.t.X + (b.t.X-a.t.X)*f, Y: a.t.Y + (b.t.Y-a.t.Y)*f + 40}
		t.Alias = makeAlias(t.Name, taken)
		ids[t.ID], taken[strings.ToLower(t.Alias)] = true, true
		c.Tables = append(c.Tables, t)
		nodes[i] = &node{t: &c.Tables[len(c.Tables)-1], et: chain[i], alias: t.Alias}
		bridges = append(bridges, chain[i].Name)
	}
	for i := 0; i+1 < len(chain); i++ {
		x, y := nodes[i], nodes[i+1]
		r := makeRel(rels[i], x, y, rels[i].Child == x.et && rels[i].Parent == y.et)
		j := Join{ID: freshID("j", jids), Left: x.t.ID, Right: y.t.ID, Kind: defaultKind(r), On: r.pairs, FK: r.name}
		jids[j.ID] = true
		c.Joins = append(c.Joins, j)
	}
	if len(bridges) == 0 {
		return &c, "Join " + b.alias + " to " + a.alias
	}
	return &c, "Join through " + strings.Join(bridges, ", ")
}

// Related is a table one foreign key away from a table of the canvas, for
// the palette's "related" list: a click adds it, joined.
type Related struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Label  string `json:"label"`
	From   string `json:"from"` // the canvas table's ID
	Via    string `json:"via"`  // "orders.customer_id → customers.id"
	Many   bool   `json:"many"` // the related table is the many side
}

// related lists them: each canvas table's parents and children not already
// joined to it, the canvas's own order, at most max.
func related(tr *tree, cat *Catalog, max int) []Related {
	if cat == nil || cat.Schema == nil {
		return nil
	}
	var out []Related
	seen := map[string]bool{}
	for _, n := range tr.order {
		if n.et == nil || n.inSubquery() {
			continue
		}
		for _, r := range cat.Schema.Rels {
			var other *erd.Table
			var many bool
			switch {
			case r.Child == n.et:
				other = r.Parent
			case r.Parent == n.et:
				other, many = r.Child, true
			default:
				continue
			}
			if joinedTo(tr, n, other) {
				continue
			}
			key := n.t.ID + "|" + other.Schema + "." + other.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Related{Schema: other.Schema, Name: other.Name, Label: other.Label, From: n.t.ID,
				Via:  r.Child.Name + "." + strings.Join(r.ChildCols, ",") + " → " + r.Parent.Name + "." + strings.Join(r.ParentCols, ","),
				Many: many})
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}

// joinedTo reports whether n is already joined to a copy of table et.
func joinedTo(tr *tree, n *node, et *erd.Table) bool {
	for _, j := range tr.d.Joins {
		var other string
		switch n.t.ID {
		case j.Left:
			other = j.Right
		case j.Right:
			other = j.Left
		default:
			continue
		}
		if o := tr.nodes[other]; o != nil && o.et == et {
			return true
		}
	}
	return false
}
