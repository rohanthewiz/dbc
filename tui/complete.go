package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// The editor's completion popup: the columns, tables, join clauses,
// keywords, functions and types sqlcomplete suggests at the caret, from the
// active connection's schema — the list dbc web's Monaco shows, by the same
// rules.
//
//	SELECT o.cu▮ FROM orders o
//	         ┌──────────────────────────────────┐┌────────────────────────────┐
//	         │· customer_id   bigint FK NOT NULL ││public.orders.customer_id   │
//	         │· created_at    timestamptz        ││bigint NOT NULL             │
//	         └──────────────────────────────────┘│references customers(id)    │
//	                                             └────────────────────────────┘
//
// WHEN IT OPENS. Ctrl+Space asks anywhere. It also opens by itself after a
// "." (o.▮), a "::" (a cast) and on the second letter of a word, and follows
// the typing while open: each edit asks again, and a character that ends the
// word (a space, a comma) closes it. A terminal has no hover to dismiss a
// stray popup with, so it never opens on the first letter, where nearly
// every word would set it off.
//
// THE KEYS while it is open: ↑/↓ (PgUp/PgDn) move, Tab picks, Enter picks
// only when the pick changes the text — at the end of a word already typed
// in full, Enter is a new line as always — and Esc closes. Every other key
// goes to the editor as if the popup were not there. A click on a row picks
// it; a click elsewhere closes it.
//
// THE SCHEMA is the workspace's cache (workspace.Complete). The first
// request on a connection finds it cold: the popup asks LoadCompletions as a
// tea.Cmd, off the event loop, and opens when it lands — if the caret is
// still where a popup is wanted.

// complRows is how many suggestions the popup shows at once.
const complRows = 8

// complPopup is the open popup.
type complPopup struct {
	res      sqlcomplete.Result
	cur, top int
	rect     Rect // the list, as last drawn: what a click is mapped from
	// ver is the editor version the popup answers for. Anything that
	// changes the buffer behind its back — a history pick, a tab switch, a
	// paste — leaves it stale, and a stale popup is closed (live).
	ver int
}

// complLive is the popup when it still answers for the buffer and the
// editor has the keyboard, closing it otherwise.
func (m *Model) complLive() *complPopup {
	if m.compl != nil && (m.focus != focusEditor || m.compl.ver != m.editor.version) {
		m.compl = nil
	}
	return m.compl
}

// complState is the model's completion bookkeeping besides the popup.
type complState struct {
	loading  bool   // a LoadCompletions is in flight
	want     bool   // open the popup when it lands
	explicit bool   // …because Ctrl+Space asked, not typing
	noted    string // the last load error logged, so it is logged once
}

// complLoadedMsg is a LoadCompletions landing.
type complLoadedMsg struct{ err error }

// openCompletion asks for suggestions at the caret and shows them, or loads
// the schema first. explicit is Ctrl+Space: it opens on an empty prefix and
// says so when there is nothing to suggest.
func (m *Model) openCompletion(explicit bool) tea.Cmd {
	res, ready := m.ws.Complete(m.editor.Text(), m.editor.Caret())
	if !ready {
		m.complSt.want, m.complSt.explicit = true, explicit
		if m.complSt.loading {
			return nil
		}
		m.complSt.loading = true
		if explicit {
			m.setStatus("reading the schema for completion…")
		}
		ws := m.ws
		return func() tea.Msg {
			return complLoadedMsg{err: ws.LoadCompletions(context.Background())}
		}
	}
	if len(res.Items) == 0 {
		m.compl = nil
		if explicit {
			m.setStatus("no suggestions here")
		}
		return nil
	}
	// keep the highlighted row on the same item as the list narrows
	cur := 0
	if old := m.compl; old != nil && old.cur < len(old.res.Items) {
		was := old.res.Items[old.cur].Label
		for i, it := range res.Items {
			if it.Label == was {
				cur = i
				break
			}
		}
	}
	m.compl = &complPopup{res: res, cur: cur, ver: m.editor.version}
	m.compl.scroll()
	return nil
}

// complLoaded reopens the popup a cold cache held up, and reports a failed
// load once (completion goes on with the vocabulary alone).
func (m *Model) complLoaded(msg complLoadedMsg) tea.Cmd {
	m.complSt.loading = false
	if msg.err != nil {
		if note := msg.err.Error(); note != m.complSt.noted {
			m.complSt.noted = note
			m.log(logWarn, "completion is without the schema: "+note)
		}
	}
	if !m.complSt.want || m.focus != focusEditor {
		return nil
	}
	m.complSt.want = false
	if !m.complSt.explicit && !m.complAutoHere() {
		return nil // the user typed on past where it was wanted
	}
	return m.openCompletion(m.complSt.explicit)
}

// complKey handles a key while the popup is open, and reports whether it
// used it; an unused key goes on to the editor.
func (m *Model) complKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	p := m.compl
	switch k.String() {
	case "up":
		p.move(-1)
	case "down":
		p.move(1)
	case "pgup":
		p.move(-complRows)
	case "pgdown":
		p.move(complRows)
	case "esc":
		m.compl = nil
	case "tab":
		m.complPick(p.cur)
	case "enter":
		it := p.res.Items[p.cur]
		if it.Insert == p.res.Prefix {
			m.compl = nil
			return nil, false // nothing to change: Enter is a new line
		}
		m.complPick(p.cur)
	default:
		// an app chord (^R, ^X …) acts on the buffer as it stands, and the
		// popup would float over its outcome; a typed key goes on to the
		// editor, which refreshes the popup (complAfterKey)
		if k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper) != 0 {
			m.compl = nil
		}
		return nil, false
	}
	return nil, true
}

// complPick replaces the word being typed with item i and closes the popup.
// A schema ("public.") or a qualified pick ends in a dot and asks again at
// once, for the table or column after it.
func (m *Model) complPick(i int) tea.Cmd {
	p := m.compl
	m.compl = nil
	if p == nil || i < 0 || i >= len(p.res.Items) {
		return nil
	}
	it := p.res.Items[i]
	m.editor.Replace(p.res.From, p.res.To, it.Insert, it.Cursor)
	m.drag.follow = true
	if strings.HasSuffix(it.Insert, ".") {
		return m.openCompletion(false)
	}
	return nil
}

// complAfterKey follows an editor key: it refreshes an open popup as the word
// grows or shrinks, opens one where typing calls for it, and closes it when
// the caret leaves the word.
func (m *Model) complAfterKey(k tea.KeyPressMsg, before int) tea.Cmd {
	if m.editor.version == before {
		// a move, not an edit: the word the popup was for is left
		if s := k.String(); s != "up" && s != "down" {
			m.compl = nil
		}
		return nil
	}
	typed := k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper) == 0
	switch {
	case typed && m.complAutoHere():
		return m.openCompletion(false)
	case m.compl != nil && !typed && m.complInWord():
		// a backspace inside the word: follow it
		return m.openCompletion(false)
	}
	m.compl = nil
	return nil
}

// complAutoHere reports whether the text before the caret calls for a popup
// without being asked: right after "." or "::", or two letters into a word —
// or any length into one while the popup is already open.
func (m *Model) complAutoHere() bool {
	text, caret := m.editor.Text(), m.editor.Caret()
	if caret == 0 {
		return false
	}
	switch {
	case text[caret-1] == '.':
		return true
	case caret >= 2 && text[caret-2:caret] == "::":
		return true
	}
	n := wordLenBefore(text, caret)
	if n == 0 || isDigitByte(text[caret-n]) {
		return false
	}
	return n >= 2 || m.compl != nil
}

// complInWord reports whether the caret is still in a word, or right after
// the "." a popup was opened on.
func (m *Model) complInWord() bool {
	text, caret := m.editor.Text(), m.editor.Caret()
	return caret > 0 && (wordLenBefore(text, caret) > 0 || text[caret-1] == '.')
}

// wordLenBefore is the length in bytes of the identifier that ends at caret.
func wordLenBefore(text string, caret int) int {
	i := caret
	for i > 0 {
		b := text[i-1]
		if b == '_' || b == '$' || isDigitByte(b) || (b|0x20 >= 'a' && b|0x20 <= 'z') || b >= 0x80 {
			i--
			continue
		}
		break
	}
	return caret - i
}

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// move steps the highlight by d, clamped, keeping it on screen.
func (p *complPopup) move(d int) {
	p.cur = min(max(p.cur+d, 0), len(p.res.Items)-1)
	p.scroll()
}

func (p *complPopup) scroll() {
	if p.cur < p.top {
		p.top = p.cur
	}
	if p.cur >= p.top+complRows {
		p.top = p.cur - complRows + 1
	}
}

// itemAt is the item drawn at screen row y, or -1.
func (p *complPopup) itemAt(x, y int) int {
	if !p.rect.Contains(x, y) {
		return -1
	}
	i := p.top + y - p.rect.Y - 1
	if i < p.top || i >= len(p.res.Items) || i >= p.top+complRows {
		return -1
	}
	return i
}

// complGlyph marks an item's kind in the popup's first column.
var complGlyph = map[sqlcomplete.Kind]string{
	sqlcomplete.KindColumn: "·", sqlcomplete.KindTable: "▦", sqlcomplete.KindView: "◫",
	sqlcomplete.KindSchema: "◆", sqlcomplete.KindAlias: "@", sqlcomplete.KindJoin: "⋈",
	sqlcomplete.KindKeyword: "k", sqlcomplete.KindFunction: "ƒ", sqlcomplete.KindType: "τ",
}

// draw paints the popup under the word being typed — at (x, y), the caret's
// cell — or above it when there is no room below, and the highlighted item's
// documentation (a table's DDL, a function's description) in a box beside
// it when the screen is wide enough.
func (p *complPopup) draw(c *Canvas, st styles, x, y int) {
	n := min(len(p.res.Items), complRows)
	lw, dw := 0, 0
	for _, it := range p.res.Items[p.top:min(p.top+complRows, len(p.res.Items))] {
		lw = max(lw, width(it.Label))
		dw = max(dw, width(it.Detail))
	}
	lw, dw = min(lw, 48), min(dw, 32)
	w := 2 + lw + 2 + dw + 2 // glyph·label  detail
	if dw == 0 {
		w -= 2
	}
	w = min(w+2, c.W)
	h := n + 2
	x = max(x-width(p.res.Prefix)-2, 0) // the label's text under the word
	if x+w > c.W {
		x = max(c.W-w, 0)
	}
	top := y + 1
	if top+h > c.H {
		top = max(y-h, 0)
	}
	p.rect = Rect{x, top, w, h}

	bg := st.raised
	s := c.Sub(p.rect)
	s.Fill(bg)
	title := ""
	if len(p.res.Items) > complRows {
		title = itoa(p.cur+1) + "/" + itoa(len(p.res.Items))
		if p.res.Incomplete {
			title += "+" // cut at sqlcomplete.MaxItems: type on to narrow it
		}
	}
	s.Box(onBg(st.borderFocus, bg), title, onBg(st.muted, bg))
	for row := 0; row < n; row++ {
		i := p.top + row
		it := p.res.Items[i]
		rs := bg
		if i == p.cur {
			rs = st.buttonHover
		}
		line := s.Sub(Rect{1, row + 1, w - 2, 1})
		line.Fill(rs)
		gs := onBg(st.accent, rs)
		if i == p.cur {
			gs = rs
		}
		line.Put(0, 0, complGlyph[it.Kind], gs)
		line.Put(2, 0, truncate(it.Label, lw), rs)
		if dw > 0 && it.Detail != "" {
			ds := rs
			if i != p.cur {
				ds = onBg(st.muted, rs)
			}
			line.PutRight(line.W()-1, 0, truncate(it.Detail, dw), ds)
		}
	}

	// the documentation box, to the right of the list (or its left)
	doc := p.res.Items[p.cur].Doc
	if doc == "" {
		return
	}
	lines := strings.Split(doc, "\n")
	if len(lines) > 14 {
		lines = append(lines[:13], "…")
	}
	dwid := 0
	for _, l := range lines {
		dwid = max(dwid, width(l))
	}
	dwid = min(dwid+2, 64)
	dx := p.rect.X + p.rect.W
	if dx+dwid > c.W {
		dx = p.rect.X - dwid
	}
	if dx < 0 || dwid < 12 {
		return // no room: the list alone
	}
	dr := Rect{dx, p.rect.Y, dwid, len(lines) + 2}
	if dr.Y+dr.H > c.H {
		dr.Y = max(c.H-dr.H, 0)
	}
	ds := c.Sub(dr)
	ds.Fill(bg)
	ds.Box(onBg(st.border, bg), "", bg)
	for i, l := range lines {
		ds.Put(1, i+1, truncate(l, dwid-2), bg)
	}
}
