package explain

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// ParseSQLite reads EXPLAIN QUERY PLAN: rows of (id, parent, notused,
// detail), where parent names the row a row hangs under (0 = the top). It is
// the sparest of the formats — no costs, no row counts, only what SQLite
// will do:
//
//	SEARCH o USING INDEX orders_status (status=?)
//	SEARCH u USING INTEGER PRIMARY KEY (rowid=?)
//	USE TEMP B-TREE FOR GROUP BY
//	USE TEMP B-TREE FOR ORDER BY
//
// Two rows side by side that each read a table are a nested-loop join — the
// first is the outer loop, each later one runs once per row of those before
// it — but SQLite lists them as flat siblings. They are gathered under a
// "Nested loop" step here, because "these two are one join, outer first" is
// exactly what a reader of the flat list has to know and is easy to miss.
// Nothing else is restructured: the temp B-trees stay where SQLite put them.
func ParseSQLite(cols []string, rows [][]string) (*Plan, error) {
	ci := map[string]int{}
	for i, c := range cols {
		ci[strings.ToLower(c)] = i
	}
	iid, ok1 := ci["id"]
	ipar, ok2 := ci["parent"]
	idet, ok3 := ci["detail"]
	if !ok1 || !ok2 || !ok3 {
		return nil, serr.New("not an EXPLAIN QUERY PLAN result", "columns", strings.Join(cols, ","))
	}
	root := &Node{Op: "Query", Kind: KindResult}
	byID := map[int]*Node{0: root}
	depth := map[int]int{0: 0} // for indenting the Raw copy as the tree nests
	var raw strings.Builder
	for _, r := range rows {
		if len(r) <= max(iid, ipar, idet) {
			continue
		}
		id, _ := strconv.Atoi(strings.TrimSpace(r[iid]))
		par, _ := strconv.Atoi(strings.TrimSpace(r[ipar]))
		n := parseSQLiteDetail(r[idet])
		byID[id] = n
		parent := byID[par]
		if parent == nil {
			parent, par = root, 0
		}
		depth[id] = depth[par] + 1
		parent.Children = append(parent.Children, n)
		if raw.Len() > 0 {
			raw.WriteByte('\n')
		}
		raw.WriteString(strings.Repeat("   ", depth[id]-1) + r[idet])
	}
	if len(root.Children) == 0 {
		return nil, serr.New("empty EXPLAIN output")
	}
	groupLoops(root)
	p := &Plan{Engine: SQLite, Root: root, Raw: raw.String()}
	if len(root.Children) == 1 && root.Children[0].Kind != KindScan && root.Children[0].Kind != KindIndex {
		p.Root = root.Children[0]
	}
	p.Finalize()
	return p, nil
}

var (
	// "SEARCH t USING INDEX i (a=? AND b>?)", "SCAN t USING COVERING INDEX i",
	// "SEARCH t USING INTEGER PRIMARY KEY (rowid=?)", "SCAN t"
	sqAccessRe = regexp.MustCompile(`^(SCAN|SEARCH)\s+(\S+)(?:\s+AS\s+(\S+))?(?:\s+USING\s+(.*?))?(?:\s+\((.*)\))?$`)
)

// parseSQLiteDetail turns one detail line into a step.
func parseSQLiteDetail(detail string) *Node {
	d := strings.TrimSpace(detail)
	n := &Node{Op: d}
	if m := sqAccessRe.FindStringSubmatch(d); m != nil {
		n.Op = m[1]
		n.Alias = m[2]
		if m[3] != "" {
			n.Relation, n.Alias = m[2], m[3]
		}
		using := m[4]
		n.addProp("Using", using)
		n.addProp("Constraint", m[5])
		switch {
		case using == "" && m[1] == "SCAN":
			n.Op, n.Kind = "SCAN", KindScan
		case strings.Contains(using, "AUTOMATIC"):
			n.Kind = KindIndex
			n.Index = "automatic index"
			n.flag("auto-index")
		case strings.Contains(using, "PRIMARY KEY"):
			n.Kind = KindIndex
			n.Index = "primary key"
		case strings.Contains(using, "INDEX"):
			n.Kind = KindIndex
			f := strings.Fields(using)
			n.Index = f[len(f)-1]
			if strings.Contains(using, "COVERING") {
				n.addProp("Covering", "yes — reads only the index, never the table")
			}
		default:
			n.Kind = KindIndex
		}
		if m[1] == "SCAN" && n.Kind == KindIndex {
			// a SCAN through an index still visits every entry: it is a
			// full pass in index order, not a lookup
			n.Op = "SCAN (index order)"
		}
		return n
	}
	up := strings.ToUpper(d)
	switch {
	case strings.HasPrefix(up, "USE TEMP B-TREE FOR"):
		what := strings.TrimSpace(d[len("USE TEMP B-TREE FOR"):])
		n.Op = "Temp B-tree for " + strings.ToLower(what)
		n.flag("temp-btree")
		n.addProp("Purpose", what)
		if strings.Contains(up, "ORDER BY") {
			n.Kind = KindSort
		} else {
			n.Kind = KindAgg
		}
	case strings.HasPrefix(up, "CORRELATED"):
		n.Kind = KindSubquery
		n.flag("correlated")
	case strings.Contains(up, "SUBQUERY"), strings.HasPrefix(up, "MATERIALIZE"), strings.HasPrefix(up, "CO-ROUTINE"):
		n.Kind = KindSubquery
	case strings.HasPrefix(up, "COMPOUND"), strings.Contains(up, "UNION"), strings.HasPrefix(up, "LEFT-MOST"),
		up == "LEFT", up == "RIGHT", strings.HasPrefix(up, "MERGE"), strings.HasPrefix(up, "EXCEPT"),
		strings.HasPrefix(up, "INTERSECT"):
		n.Kind = KindSet
	case strings.HasPrefix(up, "BLOOM FILTER"):
		n.Kind = KindFilter
	case strings.HasPrefix(up, "MULTI-INDEX OR"):
		n.Kind = KindIndex
	case strings.HasPrefix(up, "SCAN CONSTANT ROW"):
		n.Kind = KindResult
	}
	return n
}

// groupLoops gathers each run of two or more sibling table accesses under a
// "Nested loop" step (see ParseSQLite), recursively.
func groupLoops(n *Node) {
	for _, c := range n.Children {
		groupLoops(c)
	}
	var access []int
	for i, c := range n.Children {
		if (c.Op == "SCAN" || c.Op == "SEARCH" || strings.HasPrefix(c.Op, "SCAN (")) && len(c.Children) == 0 {
			access = append(access, i)
		}
	}
	if len(access) < 2 {
		return
	}
	loop := &Node{Op: "Nested loop", Kind: KindJoin}
	loop.addProp("Order", "outer → inner: each table below the first is searched once per row of those above it")
	in := map[int]bool{}
	for _, i := range access {
		loop.Children = append(loop.Children, n.Children[i])
		in[i] = true
	}
	kids := make([]*Node, 0, len(n.Children)-len(access)+1)
	for i, c := range n.Children {
		switch {
		case i == access[0]:
			kids = append(kids, loop)
		case !in[i]:
			kids = append(kids, c)
		}
	}
	n.Children = kids
}
