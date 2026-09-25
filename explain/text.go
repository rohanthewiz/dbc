package explain

import (
	"fmt"
	"math"
	"strings"

	"github.com/rivo/uniseg"
)

// TextOptions shape Text.
type TextOptions struct {
	Width    int    // total line width; 0 = 100
	Metric   Metric // "" = the plan's best
	Color    bool   // ANSI colors, for a terminal
	Insights bool   // append the findings
}

// ANSI styles for the text rendering. They are the classic 16-color codes
// rather than dbc's truecolor palette: headless output lands in whatever
// terminal, pager or CI log the user has, and those all honor these.
const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
	ansiYel   = "\x1b[33m"
	ansiCyan  = "\x1b[36m"
)

// Text renders the plan as an indented tree with a column of numbers and a
// bar per step — the headless `dbc explain` output, the "copy plan as text"
// clipboard payload, and what the assistant is shown.
//
//	Hash Join · (o.user_id = u.id)             36,650 rows          8.4 ms  31% ███▏
//	├─ Bitmap Heap Scan · orders o             50,000 rows          6.2 ms  23% ██▎
//	│  └─ Bitmap Index Scan · orders_status    50,000 rows          0.9 ms   3% ▎
//	└─ Hash                                    15,659 rows          2.3 ms   9% ▉
//
// The bar and percentage are each step's OWN share (self time, self cost),
// so they add up to 100% down the column and point straight at the step
// that matters; the tree lines carry the nesting.
func (p *Plan) Text(o TextOptions) string {
	if o.Width <= 0 {
		o.Width = 100
	}
	m := o.Metric
	if m == "" {
		m = p.Metric
	}
	paint := func(code, s string) string {
		if !o.Color || s == "" {
			return s
		}
		return code + s + ansiReset
	}
	var b strings.Builder
	b.WriteString(paint(ansiBold, p.Headline()) + "\n")
	for _, n := range p.Notes {
		b.WriteString(paint(ansiDim, "note: "+n) + "\n")
	}
	b.WriteString("\n")

	const numW, barW = 34, 10 // rows · factor · value · pct, then the bar
	treeW := max(o.Width-numW-barW-2, 24)
	lines := TreeLines(p.Root)
	for _, tl := range lines {
		n := tl.Node
		label := tl.Prefix + n.Op
		if t := n.Target(); t != "" {
			label += " · " + t
		}
		sum := ""
		if s := n.Summary(); s != "" {
			sum = "  " + s
		}
		label = TruncateCells(label, treeW)
		room := treeW - uniseg.StringWidth(label)
		sum = TruncateCells(sum, room)
		pad := strings.Repeat(" ", max(room-uniseg.StringWidth(sum), 0))

		rows, factor := RowsCell(n, p.Analyzed)
		val, share := "", p.Share(n, m)
		if m != MetricShape {
			val = FmtMetric(m, n.Self(m))
		}
		pctS := fmt.Sprintf("%3.0f%%", share*100)
		if m == MetricShape {
			pctS = ""
		}
		// fmt pads by rune, and every glyph here (×, ↑, µ) is one cell wide,
		// so rune padding is cell padding
		barCode := ansiGreen
		switch {
		case share >= 0.5:
			barCode = ansiRed
		case share >= 0.2:
			barCode = ansiYel
		}
		line := paint(ansiDim, tl.Prefix) + strings.TrimPrefix(label, tl.Prefix) + paint(ansiDim, sum) + pad + " " +
			paint(ansiDim, fmt.Sprintf("%12s", rows)) + " " + paint(ansiYel, fmt.Sprintf("%6s", factor)) + " " +
			fmt.Sprintf("%9s %4s", val, pctS) + " " + paint(barCode, Bar(share, barW))
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}

	if o.Insights && len(p.Insights) > 0 {
		b.WriteString("\n" + paint(ansiBold, "Insights") + "\n")
		for _, in := range p.Insights {
			code := ansiCyan
			switch in.Severity {
			case SevCrit:
				code = ansiRed
			case SevWarn:
				code = ansiYel
			}
			where := ""
			if n := p.Node(in.NodeID); n != nil {
				where = paint(ansiDim, "  ["+n.Title()+"]")
			}
			b.WriteString("  " + paint(code, in.Severity.Glyph()) + " " + paint(ansiBold, in.Title) + where + "\n")
			for _, l := range WrapWords(in.Detail, o.Width-6) {
				b.WriteString("    " + l + "\n")
			}
			if in.Fix != "" {
				for i, l := range WrapWords(in.Fix, o.Width-8) {
					lead := "    → "
					if i > 0 {
						lead = "      "
					}
					b.WriteString(lead + l + "\n")
				}
			}
			if in.SQL != "" {
				b.WriteString("      " + paint(ansiCyan, in.SQL) + "\n")
			}
		}
	}
	return b.String()
}

// Headline is the plan's one-line summary: engine, how it was obtained, the
// totals, the size.
func (p *Plan) Headline() string {
	parts := []string{p.Engine}
	switch {
	case p.Analyzed:
		parts = append(parts, "analyzed")
	case p.Measured:
		parts = append(parts, "estimated plan, measured run")
	default:
		parts = append(parts, "estimated")
	}
	switch {
	case p.Measured:
		parts = append(parts, "ran in "+FmtMs(p.ExecutionMs), fmtRowCount(p.ResultRows)+" rows")
	case p.ExecutionMs > 0:
		parts = append(parts, "execution "+FmtMs(p.ExecutionMs))
	}
	if p.PlanningMs > 0 {
		parts = append(parts, "planning "+FmtMs(p.PlanningMs))
	}
	if p.Root != nil && p.Root.HasCost {
		parts = append(parts, "cost "+FmtCost(p.Root.TotalCost))
	}
	parts = append(parts, pluralS(len(p.nodes), "step"))
	return "Plan · " + strings.Join(parts, " · ")
}

// Glyph is the one-cell marker the views draw for a severity.
func (s Severity) Glyph() string {
	switch s {
	case SevCrit:
		return "✖"
	case SevWarn:
		return "▲"
	}
	return "●"
}

// RowsCell is the rows column for a step: actual rows (across loops) when
// the plan was analyzed, "~estimate" otherwise, and a misestimate marker
// when the two disagree by ×2 or more.
func RowsCell(n *Node, analyzed bool) (rows, factor string) {
	switch {
	case n.NeverExecuted:
		return "never ran", ""
	case n.HasActual:
		rows = fmtRowCount(n.RowsOut) + " rows"
	case n.HasEst:
		rows = "~" + fmtRowCount(n.RowsOut) + " rows"
	case n.TableRows > 0:
		// an engine with no row numbers (SQLite, bytdb): the size of the
		// table the step reads is the best proxy for its work there is
		rows = "table " + FmtRows(n.TableRows)
	}
	if analyzed && n.Misestimate != 0 {
		switch {
		case n.Misestimate >= 2:
			factor = "×" + FmtRows(math.Round(n.Misestimate)) + "↑"
		case n.Misestimate <= 0.5:
			factor = "×" + FmtRows(math.Round(1/n.Misestimate)) + "↓"
		}
	}
	return rows, factor
}

// fmtRowCount is a row count in full, keeping one decimal for the fractional
// estimates MySQL gives the inner side of a join ("~0.3 rows").
func fmtRowCount(v float64) string {
	if v > 0 && v < 10 && v != math.Trunc(v) {
		return FmtRows(v)
	}
	return FmtCount(v)
}

// pluralS is "1 step" / "7 steps".
func pluralS(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// TreeLine is one step with the tree-drawing prefix that goes before it.
type TreeLine struct {
	Node   *Node
	Prefix string // "│  ├─ " and friends
}

// TreeLines flattens the tree in preorder with box-drawing prefixes, the way
// `tree` draws a directory — each child's line says whether more siblings
// follow (├─) or it is the last (└─), and the columns to its left carry
// its ancestors' continuations (│).
func TreeLines(root *Node) []TreeLine {
	var out []TreeLine
	var walk func(n *Node, lead string, last, top bool)
	walk = func(n *Node, lead string, last, top bool) {
		pre, next := "", ""
		if !top {
			if last {
				pre, next = lead+"└─ ", lead+"   "
			} else {
				pre, next = lead+"├─ ", lead+"│  "
			}
		}
		out = append(out, TreeLine{Node: n, Prefix: pre})
		for i, c := range n.Children {
			walk(c, next, i == len(n.Children)-1, false)
		}
	}
	if root != nil {
		walk(root, "", true, true)
	}
	return out
}

// eighths are the partial block characters, 1/8 to 8/8 of a cell.
var eighths = []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}

// Bar draws frac (0–1) as a bar of at most cells cells, in eighth-cell
// steps, so a 3% share still shows as a sliver rather than nothing. A zero
// share draws nothing at all, which reads differently from "tiny" on purpose.
func Bar(frac float64, cells int) string {
	if frac <= 0 || cells <= 0 {
		return ""
	}
	e := int(math.Round(math.Min(frac, 1) * float64(cells*8)))
	e = max(e, 1)
	return strings.Repeat("█", e/8) + func() string {
		if e%8 == 0 {
			return ""
		}
		return eighths[e%8-1]
	}()
}

// TruncateCells shortens s to at most w terminal cells, ending in "…".
func TruncateCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if uniseg.StringWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used, state := 0, -1
	for s != "" {
		var g string
		var gw int
		g, s, gw, state = uniseg.FirstGraphemeClusterInString(s, state)
		if used+gw > w-1 {
			break
		}
		b.WriteString(g)
		used += gw
	}
	return b.String() + "…"
}

// WrapWords wraps prose at word boundaries to w cells.
func WrapWords(s string, w int) []string {
	if s == "" {
		return nil
	}
	w = max(w, 10)
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case uniseg.StringWidth(line)+1+uniseg.StringWidth(word) <= w:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
