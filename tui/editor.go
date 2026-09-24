package tui

import (
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/rivo/uniseg"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// editor is a small text editor: the SQL editor, the chat composer, and (with
// single set) every one-line input field.
//
// WHY NOT bubbles/textarea. The textarea soft-wraps, and its wrap, cursor
// line and scroll are private — cats-todo had to copy its wrap() and walk a
// copy of the model line by line just to turn a click into a caret. SQL is
// written in lines the author chose, so this editor does not wrap: long lines
// scroll horizontally. That makes the screen a plain window onto the text,
//
//	screen (x, y)  ──►  row = top + y,  display column = left + x  ──►  rune
//
// and a click resolves to exactly one position with no guessing, which is the
// whole of "mouse as a first-class citizen" for a text field.
//
// POSITIONS are (row, col) with col counted in RUNES, not bytes or cells. The
// display column is derived when drawing and when mapping a click, using each
// rune's cell width, so a CJK identifier or an emoji in a string literal
// neither misplaces the caret nor shifts a click target.
//
// TABS become four spaces on the way in (typed, pasted, or loaded). Keeping a
// tab would mean a display column that depends on where it sits, which every
// mapping above would have to special-case; a SQL scratchpad loses nothing by
// storing spaces.
type editor struct {
	lines [][]rune
	cur   pos // the caret
	anc   pos // the other end of the selection, when sel is true
	sel   bool

	top, left int // first visible row, first visible display column
	goal      int // display column kept across up/down through short lines

	single      bool   // one line only: Enter is not ours, newlines paste as spaces
	sqlMode     bool   // syntax highlighting and the current-statement marker
	placeholder string // shown dimmed while the text is empty

	undo, redo []snapshot
	lastKind   editKind  // what the previous edit was, for coalescing typing
	lastAt     time.Time // when it happened

	// view is the text area as last drawn, in canvas coordinates: where the
	// mouse is mapped from. Zero until the first draw.
	view Rect

	// version counts edits; the highlighter caches against it.
	version int
	hlVer   int
	hl      [][]Style
}

// pos is a caret position: row, and column in runes.
type pos struct{ row, col int }

func (p pos) before(q pos) bool { return p.row < q.row || (p.row == q.row && p.col < q.col) }

// snapshot is one undo step.
type snapshot struct {
	lines [][]rune
	cur   pos
}

// editKind classifies an edit so consecutive typing undoes as one step
// rather than one character at a time.
type editKind int

const (
	editOther editKind = iota
	editType
	editDelete
)

// undoCap bounds the undo stack. Snapshots are whole-buffer copies, which is
// cheap at scratchpad sizes and makes undo exact; the cap keeps a long
// session from holding hundreds of copies of a large buffer.
const undoCap = 200

// coalesceWindow is how long a pause in typing may be before the next
// keystroke starts a new undo step.
const coalesceWindow = time.Second

func newEditor(single bool) *editor {
	return &editor{lines: [][]rune{{}}, single: single}
}

// Text returns the buffer.
func (e *editor) Text() string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// SetText replaces the buffer and clears undo — used for loading, not
// editing, so there is nothing to go back to.
func (e *editor) SetText(s string) {
	e.lines = splitLines(normalize(s, e.single))
	e.cur, e.sel = pos{}, false
	e.top, e.left, e.goal = 0, 0, 0
	e.undo, e.redo = nil, nil
	e.version++
}

// normalize applies the input rules: CRLF to LF, tabs to spaces, and for a
// one-line field, newlines to spaces.
func normalize(s string, single bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	if single {
		s = strings.ReplaceAll(s, "\n", " ")
	}
	return s
}

func splitLines(s string) [][]rune {
	parts := strings.Split(s, "\n")
	out := make([][]rune, len(parts))
	for i, p := range parts {
		out[i] = []rune(p)
	}
	return out
}

// Empty reports whether the buffer holds nothing but whitespace.
func (e *editor) Empty() bool { return strings.TrimSpace(e.Text()) == "" }

// offset converts a position to a byte offset into Text() — the currency
// sqlsplit deals in.
func (e *editor) offset(p pos) int {
	n := 0
	for r := 0; r < p.row && r < len(e.lines); r++ {
		n += len(string(e.lines[r])) + 1
	}
	if p.row < len(e.lines) {
		n += len(string(e.lines[p.row][:min(p.col, len(e.lines[p.row]))]))
	}
	return n
}

// posAt converts a byte offset back to a position.
func (e *editor) posAt(off int) pos {
	for r, l := range e.lines {
		lb := len(string(l))
		if off <= lb {
			return pos{r, len([]rune(string(l)[:max(off, 0)]))}
		}
		off -= lb + 1
	}
	last := len(e.lines) - 1
	return pos{last, len(e.lines[last])}
}

// Caret returns the caret's byte offset.
func (e *editor) Caret() int { return e.offset(e.cur) }

// Selection returns the selected text and its byte range, or "" when nothing
// is selected.
func (e *editor) Selection() (text string, start, end int) {
	if !e.sel || e.cur == e.anc {
		return "", e.Caret(), e.Caret()
	}
	a, b := e.ordered()
	start, end = e.offset(a), e.offset(b)
	return e.Text()[start:end], start, end
}

// ordered returns the selection's ends in buffer order.
func (e *editor) ordered() (pos, pos) {
	if e.cur.before(e.anc) {
		return e.cur, e.anc
	}
	return e.anc, e.cur
}

// ---------------------------------------------------------------------------
// Editing
// ---------------------------------------------------------------------------

// checkpoint records an undo step before an edit, merging it into the
// previous one when it continues a run of the same kind of edit.
func (e *editor) checkpoint(kind editKind) {
	now := time.Now()
	merge := kind != editOther && kind == e.lastKind && now.Sub(e.lastAt) < coalesceWindow
	e.lastKind, e.lastAt = kind, now
	e.redo = nil
	e.version++
	if merge {
		return
	}
	cp := make([][]rune, len(e.lines))
	for i, l := range e.lines {
		cp[i] = append([]rune(nil), l...)
	}
	e.undo = append(e.undo, snapshot{lines: cp, cur: e.cur})
	if len(e.undo) > undoCap {
		e.undo = e.undo[len(e.undo)-undoCap:]
	}
}

// Undo and Redo swap the buffer with the top of one stack, pushing the
// current state onto the other.
func (e *editor) Undo() bool { return e.swap(&e.undo, &e.redo) }
func (e *editor) Redo() bool { return e.swap(&e.redo, &e.undo) }

func (e *editor) swap(from, to *[]snapshot) bool {
	if len(*from) == 0 {
		return false
	}
	s := (*from)[len(*from)-1]
	*from = (*from)[:len(*from)-1]
	*to = append(*to, snapshot{lines: e.lines, cur: e.cur})
	e.lines, e.cur, e.sel = s.lines, s.cur, false
	e.lastKind = editOther
	e.version++
	return true
}

// deleteSelection removes the selected text, leaving the caret where it was.
// It reports whether there was a selection.
func (e *editor) deleteSelection() bool {
	if !e.sel || e.cur == e.anc {
		e.sel = false
		return false
	}
	a, b := e.ordered()
	head := e.lines[a.row][:a.col]
	tail := e.lines[b.row][b.col:]
	joined := append(append([]rune(nil), head...), tail...)
	e.lines = append(e.lines[:a.row], append([][]rune{joined}, e.lines[b.row+1:]...)...)
	e.cur, e.sel = a, false
	return true
}

// Insert types or pastes text at the caret, replacing the selection.
func (e *editor) Insert(s string) {
	s = normalize(s, e.single)
	if s == "" {
		return
	}
	kind := editOther
	endsWord := false
	if len([]rune(s)) == 1 && !e.sel {
		kind = editType
		endsWord = !isWord([]rune(s)[0])
	}
	e.checkpoint(kind)
	// A space or punctuation joins the word it follows and closes the undo
	// step, so the next keystroke starts a new one: "select 1" undoes as
	// "select " then "", the way most editors group typing.
	defer func() {
		if endsWord {
			e.lastKind = editOther
		}
	}()
	e.deleteSelection()

	ins := splitLines(s)
	line := e.lines[e.cur.row]
	head := append([]rune(nil), line[:e.cur.col]...)
	tail := append([]rune(nil), line[e.cur.col:]...)
	if len(ins) == 1 {
		e.lines[e.cur.row] = append(append(head, ins[0]...), tail...)
		e.cur.col += len(ins[0])
	} else {
		first := append(head, ins[0]...)
		last := append(append([]rune(nil), ins[len(ins)-1]...), tail...)
		mid := ins[1 : len(ins)-1]
		rows := append([][]rune{first}, mid...)
		rows = append(rows, last)
		e.lines = append(e.lines[:e.cur.row], append(rows, e.lines[e.cur.row+1:]...)...)
		e.cur = pos{e.cur.row + len(ins) - 1, len(ins[len(ins)-1])}
	}
	e.goal = e.dispCol(e.cur)
}

// Newline breaks the line at the caret, carrying the current line's indent
// onto the new one — SQL is written in indented clauses, and retyping the
// indent on every line is the first thing an editor without this makes you
// do.
func (e *editor) Newline() {
	if e.single {
		return
	}
	line := e.lines[e.cur.row]
	indent := 0
	for indent < len(line) && line[indent] == ' ' && indent < e.cur.col {
		indent++
	}
	e.Insert("\n" + strings.Repeat(" ", indent))
}

// Backspace deletes the selection, or the rune before the caret, joining
// lines at a line start.
func (e *editor) Backspace() {
	if e.sel && e.cur != e.anc {
		e.checkpoint(editOther)
		e.deleteSelection()
		return
	}
	e.sel = false
	if e.cur.col == 0 && e.cur.row == 0 {
		return
	}
	e.checkpoint(editDelete)
	if e.cur.col > 0 {
		l := e.lines[e.cur.row]
		e.lines[e.cur.row] = append(l[:e.cur.col-1:e.cur.col-1], l[e.cur.col:]...)
		e.cur.col--
	} else {
		prev := e.lines[e.cur.row-1]
		col := len(prev)
		e.lines[e.cur.row-1] = append(append([]rune(nil), prev...), e.lines[e.cur.row]...)
		e.lines = append(e.lines[:e.cur.row], e.lines[e.cur.row+1:]...)
		e.cur = pos{e.cur.row - 1, col}
	}
	e.goal = e.dispCol(e.cur)
}

// Delete removes the selection or the rune after the caret.
func (e *editor) Delete() {
	if e.sel && e.cur != e.anc {
		e.checkpoint(editOther)
		e.deleteSelection()
		return
	}
	e.sel = false
	l := e.lines[e.cur.row]
	if e.cur.col == len(l) && e.cur.row == len(e.lines)-1 {
		return
	}
	e.checkpoint(editDelete)
	if e.cur.col < len(l) {
		e.lines[e.cur.row] = append(l[:e.cur.col:e.cur.col], l[e.cur.col+1:]...)
	} else {
		e.lines[e.cur.row] = append(append([]rune(nil), l...), e.lines[e.cur.row+1]...)
		e.lines = append(e.lines[:e.cur.row+1], e.lines[e.cur.row+2:]...)
	}
}

// DeleteWordBack removes from the caret back to the start of the word.
func (e *editor) DeleteWordBack() {
	if e.sel && e.cur != e.anc {
		e.Backspace()
		return
	}
	start := e.wordLeft(e.cur)
	if start == e.cur {
		e.Backspace()
		return
	}
	e.anc, e.sel = start, true
	e.checkpoint(editOther)
	e.deleteSelection()
}

// ---------------------------------------------------------------------------
// Movement
// ---------------------------------------------------------------------------

// move places the caret, extending the selection when extend is set and
// collapsing it otherwise.
func (e *editor) move(to pos, extend bool) {
	if extend {
		if !e.sel {
			e.anc, e.sel = e.cur, true
		}
	} else {
		e.sel = false
	}
	e.cur = e.clamp(to)
	e.lastKind = editOther
}

func (e *editor) clamp(p pos) pos {
	p.row = max(0, min(p.row, len(e.lines)-1))
	p.col = max(0, min(p.col, len(e.lines[p.row])))
	return p
}

func (e *editor) left1(p pos) pos {
	if p.col > 0 {
		return pos{p.row, p.col - 1}
	}
	if p.row > 0 {
		return pos{p.row - 1, len(e.lines[p.row-1])}
	}
	return p
}

func (e *editor) right1(p pos) pos {
	if p.col < len(e.lines[p.row]) {
		return pos{p.row, p.col + 1}
	}
	if p.row < len(e.lines)-1 {
		return pos{p.row + 1, 0}
	}
	return p
}

// wordLeft and wordRight skip one word, the way alt+arrow does elsewhere.
func (e *editor) wordLeft(p pos) pos {
	p = e.left1(p)
	for p.col > 0 && !isWord(e.lines[p.row][p.col]) {
		p.col--
	}
	for p.col > 0 && isWord(e.lines[p.row][p.col-1]) {
		p.col--
	}
	return p
}

func (e *editor) wordRight(p pos) pos {
	l := e.lines[p.row]
	if p.col >= len(l) {
		return e.right1(p)
	}
	for p.col < len(l) && !isWord(l[p.col]) {
		p.col++
	}
	for p.col < len(l) && isWord(l[p.col]) {
		p.col++
	}
	return p
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// vertical moves the caret n rows, keeping the goal display column.
func (e *editor) vertical(n int, extend bool) {
	row := max(0, min(e.cur.row+n, len(e.lines)-1))
	e.move(pos{row, e.colAtDisp(row, e.goal)}, extend)
}

// SelectAll selects the whole buffer.
func (e *editor) SelectAll() {
	last := len(e.lines) - 1
	e.anc, e.cur, e.sel = pos{}, pos{last, len(e.lines[last])}, true
}

// selectWord selects the word at p, for a double-click.
func (e *editor) selectWord(p pos) {
	l := e.lines[p.row]
	a, b := p.col, p.col
	for a > 0 && isWord(l[a-1]) {
		a--
	}
	for b < len(l) && isWord(l[b]) {
		b++
	}
	e.anc, e.cur, e.sel = pos{p.row, a}, pos{p.row, b}, a != b
}

// selectLine selects row p.row, for a triple-click.
func (e *editor) selectLine(p pos) {
	e.anc, e.sel = pos{p.row, 0}, true
	if p.row < len(e.lines)-1 {
		e.cur = pos{p.row + 1, 0}
	} else {
		e.cur = pos{p.row, len(e.lines[p.row])}
	}
}

// ---------------------------------------------------------------------------
// Display columns
// ---------------------------------------------------------------------------

// runeWidth is a rune's cell width; control characters draw as one cell.
func runeWidth(r rune) int {
	if r < 0x20 {
		return 1
	}
	w := uniseg.StringWidth(string(r))
	return max(w, 0)
}

// dispCol is p's display column.
func (e *editor) dispCol(p pos) int {
	w := 0
	for _, r := range e.lines[p.row][:min(p.col, len(e.lines[p.row]))] {
		w += runeWidth(r)
	}
	return w
}

// colAtDisp is the rune column in row whose cell covers display column d.
// A click on the right half of a wide rune lands after it, the way a caret
// placed by eye would.
func (e *editor) colAtDisp(row, d int) int {
	w := 0
	for i, r := range e.lines[row] {
		rw := runeWidth(r)
		if d < w+rw {
			if d-w >= (rw+1)/2 && rw > 1 {
				return i + 1
			}
			return i
		}
		w += rw
	}
	return len(e.lines[row])
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// HandleKey applies a key to the editor and reports whether it used it. Keys
// it does not use (Tab, Enter in a one-line field, the app's ctrl chords) are
// left for the caller.
func (e *editor) HandleKey(k tea.KeyPressMsg) bool {
	s := k.String()
	switch s {
	case "left", "shift+left":
		if e.sel && e.cur != e.anc && s == "left" {
			a, _ := e.ordered()
			e.move(a, false)
		} else {
			e.move(e.left1(e.cur), s == "shift+left")
		}
	case "right", "shift+right":
		if e.sel && e.cur != e.anc && s == "right" {
			_, b := e.ordered()
			e.move(b, false)
		} else {
			e.move(e.right1(e.cur), s == "shift+right")
		}
	case "alt+left", "ctrl+left", "alt+shift+left", "ctrl+shift+left", "alt+b":
		e.move(e.wordLeft(e.cur), strings.Contains(s, "shift"))
	case "alt+right", "ctrl+right", "alt+shift+right", "ctrl+shift+right", "alt+f":
		e.move(e.wordRight(e.cur), strings.Contains(s, "shift"))
	case "up", "shift+up":
		if e.single {
			return false
		}
		e.vertical(-1, s == "shift+up")
		return true
	case "down", "shift+down":
		if e.single {
			return false
		}
		e.vertical(1, s == "shift+down")
		return true
	case "pgup", "shift+pgup":
		if e.single {
			return false
		}
		e.vertical(-max(e.view.H-1, 1), s == "shift+pgup")
		return true
	case "pgdown", "shift+pgdown":
		if e.single {
			return false
		}
		e.vertical(max(e.view.H-1, 1), s == "shift+pgdown")
		return true
	case "home", "shift+home":
		// first press goes to the first non-blank, a second to column 0
		l := e.lines[e.cur.row]
		ind := 0
		for ind < len(l) && l[ind] == ' ' {
			ind++
		}
		to := ind
		if e.cur.col == ind {
			to = 0
		}
		e.move(pos{e.cur.row, to}, s == "shift+home")
	case "end", "shift+end":
		e.move(pos{e.cur.row, len(e.lines[e.cur.row])}, s == "shift+end")
	case "ctrl+home", "ctrl+shift+home":
		e.move(pos{}, strings.Contains(s, "shift"))
	case "ctrl+end", "ctrl+shift+end":
		last := len(e.lines) - 1
		e.move(pos{last, len(e.lines[last])}, strings.Contains(s, "shift"))
	case "backspace", "shift+backspace":
		e.Backspace()
	case "delete":
		e.Delete()
	case "alt+backspace", "ctrl+w", "ctrl+backspace":
		e.DeleteWordBack()
	case "ctrl+u":
		// delete to line start, the readline way
		e.anc, e.sel = pos{e.cur.row, 0}, true
		e.checkpoint(editOther)
		e.deleteSelection()
	case "enter", "shift+enter", "alt+enter", "ctrl+j":
		if e.single {
			return false
		}
		e.Newline()
	case "ctrl+z":
		e.Undo()
	case "ctrl+shift+z", "alt+z":
		e.Redo()
	case "alt+a", "super+a":
		e.SelectAll()
	default:
		// printable text, including a space
		if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper) == 0 {
			e.Insert(k.Text)
			return true
		}
		return false
	}
	if !strings.Contains(s, "up") && !strings.Contains(s, "down") {
		e.goal = e.dispCol(e.cur)
	}
	return true
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

// posAtScreen maps a screen cell to a buffer position, clamping clicks past
// the end of a line or below the last line to the nearest real position.
func (e *editor) posAtScreen(x, y int) pos {
	row := e.top + (y - e.view.Y)
	row = max(0, min(row, len(e.lines)-1))
	return pos{row, e.colAtDisp(row, e.left+max(x-e.view.X, 0))}
}

// Click places the caret (extending the selection with shift) and applies
// double- and triple-click word and line selection.
func (e *editor) Click(x, y, clicks int, shift bool) {
	p := e.posAtScreen(x, y)
	switch {
	case clicks >= 3:
		e.selectLine(p)
	case clicks == 2:
		e.selectWord(p)
	default:
		e.move(p, shift)
	}
	e.goal = e.dispCol(e.cur)
}

// Drag extends the selection to the cell under the pointer, scrolling when
// the pointer is dragged past an edge.
func (e *editor) Drag(x, y int) {
	if !e.sel {
		e.anc, e.sel = e.cur, true
	}
	switch {
	case y < e.view.Y && e.top > 0:
		e.top--
	case y >= e.view.Y+e.view.H && e.top < len(e.lines)-1:
		e.top++
	}
	e.cur = e.posAtScreen(x, y)
	e.goal = e.dispCol(e.cur)
}

// Scroll moves the view by rows (wheel) without moving the caret.
func (e *editor) Scroll(rows, cols int) {
	e.top = max(0, min(e.top+rows, len(e.lines)-1))
	e.left = max(0, e.left+cols)
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// gutterWidth is the width of the line-number gutter in SQL mode: digits for
// the largest line number, plus a marker column and a space.
func (e *editor) gutterWidth() int {
	if !e.sqlMode {
		return 0
	}
	d := len(itoa(len(e.lines)))
	return max(d, 2) + 2
}

// scrollToCaret adjusts top/left so the caret is inside a view of w×h.
func (e *editor) scrollToCaret(w, h int) {
	if h > 0 {
		if e.cur.row < e.top {
			e.top = e.cur.row
		} else if e.cur.row >= e.top+h {
			e.top = e.cur.row - h + 1
		}
	}
	if w > 0 {
		d := e.dispCol(e.cur)
		if d < e.left {
			e.left = d
		} else if d >= e.left+w {
			e.left = d - w + 1
		}
	}
}

// Draw paints the editor into s and returns the caret's canvas position when
// it is visible — the caller turns that into the terminal cursor when the
// editor has focus. curStmt is the byte range of the statement under the
// caret, marked in the gutter so the user can see what Ctrl+R will run.
func (e *editor) Draw(s Surface, st styles, bg Style, curStmt [2]int, follow bool) (cx, cy int, visible bool) {
	s.Fill(bg)
	g := e.gutterWidth()
	text := s.Sub(Rect{g, 0, s.W() - g, s.H()})
	e.view = text.Rect()
	if follow {
		e.scrollToCaret(text.W(), text.H())
	}

	if e.Empty() && e.placeholder != "" && len(e.lines) == 1 {
		text.Put(0, 0, e.placeholder, onBg(st.muted, bg))
	}

	var hl [][]Style
	if e.sqlMode {
		hl = e.highlight(st)
	}
	stmtA, stmtB := -1, -1
	if e.sqlMode && curStmt[1] > curStmt[0] {
		stmtA, stmtB = e.posAt(curStmt[0]).row, e.posAt(curStmt[1]).row
	}
	selA, selB := e.ordered()
	hasSel := e.sel && e.cur != e.anc

	for y := 0; y < s.H(); y++ {
		row := e.top + y
		if row >= len(e.lines) {
			break
		}
		if g > 0 {
			num := itoa(row + 1)
			ls := onBg(st.lineNo, bg)
			if row == e.cur.row {
				ls = onBg(st.muted, bg)
			}
			s.PutRight(g-2, y, num, ls)
			if row >= stmtA && row <= stmtB {
				s.Put(g-2, y, "▎", onBg(st.accent, bg))
			}
		}
		x := -e.left
		for i, r := range e.lines[row] {
			cs := bg
			if hl != nil && i < len(hl[row]) {
				cs = hl[row][i]
			}
			if hasSel && inRange(pos{row, i}, selA, selB) {
				cs = cs.WithBg(st.selRange.Bg)
			}
			x = text.Put(x, y, string(r), cs)
			if x >= text.W() {
				break
			}
		}
		// a selection that runs past the end of a line shows one cell of
		// highlight there, so a selected newline is visible
		if hasSel && inRange(pos{row, len(e.lines[row])}, selA, selB) && row < selB.row {
			text.Put(x, y, " ", bg.WithBg(st.selRange.Bg))
		}
	}

	cx = e.dispCol(e.cur) - e.left
	cy = e.cur.row - e.top
	if cx < 0 || cx >= text.W() || cy < 0 || cy >= text.H() {
		return 0, 0, false
	}
	return text.Rect().X + cx, text.Rect().Y + cy, true
}

func inRange(p, a, b pos) bool { return !p.before(a) && p.before(b) }

// highlight returns a style per rune per line, from sqlsplit's lexer. It is
// cached against the edit version: the frame redraws on every mouse motion,
// and relexing an unchanged buffer each time would be waste.
func (e *editor) highlight(st styles) [][]Style {
	if e.hl != nil && e.hlVer == e.version {
		return e.hl
	}
	text := e.Text()
	toks := sqlsplit.Lex(text)
	out := make([][]Style, len(e.lines))
	ti := 0
	off := 0
	for r, l := range e.lines {
		out[r] = make([]Style, len(l))
		for i, ch := range l {
			for ti < len(toks) && toks[ti].End <= off {
				ti++
			}
			stl := st.base
			if ti < len(toks) && off >= toks[ti].Start && off < toks[ti].End {
				stl = tokenStyle(st, toks[ti].Kind)
			}
			out[r][i] = stl
			off += len(string(ch))
		}
		off++ // the newline
	}
	e.hl, e.hlVer = out, e.version
	return out
}

func tokenStyle(st styles, k sqlsplit.TokenKind) Style {
	switch k {
	case sqlsplit.TokKeyword:
		return st.synKeyword
	case sqlsplit.TokString:
		return st.synString
	case sqlsplit.TokNumber:
		return st.synNumber
	case sqlsplit.TokComment:
		return st.synComment
	case sqlsplit.TokIdent:
		return st.synIdent
	case sqlsplit.TokParam:
		return st.synParam
	}
	return st.base
}

// itoa is strconv.Itoa without the import in every file that draws numbers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
