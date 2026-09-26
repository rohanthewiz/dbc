package explain

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/theme"
)

// Mermaid renders the plan as a Mermaid flowchart — the form a plan takes
// in a GitHub or GitLab comment, a wiki page, or a Notion doc, which draw a
// ```mermaid block as a diagram without anyone installing anything.
//
//	flowchart BT                        ┌───────────────┐
//	  n0["Hash Join<br/>…"]             │ n0 Hash Join  │  ← the root on top
//	  n1["Seq Scan · orders<br/>…"]     └───────▲───────┘
//	  n1 -->|"36.7k rows"| n0                   │ 36.7k rows
//	                                    ┌───────┴───────┐
//	                                    │ n1 Seq Scan   │
//	                                    └───────────────┘
//
// WHY BOTTOM-TO-TOP. Every other view draws the root on top and the data
// flowing up into it. In Mermaid an edge A --> B puts B after A in the
// flow's direction, so "BT" with child --> parent gives the same picture —
// root on top, arrows pointing the way the rows go — without the reversed
// arrowheads a TD chart would need.
//
// WHAT A NODE SAYS. The same lines the graph's cards carry: the operation
// and what it works on, the step's most telling detail, rows, and its share
// of the metric the plan is best measured by. Heat and findings become
// classDefs (see mermaidClass) so a renderer that draws colors shows where
// the time goes, and one that does not still has the words.
//
// ESCAPING. Labels are double-quoted, and inside one Mermaid reads its own
// entity codes: #quot; for a quote, and #<decimal>; for anything else. A
// plan's text is the user's SQL — full of quotes, <, >, # and | — so every
// character Mermaid or its HTML labels could read as syntax is written as a
// code (see mermaidText). The result never breaks the chart, whatever the
// statement holds.
func (p *Plan) Mermaid() string {
	var b strings.Builder
	// The headline rides along as a comment: invisible in the drawing, but
	// the one line that says what the chart is when the source is read.
	b.WriteString("%% " + mermaidComment(p.Headline()) + "\n")
	if p.Conn != "" {
		b.WriteString("%% connection: " + mermaidComment(p.Conn) + "\n")
	}
	b.WriteString("flowchart BT\n")
	if p.Root == nil {
		b.WriteString("  n0[\"empty plan\"]\n")
		return b.String()
	}
	m := p.Metric
	if m == "" {
		m = p.Metrics()[0]
	}
	sev := worstSeverities(p)

	// Nodes first, then edges: Mermaid accepts either order, but a reader
	// of the source finds each step's label in one place, in plan order.
	var edges []string
	var walk func(n *Node)
	walk = func(n *Node) {
		fmt.Fprintf(&b, "  n%d[\"%s\"]\n", n.ID, mermaidLabel(p, n, m, sev[n.ID]))
		for _, c := range n.Children {
			edges = append(edges, mermaidEdge(n, c))
			walk(c)
		}
	}
	walk(p.Root)
	for _, e := range edges {
		b.WriteString(e)
	}

	// Classes: one line per class listing its members, so a big plan does
	// not repeat a style per node.
	classes := map[string][]string{}
	var order []string
	for _, n := range p.nodes {
		c := mermaidClass(p, n, m, sev[n.ID])
		if c == "" {
			continue
		}
		if _, seen := classes[c]; !seen {
			order = append(order, c)
		}
		classes[c] = append(classes[c], fmt.Sprintf("n%d", n.ID))
	}
	if len(order) > 0 {
		b.WriteString(mermaidClassDefs())
		for _, c := range order {
			fmt.Fprintf(&b, "  class %s %s\n", strings.Join(classes[c], ","), c)
		}
	}
	return b.String()
}

// mermaidLabel is a step's node text, lines joined by <br/>.
func mermaidLabel(p *Plan, n *Node, m Metric, sev Severity) string {
	top := n.Op
	if sev != "" {
		// the severity rides in words as well as color: "▲ warn" survives
		// a renderer with no styles, or a reader who cannot tell the hues
		top = sev.Glyph() + " " + top
	}
	lines := []string{mermaidText(top)}
	if t := n.Target(); t != "" {
		lines = append(lines, mermaidText(t))
	}
	if s := n.Summary(); s != "" {
		lines = append(lines, "<i>"+mermaidText(clipRunes(s, 60))+"</i>")
	}
	var nums []string
	switch {
	case n.NeverExecuted:
		nums = append(nums, "never ran")
	case n.HasActual:
		nums = append(nums, fmtRowCount(n.RowsOut)+" rows")
	case n.HasEst:
		nums = append(nums, "~"+fmtRowCount(n.RowsOut)+" rows")
	}
	if p.Analyzed {
		if f := FmtFactor(n.Misestimate); f != "" {
			nums = append(nums, f+" than estimated")
		}
	}
	// the share is only shown where it measures something: shape is a
	// heuristic, and a label step with none of the metric would read "0%"
	if m != MetricShape && n.Self(m) > 0 {
		nums = append(nums, fmt.Sprintf("%s · %.0f%%", FmtMetric(m, n.Self(m)), p.Share(n, m)*100))
	}
	if len(nums) > 0 {
		lines = append(lines, mermaidText(strings.Join(nums, " · ")))
	}
	return strings.Join(lines, "<br/>")
}

// mermaidEdge is the data-flow edge from child c up into parent n, labeled
// the way the graph labels it: the relationship where it says something,
// and the rows that flow.
func mermaidEdge(n, c *Node) string {
	var parts []string
	// "Outer" on an only child says nothing — every single input is the
	// outer one; on a join's two inputs, or a SubPlan, it is the point
	if c.Relationship != "" && (len(n.Children) > 1 || (c.Relationship != "Outer" && c.Relationship != "Inner")) {
		parts = append(parts, c.Relationship)
	}
	if c.RowsOut > 0 {
		parts = append(parts, FmtRows(c.RowsOut)+" rows")
	}
	if len(parts) == 0 {
		return fmt.Sprintf("  n%d --> n%d\n", c.ID, n.ID)
	}
	return fmt.Sprintf("  n%d -->|\"%s\"| n%d\n", c.ID, mermaidText(strings.Join(parts, " · ")), n.ID)
}

// mermaidClass picks the one class a step is drawn with. A finding outranks
// heat — a critical finding on a cheap step is still the thing to look at —
// and heat uses the same breakpoints the graph's color ramp turns at.
func mermaidClass(p *Plan, n *Node, m Metric, sev Severity) string {
	switch {
	case n.NeverExecuted:
		return "never"
	case sev == SevCrit:
		return "crit"
	case sev == SevWarn:
		return "warn"
	}
	if m == MetricShape {
		return ""
	}
	switch s := p.Share(n, m); {
	case s >= 0.4:
		return "hot"
	case s >= 0.15:
		return "warm"
	}
	return ""
}

// mermaidClassDefs styles the classes from the light palette: a chart
// pasted into a document is almost always read on white, and the dbc
// palette's light variant is the one made for that.
func mermaidClassDefs() string {
	l := theme.Light()
	return "  classDef hot fill:" + theme.Blend(l.Err, l.Panel, 0.18) + ",stroke:" + l.Err + ",stroke-width:2px\n" +
		"  classDef warm fill:" + theme.Blend(l.Warn, l.Panel, 0.18) + ",stroke:" + l.Warn + ",stroke-width:2px\n" +
		"  classDef crit stroke:" + l.Err + ",stroke-width:3px\n" +
		"  classDef warn stroke:" + l.Warn + ",stroke-width:3px\n" +
		"  classDef never stroke-dasharray:4 3,opacity:0.6\n"
}

// worstSeverities maps each step to its worst finding's severity.
func worstSeverities(p *Plan) map[int]Severity {
	rank := map[Severity]int{SevCrit: 3, SevWarn: 2, SevInfo: 1}
	out := map[int]Severity{}
	for _, in := range p.Insights {
		if in.NodeID >= 0 && rank[in.Severity] > rank[out[in.NodeID]] {
			out[in.NodeID] = in.Severity
		}
	}
	return out
}

// mermaidText escapes s for a double-quoted Mermaid label. Inside the
// quotes Mermaid's shape and statement syntax — ( ) [ ] { } ; : | — is
// plain text, so the SQL stays readable in the source. What still needs a
// #<decimal>; entity code is what the quotes do not protect: the quote
// itself, # (it starts an entity code), a leading backtick (it makes a
// "markdown string"), and < > & (the label is rendered as HTML).
// Newlines become spaces: a label's lines are the <br/>s mermaidLabel puts
// between them, not whatever the SQL happened to wrap at.
func mermaidText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r == '"':
			b.WriteString("#quot;")
		case strings.ContainsRune("#<>&`", r):
			fmt.Fprintf(&b, "#%d;", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// mermaidComment keeps a %% comment on one line.
func mermaidComment(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

// clipRunes shortens s to at most n runes, ending in … when cut.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
