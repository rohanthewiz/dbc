package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/snippet"
	"github.com/rohanthewiz/dbc/userdata"
)

// Live snippets in the SQL editor: a saved template (package snippet has
// the syntax, userdata.Snippets the store) expanded at the caret, whose tab
// stops are then filled in one after another.
//
// HOW ONE IS INSERTED. Typing its name and pressing Tab, or picking it in
// the snippets dialog (⌥S, the editor menu's "Insert snippet…"). Only the
// dialog can wrap a selection — typing a name would replace it — so a
// $SELECTION$ snippet is a dialog pick with text selected.
//
// THE LIVE SESSION. After the insert the first stop is selected, its
// default with it, and the editor is in the session until it ends:
//
//	SELECT ▒col▒, count(*) AS n      ▒…▒ the stop being filled: lit
//	FROM _table_                     _…_ a stop still to come: underlined
//	GROUP BY ▒col▒                   a stop used twice mirrors: typing in
//	                                 the first use rewrites the second
//
//	Tab        the next stop, by number; past the last, the caret goes to
//	           $END$ (else the snippet's end) and the session ends
//	Shift+Tab  the previous stop
//	Esc        ends it, the text as it is
//
// It also ends when the caret leaves the stop being filled (a click, an
// arrow past its edge) or an edit lands outside it (Ctrl+Z past the
// insert, a tab switch): the stops are byte offsets, and only an edit
// inside the current one can be followed.
//
// WHY THE KEYS GO WHERE THEY DO. Tab is the app's focus key and the
// completion popup's pick, so snippetKey runs before both, and claims Tab
// only when it has something to do: a snippet's exact name before the
// caret (which beats the popup — a name you saved is the word you meant),
// or a session to step through (which yields to an open popup, so the
// popup's Tab still picks a table name typed into a stop; the next Tab
// moves on).

// snipSession is the live snippet. Its offsets are into the editor's
// Text() as of ver; snipSync moves them along with each edit inside the
// current stop, and ends the session on any other.
type snipSession struct {
	name  string
	stops []snippet.Stop // by number: the order Tab visits them
	cur   int            // the stop being filled (an index into stops)
	end   int            // where the caret goes after the last stop
	text  string         // Text() at ver
	ver   int
}

// liveSnip is the session while it still describes the buffer.
func (e *editor) liveSnip() *snipSession {
	if e.snip != nil && e.snip.ver != e.version {
		// an edit no sync has seen: only possible between a change and the
		// end of its Update, and drawing then would box the wrong text
		return nil
	}
	return e.snip
}

// snipStart begins a session over x, inserted at byte offset at, on the
// first stop.
func (e *editor) snipStart(name string, x snippet.Expansion, at int) {
	sn := &snipSession{name: name, end: at + x.End}
	for _, st := range x.Stops {
		spans := make([]snippet.Span, len(st.Spans))
		for i, sp := range st.Spans {
			spans[i] = snippet.Span{From: at + sp.From, To: at + sp.To}
		}
		st.Spans = spans
		sn.stops = append(sn.stops, st)
	}
	e.snip = sn
	e.snipSelect()
}

// snipSelect selects the current stop's first use — its default, so typing
// replaces it — and notes the buffer as the session's.
func (e *editor) snipSelect() {
	sn := e.snip
	sp := sn.stops[sn.cur].Spans[0]
	e.selectSpan(sp.From, sp.To)
	e.lastKind = editOther // typing into the stop starts its own undo step
	sn.text, sn.ver = e.Text(), e.version
}

// snipStep moves d stops on (±1). Past the last stop the caret goes to the
// end mark and the session is over; before the first it stays put.
func (e *editor) snipStep(d int) {
	sn := e.snip
	switch to := sn.cur + d; {
	case to >= len(sn.stops):
		e.snip = nil
		e.sel = false
		e.cur = e.posAt(sn.end)
		e.goal = e.dispCol(e.cur)
	case to >= 0:
		sn.cur = to
		e.snipSelect()
	}
}

// snipSync brings the session in step with the buffer, once per Update:
// an edit inside the current stop is copied into the stop's other uses
// and every offset after it moves; anything else ends the session.
//
// "Inside" is tested on the text, not on the keys that made the change: a
// typed letter, a paste, a completion's pick and a Ctrl+Z all arrive the
// same way (snipAbsorb).
func (e *editor) snipSync() {
	sn := e.snip
	if sn == nil {
		return
	}
	if e.version != sn.ver {
		if !e.snipAbsorb() {
			e.snip = nil
			return
		}
	}
	// the caret (and a selection's other end) must stay on the stop: one
	// that wanders off has left the snippet, as in any editor with these
	sp := sn.stops[sn.cur].Spans[0]
	c := e.Caret()
	inside := func(off int) bool { return off >= sp.From && off <= sp.To }
	if !inside(c) || (e.sel && !inside(e.offset(e.anc))) {
		e.snip = nil
	}
}

// snipAbsorb takes in an edit to the buffer: true when it was one of the
// two shapes a stop's edits take, with the mirrors rewritten and the
// offsets moved.
//
// AN EDIT TO THE FIRST USE (a key, a paste, a completion's pick). With the
// use at [from, to) of the old text, the old text before from and after to
// are both still there, at the start and the end of the new text; what is
// between them is the stop's new value, which the mirrors are then given:
//
//	old:  SELECT ▒col▒, count(*) … GROUP BY ▒col▒
//	      └─ head ─┘    └──────── tail ─────────┘
//	new:  SELECT ▒cust▒, count(*) … GROUP BY ▒col▒   head and tail intact:
//	                                                 value "cust"
//	out:  SELECT ▒cust▒, count(*) … GROUP BY ▒cust▒  the mirror rewritten
//
// THE SAME CHANGE TO EVERY USE (Ctrl+Z or redo of mirrored typing: the
// snapshot holds the mirrors as they were). The uses all grew or shrank by
// the same amount, so the length change splits evenly between them, which
// gives the value's length; the edit was this shape when the old text with
// every use holding that value is exactly the new text.
func (e *editor) snipAbsorb() bool {
	sn := e.snip
	old, now := sn.text, e.Text()
	first := sn.stops[sn.cur].Spans[0]
	if len(now) < first.From || old[:first.From] != now[:first.From] {
		return false
	}
	caret := e.Caret() - first.From // within the value, if the caret is still in it

	var val string
	var r snipLayout
	tail := len(old) - first.To
	uses, delta := len(sn.stops[sn.cur].Spans), len(now)-len(old)
	switch {
	case len(now) >= first.From+tail && old[first.To:] == now[len(now)-tail:]:
		val = now[first.From : len(now)-tail]
		r = sn.rebuild(val)
	case uses > 1 && delta%uses == 0:
		n := first.To - first.From + delta/uses
		if n < 0 || first.From+n > len(now) {
			return false
		}
		val = now[first.From : first.From+n]
		if r = sn.rebuild(val); r.text != now {
			return false
		}
	default:
		return false
	}

	if r.text != now {
		// the edit itself was already an undo step (or merged into the
		// typing before it); the mirrors join it, so one Ctrl+Z takes the
		// letter back from every use at once
		e.lines = splitLines(r.text)
		e.version++
	}
	for i := range sn.stops {
		sn.stops[i].Spans = r.spans[i]
	}
	sn.end = r.end
	e.sel = false
	e.cur = e.posAt(sn.stops[sn.cur].Spans[0].From + max(0, min(caret, len(val))))
	e.goal = e.dispCol(e.cur)
	sn.text, sn.ver = r.text, e.version
	return true
}

// snipLayout is the session's text with the current stop holding a new
// value: the text, every stop's spans in it, and the end mark.
type snipLayout struct {
	text  string
	spans [][]snippet.Span // by stop, as sn.stops
	end   int
}

// rebuild lays the session's text out again with every use of the current
// stop holding val and every other span as it was. It works from the text
// the offsets are into (sn.text) and changes nothing on the session.
func (sn *snipSession) rebuild(val string) snipLayout {
	old := sn.text
	type ref struct{ stop, span int }
	var order []ref
	out := snipLayout{spans: make([][]snippet.Span, len(sn.stops))}
	for i, st := range sn.stops {
		out.spans[i] = make([]snippet.Span, len(st.Spans))
		for j := range st.Spans {
			order = append(order, ref{i, j})
		}
	}
	// spans never overlap, so ordering by start is text order
	start := func(r ref) int { return sn.stops[r.stop].Spans[r.span].From }
	slices.SortFunc(order, func(a, b ref) int { return start(a) - start(b) })

	var b strings.Builder
	at, shift := 0, 0
	for _, r := range order {
		sp := sn.stops[r.stop].Spans[r.span]
		b.WriteString(old[at:sp.From])
		from := b.Len()
		if r.stop == sn.cur {
			b.WriteString(val)
		} else {
			b.WriteString(old[sp.From:sp.To])
		}
		// the end mark moves with the spans before it; one at a span's own
		// start stays in front of it, one at its end follows it
		if sp.To <= sn.end {
			shift += b.Len() - from - (sp.To - sp.From)
		}
		at = sp.To
		out.spans[r.stop][r.span] = snippet.Span{From: from, To: b.Len()}
	}
	b.WriteString(old[at:])
	out.text, out.end = b.String(), sn.end+shift
	return out
}

// snippetKey is the editor's snippet keys (see the top of the file); used
// reports whether it took the key. It runs before the completion popup and
// the app's chords.
func (m *Model) snippetKey(k tea.KeyPressMsg) (cmd tea.Cmd, used bool) {
	if m.focus != focusEditor {
		return nil, false
	}
	e := m.editor
	sn := e.liveSnip()
	switch k.String() {
	case "tab":
		if sn == nil {
			return m.expandWord()
		}
		if m.complLive() != nil {
			return nil, false // the popup's Tab picks; the next one moves on
		}
		e.snipStep(1)
	case "shift+tab":
		if sn == nil {
			return nil, false
		}
		m.compl = nil
		e.snipStep(-1)
	case "esc":
		if sn == nil || m.complLive() != nil {
			return nil, false // Esc closes the popup first
		}
		e.snip = nil
		m.logf(logInfo, "snippet %s: done", sn.name)
	default:
		return nil, false
	}
	m.drag.follow = true
	return nil, true
}

// expandWord is Tab with no session: when the word before the caret is a
// snippet's name, the word becomes the snippet.
func (m *Model) expandWord() (tea.Cmd, bool) {
	word, from, ok := m.editor.wordBeforeCaret()
	if !ok {
		return nil, false
	}
	sn, found, err := m.snips.Get(word)
	if err != nil {
		// Tab still does what it did before snippets: the error is said,
		// not allowed to take the key
		m.logf(logWarn, "snippets: %s", serr.StringFromErr(err))
		return nil, false
	}
	if !found {
		return nil, false
	}
	m.compl = nil
	return nil, m.insertSnippet(sn, from, m.editor.Caret())
}

// wordBeforeCaret is the word the caret ends, by the editor's word rule,
// with its start — when there is one to expand: no selection, the caret at
// the word's end (not inside it), and no "." before it (o.sel is a column,
// not a snippet).
func (e *editor) wordBeforeCaret() (string, int, bool) {
	if e.sel && e.cur != e.anc {
		return "", 0, false
	}
	line := e.lines[e.cur.row]
	i := e.cur.col
	for i > 0 && isWord(line[i-1]) {
		i--
	}
	if i == e.cur.col || (e.cur.col < len(line) && isWord(line[e.cur.col])) || (i > 0 && line[i-1] == '.') {
		return "", 0, false
	}
	return string(line[i:e.cur.col]), e.offset(pos{e.cur.row, i}), true
}

// insertSnippet replaces the editor's bytes [from, to) — the name typed, a
// selection, or nothing at the caret — with sn expanded, and starts the
// live session on its first stop. It reports whether it inserted; a body
// that no longer parses (saved by an older dbc, or edited by hand) is
// refused with the reason.
//
// The expansion is one Replace, so one Ctrl+Z takes the whole snippet back
// — the stops typed into after it are their own steps.
func (m *Model) insertSnippet(sn userdata.Snippet, from, to int) bool {
	e := m.editor
	// the editor stores four spaces for a tab (normalize): the template
	// must say the same, or every offset after a tab would be off by three
	tpl, err := snippet.Parse(normalize(sn.Body, false))
	if err != nil {
		m.logf(logErr, "snippet %s: %v — fix it in the snippets dialog (⌥S)", sn.Name, err)
		return false
	}
	text := e.Text()
	lineStart := strings.LastIndexByte(text[:from], '\n') + 1
	line := text[lineStart:]
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	ctx := snippet.Context{
		Indent:    line[:len(line)-len(strings.TrimLeft(line, " "))],
		SelPrefix: text[lineStart:from],
	}
	// the text replaced is the selection only when it was one: a typed
	// name is not wrapped by its own snippet
	if s, a, b := e.Selection(); s != "" && a == from && b == to {
		ctx.Selection = s
	}
	x := snippet.Expand(tpl, ctx)
	e.Replace(from, to, x.Text, x.End)
	m.focus = focusEditor
	m.drag.follow = true
	if len(x.Stops) == 0 {
		m.logf(logOk, "snippet %s inserted · ^Z takes it back", sn.Name)
		return true
	}
	e.snipStart(sn.Name, x, from)
	m.logf(logInfo, "snippet %s · Tab next field · ⇧Tab back · Esc done", sn.Name)
	return true
}

// insertPicked is the snippets dialog's pick: sn in place of the selection
// (its $SELECTION$), or at the caret.
func (m *Model) insertPicked(sn userdata.Snippet) {
	_, from, to := m.editor.Selection()
	m.insertSnippet(sn, from, to)
}

// snippetLabel names the editor menu's snippets row by what a pick will do.
func snippetLabel(sel string) string {
	if sel != "" {
		return "Surround with snippet…"
	}
	return "Insert snippet…"
}
