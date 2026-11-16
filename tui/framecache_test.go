package tui

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/sqlcomplete"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/theme"
)

// The frame caches of N-278 — the editor's text, line starts and split, the
// log's wrapped rows, the transcript's laid-out messages, Inspect's lines —
// each stand in for work that was redone on every frame. These tests hold
// each one to what the uncached code computed, across the changes that
// must invalidate it.

// refText, refOffset and refPosAt are the editor's Text, offset and posAt
// as they were before the buffer cache: a string per line, every time.
func refText(e *editor) string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

func refOffset(e *editor, p pos) int {
	n := 0
	for r := 0; r < p.row && r < len(e.lines); r++ {
		n += len(string(e.lines[r])) + 1
	}
	if p.row < len(e.lines) {
		n += len(string(e.lines[p.row][:min(p.col, len(e.lines[p.row]))]))
	}
	return n
}

func refPosAt(e *editor, off int) pos {
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

// editPieces is what the random edits type: multi-byte runes, a ZWJ
// sequence, newlines in each spelling, statements and a ; inside a string.
//
// The halves of tokens that run over lines — an opening /* or ' or $$
// alone, a closing */ — make the coloring carry from row to row (N-279),
// and "CREATE TRIGGER … BEGIN" a body the split must carry.
var editPieces = []string{
	"a", "é", "中", "😀", "\U0001F468‍\U0001F469", "\n", "\r\n", "\t", "  ",
	"SELECT 1;", "'x;y'", "-- note\n", "/* c */", "$$ ; $$", "\\'", "BEGIN;\n",
	"/*", "*/", "'", "$$", "\n'a\n", "/* x\ny */", "\\", "--",
	"CREATE TRIGGER t AFTER INSERT ON o FOR EACH ROW BEGIN\n UPDATE s SET n = 1;\n",
}

// randPos is a position in e's buffer (caret positions: col ≤ line length).
func randPos(rng *rand.Rand, e *editor) pos {
	r := rng.IntN(len(e.lines))
	return pos{r, rng.IntN(len(e.lines[r]) + 1)}
}

// checkEditorCaches compares every cached read with the uncached one.
func checkEditorCaches(t *testing.T, e *editor, step string) {
	t.Helper()
	want := refText(e)
	if got := e.Text(); got != want {
		t.Fatalf("%s: Text() = %q, want %q", step, got, want)
	}
	for r, l := range e.lines {
		for _, c := range []int{0, len(l) / 2, len(l)} {
			p := pos{r, c}
			off := e.offset(p)
			if ref := refOffset(e, p); off != ref {
				t.Fatalf("%s: offset(%v) = %d, want %d", step, p, off, ref)
			}
			// on a rune start the two posAt agree, and round-trip
			if got, ref := e.posAt(off), refPosAt(e, off); got != ref || got != p {
				t.Fatalf("%s: posAt(%d) = %v, old %v, want %v", step, off, got, ref, p)
			}
		}
	}
	if past := e.offset(pos{len(e.lines), 0}); past != refOffset(e, pos{len(e.lines), 0}) {
		t.Fatalf("%s: offset past the end = %d", step, past)
	}
	if got, ref := e.posAt(len(want)+5), refPosAt(e, len(want)+5); got != ref {
		t.Fatalf("%s: posAt past the end = %v, want %v", step, got, ref)
	}
	if got := e.posAt(-3); got != (pos{}) {
		t.Fatalf("%s: posAt(-3) = %v", step, got)
	}
	// the editor's own dialect, so the split is followed edit by edit
	// (asking for another makes a whole split, which the next check of
	// this one then follows on from)
	if got, ref := e.Stmts(e.dialect), e.dialect.Split(want); !reflect.DeepEqual(got, ref) {
		t.Fatalf("%s: Stmts(%v) = %+v, want %+v", step, e.dialect, got, ref)
	}
}

// refKinds is the coloring as the editor made it before N-279: the whole
// buffer lexed at once, a token kind per rune (-1 for none).
func refKinds(e *editor) [][]sqlsplit.TokenKind {
	toks := e.dialect.Lex(refText(e))
	out := make([][]sqlsplit.TokenKind, len(e.lines))
	ti, off := 0, 0
	for r, l := range e.lines {
		out[r] = make([]sqlsplit.TokenKind, len(l))
		for i, ch := range l {
			for ti < len(toks) && toks[ti].End <= off {
				ti++
			}
			out[r][i] = -1
			if ti < len(toks) && off >= toks[ti].Start && off < toks[ti].End {
				out[r][i] = toks[ti].Kind
			}
			off += len(string(ch))
		}
		off++
	}
	return out
}

// checkHighlight lexes the coloring down to row upto, as a frame drawing
// that far would, and holds every row it knows for certain to the whole
// buffer's lex. The rows past it are left as they are — tentative or not
// lexed — for later edits and checks to meet.
func checkHighlight(t *testing.T, e *editor, upto int, step string) {
	t.Helper()
	h := e.highlight(upto)
	if h.done <= min(upto, len(e.lines)-1) {
		t.Fatalf("%s: lexed to row %d, asked for %d", step, h.done, upto)
	}
	ref := refKinds(e)
	for r := 0; r < h.done; r++ {
		got := make([]sqlsplit.TokenKind, len(e.lines[r]))
		for i := range got {
			got[i] = -1
		}
		for _, tk := range h.ln[r].toks {
			for i := tk.from; i < tk.to; i++ {
				got[i] = tk.kind
			}
		}
		if !reflect.DeepEqual(got, ref[r]) {
			t.Fatalf("%s (%v): row %d %q colored %v, want %v (in %+v)", step, e.dialect, r, string(e.lines[r]), got, ref[r], h.ln[r].in)
		}
	}
}

// undoOracle is what undo must give back, kept beside the editor: the text
// before each step, recorded as the step opens. An undo must restore that
// text exactly; a row changed in place under a kept step would not.
type undoOracle struct {
	undo, redo []string
	base       [][]rune // the open step's base last seen, to tell a new step
}

// before notes the editor's state ahead of an operation; after reads what
// the operation did to the stacks and checks the text against it.
func (o *undoOracle) after(t *testing.T, e *editor, text string, nu, nr int, step string) {
	t.Helper()
	now := refText(e)
	// a step that opened is told by its base, a snapshot no step had
	// before: a redo pushes a closed step, and typing after an undo moves
	// both stacks by the same counts as a redo would
	opened := false
	if len(e.undo) > 0 {
		top := e.undo[len(e.undo)-1]
		opened = top.base != nil && (len(o.base) == 0 || &top.base[0] != &o.base[0])
	}
	switch {
	case opened:
		// its base is the text before this operation
		o.undo, o.base = append(o.undo, text), e.undo[len(e.undo)-1].base
		if len(e.redo) == 0 {
			o.redo = nil
		}
	case len(e.undo) == 0 && len(e.redo) == 0:
		// SetText: a new buffer with no history (an undo leaves a step
		// on the redo stack, a redo one on the undo stack)
		o.undo, o.redo = nil, nil
	case len(e.redo) == nr+1:
		// an undo: the redo stack grows by the step it undid (the undo
		// stack may also lose its oldest to the byte cap, below)
		want := o.undo[len(o.undo)-1]
		o.undo, o.redo = o.undo[:len(o.undo)-1], append(o.redo, text)
		if now != want {
			t.Fatalf("%s: undo gave %q, want %q", step, now, want)
		}
	case len(e.redo) == nr-1:
		want := o.redo[len(o.redo)-1]
		o.redo, o.undo = o.redo[:len(o.redo)-1], append(o.undo, text)
		if now != want {
			t.Fatalf("%s: redo gave %q, want %q", step, now, want)
		}
	default:
		if len(e.redo) == 0 {
			o.redo = nil
		}
	}
	// the caps drop the oldest steps
	o.undo = o.undo[max(len(o.undo)-len(e.undo), 0):]
	if len(o.undo) != len(e.undo) || len(o.redo) != len(e.redo) {
		t.Fatalf("%s: the oracle holds %d/%d steps, the editor %d/%d", step, len(o.undo), len(o.redo), len(e.undo), len(e.redo))
	}
}

// Every editing path keeps the cached text, line starts and split equal to
// a fresh join and split: typed keys, pastes, deletes, undo/redo, replace,
// a rename's edits, SetText, and a View/Restore over changed text. Reads
// are interleaved at random, so a stale entry left by one edit would be
// read by the next step rather than rebuilt by luck.
func TestEditorCachesFollowEveryEdit(t *testing.T) {
	keys := []string{"backspace", "delete", "enter", "left", "right", "up", "down",
		"home", "end", "ctrl+z", "ctrl+y", "shift+left", "shift+right", "shift+down",
		"alt+backspace", "ctrl+a"}
	dialects := []sqlsplit.Dialect{sqlsplit.Postgres, sqlsplit.MySQL, sqlsplit.SQLite, sqlsplit.Generic}
	for seed := range uint64(60) {
		rng := rand.New(rand.NewPCG(seed, 278))
		e := newEditor(false)
		e.sqlMode = true
		e.setDialect(dialects[seed%uint64(len(dialects))])
		e.SetText("SELECT 1;\nSELECT 'a;b';\n-- c\nSELECT 2")
		var o undoOracle
		var v11text string
		for i := range 300 {
			step := fmt.Sprintf("seed %d step %d", seed, i)
			text, nu, nr := refText(e), len(e.undo), len(e.redo)
			saved := o
			op := rng.IntN(12)
			switch op {
			case 0, 1, 2:
				e.Insert(editPieces[rng.IntN(len(editPieces))])
			case 3:
				e.HandleKey(keyMsg(keys[rng.IntN(len(keys))]))
			case 4:
				e.move(randPos(rng, e), rng.IntN(2) == 0)
			case 5:
				e.Backspace()
			case 6:
				e.Delete()
			case 7:
				if rng.IntN(2) == 0 {
					e.Undo()
				} else {
					e.Redo()
				}
			case 8:
				a, b := e.offset(randPos(rng, e)), e.offset(randPos(rng, e))
				a, b = min(a, b), max(a, b)
				s := editPieces[rng.IntN(len(editPieces))]
				e.Replace(a, b, s, rng.IntN(len(s)+2)-1)
			case 9:
				// a rename: two non-overlapping edits as one step
				a, b := e.offset(randPos(rng, e)), e.offset(randPos(rng, e))
				a, b = min(a, b), max(a, b)
				e.ApplyEdits([]sqlcomplete.Edit{{From: b, To: b, Text: "x中"}, {From: a, To: a, Text: "y"}})
			case 10:
				if rng.IntN(8) == 0 {
					e.SetText(strings.Repeat("SELECT 1; ", rng.IntN(4)) + "\n中")
				} else {
					e.DeleteWordBack()
				}
			case 11:
				// a console swap and back, the text changed meanwhile
				v := e.View()
				v11text = v.text
				e.SetText("other")
				e.Text()
				text := v.text
				if rng.IntN(2) == 0 {
					text += "\nSELECT 3;"
				}
				e.Restore(v, text)
			}
			if op == 11 {
				// the swap parks the stacks and puts them back; a changed
				// text is then one more step
				o = saved
				o.base = nil
				text, nu, nr = v11text, len(e.undo), len(e.redo)
				if e.Text() != v11text {
					nu--
				}
			}
			o.after(t, e, text, nu, nr, step)
			if rng.IntN(3) == 0 {
				checkEditorCaches(t, e, step)
			}
			if rng.IntN(2) == 0 {
				// a frame: drawn rows near the caret, or anywhere
				upto := e.cur.row + rng.IntN(10)
				if rng.IntN(4) == 0 {
					upto = rng.IntN(len(e.lines) + 3)
				}
				checkHighlight(t, e, upto, step)
			}
		}
		checkEditorCaches(t, e, fmt.Sprintf("seed %d end", seed))
		checkHighlight(t, e, len(e.lines), fmt.Sprintf("seed %d end", seed))
	}
}

// posAt on an offset inside a rune's bytes stays on the line: the old
// conversion counted each stray byte as a rune and could put the column
// past the line's end.
func TestEditorPosAtMidRuneStaysOnTheLine(t *testing.T) {
	e := newEditor(false)
	e.SetText("中文\nab")
	for off := range len(e.Text()) + 1 {
		p := e.posAt(off)
		if p.col > len(e.lines[p.row]) {
			t.Errorf("posAt(%d) = %v, past line %d's %d runes", off, p, p.row, len(e.lines[p.row]))
		}
	}
	if p := e.posAt(1); p != (pos{0, 1}) {
		t.Errorf("posAt(1), inside 中, = %v, want the end of 中", p)
	}
}

// A theme from the cats host recolors the editor on the next frame, not
// after the next keystroke: the highlight is kept per palette as well as
// per edit.
func TestEditorHighlightFollowsTheTheme(t *testing.T) {
	e := newEditor(false)
	e.sqlMode = true
	e.SetText("SELECT 1")
	dark := newStyles(theme.Default())
	if got := e.rowStyles(dark, 0)[0]; got != dark.synKeyword {
		t.Fatalf("keyword = %+v, want %+v", got, dark.synKeyword)
	}
	pal := theme.Default()
	pal.Accent = "#ff0000"
	other := newStyles(pal)
	if got := e.rowStyles(other, 0)[0]; got != other.synKeyword {
		t.Errorf("after a theme change the keyword kept %+v, want %+v", got, other.synKeyword)
	}
}

// The log's kept rows equal a wrap of every line from scratch, through
// adds (one line and several), trims past logMaxLines (including more lines
// than were ever drawn), width changes, clear and the holding log's take.
func TestLogWrapCacheMatchesAFullWrap(t *testing.T) {
	full := func(l *logPane, w int) []visual {
		var rows []visual
		for _, ln := range l.lines {
			for i, part := range wrap(ln.text, w) {
				v := visual{kind: ln.kind, text: part}
				if i == 0 {
					v.stamp = ln.at.Format("15:04:05")
				}
				rows = append(rows, v)
			}
		}
		return rows
	}
	rng := rand.New(rand.NewPCG(278, 1))
	l, hold := newLogPane(), newLogPane()
	widths := []int{10, 23, 40, 77}
	w := widths[0]
	text := func() string {
		n := rng.IntN(5)
		if n == 4 {
			return strings.Repeat("word 中 ", 30) // wraps at every width
		}
		return strings.Repeat("ab ", n*7) + fmt.Sprint(rng.IntN(1000))
	}
	for i := range 4000 {
		switch op := rng.IntN(100); {
		case op < 70:
			l.add(logKind(rng.IntN(6)), text())
		case op < 78:
			l.add(logInfo, text()+"\n"+text()+"\n") // a multi-line message
		case op < 82:
			// a burst bigger than the log between two frames
			for range logMaxLines + rng.IntN(50) {
				l.add(logInfo, "burst")
			}
		case op < 88:
			w = widths[rng.IntN(len(widths))]
		case op < 89:
			l.clear()
		case op < 95:
			hold.add(logWarn, text())
		default:
			l.take(hold)
			if len(hold.lines) != 0 || len(hold.wrapped(w)) != 0 {
				t.Fatal("take left lines in the holding log")
			}
		}
		if rng.IntN(4) == 0 || i == 3999 {
			if got, want := l.wrapped(w), full(l, w); !reflect.DeepEqual(got, want) {
				t.Fatalf("step %d: %d kept rows, want %d", i, len(got), len(want))
			}
		}
	}
}

// The transcript's kept rows equal a fresh layout of the same messages —
// through streamed chunks, the end of a stream (the copy chip appears), a
// new width, dialect or palette, and a new conversation with fewer
// messages than the last.
func TestChatRowsCacheMatchesAFreshLayout(t *testing.T) {
	st := newStyles(theme.Default())
	p := newChatPane()
	check := func(step string, w int) {
		t.Helper()
		got := p.layoutRows(st, w)
		fresh := newChatPane()
		fresh.msgs, fresh.streaming, fresh.dialect = p.msgs, p.streaming, p.dialect
		if want := fresh.layoutRows(st, w); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %d rows, a fresh layout has %d", step, len(got), len(want))
		}
	}
	p.add(roleUser, "why is it slow?")
	p.streaming = true
	p.appendAgent("It scans. ")
	check("first chunk", 60)
	p.appendAgent("Add an index:\n\n```sql\nCREATE INDEX ON t (a);\n")
	check("a code block opens", 60)
	p.appendAgent("SELECT 'a\\' FROM t;\n```\n")
	check("and closes", 60)
	p.streaming = false
	check("the stream ends: the copy chip", 60)
	check("a narrower pane", 31)
	p.dialect = sqlsplit.MySQL // 'a\' reads differently
	check("another dialect", 31)
	pal := theme.Default()
	pal.Fg = "#123456"
	st = newStyles(pal)
	check("another theme", 31)
	p.add(roleUser, "and now?")
	p.add(roleErr, "agent stopped")
	check("more messages", 31)
	p.msgs = nil
	p.add(roleInfo, "model: x")
	check("a new conversation", 31)
}

// Inspect wraps its value once per width: the lines are kept across
// frames and clicks, and a resize wraps them again at the new width.
func TestInspectWrapsOncePerWidth(t *testing.T) {
	m := newTestModel(t)
	in := &inspectModal{value: strings.Repeat("a long line of text to wrap ", 400) + "\nend", hoverBtn: -1}
	m.openModal(in)
	frame(m)
	first := in.lines
	if len(first) < 10 {
		t.Fatalf("%d lines", len(first))
	}
	frame(m)
	m.modalRect() // as each click and wheel step does
	if &in.lines[0] != &first[0] || in.linesW == 0 {
		t.Fatal("a second frame wrapped the value again")
	}
	w0 := in.linesW
	drive(t, m, tea.WindowSizeMsg{Width: 200, Height: 50})
	frame(m)
	if in.linesW == w0 {
		t.Fatalf("a resize kept the width %d", w0)
	}
	var want []string
	for _, l := range strings.Split(in.value, "\n") {
		want = append(want, wrap(l, in.linesW)...)
	}
	if !reflect.DeepEqual(in.lines, want) {
		t.Errorf("after a resize: %d lines, want %d", len(in.lines), len(want))
	}
}

// byteEdit, from a row diff, describes the edit exactly: the old text with
// its bytes [at, oldEnd) replaced by the new text's [at, newEnd) is the
// new text. Splitting.Edit checks only that the lengths add up, and falls
// back to a whole split when they do not, so a wrong end there would cost
// every keystroke a whole split again without failing a split test.
func TestByteEditDescribesTheEdit(t *testing.T) {
	for seed := range uint64(40) {
		rng := rand.New(rand.NewPCG(seed, 2791))
		e := newEditor(false)
		e.SetText("SELECT 1;\nSELECT 'a;b';\n-- c\nSELECT 2")
		for i := range 200 {
			rows, old := e.snap(), e.Text()
			// several edits between two reads, as between two frames
			for range 1 + rng.IntN(3) {
				switch rng.IntN(5) {
				case 0, 1:
					e.Insert(editPieces[rng.IntN(len(editPieces))])
				case 2:
					e.Backspace()
				case 3:
					e.Delete()
				default:
					e.move(randPos(rng, e), false)
					if rng.IntN(2) == 0 {
						e.Undo()
					}
				}
			}
			b := e.buffer()
			p, s := rowsDiff(rows, b.rows)
			at, oldEnd, newEnd := b.byteEdit(p, s, len(old))
			if at < 0 || at > oldEnd || oldEnd > len(old) || at > newEnd || newEnd > len(b.text) ||
				old[:at]+b.text[at:newEnd]+old[oldEnd:] != b.text {
				t.Fatalf("seed %d step %d: rows %d|%d, edit [%d,%d)→[%d,%d) does not take %q to %q", seed, i, p, s, at, oldEnd, at, newEnd, old, b.text)
			}
		}
	}
}
