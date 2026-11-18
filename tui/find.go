package tui

import (
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// Find in the grid, the log and the editor (N-292). The Tables pane had
// its own filter line (tablefind.go); nothing else could be searched, so a
// value in a 1,000-row result, an error twenty screens up the log, or a
// name in a long migration was found by scrolling.
//
// One find line serves the three panes. It is drawn on the pane's last row
// (the pane's own content gives that row up while the line is open, so
// nothing is hidden under it) and works the same way in each:
//
//	┌ Results · 1,000 rows ───────────────────────┐
//	│ id  name      email                          │
//	│ 41  Ada       ada@example.com   ◄ the match │   the cursor, on the cell
//	│ …                                            │
//	│ ⌕ example▏                    n next · N prev │   the find line
//	└──────────────────────────────────────────────┘
//
//	/ (grid, log) · Ctrl+F (those and the editor)   open the line, or go back into it
//	typing                                         jump to the first match from the cursor on
//	Enter · ↓                                      the next match (the line stays open)
//	⇧Enter · ↑                                     the previous one
//	Esc                                            close the line: the cursor stays on the match
//	n · N  (grid, log; off the line)               next · previous match with the line's text
//
// Matching is case-insensitive and anywhere in the text: a cell's display
// text (the grid), a line's message (the log) or the buffer (the editor,
// where the match is selected, so typing replaces it and Ctrl+C copies
// it). Every search wraps at the end and says so. A query that finds
// nothing is drawn in the error colour and moves nothing.
//
// In the editor the line also takes ":N", go to line N — Ctrl+G is the
// cats agents' key, and a second line for one number would be one more
// thing to learn.
//
// The state is the Model's, one line per pane, and is dropped on a tab
// switch: a find is about what is on screen now, and a parked tab's grid
// and log are not.

// findTarget is which pane a find line searches.
type findTarget int

const (
	findGrid findTarget = iota
	findLog
	findEditor
)

// findLine is one pane's find: its text, and whether keys go to it.
type findLine struct {
	target  findTarget
	input   *editor
	editing bool
	miss    bool   // the last search found nothing
	note    string // drawn at the line's right end ("wrapped", "line 42")
}

func newFindLine(t findTarget) *findLine {
	f := &findLine{target: t, input: newEditor(true), editing: true}
	switch t {
	case findEditor:
		f.input.placeholder = "find in the buffer, or :line"
	case findLog:
		f.input.placeholder = "find in the log"
	default:
		f.input.placeholder = "find in the results"
	}
	return f
}

// query is the line's text, as searched: case folded. Spaces inside and
// around are kept, since "id " and " id" find different things.
func (f *findLine) query() string { return foldLower(f.input.Text()) }

// findOf is the find line of a pane, or nil.
func (m *Model) findOf(t findTarget) *findLine {
	switch t {
	case findGrid:
		return m.gfind
	case findLog:
		return m.lfind
	}
	return m.efind
}

// setFind replaces a pane's find line (nil closes it).
func (m *Model) setFind(t findTarget, f *findLine) {
	switch t {
	case findGrid:
		m.gfind = f
	case findLog:
		m.lfind = f
		if f == nil {
			m.logp.mark = -1
		}
	default:
		m.efind = f
	}
}

// focusFind is the find target of the focused pane, and whether it has
// one.
func (m *Model) focusFind() (findTarget, bool) {
	switch m.focus {
	case focusGrid:
		// the plan view has its own keys; a find is the grid's
		return findGrid, !(m.resTab == tabPlan && m.planv.plan != nil)
	case focusLog:
		return findLog, true
	case focusEditor:
		return findEditor, true
	}
	return 0, false
}

// openFind is / and Ctrl+F: into the focused pane's find line, made on
// the first use, its text kept from the last find so Enter repeats it.
func (m *Model) openFind() bool {
	t, ok := m.focusFind()
	if !ok {
		return false
	}
	f := m.findOf(t)
	if f == nil {
		f = newFindLine(t)
		m.setFind(t, f)
	}
	f.editing = true
	if t == findEditor {
		m.compl = nil // the popup hangs off the caret, which the line now has
	}
	return true
}

// findEditing is the find line keys go to, or nil: the focused pane's,
// while it is being typed in.
func (m *Model) findEditingLine() *findLine {
	t, ok := m.focusFind()
	if !ok {
		return nil
	}
	if f := m.findOf(t); f != nil && f.editing {
		return f
	}
	return nil
}

// findKey handles a key while a find line is being typed in.
func (m *Model) findKey(f *findLine, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		// close it; the cursor stays where the find put it
		m.setFind(f.target, nil)
		return nil
	case "enter", "down", "ctrl+n":
		if f.target == findEditor && m.gotoLine(f) {
			return nil
		}
		m.findStep(f, 1, true)
		return nil
	case "shift+enter", "up", "ctrl+p":
		m.findStep(f, -1, true)
		return nil
	case "tab", "shift+tab":
		// leave the line, keep the find (n/N work from the pane)
		f.editing = false
		dir := 1
		if k.String() == "shift+tab" {
			dir = -1
		}
		m.cycleFocus(dir)
		return nil
	case "ctrl+f":
		// pressed again on the line: the next match, as a browser's does
		m.findStep(f, 1, true)
		return nil
	}
	before := f.input.Text()
	f.input.HandleKey(k)
	if f.input.Text() != before {
		// a new query: from where the cursor is, the cell it is on
		// included, so typing on narrows to the match already found
		m.findStep(f, 1, false)
	}
	return nil
}

// findPaste puts pasted text in the line being typed in.
func (m *Model) findPaste(f *findLine, s string) {
	f.input.Insert(strings.ReplaceAll(strings.TrimRight(s, "\r\n"), "\n", " "))
	m.findStep(f, 1, false)
}

// findStep searches the line's pane for its text: dir 1 forward, -1 back.
// next false starts at the cursor itself (typing), true just past it (the
// next match). It moves the pane to the match, or marks a miss.
func (m *Model) findStep(f *findLine, dir int, next bool) {
	f.note, f.miss = "", false
	q := f.query()
	if q == "" {
		if f.target == findLog {
			m.logp.mark = -1
		}
		return
	}
	var found, wrapped bool
	switch f.target {
	case findGrid:
		found, wrapped = m.grid.find(q, dir, next)
	case findLog:
		found, wrapped = m.logp.find(q, dir, next)
	case findEditor:
		found, wrapped = editorFind(m.editor, q, dir, next)
		if found {
			m.drag.follow = true // scroll the selection into view
		}
	}
	f.miss = !found
	switch {
	case !found:
		f.note = "no match"
	case wrapped && dir > 0:
		f.note = "wrapped to the top"
	case wrapped:
		f.note = "wrapped to the end"
	}
}

// findNext is n / N off the line: the pane's find, again. It reports
// whether there was a find to repeat (else the key is the pane's).
func (m *Model) findNext(t findTarget, dir int) bool {
	f := m.findOf(t)
	if f == nil || f.input.Text() == "" {
		return false
	}
	m.findStep(f, dir, true)
	return true
}

// gotoLine is the editor line's ":N": the caret to the start of line N
// (clamped to the buffer), the line closed. It reports whether the text
// was a line number.
func (m *Model) gotoLine(f *findLine) bool {
	s := strings.TrimSpace(f.input.Text())
	if !strings.HasPrefix(s, ":") {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s[1:]))
	if err != nil {
		f.miss, f.note = true, "not a line number"
		return true
	}
	e := m.editor
	row := max(0, min(n-1, len(e.lines)-1))
	e.move(pos{row, 0}, false)
	m.drag.follow = true
	m.setFind(findEditor, nil)
	m.logf(logInfo, "line %d of %d", row+1, len(e.lines))
	return true
}

// drawFind draws a find line on row y of s, and returns the caret while it
// is being typed in and its pane has the keyboard.
func (m *Model) drawFind(f *findLine, s Surface, y int, focused bool) *caret {
	r := s.Sub(Rect{0, y, s.W(), 1})
	st := m.st.raised
	r.Fill(st)
	icon := st.WithFg(m.st.accent.Fg)
	textSt := st
	if f.miss {
		// the text in the error colour: what was typed is not there
		icon, textSt = st.WithFg(m.st.err.Fg), st.WithFg(m.st.err.Fg)
	}
	r.Put(1, 0, "⌕", icon)
	hint := f.note
	if hint == "" && f.input.Text() != "" {
		hint = "Enter next · ⇧Enter prev · Esc"
		if !f.editing && f.target != findEditor {
			hint = "n next · N prev"
		}
	}
	right := 0
	if hint != "" && r.W() > width(hint)+20 {
		right = width(hint) + 2
		r.PutRight(r.W()-1, 0, hint, st.WithFg(m.st.muted.Fg))
	}
	cx, cy, ok := f.input.Draw(r.Sub(Rect{3, 0, max(r.W()-4-right, 1), 1}), m.st, textSt, [2]int{}, true)
	if ok && f.editing && focused && m.modal == nil && m.menu == nil {
		return &caret{cx, cy}
	}
	return nil
}

// withFind lays a pane's body out around its find line: draw gets the
// rows above the line, and the line takes the last row. With no line,
// draw gets the whole body.
func (m *Model) withFind(f *findLine, s Surface, focused bool, draw func(Surface)) *caret {
	if f == nil || s.H() < 3 {
		draw(s)
		return nil
	}
	draw(s.Sub(Rect{0, 0, s.W(), s.H() - 1}))
	return m.drawFind(f, s, s.H()-1, focused)
}

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

// foldLower is s lower-cased for matching: ASCII by a byte loop that does
// not allocate when s is already lower case, anything else by
// strings.ToLower.
func foldLower(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			return strings.ToLower(s)
		}
		if 'A' <= c && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

// containsFold reports whether s holds q, ignoring case; q is already
// folded (foldLower). The common case — ASCII text, an ASCII query — is a
// scan with no allocation; a cell of a million-row result is tested per
// keystroke, so lower-casing every cell first would be a copy of the
// result each time.
func containsFold(s, q string) bool {
	return indexFold(s, q, 0) >= 0
}

// indexFold is the byte offset of the first q in s at or after from,
// ignoring case, or -1. q is folded. Text past ASCII is lowered and
// searched as strings; a rune whose lower case has another length in bytes
// (rare: a few Unicode letters) can then put the offset off by a byte or
// two, which only the editor reads, and it re-anchors on rune bounds.
func indexFold(s, q string, from int) int {
	if from < 0 {
		from = 0
	}
	if q == "" || from > len(s) {
		return -1
	}
	if !isASCII(q) || !isASCII(s[from:]) {
		low := strings.ToLower(s[from:])
		i := strings.Index(low, q)
		if i < 0 {
			return -1
		}
		return from + i
	}
	n := len(q)
	first := q[0]
	for i := from; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != first {
			continue
		}
		j := 1
		for j < n && lowerASCII(s[i+j]) == q[j] {
			j++
		}
		if j == n {
			return i
		}
	}
	return -1
}

// lastIndexFold is the byte offset of the last q in s that starts before
// before, ignoring case, or -1. Forward scans from the start: the buffers
// searched are at most a few MB, and a backward step is a key press.
func lastIndexFold(s, q string, before int) int {
	last := -1
	for i := indexFold(s, q, 0); i >= 0 && i < before; i = indexFold(s, q, i+1) {
		last = i
	}
	return last
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// ---------------------------------------------------------------------------
// The panes' searches
// ---------------------------------------------------------------------------

// find moves the grid's cursor to the next cell, in reading order (row by
// row, the shown columns left to right), whose display text holds q. next
// false tests the cursor's cell first. It reports a match and whether the
// search wrapped past the last cell (or, back, the first). A hidden
// column is not searched: a find takes what the user sees, as a copy does.
func (g *grid) find(q string, dir int, next bool) (found, wrapped bool) {
	rows, cols := g.Rows(), g.Cols()
	if rows == 0 || cols == 0 {
		return false, false
	}
	total := rows * cols
	at := g.cur.row*cols + g.cur.col
	start := 0
	if next {
		start = 1
	}
	for k := start; k <= total; k++ {
		if k == total && !next {
			break // the cursor's cell, once, is enough
		}
		i := at + dir*k
		w := i < 0 || i >= total
		i = ((i % total) + total) % total
		r, c := i/cols, i%cols
		// a NULL is matched by its "NULL", as it is drawn: a find takes
		// what the user sees
		text, _, ok := g.value(r, c)
		if ok && containsFold(text, q) {
			g.moveTo(r, c, false)
			return true, w
		}
	}
	return false, false
}

// find moves the log's mark to the next line holding q and scrolls it into
// view. Lines are searched by their message, not their timestamp.
func (l *logPane) find(q string, dir int, next bool) (found, wrapped bool) {
	n := len(l.lines)
	if n == 0 {
		return false, false
	}
	at := l.mark
	if at < 0 || at >= n {
		// no line yet: forward from the oldest shown, back from the newest
		at = 0
		if dir < 0 {
			at = n - 1
		}
		next = false
	}
	start := 0
	if next {
		start = 1
	}
	for k := start; k <= n; k++ {
		if k == n && !next {
			break
		}
		i := at + dir*k
		w := i < 0 || i >= n
		i = ((i % n) + n) % n
		if containsFold(l.lines[i].text, q) {
			l.mark = i
			l.reveal(i)
			return true, w
		}
	}
	l.mark = -1
	return false, false
}

// reveal scrolls so line i's first row is on screen, about a third of the
// way down, and unpins the view from the newest line. Before the first
// draw (no rows wrapped yet) it does nothing; the draw that follows keeps
// following the bottom.
func (l *logPane) reveal(i int) {
	if i >= len(l.nrows) || l.view.H <= 0 {
		return
	}
	row := 0
	for _, n := range l.nrows[:i] {
		row += n
	}
	maxTop := max(l.total-l.view.H, 0)
	l.top = max(0, min(row-l.view.H/3, maxTop))
	l.follow = l.top >= maxTop
}

// markRows is the visual row range [from, to) of the marked line, or an
// empty range.
func (l *logPane) markRows() (from, to int) {
	if l.mark < 0 || l.mark >= len(l.nrows) {
		return 0, 0
	}
	for _, n := range l.nrows[:l.mark] {
		from += n
	}
	return from, from + l.nrows[l.mark]
}

// editorFind selects the next q in the buffer from the caret (next false:
// a match at the caret itself counts; true: past the selection), or the
// previous one before it, wrapping at either end.
func editorFind(e *editor, q string, dir int, next bool) (found, wrapped bool) {
	text := e.Text()
	selText, selStart, selEnd := e.Selection()
	caret := e.Caret()
	if selText == "" {
		selStart, selEnd = caret, caret
	}
	i := -1
	if dir > 0 {
		from := selStart
		if next {
			from = selEnd
			if selEnd == selStart {
				from = selEnd + 1
			}
		}
		if i = indexFold(text, q, from); i < 0 {
			i, wrapped = indexFold(text, q, 0), true
		}
	} else {
		if i = lastIndexFold(text, q, selStart); i < 0 {
			i, wrapped = lastIndexFold(text, q, len(text)+1), true
		}
	}
	if i < 0 {
		return false, false
	}
	end := i + len(q)
	if !isASCII(q) {
		// the folded query may differ in length from the text it matched;
		// take as many runes of the text as the query has
		end = i
		for k := 0; k < utf8.RuneCountInString(q) && end < len(text); k++ {
			_, size := utf8.DecodeRuneInString(text[end:])
			end += size
		}
	}
	e.anc = e.posAt(i)
	e.cur = e.posAt(end)
	e.sel = e.anc != e.cur
	return true, wrapped
}

// findLineKey reports whether a key belongs to a find line being typed in:
// typed text, its own keys and the line editor's (movement, deleting a
// word). The app's other chords are not its: they go on to do what they
// always do, so Ctrl+R runs and Ctrl+Q quits with the line open.
func findLineKey(k tea.KeyPressMsg) bool {
	s := k.String()
	if len(k.Text) > 0 && !strings.HasPrefix(s, "ctrl+") && !strings.HasPrefix(s, "alt+") {
		return true // a character typed
	}
	switch s {
	case "esc", "enter", "shift+enter", "up", "down", "tab", "shift+tab", "ctrl+n", "ctrl+p", "ctrl+f",
		"backspace", "delete", "left", "right", "home", "end", "space",
		"ctrl+w", "ctrl+u", "ctrl+backspace", "alt+backspace", "ctrl+left", "ctrl+right",
		"alt+left", "alt+right", "shift+left", "shift+right", "shift+home", "shift+end",
		"ctrl+z":
		return true
	}
	return false
}

// findPaneKey is a find's keys in the grid and the log, off the line: /
// back into it, n and N to step, Esc to close it. It reports whether the
// key was one of them.
func (m *Model) findPaneKey(t findTarget, k tea.KeyPressMsg) bool {
	switch k.String() {
	case "/":
		return m.openFind()
	case "n":
		return m.findNext(t, 1)
	case "N":
		return m.findNext(t, -1)
	case "esc":
		if m.findOf(t) != nil {
			m.setFind(t, nil)
			return true
		}
	}
	return false
}

// sortByCursor is the grid's o: a header click on the cursor's column.
func (m *Model) sortByCursor() {
	g := m.grid
	if g.Cols() == 0 {
		m.log(logWarn, noResult)
		return
	}
	g.Sort(g.cur.col)
	switch {
	case g.sortCol < 0:
		m.log(logInfo, "back to the result's order")
	case g.sortDesc:
		m.logf(logInfo, "sorted by %s, descending — o again for the result's order", g.colName(g.cur.col))
	default:
		m.logf(logInfo, "sorted by %s — o again for descending", g.colName(g.cur.col))
	}
}

// dropFinds closes every find line and its log mark: a tab switch puts
// another grid, log and buffer on screen.
func (m *Model) dropFinds() {
	m.gfind, m.lfind, m.efind = nil, nil, nil
	for _, lp := range m.logs {
		lp.mark = -1
	}
	if m.logp != nil {
		m.logp.mark = -1
	}
}
