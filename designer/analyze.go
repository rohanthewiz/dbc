package designer

import (
	"slices"
)

// Analysis is everything the page draws from one design: the statement, the
// findings, the story, and per table and per join what the cards and edges
// show. It is recomputed whole on every edit — a design is a few tables,
// and the work is microseconds next to the request carrying it.
type Analysis struct {
	// Design is the design as analyzed: normalized (every table aliased,
	// every ID unique). The page keeps this one, so what it saves is what
	// the SQL was written from.
	Design  Design      `json:"design"`
	SQL     string      `json:"sql"`
	Valid   bool        `json:"valid"` // there is a statement, and no error: it may be run
	Grouped bool        `json:"grouped"`
	Names   []string    `json:"names"` // the result's column names, in order
	Lints   []Lint      `json:"lints"`
	Story   []string    `json:"story"`
	Tables  []TableView `json:"tables"`
	Joins   []JoinView  `json:"joins"`
	Related []Related   `json:"related"`
	Dialect string      `json:"dialect"`
}

// TableView is one card: the table, its role, its size, its columns with
// their key roles, and whether a result row is one of its rows.
type TableView struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Alias     string    `json:"alias"`
	Role      string    `json:"role"` // main, joined, cross, exists, notexists, summary
	View      bool      `json:"view,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
	Rows      int64     `json:"rows"`
	RowsKnown bool      `json:"rowsKnown"`
	Grain     bool      `json:"grain,omitempty"`    // no join repeats its rows
	Repeated  bool      `json:"repeated,omitempty"` // a join repeats its rows (certainly)
	Optional  bool      `json:"optional,omitempty"` // an outer join may leave it empty
	Cols      []ColView `json:"cols"`
	Indexes   []Index   `json:"indexes,omitempty"`
}

// ColView is one column row of a card.
type ColView struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Fam      string `json:"fam"`
	Nullable bool   `json:"nullable,omitempty"`
	PK       bool   `json:"pk,omitempty"`
	FK       bool   `json:"fk,omitempty"`
	Unique   bool   `json:"unique,omitempty"`
	// Indexed: some index starts with it, so a filter on it alone can be
	// served by an index. Only when indexes are known; keys always count.
	Indexed bool `json:"indexed,omitempty"`
	// NoPrefix and NoRange narrow Indexed when the indexes are listed:
	// none of those indexes can serve a starts-with on it (NoPrefix: a
	// text column on Postgres whose b-trees keep a linguistic collation's
	// order), or a range (NoRange: each is a pattern class, or hash).
	// Catalog.finds is the test; the page words the ⚡'s title from them.
	NoPrefix bool `json:"noPrefix,omitempty"`
	NoRange  bool `json:"noRange,omitempty"`
}

// JoinView is one edge: its shape and what it does to the rows, in words,
// and the foreign keys it could follow instead.
type JoinView struct {
	ID      string   `json:"id"`
	Card    string   `json:"card"`              // 1:1, N:1, 1:N, N:N, ?:?
	Keyword string   `json:"keyword,omitempty"` // how it is written: JOIN, LEFT JOIN, EXISTS…
	Fans    bool     `json:"fans,omitempty"`    // read from the query's start, it repeats rows
	Text    string   `json:"text"`
	Choices []Choice `json:"choices,omitempty"`
}

// Choice is a foreign key a join could follow.
type Choice struct {
	FK    string `json:"fk"`
	Label string `json:"label"`
	On    []Pair `json:"on"`
}

// Analyze builds and judges a design against a catalog.
func Analyze(d Design, cat *Catalog) *Analysis {
	if cat == nil {
		cat = &Catalog{}
	}
	d = d.clone()
	normalize(&d)
	tr := plan(&d, cat)
	g := &gen{d: &d, tr: tr, dia: dialectOf(cat.Driver)}
	g.grouped = isGrouped(&d, tr)
	a := &Analysis{Design: d, Grouped: g.grouped, Dialect: g.dia.name}
	if len(d.Tables) > 0 {
		a.SQL = g.write()
	}
	a.Names = g.names
	a.Lints = (&linter{d: &d, tr: tr, g: g, cat: cat}).lint()
	a.Story = (&teller{d: &d, tr: tr, g: g, cat: cat}).tell()
	a.Valid = len(d.Tables) > 0 && !slices.ContainsFunc(a.Lints, func(l Lint) bool { return l.Sev == SevError })
	a.Related = related(tr, cat, 16)
	if a.Related == nil {
		a.Related = []Related{}
	}

	for i := range d.Tables {
		n := tr.nodes[d.Tables[i].ID]
		a.Tables = append(a.Tables, tableView(tr, cat, n))
	}
	for i := range d.Joins {
		a.Joins = append(a.Joins, joinView(tr, cat, &d.Joins[i]))
	}
	return a
}

func tableView(tr *tree, cat *Catalog, n *node) TableView {
	v := TableView{ID: n.t.ID, Label: n.t.Name, Alias: n.alias, Role: n.role(), Missing: n.et == nil,
		Optional: n.optional, Cols: []ColView{}}
	if n.et == nil {
		return v
	}
	v.Label, v.View = n.et.Label, n.et.View
	v.Rows, v.RowsKnown = cat.rows(n.et)
	if r := v.Role; r == "main" || r == "joined" || r == "cross" {
		ms := tr.multipliers(n)
		v.Grain = len(ms) == 0 && len(tr.order) > 1 && n.comp == 0
		v.Repeated = slices.ContainsFunc(ms, func(m mult) bool { return m.known })
	}
	v.Indexes = cat.indexes(n.et)
	for _, c := range n.et.Cols {
		cv := ColView{Name: c.Name, Type: c.Type, Fam: string(familyOf(c.Type)), Nullable: c.Nullable,
			PK: c.PK, FK: c.FK, Unique: c.Unique}
		if cat.IndexesKnown || c.PK || c.Unique {
			_, cv.Indexed = cat.leads(n.et, c.Name)
		}
		// only a listed index's class and collation are known: a key
		// assumed indexed (indexes unknown) is not narrowed by a guess
		if cv.Indexed && cat.IndexesKnown {
			cv.NoRange = !cat.finds(n.et, c.Name, need{rng: true})
			cv.NoPrefix = familyOf(c.Type) == famText && !cat.finds(n.et, c.Name, need{prefix: true})
		}
		v.Cols = append(v.Cols, cv)
	}
	return v
}

func joinView(tr *tree, cat *Catalog, j *Join) JoinView {
	v := JoinView{ID: j.ID, Card: tr.cardinality(j)}
	a, b := tr.nodes[j.Left], tr.nodes[j.Right]
	if a == nil || b == nil {
		v.Text = "one end is missing"
		return v
	}
	for _, r := range relsBetween(cat, a, b) {
		v.Choices = append(v.Choices, Choice{FK: r.name, Label: r.label, On: r.pairs})
	}
	// the tree edge this join is, if it is one (a cycle join is not)
	var c *node
	for _, x := range []*node{a, b} {
		if x.via == j {
			c = x
		}
	}
	switch {
	case c == nil:
		v.Keyword = "AND"
		v.Text = "also matched (it closes a loop of joins)"
	case j.Kind.tests():
		v.Keyword = "EXISTS"
		if j.Kind == JoinNotExists {
			v.Keyword = "NOT EXISTS"
		}
		v.Text = kindWords[j.Kind] + ": " + c.parent.alias + " rows are tested, never repeated"
	case j.Kind == JoinSummary:
		v.Keyword = "LEFT JOIN (…)"
		v.Text = c.alias + " is summarized per " + c.parent.alias + ": one row each"
	default:
		v.Keyword = joinKeyword(c)
		fans, known := tr.fansOut(c)
		v.Fans = fans && known
		switch {
		case !fans:
			v.Text = "at most one " + c.alias + " per " + c.parent.alias
		case known:
			v.Text = "many " + c.alias + " per " + c.parent.alias + ": " + c.parent.alias + " rows repeat"
		default:
			v.Text = c.alias + " has no keys: rows may repeat"
		}
	}
	return v
}
