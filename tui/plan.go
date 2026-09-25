package tui

import (
	"fmt"
	"math"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/theme"
)

// planView is the Plan tab of the results pane: an explained statement as a
// tree, a flame graph, or a list of findings, with the selected step's
// details beside it.
//
//	┌ ◈ postgres · analyzed · 26.8 ms · 7 steps             was 131 ms → 26.8 ms ▼80% ┐  headline
//	│ [Tree] [Flame] [Insights ✖1 ▲2]   size by [time] [cost] [rows]  ▶ Analyze ↗ ⧉  │  chips
//	│ step                          rows    est   self time   %      │ ⋈ Hash Join    │
//	│ ▾ ⇅ Sort · (sum…) DESC           5            16 µs    0% ▏    │ time  9.9 ms … │
//	│   └─ ▾ Σ HashAggregate            5            5.6 ms   21% ██▏ │ rows  36,650 … │
//	│      └─ ▸ ⋈ Hash Join ▲      36,650   ×2↑     9.9 ms   37% ███▊ │ Hash Cond …    │
//	└──────────────────────────────── tree ──────────────────────────┴──── detail ────┘
//
// ONE SELECTION, THREE VIEWS. The tree, the flame graph and the findings all
// point at the same step (cur): picking a block in the flame graph and
// switching to the tree lands on that step, and a finding's "go to step"
// selects it in the tree. The detail panel always describes cur.
//
// GEOMETRY IS RECORDED AS DRAWN, the way the modals record their chips: each
// draw rebuilds rows, chips and flame boxes in canvas coordinates, and a
// click is resolved against the same rects — so what is clicked is what was
// on screen, whatever the pane's size.
type planView struct {
	plan *explain.Plan
	// prev is the plan this one replaced when both explain the same
	// statement on the same connection — the "before" of a before/after,
	// which is what a user adding an index wants to see.
	prev *explain.Plan

	mode      planMode
	metric    explain.Metric
	collapsed map[int]bool
	cur       int // selected step (node ID)

	top       int  // first visible tree row
	follow    bool // scroll the selection into view on the next draw
	insCur    int  // selected finding
	insTop    int  // first visible finding line
	detailTop int  // first visible detail line
	flameRoot int  // the step the flame graph is zoomed into

	// Geometry of the last draw, in canvas coordinates.
	area    Rect
	rows    []planRowHit
	chips   []planChip
	flame   []flameBox
	ins     []insightHit
	tree    Rect
	detail  Rect
	flameR  Rect
	visRows int

	hoverChip  planChipID
	hoverNode  int // step under the mouse in the tree or flame graph, -1 none
	hoverInsig int

	// (The plan as the assistant is shown it is cached by the workspace,
	// which builds the chat's context — see workspace.planForChatLocked.)
}

type planMode int

const (
	planTree planMode = iota
	planFlame
	planInsights
)

// planRowHit is one drawn tree row: its step and the twisty that folds it.
type planRowHit struct {
	id     int
	r      Rect
	twisty Rect
}

type planChipID int

const (
	chipNone planChipID = iota
	chipTree
	chipFlame
	chipInsights
	chipMetric  // + metric index
	chipAnalyze = chipMetric + 10
	chipBrowser = chipMetric + 11
	chipCopy    = chipMetric + 12
	chipResults = chipMetric + 13
)

type planChip struct {
	id planChipID
	r  Rect
}

// flameBox is one step's block in the flame graph.
type flameBox struct {
	id     int
	x0, x1 int // canvas columns, [x0, x1)
	y, h   int
}

// insightHit is one drawn finding with its action chips.
type insightHit struct {
	idx       int
	r         Rect // the whole card
	copySQL   Rect
	insertSQL Rect
	goTo      Rect
}

func newPlanView() *planView {
	return &planView{collapsed: map[int]bool{}, hoverNode: -1, hoverInsig: -1}
}

// set installs a plan. The view state is reset — a new plan's step IDs mean
// different steps — except the mode, which is the user's choice of how to
// look, and the metric when the new plan offers it.
func (v *planView) set(p *explain.Plan) {
	if v.plan != nil && samePlanSubject(v.plan, p) {
		v.prev = v.plan
	} else {
		v.prev = nil
	}
	v.plan = p
	v.collapsed = map[int]bool{}
	v.cur, v.top, v.insCur, v.insTop, v.detailTop, v.flameRoot = 0, 0, 0, 0, 0, 0
	keep := false
	for _, m := range p.Metrics() {
		keep = keep || m == v.metric
	}
	if !keep {
		v.metric = p.Metric
	}
	// Open on the step that matters: the first finding's, when there is a
	// serious one — the user's eye should land where the problem is.
	if len(p.Insights) > 0 && p.Insights[0].Severity != explain.SevInfo && p.Insights[0].NodeID >= 0 {
		v.cur = p.Insights[0].NodeID
	}
	// the tree's height is known only when it is drawn, so scrolling to
	// that step waits for the draw
	v.follow = true
}

// samePlanSubject reports whether two plans explain the same statement on
// the same connection, give or take whitespace — the condition for showing
// one as the "before" of the other.
func samePlanSubject(a, b *explain.Plan) bool {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	return a.Conn == b.Conn && a.Engine == b.Engine && norm(a.Statement) == norm(b.Statement) && a.Statement != ""
}

// node returns the selected step.
func (v *planView) node() *explain.Node {
	if v.plan == nil {
		return nil
	}
	if n := v.plan.Node(v.cur); n != nil {
		return n
	}
	return v.plan.Root
}

// visible lists the tree's rows: every step not inside a folded one.
func (v *planView) visible() []explain.TreeLine {
	var out []explain.TreeLine
	skipBelow := -1
	for _, tl := range explain.TreeLines(v.plan.Root) {
		if skipBelow >= 0 {
			if tl.Node.Depth > skipBelow {
				continue
			}
			skipBelow = -1
		}
		out = append(out, tl)
		if v.collapsed[tl.Node.ID] && len(tl.Node.Children) > 0 {
			skipBelow = tl.Node.Depth
		}
	}
	return out
}

// select moves the selection to id, unfolding its ancestors so it is on
// screen in the tree.
func (v *planView) selectNode(id int) {
	n := v.plan.Node(id)
	if n == nil {
		return
	}
	v.cur, v.detailTop = id, 0
	for a := n.Parent(); a != nil; a = a.Parent() {
		delete(v.collapsed, a.ID)
	}
	v.ensureVisible()
}

// ensureVisible scrolls the tree so the selected row shows.
func (v *planView) ensureVisible() {
	if v.visRows <= 0 {
		return
	}
	for i, tl := range v.visible() {
		if tl.Node.ID == v.cur {
			if i < v.top {
				v.top = i
			} else if i >= v.top+v.visRows {
				v.top = i - v.visRows + 1
			}
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Keys and mouse
// ---------------------------------------------------------------------------

// key handles the plan's own keys; the Model handles the ones that start
// work (a, e, b, y — see planKey in explain.go). It reports whether it used k.
func (v *planView) key(k tea.KeyPressMsg) bool {
	if v.plan == nil {
		return false
	}
	switch k.String() {
	case "1":
		v.mode = planTree
	case "2":
		v.mode = planFlame
	case "3":
		v.mode = planInsights
	case "v":
		v.mode = (v.mode + 1) % 3
	case "m":
		ms := v.plan.Metrics()
		for i, m := range ms {
			if m == v.metric {
				v.metric = ms[(i+1)%len(ms)]
				return true
			}
		}
		v.metric = ms[0]
	default:
		switch v.mode {
		case planTree:
			return v.treeKey(k)
		case planFlame:
			return v.flameKey(k)
		case planInsights:
			return v.insightKey(k)
		}
		return false
	}
	return true
}

func (v *planView) treeKey(k tea.KeyPressMsg) bool {
	vis := v.visible()
	at := 0
	for i, tl := range vis {
		if tl.Node.ID == v.cur {
			at = i
		}
	}
	move := func(i int) {
		i = max(0, min(i, len(vis)-1))
		v.cur, v.detailTop = vis[i].Node.ID, 0
		v.ensureVisible()
	}
	n := v.node()
	switch k.String() {
	case "up", "k":
		move(at - 1)
	case "down", "j":
		move(at + 1)
	case "pgup":
		move(at - max(v.visRows-1, 1))
	case "pgdown":
		move(at + max(v.visRows-1, 1))
	case "home", "g":
		move(0)
	case "end", "G":
		move(len(vis) - 1)
	case "left", "h":
		// fold, then climb: the file-tree convention
		if len(n.Children) > 0 && !v.collapsed[n.ID] {
			v.collapsed[n.ID] = true
		} else if p := n.Parent(); p != nil {
			v.selectNode(p.ID)
		}
	case "right", "l":
		if len(n.Children) > 0 {
			if v.collapsed[n.ID] {
				delete(v.collapsed, n.ID)
			} else {
				v.selectNode(n.Children[0].ID)
			}
		}
	case "enter", "space":
		v.toggle(n.ID)
	case "*":
		// unfold everything
		v.collapsed = map[int]bool{}
	case "shift+down", "J":
		v.detailTop++
	case "shift+up", "K":
		v.detailTop = max(v.detailTop-1, 0)
	default:
		return false
	}
	return true
}

// toggle folds or unfolds a step with children.
func (v *planView) toggle(id int) {
	if n := v.plan.Node(id); n != nil && len(n.Children) > 0 {
		if v.collapsed[id] {
			delete(v.collapsed, id)
		} else {
			v.collapsed[id] = true
		}
	}
}

// flameKey walks the flame graph by structure: up to the parent, down to
// the first child, sideways among siblings; Enter zooms into the selection,
// Esc back out.
func (v *planView) flameKey(k tea.KeyPressMsg) bool {
	n := v.node()
	sib := func(d int) {
		p := n.Parent()
		if p == nil {
			return
		}
		for i, c := range p.Children {
			if c.ID == n.ID && i+d >= 0 && i+d < len(p.Children) {
				v.cur = p.Children[i+d].ID
				return
			}
		}
	}
	switch k.String() {
	case "up", "k":
		if p := n.Parent(); p != nil {
			v.cur = p.ID
		}
	case "down", "j":
		if len(n.Children) > 0 {
			v.cur = n.Children[0].ID
		}
	case "left", "h":
		sib(-1)
	case "right", "l":
		sib(1)
	case "enter", "space":
		v.flameRoot = v.cur
	case "esc", "backspace":
		if r := v.plan.Node(v.flameRoot); r != nil && r.Parent() != nil {
			v.flameRoot = r.Parent().ID
		} else {
			return false
		}
	default:
		return false
	}
	v.detailTop = 0
	return true
}

func (v *planView) insightKey(k tea.KeyPressMsg) bool {
	n := len(v.plan.Insights)
	switch k.String() {
	case "up", "k":
		v.insCur = max(v.insCur-1, 0)
	case "down", "j":
		v.insCur = min(v.insCur+1, max(n-1, 0))
	case "home", "g":
		v.insCur = 0
	case "end", "G":
		v.insCur = max(n-1, 0)
	case "enter", "space":
		v.goToInsight(v.insCur)
	default:
		return false
	}
	return true
}

// goToInsight shows the step a finding is about, in the tree.
func (v *planView) goToInsight(i int) {
	if i < 0 || i >= len(v.plan.Insights) {
		return
	}
	if id := v.plan.Insights[i].NodeID; id >= 0 {
		v.mode = planTree
		v.selectNode(id)
	}
}

// click handles a left press inside the plan area. It returns a chip the
// Model must act on (analyze, browser, copy, results), or chipNone.
func (v *planView) click(x, y, clicks int) planChipID {
	for _, c := range v.chips {
		if c.r.Contains(x, y) {
			switch {
			case c.id == chipTree:
				v.mode = planTree
			case c.id == chipFlame:
				v.mode = planFlame
			case c.id == chipInsights:
				v.mode = planInsights
			case c.id >= chipMetric && c.id < chipMetric+4:
				if ms := v.plan.Metrics(); int(c.id-chipMetric) < len(ms) {
					v.metric = ms[c.id-chipMetric]
				}
			default:
				return c.id
			}
			return chipNone
		}
	}
	switch v.mode {
	case planTree:
		for _, r := range v.rows {
			if r.twisty.Contains(x, y) {
				v.toggle(r.id)
				v.cur = r.id
				return chipNone
			}
			if r.r.Contains(x, y) {
				if v.cur == r.id && clicks >= 2 {
					v.toggle(r.id)
				}
				v.cur, v.detailTop = r.id, 0
				return chipNone
			}
		}
	case planFlame:
		for _, b := range v.flame {
			if x >= b.x0 && x < b.x1 && y >= b.y && y < b.y+b.h {
				v.cur, v.detailTop = b.id, 0
				if clicks >= 2 {
					v.flameRoot = b.id
				}
				return chipNone
			}
		}
		// a click on the breadcrumb row above the graph zooms back out
		if y == v.flameR.Y-1 && v.flameRoot != 0 {
			v.flameRoot = 0
		}
	case planInsights:
		for _, h := range v.ins {
			switch {
			case h.goTo.Contains(x, y):
				v.goToInsight(h.idx)
			case h.copySQL.Contains(x, y):
				v.insCur = h.idx
				return chipCopySQL
			case h.insertSQL.Contains(x, y):
				v.insCur = h.idx
				return chipInsertSQL
			case h.r.Contains(x, y):
				v.insCur = h.idx
				if clicks >= 2 {
					v.goToInsight(h.idx)
				}
			}
		}
	}
	return chipNone
}

// The findings' own action chips, reported to the Model like the header's.
const (
	chipCopySQL   = chipMetric + 20
	chipInsertSQL = chipMetric + 21
)

// selectAt selects the step drawn at (x, y), if any — what a right-click
// does before its menu opens, so the menu's "this step" is the one clicked.
func (v *planView) selectAt(x, y int) {
	for _, r := range v.rows {
		if r.r.Contains(x, y) {
			v.cur, v.detailTop = r.id, 0
		}
	}
	for _, b := range v.flame {
		if x >= b.x0 && x < b.x1 && y >= b.y && y < b.y+b.h {
			v.cur, v.detailTop = b.id, 0
		}
	}
}

// hover records what is under the mouse, for highlight feedback.
func (v *planView) hover(x, y int) {
	v.hoverChip, v.hoverNode, v.hoverInsig = chipNone, -1, -1
	if !v.area.Contains(x, y) {
		return
	}
	for _, c := range v.chips {
		if c.r.Contains(x, y) {
			v.hoverChip = c.id
		}
	}
	for _, r := range v.rows {
		if r.r.Contains(x, y) {
			v.hoverNode = r.id
		}
	}
	for _, b := range v.flame {
		if x >= b.x0 && x < b.x1 && y >= b.y && y < b.y+b.h {
			v.hoverNode = b.id
		}
	}
	for _, h := range v.ins {
		if h.r.Contains(x, y) {
			v.hoverInsig = h.idx
		}
	}
}

// wheel scrolls whichever part is under the pointer.
func (v *planView) wheel(x, y, dy int) {
	switch {
	case v.detail.Contains(x, y):
		v.detailTop = max(v.detailTop+dy, 0)
	case v.mode == planTree:
		v.top = max(v.top+dy, 0)
	case v.mode == planInsights:
		v.insTop = max(v.insTop+dy, 0)
	}
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// draw paints the plan into s.
func (v *planView) draw(s Surface, st styles, focused bool) {
	s.Fill(st.base)
	v.area = s.Rect()
	v.rows, v.chips, v.flame, v.ins = v.rows[:0], v.chips[:0], v.flame[:0], v.ins[:0]
	v.tree, v.detail, v.flameR = Rect{}, Rect{}, Rect{}
	if v.plan == nil {
		msg := "Ctrl+X explains the statement under the caret · Alt+X runs it with ANALYZE"
		s.Put(max((s.W()-width(msg))/2, 1), s.H()/2, msg, st.muted)
		return
	}
	v.drawHeadline(s.Sub(Rect{0, 0, s.W(), 1}), st)
	v.drawChips(s.Sub(Rect{0, 1, s.W(), 1}), st)
	y := 2
	if len(v.plan.Notes) > 0 && s.H() > 10 {
		note := "ⓘ " + v.plan.Notes[0]
		if len(v.plan.Notes) > 1 {
			note += fmt.Sprintf("  (+%d more in Insights)", len(v.plan.Notes)-1)
		}
		s.Put(1, y, truncate(note, s.W()-2), st.muted.Italic())
		y++
	}
	body := s.Sub(Rect{0, y, s.W(), s.H() - y})
	switch v.mode {
	case planTree:
		v.drawTreeMode(body, st, focused)
	case planFlame:
		v.drawFlameMode(body, st, focused)
	case planInsights:
		v.drawInsights(body, st, focused)
	}
}

// drawHeadline is the plan's one-line summary, with the comparison against
// the previous plan of the same statement on the right.
func (v *planView) drawHeadline(s Surface, st styles) {
	p := v.plan
	band := st.header
	s.Fill(band)
	x := s.Put(1, 0, "◈ ", band)
	x = s.Put(x, 0, strings.TrimPrefix(p.Headline(), "Plan · "), band.WithFg(st.base.Fg))
	if cmp, good := v.comparison(); cmp != "" {
		cs := band.WithFg(st.ok.Fg).Bold()
		if !good {
			cs = band.WithFg(st.err.Fg).Bold()
		}
		if s.W()-x-3 > width(cmp) {
			s.PutRight(s.W()-1, 0, cmp, cs)
		}
	}
}

// comparison describes this plan against the previous one of the same
// statement, by the best figure both have: measured time, else estimated
// cost. good says whether it went the right way.
func (v *planView) comparison() (text string, good bool) {
	a, b := v.prev, v.plan
	if a == nil {
		return "", false
	}
	var before, after float64
	var f func(float64) string
	switch {
	case a.ExecutionMs > 0 && b.ExecutionMs > 0:
		before, after, f = a.ExecutionMs, b.ExecutionMs, explain.FmtMs
	case a.Root.HasCost && b.Root.HasCost:
		before, after, f = a.Root.TotalCost, b.Root.TotalCost, func(c float64) string { return "cost " + explain.FmtCost(c) }
	default:
		return "", false
	}
	if before <= 0 {
		return "", false
	}
	change := (after - before) / before * 100
	arrow := "▼"
	if change > 0 {
		arrow = "▲"
	}
	if math.Abs(change) < 1 {
		return fmt.Sprintf("same as before: %s", f(after)), true
	}
	return fmt.Sprintf("was %s → %s %s%.0f%%", f(before), f(after), arrow, math.Abs(change)), change < 0
}

// drawChips draws the view switcher, the metric picker and the actions.
func (v *planView) drawChips(s Surface, st styles) {
	p := v.plan
	x := 1
	addChip := func(id planChipID, label string, on bool) {
		sty := st.button
		switch {
		case v.hoverChip == id:
			sty = st.buttonHover
		case on:
			sty = st.buttonHot
		}
		r := chip(s, x, 0, " "+label+" ", sty)
		v.chips = append(v.chips, planChip{id, r})
		x += r.W + 1
	}
	addChip(chipTree, "Tree", v.mode == planTree)
	addChip(chipFlame, "Flame", v.mode == planFlame)
	crit, warn := 0, 0
	for _, in := range p.Insights {
		switch in.Severity {
		case explain.SevCrit:
			crit++
		case explain.SevWarn:
			warn++
		}
	}
	label := "Insights"
	if crit > 0 {
		label += fmt.Sprintf(" ✖%d", crit)
	}
	if warn > 0 {
		label += fmt.Sprintf(" ▲%d", warn)
	}
	addChip(chipInsights, label, v.mode == planInsights)

	// right side first, so the metric chips give way on a narrow pane
	right := []struct {
		id    planChipID
		label string
	}{{chipResults, "▦ Results"}, {chipCopy, "⧉ Copy"}, {chipBrowser, "↗ Browser"}}
	if !p.Analyzed && !p.Measured {
		right = append(right, struct {
			id    planChipID
			label string
		}{chipAnalyze, "▶ Analyze"})
	}
	rx := s.W() - 1
	for _, c := range right {
		w := width(c.label) + 2
		if rx-w <= x+20 {
			break
		}
		rx -= w
		sty := st.button
		if v.hoverChip == c.id {
			sty = st.buttonHover
		} else if c.id == chipAnalyze {
			sty = st.buttonHot
		}
		r := chip(s, rx, 0, " "+c.label+" ", sty)
		v.chips = append(v.chips, planChip{c.id, r})
		rx--
	}

	ms := p.Metrics()
	if x+10 < rx && len(ms) > 1 {
		x = s.Put(x+1, 0, "size by", st.muted) + 1
		for i, m := range ms {
			if x+width(m.Label())+3 >= rx {
				break
			}
			addChip(chipMetric+planChipID(i), m.Label(), m == v.metric)
		}
	}
}

// drawTreeMode lays out the tree and, where there is room, the details.
func (v *planView) drawTreeMode(s Surface, st styles, focused bool) {
	w, h := s.W(), s.H()
	switch {
	case w >= 100:
		dw := max(34, min(w*32/100, 56))
		v.drawTree(s.Sub(Rect{0, 0, w - dw, h}), st, focused)
		v.drawDetail(s.Sub(Rect{w - dw, 0, dw, h}), st, true)
	case h >= 16:
		dh := max(6, min(h*2/5, 14))
		v.drawTree(s.Sub(Rect{0, 0, w, h - dh}), st, focused)
		v.drawDetail(s.Sub(Rect{0, h - dh, w, dh}), st, false)
	default:
		v.drawTree(s, st, focused)
	}
}

// Column widths of the tree's right-hand figures.
const (
	colRows   = 12
	colFactor = 6
	colValue  = 9
	colPct    = 4
	colBar    = 10
)

// drawTree draws the step list: tree lines, figures, share bars.
func (v *planView) drawTree(s Surface, st styles, focused bool) {
	p := v.plan
	v.tree = s.Rect()
	w := s.W()
	// the figures give way right to left as the pane narrows
	// the est column only when some step's estimate is off by ×2 or more —
	// an always-empty column would cost the step names its width
	anyFactor := false
	for _, n := range p.Nodes() {
		if _, f := explain.RowsCell(n, p.Analyzed); f != "" {
			anyFactor = true
		}
	}
	showBar, showFactor := w >= 70, w >= 56 && anyFactor
	numW := colRows + 1 + colValue + 1 + colPct
	if showFactor {
		numW += colFactor + 1
	}
	if showBar {
		numW += colBar + 1
	}
	if v.metric == explain.MetricShape {
		numW = colRows + 1
		if showBar {
			numW += colBar + 1
		}
	}
	nameW := max(w-numW-1, 12)

	// column header
	hs := s.Sub(Rect{0, 0, w, 1})
	hs.Fill(st.header)
	hs.Put(1, 0, "step", st.header)
	hx := nameW + 1
	rowsLabel := "rows"
	switch {
	case p.Analyzed:
	case p.Root != nil && p.Metrics()[0] == explain.MetricShape:
		rowsLabel = "table size" // no estimates at all: the sizes db looked up
	default:
		rowsLabel = "est. rows"
	}
	hs.PutRight(hx+colRows, 0, rowsLabel, st.header)
	hx += colRows + 1
	if v.metric != explain.MetricShape {
		if showFactor {
			hs.PutRight(hx+colFactor, 0, "est", st.header)
			hx += colFactor + 1
		}
		hs.PutRight(hx+colValue, 0, "self "+v.metric.Label(), st.header)
		hx += colValue + 1
		hs.PutRight(hx+colPct, 0, "%", st.header)
		hx += colPct + 1
	}
	if showBar && hx+colBar <= w {
		hs.Put(hx, 0, "share", st.header)
	}

	vis := v.visible()
	v.visRows = max(s.H()-1, 0)
	if v.follow {
		v.ensureVisible()
		v.follow = false
	}
	v.top = max(0, min(v.top, max(len(vis)-v.visRows, 0)))
	marks := v.insightMarks()
	for i := 0; i < v.visRows && v.top+i < len(vis); i++ {
		tl := vis[v.top+i]
		n := tl.Node
		y := i + 1
		row := s.Sub(Rect{0, y, w, 1})
		bg := st.base
		switch {
		case n.ID == v.cur && focused:
			bg = st.sel
		case n.ID == v.cur:
			bg = st.hover.Bold()
		case n.ID == v.hoverNode:
			bg = st.hover
		}
		row.Fill(bg)
		on := func(sty Style) Style { return sty.WithBg(bg.Bg) }
		if n.NeverExecuted {
			on = func(sty Style) Style { return sty.WithBg(bg.Bg).WithFg(st.muted.Fg).Dim() }
		}

		// The fold marker takes the place of the connector's dash, so a
		// step with children reads "├▾ ⋈ Hash Join" and a leaf "├─ ▤ Seq
		// Scan": every glyph stays in the same column, with no blank gutter
		// where a leaf has nothing to fold.
		name := row.Sub(Rect{0, 0, nameW, 1})
		pre := tl.Prefix
		fold := ""
		if len(n.Children) > 0 {
			fold = "▾"
			if v.collapsed[n.ID] {
				fold = "▸"
			}
			if pre == "" {
				pre = " " // the root: the marker stands alone
			}
			pre = strings.TrimSuffix(strings.TrimSuffix(pre, " "), "─")
		}
		x := name.Put(1, 0, pre, on(st.border))
		tr := Rect{name.Rect().X + x, name.Rect().Y, 1, 1}
		if fold != "" {
			x = name.Put(x, 0, fold+" ", on(st.accent).Bold())
		}
		x = name.Put(x, 0, n.Kind.Glyph()+" ", on(kindStyle(st, n.Kind)))
		// what trails the name — the finding marker, the fold count — is
		// reserved first, so a long target is cut with "…" before they are
		trail := ""
		sev, marked := marks[n.ID]
		if marked {
			trail += " " + sev.Glyph()
		}
		if v.collapsed[n.ID] {
			trail += fmt.Sprintf(" +%d", countBelow(n))
		}
		room := max(nameW-x-width(trail)-1, 4)
		op := truncate(n.Op, room)
		x = name.Put(x, 0, op, on(st.base).Bold())
		if t := n.Target(); t != "" && room-width(op) > 4 {
			x = name.Put(x, 0, truncate(" · "+t, room-width(op)), on(st.base))
		}
		if marked {
			x = name.Put(x+1, 0, sev.Glyph(), on(sevStyle(st, sev)).Bold())
		}
		if v.collapsed[n.ID] {
			x = name.Put(x+1, 0, fmt.Sprintf("+%d", countBelow(n)), on(st.muted))
		}
		if sum := n.Summary(); sum != "" && nameW-x-3 > 6 {
			name.Put(x+2, 0, truncate(sum, nameW-x-3), on(st.muted))
		}

		// figures
		fx := nameW + 1
		rows, factor := explain.RowsCell(n, p.Analyzed)
		row.PutRight(fx+colRows, 0, rows, on(st.muted))
		fx += colRows + 1
		share := p.Share(n, v.metric)
		if v.metric != explain.MetricShape {
			if showFactor {
				row.PutRight(fx+colFactor, 0, factor, on(st.warn))
				fx += colFactor + 1
			}
			row.PutRight(fx+colValue, 0, explain.FmtMetric(v.metric, n.Self(v.metric)), on(st.base))
			fx += colValue + 1
			row.PutRight(fx+colPct, 0, fmt.Sprintf("%.0f%%", share*100), on(heatStyle(st, share)))
			fx += colPct + 1
		}
		if showBar {
			row.Put(fx, 0, explain.Bar(share, colBar), on(heatStyle(st, share)))
		}
		v.rows = append(v.rows, planRowHit{id: n.ID, r: row.Rect(), twisty: tr})
	}
	if len(vis) > v.visRows {
		drawVBar(s.Sub(Rect{w - 1, 1, 1, v.visRows}), st, v.top, v.visRows, len(vis))
	}
}

// insightMarks is the most severe finding per step, for the tree's markers.
func (v *planView) insightMarks() map[int]explain.Severity {
	rank := map[explain.Severity]int{explain.SevInfo: 1, explain.SevWarn: 2, explain.SevCrit: 3}
	out := map[int]explain.Severity{}
	for _, in := range v.plan.Insights {
		if in.NodeID < 0 || in.Severity == explain.SevInfo {
			continue
		}
		if rank[in.Severity] > rank[out[in.NodeID]] {
			out[in.NodeID] = in.Severity
		}
	}
	return out
}

func countBelow(n *explain.Node) int {
	c := 0
	for _, k := range n.Children {
		c += 1 + countBelow(k)
	}
	return c
}

// detailEntry is one item of the detail panel: a labeled figure or
// property (key set), a line of prose (key empty), or a rule. Entries are
// wrapped at draw time, once the key column's width is known.
type detailEntry struct {
	key, val string
	st       Style
	rule     bool
	indent   int // prose: continuation lines' indent
}

// drawDetail describes the selected step: every figure the engine gave,
// every property in its own words, and the findings about it.
func (v *planView) drawDetail(s Surface, st styles, side bool) {
	v.detail = s.Rect()
	bg := st.panel
	s.Fill(bg)
	if side {
		for y := 0; y < s.H(); y++ {
			s.Put(0, y, "│", onBg(st.border, bg))
		}
		s = s.Sub(Rect{2, 0, s.W() - 3, s.H()})
	} else {
		s.Put(0, 0, strings.Repeat("─", s.W()), onBg(st.border, bg))
		s = s.Sub(Rect{1, 1, s.W() - 2, s.H() - 1})
	}
	n := v.node()
	if n == nil {
		return
	}
	// title: glyph and operation, then what it works on
	x := s.Put(0, 0, n.Kind.Glyph()+" ", onBg(kindStyle(st, n.Kind), bg))
	s.Put(x, 0, truncate(n.Op, s.W()-x), onBg(st.accent, bg).Bold())
	top := 1
	if t := n.Target(); t != "" {
		s.Put(0, 1, truncate(t, s.W()), onBg(st.base, bg))
		top = 2
	}
	body := s.Sub(Rect{0, top, s.W(), s.H() - top})

	// Lay out: the key column fits the keys up to a third of the panel, and
	// values wrap to what is left — minus a cell for the scrollbar, which
	// may appear. A key longer than the column ("Rows Removed by Filter")
	// gets a line of its own with its value under it, rather than widening
	// the column and wrapping every other value.
	entries := v.detailEntries(n, st, bg)
	textW := max(body.W()-1, 10)
	keyW := 0
	for _, e := range entries {
		keyW = max(keyW, width(e.key))
	}
	keyW = min(keyW, max(10, textW/3))
	valW := max(textW-keyW-2, 8)
	type line struct {
		key, val string
		st       Style
		rule     bool
		cont     bool // a wrapped value's later line: drawn in the value column
	}
	var lines []line
	for _, e := range entries {
		switch {
		case e.rule:
			lines = append(lines, line{rule: true})
		case e.key != "":
			own := width(e.key) > keyW
			if own {
				lines = append(lines, line{key: e.key, st: e.st})
			}
			for i, l := range wrap(e.val, valW) {
				if i == 0 && !own {
					lines = append(lines, line{key: e.key, val: l, st: e.st})
				} else {
					lines = append(lines, line{val: l, st: e.st, cont: true})
				}
			}
		default:
			pad := strings.Repeat(" ", e.indent)
			for i, l := range wrap(e.val, textW-e.indent) {
				if i > 0 {
					l = pad + l
				}
				lines = append(lines, line{val: l, st: e.st})
			}
		}
	}
	v.detailTop = max(0, min(v.detailTop, max(len(lines)-body.H(), 0)))
	for i := 0; i < body.H() && v.detailTop+i < len(lines); i++ {
		l := lines[v.detailTop+i]
		switch {
		case l.rule:
			body.Put(0, i, strings.Repeat("┄", textW), onBg(st.border, bg))
		case l.cont:
			body.Put(keyW+2, i, l.val, l.st)
		case l.key == "":
			body.Put(0, i, l.val, l.st)
		case l.val == "":
			body.Put(0, i, truncate(l.key, textW), onBg(st.muted, bg)) // a long key, alone
		default:
			body.Put(0, i, l.key, onBg(st.muted, bg))
			body.Put(keyW+2, i, l.val, l.st)
		}
	}
	if len(lines) > body.H() {
		drawVBar(body.Sub(Rect{body.W() - 1, 0, 1, body.H()}), st, v.detailTop, body.H(), len(lines))
	}
}

// detailEntries builds the detail panel's content for step n.
func (v *planView) detailEntries(n *explain.Node, st styles, bg Style) []detailEntry {
	p := v.plan
	var out []detailEntry
	val := onBg(st.base, bg)
	add := func(k, s string, sty Style) {
		if s != "" {
			out = append(out, detailEntry{key: k, val: s, st: sty})
		}
	}
	prose := func(s string, sty Style, indent int) {
		if s != "" {
			out = append(out, detailEntry{val: s, st: sty, indent: indent})
		}
	}
	rule := func() { out = append(out, detailEntry{rule: true}) }

	if n.Relationship != "" {
		add("feeds", "parent, as "+n.Relationship, onBg(st.muted, bg))
	}
	if n.NeverExecuted {
		add("status", "never executed", onBg(st.warn, bg))
	}
	if n.HasActual {
		share := p.Share(n, explain.MetricTime)
		add("time", fmt.Sprintf("%s total · %s self (%.0f%%)", explain.FmtMs(n.TotalMs), explain.FmtMs(n.SelfMs), share*100),
			onBg(heatStyle(st, share), bg))
		if n.Loops > 1 {
			add("loops", fmt.Sprintf("%s × %s each", explain.FmtCount(n.Loops), explain.FmtMs(n.ActualMs)), val)
		}
		if n.Participants > 1 {
			add("parallel", fmt.Sprintf("shared by %s processes", explain.FmtCount(n.Participants)), val)
		}
	}
	switch {
	case n.HasActual && n.HasEst:
		r := fmt.Sprintf("%s actual · %s estimated", explain.FmtCount(n.RowsOut), explain.FmtCount(n.EstRows*math.Max(n.Loops, 1)))
		if f := explain.FmtFactor(n.Misestimate); f != "" {
			r += " (" + f + ")"
		}
		add("rows", r, val)
	case n.HasActual:
		add("rows", explain.FmtCount(n.RowsOut)+" actual", val)
	case n.HasEst:
		add("rows", "~"+explain.FmtCount(n.EstRows)+" estimated", val)
	}
	if n.RowsRemoved > 0 {
		rem := n.RowsRemoved * math.Max(n.Loops, 1)
		add("discarded", fmt.Sprintf("%s rows (%.0f%% of those read)", explain.FmtCount(rem),
			rem/math.Max(rem+n.RowsOut, 1)*100), onBg(st.warn, bg))
	}
	if n.TableRows > 0 {
		add("table size", explain.FmtCount(n.TableRows)+" rows", val)
	}
	if n.HasCost {
		add("cost", fmt.Sprintf("%s → %s · self %s", explain.FmtCost(n.StartupCost), explain.FmtCost(n.TotalCost),
			explain.FmtCost(n.SelfCost)), val)
	}
	if n.Width > 0 {
		add("row width", fmt.Sprintf("%.0f bytes", n.Width), val)
	}
	if n.SharedHit+n.SharedRead > 0 {
		hit := n.SharedHit / (n.SharedHit + n.SharedRead) * 100
		add("buffers", fmt.Sprintf("%s pages · %.0f%% from cache", explain.FmtCount(n.SharedHit+n.SharedRead), hit), val)
	}
	if n.SortSpill || n.Batches > 1 {
		how := "to disk"
		if n.Batches > 1 {
			how = fmt.Sprintf("in %.0f batches", n.Batches)
		}
		if n.SpillKB > 0 {
			how = explain.FmtKB(n.SpillKB) + " " + how
		}
		add("spilled", how, onBg(st.err, bg))
	}
	if !n.HasActual && !n.HasEst && !n.HasCost && n.TableRows == 0 {
		add("figures", "none — this engine reports no costs or row counts", onBg(st.muted, bg))
	}

	if len(n.Props) > 0 {
		rule()
		for _, pr := range n.Props {
			add(pr.Key, pr.Value, val)
		}
	}

	for _, in := range p.Insights {
		if in.NodeID != n.ID {
			continue
		}
		rule()
		prose(in.Severity.Glyph()+" "+in.Title, onBg(sevStyle(st, in.Severity), bg).Bold(), 2)
		if in.Detail != "" {
			prose("  "+in.Detail, onBg(st.base, bg), 2)
		}
		if in.Fix != "" {
			prose("  → "+in.Fix, onBg(st.accent, bg), 4)
		}
		if in.SQL != "" {
			prose("    "+in.SQL, onBg(st.synKeyword, bg), 4)
		}
	}
	if n.ID == p.Root.ID && len(p.Notes) > 0 {
		rule()
		for _, note := range p.Notes {
			prose("ⓘ "+note, onBg(st.muted, bg).Italic(), 2)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Flame graph
// ---------------------------------------------------------------------------

// drawFlameMode draws the flame graph above the selected step's details.
//
// It is an icicle — the root spans the full width at the top and each step's
// children share its width below it, in proportion to their INCLUSIVE share
// of the metric (their own plus everything under them). Where a parent is
// wider than its children together, the gap is the parent's own work. So a
// wide block is an expensive subtree, and a wide block with nothing under it
// is an expensive step — the one to look at. Color is each step's OWN share,
// on the same accent → warn → err scale as the tree's bars.
func (v *planView) drawFlameMode(s Surface, st styles, focused bool) {
	w, h := s.W(), s.H()
	detailH := 0
	if h >= 14 {
		detailH = max(5, min(h/3, 10))
	}
	g := s.Sub(Rect{0, 0, w, h - detailH})
	v.drawFlame(g, st, focused)
	if detailH > 0 {
		v.drawDetail(s.Sub(Rect{0, h - detailH, w, detailH}), st, false)
	}
}

func (v *planView) drawFlame(s Surface, st styles, focused bool) {
	p := v.plan
	root := p.Node(v.flameRoot)
	if root == nil {
		root, v.flameRoot = p.Root, 0
	}
	// breadcrumb: where the graph is zoomed to, and how to get out
	crumb := "whole plan"
	if root != p.Root {
		var path []string
		for a := root; a != nil; a = a.Parent() {
			path = append([]string{a.Op}, path...)
		}
		crumb = strings.Join(path, " ▸ ") + "   (Esc or click here: zoom out)"
	}
	label := "sized by " + v.metric.Label()
	if v.metric == explain.MetricShape {
		label = "sized by shape — the engine reports no numbers"
	}
	s.Put(1, 0, truncate(crumb, s.W()-width(label)-4), st.muted)
	s.PutRight(s.W()-1, 0, label, st.muted)
	g := s.Sub(Rect{0, 1, s.W(), s.H() - 1})
	v.flameR = g.Rect()

	depth := 0
	var measure func(n *explain.Node, d int)
	measure = func(n *explain.Node, d int) {
		depth = max(depth, d)
		for _, c := range n.Children {
			measure(c, d+1)
		}
	}
	measure(root, 0)
	levels := depth + 1
	lvH := 1
	if levels*2 <= g.H() {
		lvH = 2
	}
	shown := min(levels, max(g.H()/lvH, 1))

	m := v.metric
	x0 := float64(g.Rect().X)
	var place func(n *explain.Node, a, b float64, d int)
	place = func(n *explain.Node, a, b float64, d int) {
		if d >= shown || b-a < 0.5 {
			return
		}
		ia, ib := int(math.Round(a)), int(math.Round(b))
		if ib <= ia {
			ib = ia + 1
		}
		v.flame = append(v.flame, flameBox{id: n.ID, x0: ia, x1: ib, y: g.Rect().Y + d*lvH, h: lvH})
		incl := n.Inclusive(m)
		cx := a
		for _, c := range n.Children {
			cw := 0.0
			if incl > 0 {
				cw = (b - a) * c.Inclusive(m) / incl
			}
			place(c, cx, cx+cw, d+1)
			cx += cw
		}
	}
	place(root, x0, x0+float64(g.W()), 0)

	for _, b := range v.flame {
		n := p.Node(b.id)
		share := p.Share(n, m)
		bgc := hex(flameColor(st.pal, share))
		sty := Style{Fg: textOn(st.pal, flameColor(st.pal, share)), Bg: bgc}
		switch {
		case b.id == v.cur:
			sty = st.buttonHover
			if !focused {
				sty = st.sel
			}
		case b.id == v.hoverNode:
			sty = sty.WithBg(hex(theme.Blend(st.pal.Fg, flameColor(st.pal, share), 0.25)))
		}
		if n.NeverExecuted {
			sty = sty.Dim()
		}
		w := b.x1 - b.x0
		inner := w
		if w >= 3 {
			inner = w - 1 // a one-cell gap keeps siblings apart
		}
		blk := s.c.Sub(Rect{b.x0, b.y, inner, b.h})
		blk.Fill(sty)
		if inner >= 3 {
			text := n.Kind.Glyph() + " " + n.Op
			if t := n.Target(); t != "" && lvH == 1 {
				text += " · " + t
			}
			blk.Put(1, 0, truncate(text, inner-1), sty.Bold())
			if lvH == 2 {
				sub := explain.FmtMetric(m, n.Inclusive(m))
				if m == explain.MetricShape {
					sub = n.Target()
				} else if t := n.Target(); t != "" {
					sub += " · " + t
				}
				blk.Put(1, 1, truncate(sub, inner-1), sty)
			}
		}
	}
	if shown < levels {
		g.PutRight(g.W()-1, g.H()-1, fmt.Sprintf(" ▼ %d deeper levels — double-click a block to zoom in ", levels-shown), st.warn)
	}
	// the hovered step's figures, where the eye is
	if n := p.Node(v.hoverNode); n != nil && v.mode == planFlame {
		info := n.Title()
		if m != explain.MetricShape {
			info += fmt.Sprintf(" · %s incl. · %.0f%% own", explain.FmtMetric(m, n.Inclusive(m)), p.Share(n, m)*100)
		}
		g.Put(1, g.H()-1, truncate(" "+info+" ", g.W()-2), st.raised)
	}
}

// flameColor is the fill of a step's block: its own share on the heat
// scale, faded toward the panel color when small, so a graph of mostly-cheap
// steps reads as calm and the one hot block stands out.
func flameColor(p theme.Palette, share float64) string {
	return theme.Blend(heatHex(p, share), p.Panel2, 0.35+0.65*math.Min(share*2.5, 1))
}

// textOn picks the text color that reads on a block: the dark background on
// a light fill, the light foreground on a dark one.
func textOn(p theme.Palette, fill string) Color {
	r, g, b, ok := theme.ParseHex(fill)
	if !ok {
		return hex(p.Fg)
	}
	if 0.299*float64(r)+0.587*float64(g)+0.114*float64(b) > 140 {
		return hex(p.Bg)
	}
	return hex(p.Fg)
}

// ---------------------------------------------------------------------------
// Insights
// ---------------------------------------------------------------------------

// drawInsights lists the findings as cards: what, why, what to do, and the
// statement that does it with ⧉ copy and ⤓ insert beside it.
func (v *planView) drawInsights(s Surface, st styles, focused bool) {
	p := v.plan
	type line struct {
		text string
		sty  Style
		idx  int
		kind int // 0 text, 1 title, 2 sql
	}
	var lines []line
	w := s.W() - 4
	for i, in := range p.Insights {
		if i > 0 {
			lines = append(lines, line{idx: -1})
		}
		title := in.Severity.Glyph() + " " + in.Title
		lines = append(lines, line{text: title, sty: sevStyle(st, in.Severity).Bold(), idx: i, kind: 1})
		if n := p.Node(in.NodeID); n != nil {
			lines = append(lines, line{text: "  at " + n.Title(), sty: st.muted, idx: i})
		}
		for _, l := range wrap(in.Detail, w-2) {
			lines = append(lines, line{text: "  " + l, sty: st.base, idx: i})
		}
		if in.Fix != "" {
			for j, l := range wrap(in.Fix, w-4) {
				lead := "  → "
				if j > 0 {
					lead = "    "
				}
				lines = append(lines, line{text: lead + l, sty: st.accent, idx: i})
			}
		}
		if in.SQL != "" {
			lines = append(lines, line{text: in.SQL, sty: st.synKeyword.WithBg(st.chatCode.Bg), idx: i, kind: 2})
		}
	}
	if len(p.Notes) > 0 {
		lines = append(lines, line{idx: -1}, line{text: "About this plan", sty: st.muted.Bold(), idx: -1})
		for _, note := range p.Notes {
			for _, l := range wrap("ⓘ "+note, w) {
				lines = append(lines, line{text: l, sty: st.muted.Italic(), idx: -1})
			}
		}
	}
	// keep the selected card's title in view
	for i, l := range lines {
		if l.idx == v.insCur && l.kind == 1 {
			if i < v.insTop {
				v.insTop = i
			} else if i >= v.insTop+s.H() {
				v.insTop = i - s.H() + 3
			}
			break
		}
	}
	v.insTop = max(0, min(v.insTop, max(len(lines)-s.H(), 0)))
	cards := map[int]*insightHit{}
	for y := 0; y < s.H() && v.insTop+y < len(lines); y++ {
		l := lines[v.insTop+y]
		row := s.Sub(Rect{0, y, s.W(), 1})
		sel := l.idx >= 0 && l.idx == v.insCur
		if sel {
			row.Put(0, 0, "▌", st.accent)
			if focused && l.kind == 1 {
				row.Sub(Rect{1, 0, row.W() - 1, 1}).Fill(st.hover)
			}
		}
		sty := l.sty
		if sel && focused && l.kind == 1 {
			sty = sty.WithBg(st.hover.Bg)
		}
		switch l.kind {
		case 2:
			x := row.Put(4, 0, " "+truncate(l.text, max(row.W()-30, 10))+" ", sty)
			h := cards[l.idx]
			if h != nil {
				h.copySQL = chip(row, x+1, 0, " ⧉ copy ", pick(v.hoverInsig == l.idx, st.buttonHover, st.button))
				h.insertSQL = chip(row, x+1+h.copySQL.W+1, 0, " ⤓ insert ", st.button)
			}
		default:
			x := row.Put(2, 0, l.text, sty)
			if l.kind == 1 && p.Insights[l.idx].NodeID >= 0 {
				h := &insightHit{idx: l.idx}
				h.goTo = chip(row, min(x+2, row.W()-12), 0, " go to step ", st.button)
				cards[l.idx] = h
			}
		}
		if l.idx >= 0 {
			h := cards[l.idx]
			if h == nil {
				h = &insightHit{idx: l.idx}
				cards[l.idx] = h
			}
			r := row.Rect()
			if h.r.Empty() {
				h.r = r
			} else {
				h.r.H = r.Y + 1 - h.r.Y
			}
		}
	}
	for i := range p.Insights {
		if h := cards[i]; h != nil {
			v.ins = append(v.ins, *h)
		}
	}
	if len(lines) > s.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H()}), st, v.insTop, s.H(), len(lines))
	}
}

// ---------------------------------------------------------------------------
// Colors
// ---------------------------------------------------------------------------

// heatHex maps a share (0–1) onto accent → warn → err: calm under a fifth,
// alarming past a half — the thresholds the text renderer colors bars by.
func heatHex(p theme.Palette, share float64) string {
	switch {
	case share <= 0.2:
		return p.Accent
	case share <= 0.5:
		return theme.Blend(p.Warn, p.Accent, (share-0.2)/0.3)
	}
	return theme.Blend(p.Err, p.Warn, math.Min((share-0.5)/0.3, 1))
}

func heatStyle(st styles, share float64) Style {
	return st.base.WithFg(hex(heatHex(st.pal, share)))
}

// kindStyle colors a step's glyph: a full scan in the warm tone that asks
// for a look, an index access in the accent, the rest muted.
func kindStyle(st styles, k explain.Kind) Style {
	switch k {
	case explain.KindScan:
		return st.warn
	case explain.KindIndex:
		return st.accent
	case explain.KindModify:
		return st.err
	}
	return st.muted
}

func sevStyle(st styles, s explain.Severity) Style {
	switch s {
	case explain.SevCrit:
		return st.err
	case explain.SevWarn:
		return st.warn
	}
	return st.accent
}
