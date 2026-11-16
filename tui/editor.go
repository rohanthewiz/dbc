package tui

import (
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// lines is the buffer, a row of runes per line.
	//
	// A ROW IS NEVER CHANGED IN PLACE once it is in lines: an edit puts a
	// new slice where the row was (Insert's head+text+tail, Backspace's
	// three-index append) and only ever rewrites the outer slice. Undo
	// steps and the caches below keep the rows they saw by reference, and
	// tell a changed row from a kept one by identity (rowsDiff), which this
	// makes sound: the same row slice is the same text. That is what lets
	// a keystroke cost the rows it changed rather than the buffer (N-279).
	// TestEditorCachesFollowEveryEdit undoes and redoes every random edit
	// back to the texts it saw, which an in-place change would corrupt.
	lines [][]rune
	cur   pos // the caret
	anc   pos // the other end of the selection, when sel is true
	sel   bool

	top, left int // first visible row, first visible display column
	goal      int // display column kept across up/down through short lines

	single      bool   // one line only: Enter is not ours, newlines paste as spaces
	sqlMode     bool   // syntax highlighting and the current-statement marker
	placeholder string // shown dimmed while the text is empty
	// mask, when set, is drawn in place of every rune (a password field).
	// Each masked rune is one cell wide, so the caret and a click map onto
	// the dots drawn rather than onto the hidden text's own widths.
	mask rune

	// dialect is the engine the SQL is colored as (setDialect): where a
	// string ends is its rule, and the coloring must end it where the run's
	// splitter does (workspace.Pick takes the same dialect).
	dialect sqlsplit.Dialect

	undo, redo []undoStep
	undoBytes  int       // what the undo stack holds (undoStep.size), against undoBytesCap
	lastKind   editKind  // what the previous edit was, for coalescing typing
	lastAt     time.Time // when it happened

	// view is the text area as last drawn, in canvas coordinates: where the
	// mouse is mapped from. Zero until the first draw.
	view Rect

	// version counts edits; the caches below are kept against it.
	version int
	// savedVer is the edit version the text was last saved at by
	// autosave (autosave.go): an editor whose version moved past it has
	// something to save.
	savedVer int

	// rows is lines as of edit version rowsVer: the outer slice copied
	// once per version (snap), shared by every cache that keeps "the rows
	// I was made from" and by the open undo step.
	rows    [][]rune
	rowsVer int
	rowsOK  bool

	// buf, split and hl are what a frame reads off the buffer, kept for
	// the edit version they were made at — see buffer, Stmts and
	// highlight.go. The frame is redrawn on every message, a pointer
	// motion included, and before these each one joined the whole buffer
	// into a string about four times and split it into statements twice:
	// ~100 ms a frame on a 3.5 MB migration (N-278). Each is also brought
	// up to date after an edit from the rows it changed (rowsDiff), not
	// made again: a keystroke on that migration cost ~117 ms and 139 MB
	// before, the text joined, the buffer lexed and split twice (N-279).
	buf   editorBuf
	split editorSplit
	hl    hlCache

	// marks are the uses of a symbol (Shift+F12, symbol.go), drawn while
	// marksVer is still the edit version they were found at.
	marks    []editorMark
	marksVer int

	// stamp is the edit version and caret as Stamp last noted them, so a
	// caller can ask later whether the user has been back in the editor
	// since (Touched) — see Model.chatContext, which yields a script's
	// context to the editor's statement once it has.
	stamp editorStamp

	// snip is the live snippet being filled in (snippets.go): its tab
	// stops, drawn boxed, which Tab steps through. nil when none is.
	snip *snipSession
}

// editorBuf is the buffer as Text returns it, with the byte offset each
// line starts at, for the edit version ver (ok: made at all — version 0
// is a real version).
//
//	lines   [S E L E C T]  [  1 ;]  []  [S E L E C T   2]
//	text    "SELECT\n  1;\n\nSELECT 2"
//	starts   0        7     12  13
//
// starts turns a position into an offset (offset) and an offset back into
// a position (posAt) without walking every line before it, and without
// converting each line to a string just to learn its length in bytes.
type editorBuf struct {
	ok     bool
	ver    int
	text   string
	starts []int
	rows   [][]rune // the rows text was made from (an editor snapshot)
}

// rowStart is where row r starts in text+"\n" — every row followed by its
// newline, the last one too — so r = len(rows) is that string's end. The
// row arithmetic of a patch is regular in those terms; see patch.
func (b *editorBuf) rowStart(r int) int {
	if r < len(b.starts) {
		return b.starts[r]
	}
	return len(b.text) + 1
}

// editorSplit is the buffer split into statements as dialect sp.Dialect()
// reads it, for the edit version ver.
type editorSplit struct {
	ok   bool
	ver  int
	rows [][]rune // the rows split (an editor snapshot)
	n    int      // the length of the text split
	sp   *sqlsplit.Splitting
}

// editorStamp is an editor's edit version and caret at one moment; the
// zero value is "never stamped".
type editorStamp struct {
	set     bool
	version int
	cur     pos
}

// Stamp notes the editor as it is now, for Touched.
func (e *editor) Stamp() { e.stamp = editorStamp{set: true, version: e.version, cur: e.cur} }

// Touched reports whether the editor was edited, or its caret moved, since
// Stamp — true when it was never stamped. A caret move counts: clicking a
// statement is as much "I am looking at this" as typing one.
func (e *editor) Touched() bool {
	return !e.stamp.set || e.stamp.version != e.version || e.stamp.cur != e.cur
}

// pos is a caret position: row, and column in runes.
type pos struct{ row, col int }

func (p pos) before(q pos) bool { return p.row < q.row || (p.row == q.row && p.col < q.col) }

// undoStep is one undo step: the rows the step replaced, by position.
// Rows [at, at+n) of the buffer after the step took the place of rows
// (old) of the buffer before it, so undoing it is splicing old back in:
//
//	before  │ head │ old (3 rows)  │ tail │
//	after   │ head │ new (n = 1)   │ tail │     the step keeps old, at and n
//
// It used to be a copy of the whole buffer, which a 3.5 MB migration made
// a 14 MB copy per word typed (N-279). It holds only the replaced rows
// now, and those by reference: rows are never changed in place (lines).
//
// The newest step stays open while typing merges into it, holding base,
// the whole buffer before it (a snapshot: the outer slice alone). When it
// closes (closeStep: the next step begins, or Undo, or View), its last
// edit has landed, and rowsDiff against base gives at, old and n.
type undoStep struct {
	at   int
	old  [][]rune
	n    int
	cur  pos // the caret before the step
	size int // bytes it keeps alive, roughly (stepSize), for undoBytesCap

	base [][]rune // while open: the buffer before the step; nil once closed
}

// stepSize is roughly what keeping rows costs: four bytes a rune, a slice
// header a row, and the step itself. A row the buffer still holds counts
// too — it is that or walking every other step for sharing — so the cap
// errs toward dropping history early, never toward holding too much.
func stepSize(rows [][]rune) int {
	n := 64
	for _, r := range rows {
		n += 4*len(r) + 24
	}
	return n
}

// editKind classifies an edit so consecutive typing undoes as one step
// rather than one character at a time.
type editKind int

const (
	editOther editKind = iota
	editType
	editDelete
)

// undoCap and undoBytesCap bound the undo stack, by steps and by what the
// steps hold (undoStep.size). A step of typing holds a row or two, so the
// count is what binds in use; the bytes bind on pastes and deletes of whole
// buffers, where 32 MB keeps two replacements of a 3.5 MB migration (each
// 14 MB as runes). The newest step is kept whatever its size, so the last
// edit can always be undone.
const (
	undoCap      = 200
	undoBytesCap = 32 << 20
)

// coalesceWindow is how long a pause in typing may be before the next
// keystroke starts a new undo step.
const coalesceWindow = time.Second

func newEditor(single bool) *editor {
	return &editor{lines: [][]rune{{}}, single: single}
}

// Text returns the buffer.
func (e *editor) Text() string { return e.buffer().text }

// buffer is the buffer as one string and its line starts, made at most
// once per edit version.
//
// The version is bumped by checkpoint BEFORE the edit it records lands in
// lines (and by SetText, Restore, swap and the snippet mirrors after
// theirs). That is safe because nothing reads the buffer between an edit's
// checkpoint and the edit itself — the same assumption the highlight cache
// has always made. TestEditorCachesFollowEveryEdit holds every editing
// path to it, comparing the cached text with a fresh join after each step.
//
// After an edit the text is patched rather than joined again: the rows
// the edit left alone keep their bytes, copied over whole, and only the
// rows between are encoded (patch). The copy is a memmove of the buffer,
// ~0.3 ms on 3.5 MB, where encoding it rune by rune was ~12 ms.
func (e *editor) buffer() *editorBuf {
	b := &e.buf
	if b.ok && b.ver == e.version {
		return b
	}
	now := e.snap()
	p, s := 0, 0 // nothing made yet: every row is new
	if b.ok {
		p, s = rowsDiff(b.rows, now)
	}
	b.patch(now, p, s)
	b.ok, b.ver, b.rows = true, e.version, now
	return b
}

// patch brings b from the rows it was made from to now, where the first p
// and last s rows are unchanged (rowsDiff). In text+"\n" terms (rowStart)
// the old rows [p, oldN−s) are one byte range and the new rows [p, nowN−s)
// take its place:
//
//	old  │ rows < p │ old middle │ last s rows │
//	new  │ rows < p │ new middle │ last s rows │     starts after the middle move by delta
//
// The string ends with the last row's newline in those terms, which is cut
// off at the end; starts are edited in place. Before anything was made
// (!b.ok) there is no old text, not even the one newline an empty row has,
// so the old middle is empty and at 0.
func (b *editorBuf) patch(now [][]rune, p, s int) {
	oldN, nowN := len(b.starts), len(now)
	// the old middle in text+"\n", and the bytes after it
	from, to, tailN := 0, 0, 0
	if b.ok {
		from, to = b.rowStart(p), b.rowStart(oldN-s)
		tailN = len(b.text) + 1 - to
	}
	mid := 0
	for _, l := range now[p : nowN-s] {
		mid += runesLen(l) + 1
	}
	old := b.text
	var sb strings.Builder
	sb.Grow(from + mid + tailN)
	if from > len(old) {
		sb.WriteString(old)
		sb.WriteByte('\n')
	} else {
		sb.WriteString(old[:from])
	}
	starts := make([]int, 0, nowN-s-p)
	at := from
	for _, l := range now[p : nowN-s] {
		starts = append(starts, at)
		for _, r := range l {
			sb.WriteRune(r)
		}
		sb.WriteByte('\n')
		at += runesLen(l) + 1
	}
	if tailN > 0 {
		sb.WriteString(old[to:])
		sb.WriteByte('\n')
	}
	t := sb.String()
	b.text = t[:len(t)-1]

	// the middle's starts replace the old middle's; the tail's move by
	// what the middle grew or shrank
	delta := mid - (to - from)
	b.starts = slices.Replace(b.starts, p, oldN-s, starts...)
	for i := nowN - s; i < nowN; i++ {
		b.starts[i] += delta
	}
}

// byteEdit is the edit that turned a text of oldLen bytes, made from rows
// whose first p and last s are those of b's, into b.text: its bytes [at,
// oldEnd) became b.text[at:newEnd] — the terms sqlsplit's Splitting.Edit
// takes. Worked out in text+"\n" terms (rowStart), where the unchanged
// head and tail are whole rows, then cut back to the texts' lengths: only
// the final newline differs between the two spellings, and it is in the
// tail when s > 0 and in both middles otherwise.
func (b *editorBuf) byteEdit(p, s, oldLen int) (at, oldEnd, newEnd int) {
	at = b.rowStart(p)
	endT := b.rowStart(len(b.starts) - s)
	oldEndT := oldLen + 1 - (len(b.text) + 1 - endT)
	newEnd, oldEnd = min(endT, len(b.text)), min(oldEndT, oldLen)
	return min(at, newEnd, oldEnd), oldEnd, newEnd
}

// snap returns lines as of the current edit version: a copy of the outer
// slice, made once per version and shared — the rows themselves need no
// copy, never being changed in place (lines). Nothing may change it.
//
// Like the caches it serves, it trusts that nothing reads the buffer
// between an edit's checkpoint (which bumps the version) and the edit
// itself; checkpoint takes the snapshot it needs before the bump.
func (e *editor) snap() [][]rune {
	if !e.rowsOK || e.rowsVer != e.version {
		e.rows, e.rowsVer, e.rowsOK = slices.Clone(e.lines), e.version, true
	}
	return e.rows
}

// rowsDiff returns how many rows at the head (p) and at the tail (s) of old
// and now are the same, p+s never more than either holds. A row is the
// same when it is the same slice — the usual case, a pointer compare,
// since an edit replaces the rows it changes and leaves the others be — or
// failing that holds the same runes: a rename or a snippet's mirrors set
// the whole buffer afresh, and most of it is as it was.
func rowsDiff(old, now [][]rune) (p, s int) {
	n := min(len(old), len(now))
	for p < n && sameRow(old[p], now[p]) {
		p++
	}
	for s < n-p && sameRow(old[len(old)-1-s], now[len(now)-1-s]) {
		s++
	}
	return p, s
}

// sameRow reports whether a and b hold the same runes, by identity first.
func sameRow(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 || &a[0] == &b[0] {
		return true
	}
	return slices.Equal(a, b)
}

// runesLen is len(string(rs)) without making the string. An invalid rune
// (a lone surrogate, or past U+10FFFF) is written as U+FFFD — three bytes —
// by that conversion and by WriteRune alike, so it is counted as three.
func runesLen(rs []rune) int {
	n := 0
	for _, r := range rs {
		n += runeBytes(r)
	}
	return n
}

// runeBytes is the bytes r takes in a string made from runes.
func runeBytes(r rune) int {
	if l := utf8.RuneLen(r); l > 0 {
		return l
	}
	return 3 // written as U+FFFD
}

// Stmts is the buffer split into statements as d reads it, made at most
// once per edit version and dialect. Every frame asks for it twice — the
// title's "^R runs statement 2/4" and the gutter's marker — and the
// assistant's context chip a third time, and a caret move changes none of
// them: only which statement IndexAt picks out of the same list. The slice
// is shared; callers must not modify it.
//
// After an edit the split is redone around the edit only (Splitting.Edit,
// told the byte range by byteEdit): a whole split of a 3.5 MB buffer was
// ~18 ms a keystroke, and completion did one more (N-279).
func (e *editor) Stmts(d sqlsplit.Dialect) []sqlsplit.Stmt {
	s := &e.split
	if s.ok && s.ver == e.version && s.sp.Dialect() == d {
		return s.sp.Stmts()
	}
	b := e.buffer()
	if s.ok && s.sp.Dialect() == d {
		p, q := rowsDiff(s.rows, b.rows)
		at, oldEnd, newEnd := b.byteEdit(p, q, s.n)
		s.sp = s.sp.Edit(b.text, at, oldEnd, newEnd)
	} else {
		s.sp = d.Splitting(b.text)
	}
	s.ok, s.ver, s.rows, s.n = true, e.version, b.rows, len(b.text)
	return s.sp.Stmts()
}

// SetText replaces the buffer and clears undo — used for loading, not
// editing, so there is nothing to go back to.
func (e *editor) SetText(s string) {
	e.lines = splitLines(normalize(s, e.single))
	e.cur, e.sel = pos{}, false
	e.top, e.left, e.goal = 0, 0, 0
	e.undo, e.redo, e.undoBytes = nil, nil, 0
	e.version++
	e.snip = nil // its stops were offsets into the text just replaced
}

// editorView is all SetText throws away besides the text: the caret, the
// selection, the scroll and the undo history, with the text they belong
// to. View and Restore carry it across a console swap (console.go), so a
// console reopened later is where it was left rather than at the top.
type editorView struct {
	text            string // Text() when the view was taken
	cur, anc        pos
	sel             bool
	top, left, goal int
	undo, redo      []undoStep
}

// View captures the editor's state for a later Restore. The undo stacks
// are handed over, not copied: the editor is about to get another buffer
// by SetText or Restore, which drops its own references to them, and no
// edit writes into a step (swap moves them whole), so nothing mutates
// them while they are parked. The open step is closed first: its base is
// this buffer, which is about to go.
//
// A step records rows by position (undoStep), so it applies as well to
// the rows Restore splits afresh from the same text.
func (e *editor) View() editorView {
	e.closeStep()
	return editorView{
		text: e.Text(), cur: e.cur, anc: e.anc, sel: e.sel,
		top: e.top, left: e.left, goal: e.goal,
		undo: e.undo, redo: e.redo,
	}
}

// Restore loads text with v's caret, selection, scroll and undo history.
// When text is still what v was taken over, that is all. When it changed
// meanwhile (another writer saved over the file), v is put back over its
// own text and the new text replaces it as ONE undo step — dbc web's rule
// for a console changed elsewhere — so nothing in the history is lost and
// Ctrl+Z brings back the version that was left. The caret then stays on
// its row and column, clamped to the new text, rather than jumping to the
// end the way Replace leaves it.
func (e *editor) Restore(v editorView, text string) {
	e.lines = splitLines(v.text) // v.text came from Text(): already normalized
	e.cur, e.anc, e.sel = e.clamp(v.cur), e.clamp(v.anc), v.sel
	e.top, e.left, e.goal = max(0, min(v.top, len(e.lines)-1)), v.left, v.goal
	e.setUndo(v.undo, v.redo)
	e.lastKind = editOther // what is typed next starts its own undo step
	e.version++
	e.snip = nil // another buffer: the stops were offsets into this one's
	if normalize(text, e.single) == v.text {
		return
	}
	at := e.cur
	e.Replace(0, len(v.text), text, -1)
	e.cur, e.sel = e.clamp(at), false
	e.top = min(e.top, len(e.lines)-1)
	e.goal = e.dispCol(e.cur)
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
// sqlsplit deals in. A row past the last line is the offset one past the
// end (every line and its newline counted), as it always was.
func (e *editor) offset(p pos) int {
	b := e.buffer()
	if p.row >= len(e.lines) {
		return len(b.text) + 1
	}
	l := e.lines[p.row]
	return b.starts[p.row] + runesLen(l[:min(p.col, len(l))])
}

// posAt converts a byte offset back to a position. The row is found by a
// binary search of the line starts; an offset at a line's end (on its
// newline) is that line's end, not the next line's start, and one past the
// buffer's end is the last line's end. An offset inside a rune's bytes is
// taken to the end of that rune, so the column never runs past the line.
func (e *editor) posAt(off int) pos {
	if off <= 0 {
		return pos{}
	}
	b := e.buffer()
	// the last line starting at or before off
	r := sort.Search(len(b.starts), func(i int) bool { return b.starts[i] > off }) - 1
	l := e.lines[r]
	at := off - b.starts[r] // bytes into line r: at most its length, except on the last line
	col, used := 0, 0
	for col < len(l) && used < at {
		used += runeBytes(l[col])
		col++
	}
	return pos{r, col}
}

// Caret returns the caret's byte offset.
func (e *editor) Caret() int { return e.offset(e.cur) }

// lineBeforeCaret is the caret's line from its start to the caret.
func (e *editor) lineBeforeCaret() string {
	return string(e.lines[e.cur.row][:min(e.cur.col, len(e.lines[e.cur.row]))])
}

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
//
// A merge needs the step it merges into to be open: typing right after
// SetText (which clears the stacks) or after a step was closed by a View
// starts a step of its own, where it used to merge into nothing and could
// not be undone.
func (e *editor) checkpoint(kind editKind) {
	now := time.Now()
	open := len(e.undo) > 0 && e.undo[len(e.undo)-1].base != nil
	merge := open && kind != editOther && kind == e.lastKind && now.Sub(e.lastAt) < coalesceWindow
	e.lastKind, e.lastAt = kind, now
	e.redo = nil
	if !merge {
		e.closeStep()
		// the snapshot of the buffer before the edit, taken before the
		// version moves on (snap)
		e.undo = append(e.undo, undoStep{base: e.snap(), cur: e.cur})
		e.trimUndo()
	}
	e.version++
}

// closeStep turns the open step, if there is one, into the rows it
// replaced (undoStep), and trims the stack to its caps.
func (e *editor) closeStep() {
	if len(e.undo) == 0 || e.undo[len(e.undo)-1].base == nil {
		return
	}
	st := &e.undo[len(e.undo)-1]
	p, s := rowsDiff(st.base, e.lines)
	// a copy of the range: base is a whole snapshot, and a sub-slice of
	// it would keep every row of it alive
	st.at, st.n, st.old = p, len(e.lines)-s-p, slices.Clone(st.base[p:len(st.base)-s])
	st.base = nil
	st.size = stepSize(st.old)
	e.undoBytes += st.size
	e.trimUndo()
}

// trimUndo drops the oldest steps while the stack is past undoCap steps or
// undoBytesCap bytes, keeping the newest whatever its size. An open step
// counts as a step but holds nothing yet: its size is known once closed.
func (e *editor) trimUndo() {
	for len(e.undo) > undoCap || (e.undoBytes > undoBytesCap && len(e.undo) > 1) {
		e.undoBytes -= e.undo[0].size
		e.undo[0] = undoStep{} // the backing array lives on: let go of its rows
		e.undo = e.undo[1:]
	}
}

// setUndo puts parked stacks back (Restore), with what they hold.
func (e *editor) setUndo(undo, redo []undoStep) {
	e.undo, e.redo, e.undoBytes = undo, redo, 0
	for _, st := range undo {
		e.undoBytes += st.size
	}
}

// Undo and Redo swap the buffer with the top of one stack, pushing the
// current state onto the other.
func (e *editor) Undo() bool { return e.swap(&e.undo, &e.redo) }
func (e *editor) Redo() bool { return e.swap(&e.redo, &e.undo) }

// CanUndo and CanRedo say whether Undo / Redo would change anything — the
// editor menu dims those rows otherwise, as dbc web's does.
func (e *editor) CanUndo() bool { return len(e.undo) > 0 }
func (e *editor) CanRedo() bool { return len(e.redo) > 0 }

// swap undoes (from undo to redo) or redoes (from redo to undo) one step.
// The step taken off one stack is spliced back into the buffer, and the
// rows it displaces go onto the other as the step that redoes (or undoes
// again) what this one did. The buffer gets a new outer slice: the old one
// may be a snapshot's, which nothing may change.
func (e *editor) swap(from, to *[]undoStep) bool {
	e.closeStep()
	if len(*from) == 0 {
		return false
	}
	s := (*from)[len(*from)-1]
	(*from)[len(*from)-1] = undoStep{}
	*from = (*from)[:len(*from)-1]
	if from == &e.undo {
		e.undoBytes -= s.size
	}
	// a step is only ever applied to the buffer it was made against (redo
	// is dropped by every new edit), so these hold; the clamps keep a
	// broken promise from becoming a panic
	at := min(s.at, len(e.lines))
	n := min(s.n, len(e.lines)-at)
	back := undoStep{at: at, old: slices.Clone(e.lines[at : at+n]), n: len(s.old), cur: e.cur}
	back.size = stepSize(back.old)
	lines := make([][]rune, 0, len(e.lines)-n+len(s.old))
	lines = append(lines, e.lines[:at]...)
	lines = append(lines, s.old...)
	lines = append(lines, e.lines[at+n:]...)
	if len(lines) == 0 {
		lines = [][]rune{{}}
	}
	e.lines, e.cur, e.sel = lines, e.clampTo(lines, s.cur), false
	*to = append(*to, back)
	if to == &e.undo {
		e.undoBytes += back.size
		e.trimUndo()
	}
	e.lastKind = editOther
	e.version++
	return true
}

// clampTo is clamp against rows other than e.lines.
func (e *editor) clampTo(rows [][]rune, p pos) pos {
	p.row = max(0, min(p.row, len(rows)-1))
	p.col = max(0, min(p.col, len(rows[p.row])))
	return p
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

// Replace swaps the byte range [from, to) of Text() for s as one undo step —
// a completion's pick replacing the word being typed — and leaves the caret
// at byte cursor of s, or after s when cursor is negative.
func (e *editor) Replace(from, to int, s string, cursor int) {
	e.anc, e.cur = e.posAt(from), e.posAt(to)
	e.sel = from < to
	e.lastKind = editOther // its own undo step, not merged into the typing before it
	if s == "" {
		e.checkpoint(editOther)
		e.deleteSelection()
	} else {
		e.Insert(s)
	}
	e.lastKind = editOther // nor the typing after it
	if cursor >= 0 && cursor <= len(s) {
		e.cur = e.posAt(from + cursor)
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
//
// The first loop reads the rune just stepped over, at p.col. When left1
// crossed a line break, p.col is the previous line's length and the rune
// stepped over is the newline, which is no rune of the line: it is taken
// as the non-word it is, rather than read past the line's end — which
// panicked, ending dbc on Alt+Left, Ctrl+W or Alt+Backspace at the start
// of a line below a non-empty one (N-326).
func (e *editor) wordLeft(p pos) pos {
	p = e.left1(p)
	for p.col > 0 && (p.col >= len(e.lines[p.row]) || !isWord(e.lines[p.row][p.col])) {
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

// cellWidth is r's width as this editor draws it: one cell under a mask.
func (e *editor) cellWidth(r rune) int {
	if e.mask != 0 {
		return 1
	}
	return runeWidth(r)
}

// dispCol is p's display column.
func (e *editor) dispCol(p pos) int {
	w := 0
	for _, r := range e.lines[p.row][:min(p.col, len(e.lines[p.row]))] {
		w += e.cellWidth(r)
	}
	return w
}

// colAtDisp is the rune column in row whose cell covers display column d.
// A click on the right half of a wide rune lands after it, the way a caret
// placed by eye would.
func (e *editor) colAtDisp(row, d int) int {
	w := 0
	for i, r := range e.lines[row] {
		rw := e.cellWidth(r)
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

	// the coloring, lexed down to the last row drawn (highlight.go)
	var hl *hlCache
	if e.sqlMode {
		hl = e.highlight(e.top + s.H() - 1)
	}
	stmtA, stmtB := -1, -1
	if e.sqlMode && curStmt[1] > curStmt[0] {
		stmtA, stmtB = e.posAt(curStmt[0]).row, e.posAt(curStmt[1]).row
	}
	selA, selB := e.ordered()
	hasSel := e.sel && e.cur != e.anc
	// the marks as positions, once per frame: there are a handful, and
	// comparing positions per rune is cheaper than offsets per rune
	type markPos struct {
		a, b pos
		def  bool
	}
	var marks []markPos
	for _, mk := range e.liveMarks() {
		marks = append(marks, markPos{e.posAt(mk.from), e.posAt(mk.to), mk.def})
	}
	// a live snippet's stops: the one being filled is lit and underlined,
	// the others only underlined, so the fields still to come are visible
	// at a glance (an empty stop has no cells, and the caret shows it)
	var stops []markPos
	if sn := e.liveSnip(); sn != nil {
		for i, st := range sn.stops {
			for _, sp := range st.Spans {
				stops = append(stops, markPos{e.posAt(sp.From), e.posAt(sp.To), i == sn.cur})
			}
		}
	}

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
		var toks []hlTok
		if hl != nil {
			toks = hl.ln[row].toks
		}
		ti := 0
		for i, r := range e.lines[row] {
			cs := bg
			if hl != nil {
				// the row's tokens are in order: step past those that
				// end before this rune
				cs = st.base
				for ti < len(toks) && int(toks[ti].to) <= i {
					ti++
				}
				if ti < len(toks) && int(toks[ti].from) <= i {
					cs = tokenStyle(st, toks[ti].kind)
				}
			}
			for _, mk := range marks {
				if inRange(pos{row, i}, mk.a, mk.b) {
					cs = cs.WithBg(st.hover.Bg).Underline()
					if mk.def {
						cs = cs.Bold()
					}
				}
			}
			for _, sp := range stops {
				if inRange(pos{row, i}, sp.a, sp.b) {
					cs = cs.Underline()
					if sp.def { // the current stop
						cs = cs.WithBg(st.hover.Bg)
					}
				}
			}
			if hasSel && inRange(pos{row, i}, selA, selB) {
				cs = cs.WithBg(st.selRange.Bg)
			}
			if e.mask != 0 {
				r = e.mask
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

// setDialect sets the engine the buffer is colored as. A change drops the
// coloring (highlight.go checks the dialect too): switching a tab from
// MySQL to Postgres changes where 'a\' ends without an edit.
//
// The coloring keeps token kinds, not styles, and the style is looked up
// as each rune is drawn, so a theme from the cats host recolors the next
// frame with nothing to drop.
func (e *editor) setDialect(d sqlsplit.Dialect) {
	if d != e.dialect {
		e.dialect, e.hl = d, hlCache{}
	}
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

// plural is "1 column" / "3 columns" — English's regular plural, which is
// all the UI's nouns need.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}
