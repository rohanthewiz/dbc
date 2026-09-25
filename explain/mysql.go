package explain

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// ParseMySQLTree reads MySQL's tree format: EXPLAIN FORMAT=TREE (8.0.16+)
// and EXPLAIN ANALYZE (8.0.18+), which is the same tree with measurements.
// Unlike Postgres, MySQL returns the whole tree as ONE value with embedded
// newlines, and a statement with a dependent subquery gets a second tree at
// the top level:
//
//	-> Filter: (u.id < 50)  (cost=10.1 rows=49) (actual time=0.03..0.2 rows=49 loops=1)
//	    -> Index range scan on u using PRIMARY over (id < 50)  (cost=10.1 rows=49) …
//	-> Select #2 (subquery in projection; dependent)
//	    -> Aggregate: max(o.total)  (cost=4148 rows=1) (actual time=29.8..29.8 rows=1 loops=49)
//
// Nesting is four columns per level. Several top-level trees are gathered
// under a synthetic "Query" root, so every view still has one tree to draw.
func ParseMySQLTree(text string) (*Plan, error) {
	type open struct {
		n   *Node
		ind int
	}
	var tops []*Node
	var stack []open
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		body := strings.TrimSpace(line)
		if body == "" {
			continue
		}
		ind := len(line) - len(strings.TrimLeft(line, " "))
		if !strings.HasPrefix(body, "->") {
			// a line that is not a step continues the previous one's detail
			if len(stack) > 0 {
				stack[len(stack)-1].n.addProp("Note", body)
			}
			continue
		}
		n := parseMySQLStep(strings.TrimSpace(strings.TrimPrefix(body, "->")))
		for len(stack) > 0 && stack[len(stack)-1].ind >= ind {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			tops = append(tops, n)
		} else {
			p := stack[len(stack)-1].n
			p.Children = append(p.Children, n)
		}
		stack = append(stack, open{n, ind})
	}
	if len(tops) == 0 {
		return nil, serr.New("empty EXPLAIN output")
	}
	if len(tops) == 1 && strings.Contains(tops[0].Op, "not executable by iterator executor") {
		// MySQL's tree format cannot describe some statements (a single-
		// table UPDATE or DELETE); the caller falls back to the table format
		return nil, serr.New("the tree format cannot describe this statement")
	}
	p := &Plan{Engine: MySQL, Raw: text}
	if len(tops) == 1 {
		p.Root = tops[0]
	} else {
		p.Root = &Node{Op: "Query", Kind: KindResult, Children: tops}
	}
	p.Analyzed = anyNode(p.Root, func(n *Node) bool { return n.HasActual || n.NeverExecuted })
	p.Finalize()
	return p, nil
}

var (
	// The number groups are captured loosely and split afterwards: MySQL
	// writes "764e-6" and "0.0364..0.211", which a tight numeric pattern
	// would cut in the wrong place.
	myCostRe   = regexp.MustCompile(`\(cost=(\S+?) rows=(\S+?)\)`)
	myActualRe = regexp.MustCompile(`\(actual time=(\S+?) rows=(\S+?) loops=(\S+?)\)`)
	// "Index lookup on o using orders_status (status='paid')"
	// "Index range scan on u using PRIMARY over (id < 50)"
	// "Table scan on orders"
	myTargetRe = regexp.MustCompile(`^(.*?) on (\S+)(?: using (\S+))?(?: over (.*)|\s+\((.*)\))?$`)
)

// parseMySQLStep parses one step: its name, target and condition, then the
// estimate and actual groups at the end of the line.
func parseMySQLStep(text string) *Node {
	n := &Node{}
	head := text
	for _, cut := range []string{"  (cost=", " (cost=", " (actual time=", " (never executed)"} {
		if i := strings.Index(head, cut); i >= 0 {
			head = head[:i]
		}
	}
	head = strings.TrimSpace(head)
	if m := myCostRe.FindStringSubmatch(text); m != nil {
		n.HasCost, n.HasEst = true, true
		lo, hi, ok := strings.Cut(m[1], "..")
		n.TotalCost = myFloat(lo)
		if ok {
			n.StartupCost, n.TotalCost = myFloat(lo), myFloat(hi)
		}
		n.EstRows = myFloat(m[2])
	}
	if strings.Contains(text, "(never executed)") {
		n.NeverExecuted = true
	} else if m := myActualRe.FindStringSubmatch(text); m != nil {
		n.HasActual = true
		first, last, _ := strings.Cut(m[1], "..")
		n.ActualStartMs, n.ActualMs = myFloat(first), myFloat(last)
		n.ActualRows, n.Loops = myFloat(m[2]), myFloat(m[3])
	}

	// "Filter: (…)", "Sort: x DESC", "Limit: 5 row(s)", "Aggregate: max(…)"
	if k, v, ok := strings.Cut(head, ": "); ok && !strings.Contains(k, " on ") {
		n.Op = k
		switch k {
		case "Filter":
			n.addProp("Filter", v)
		case "Sort", "Sort row IDs":
			n.addProp("Sort Key", v)
		case "Limit":
			n.addProp("Limit", v)
		default:
			n.addProp("Expression", v)
		}
	} else if m := myTargetRe.FindStringSubmatch(head); m != nil {
		n.Op, n.Alias, n.Index = m[1], m[2], m[3]
		n.addProp("Range", m[4])
		n.addProp("Lookup", m[5])
	} else if i := strings.Index(head, " ("); i > 0 && strings.HasSuffix(head, ")") {
		// "Inner hash join (o.user_id = u.id)", "Select #2 (subquery …)"
		n.Op = head[:i]
		n.addProp("Condition", head[i+2:len(head)-1])
	} else {
		n.Op = head
	}

	lower := strings.ToLower(text)
	switch {
	case strings.HasPrefix(n.Op, "Select #"):
		n.Kind = KindSubquery
		if strings.Contains(lower, "dependent") {
			n.flag("dependent")
		}
	case strings.Contains(n.Op, "join"):
		n.Kind = KindJoin
	case n.Op == "Table scan" && n.Alias == "<temporary>":
		n.Kind = KindHash
		n.flag("temporary")
	}
	if strings.Contains(n.Index, "<auto_key") || strings.Contains(n.Index, "<auto_distinct_key>") {
		n.flag("auto-index")
	}
	if strings.Contains(lower, "using temporary") {
		n.flag("temporary")
	}
	return n
}

// myFloat parses MySQL's numbers, "764e-6" included.
func myFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// ParseMySQLTable reads the classic EXPLAIN table (id, select_type, table,
// type, possible_keys, key, rows, filtered, Extra — and MariaDB's ANALYZE,
// which adds r_rows and r_filtered). It is the only format every MySQL and
// MariaDB version speaks, so it is the fallback when the tree format is not
// available.
//
// The table is flat: one row per table access, grouped by the SELECT (id)
// it belongs to, in join order. It becomes a tree the only way it honestly
// can — each SELECT a subtree, its tables under a nested-loop join in the
// order MySQL reads them (MySQL joins are nested loops, outer table first,
// unless Extra says a hash join). Nothing here invents a cost: the table has
// none, so the plan offers rows, not cost, as its metric.
func ParseMySQLTable(cols []string, rows [][]string) (*Plan, error) {
	ix := map[string]int{}
	for i, c := range cols {
		ix[strings.ToLower(c)] = i
	}
	if _, ok := ix["select_type"]; !ok {
		return nil, serr.New("not a MySQL EXPLAIN table", "columns", strings.Join(cols, ","))
	}
	get := func(r []string, c string) string {
		i, ok := ix[c]
		if !ok || i >= len(r) || r[i] == "NULL" {
			return ""
		}
		return r[i]
	}

	type block struct {
		id, selType string
		tables      []*Node
	}
	var blocks []*block
	byID := map[string]*block{}
	analyzed := false
	for _, r := range rows {
		id := get(r, "id")
		b := byID[id]
		if b == nil {
			b = &block{id: id, selType: get(r, "select_type")}
			byID[id] = b
			blocks = append(blocks, b)
		}
		n := &Node{Alias: get(r, "table"), Index: get(r, "key")}
		typ := get(r, "type")
		n.Op, n.Kind = myAccess(typ)
		if v := get(r, "rows"); v != "" {
			n.EstRows, n.HasEst = myFloat(v), true
		}
		if v := get(r, "r_rows"); v != "" {
			n.ActualRows, n.HasActual, n.Loops = myFloat(v), true, 1
			analyzed = true
		}
		extra := get(r, "extra")
		n.addProp("Access type", typ)
		n.addProp("Possible keys", get(r, "possible_keys"))
		n.addProp("Key length", get(r, "key_len"))
		n.addProp("Ref", get(r, "ref"))
		if f := get(r, "filtered"); f != "" {
			n.addProp("Filtered", f+"%")
			n.FilteredPct = myFloat(f)
		}
		if f := get(r, "r_filtered"); f != "" {
			n.addProp("Actually filtered", f+"%")
		}
		n.addProp("Extra", extra)
		le := strings.ToLower(extra)
		if strings.Contains(le, "using filesort") {
			n.flag("filesort")
		}
		if strings.Contains(le, "using temporary") {
			n.flag("temporary")
		}
		if strings.Contains(le, "using join buffer") {
			n.flag("join-buffer")
		}
		if get(r, "possible_keys") != "" && n.Index == "" {
			n.flag("index-unused")
		}
		if strings.HasPrefix(n.Index, "<auto_key") {
			n.flag("auto-index")
		}
		b.tables = append(b.tables, n)
	}
	if len(blocks) == 0 {
		return nil, serr.New("empty EXPLAIN output")
	}

	subtree := func(b *block) *Node {
		var body *Node
		if len(b.tables) == 1 {
			body = b.tables[0]
		} else {
			body = &Node{Op: "Nested loop join", Kind: KindJoin, Children: b.tables}
			for _, t := range b.tables {
				if t.HasFlag("join-buffer") {
					body.Op = "Join (join buffer)"
				}
			}
		}
		if len(blocks) == 1 {
			return body
		}
		sel := &Node{Op: "Select #" + b.id, Kind: KindSubquery, Children: []*Node{body}}
		sel.addProp("Select type", b.selType)
		if strings.Contains(strings.ToLower(b.selType), "dependent") {
			sel.flag("dependent")
		}
		return sel
	}
	p := &Plan{Engine: MySQL, Analyzed: analyzed, Raw: tableRaw(cols, rows)}
	if len(blocks) == 1 {
		p.Root = subtree(blocks[0])
	} else {
		p.Root = &Node{Op: "Query", Kind: KindResult}
		for _, b := range blocks {
			p.Root.Children = append(p.Root.Children, subtree(b))
		}
	}
	p.Finalize()
	return p, nil
}

// myAccess names an access type the way the tree format would, and classes
// it. The one-word codes (ALL, ref, eq_ref) are MySQL jargon; the views show
// the words, and the code stays in the detail as "Access type".
func myAccess(typ string) (string, Kind) {
	switch strings.ToLower(typ) {
	case "all":
		return "Full table scan", KindScan
	case "index":
		return "Full index scan", KindIndex
	case "range":
		return "Index range scan", KindIndex
	case "ref", "ref_or_null":
		return "Index lookup", KindIndex
	case "eq_ref":
		return "Unique index lookup", KindIndex
	case "const", "system":
		return "Constant row", KindResult
	case "fulltext":
		return "Full-text index", KindIndex
	case "index_merge":
		return "Index merge", KindIndex
	case "unique_subquery", "index_subquery":
		return "Subquery index lookup", KindIndex
	case "":
		return "No table", KindResult
	}
	return typ, KindOther
}

// tableRaw renders a tabular EXPLAIN back as TSV, for "copy the plan".
func tableRaw(cols []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t"))
	for _, r := range rows {
		b.WriteByte('\n')
		b.WriteString(strings.Join(r, "\t"))
	}
	return b.String()
}
