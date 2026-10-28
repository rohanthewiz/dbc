package tui

import (
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/snippet"
	"github.com/rohanthewiz/dbc/userdata"
)

// The snippets dialog (⌥S): pick a snippet to insert, and keep the list —
// new, edit, delete. Typing goes straight to the filter, as in the history
// picker: the dialog is opened to insert, and a snippet is found by its
// name, so the actions are on Ctrl chords rather than letters.
//
//	╭ Snippets · wraps the selection ───────────────────────── ✕ ╮
//	│ ⌕ ct▮                                            + New    │
//	│ ⇲ cte      wrap the selection in a CTE                     │  ⇲: uses $SELECTION$
//	│   cnt      count rows per value                            │
//	│ ─────────────────────────────────────────────────────────  │
//	│ WITH $VAR1:q$ AS (                                         │  the body of the
//	│     $SELECTION$                                            │  row under the cursor
//	│ Enter insert · ^N new · ^E edit · ^D delete · Esc close    │
//	╰────────────────────────────────────────────────────────────╯
type snippetsModal struct {
	modalBase
	all    []userdata.Snippet
	filter *editor
	lst    *list
	// wraps is true when the editor had a selection as the dialog opened:
	// a pick then replaces it, and $SELECTION$ snippets are the ones that
	// make use of it, so the title says so
	wraps bool

	filterRect, newBtn Rect
	hoverNew           bool
}

// openSnippets opens the dialog, the cursor on the snippet called sel when
// there is one (after a save).
func (m *Model) openSnippets() { m.openSnippetsAt("") }

func (m *Model) openSnippetsAt(sel string) {
	sd := &snippetsModal{filter: newEditor(true), lst: newList()}
	sd.filter.placeholder = "type to filter by name or description"
	s, _, _ := m.editor.Selection()
	sd.wraps = s != ""
	if err := sd.load(m); err != nil {
		m.logf(logErr, "snippets: %s", serr.StringFromErr(err))
		return
	}
	sd.build(sel)
	m.openModal(sd)
}

func (sd *snippetsModal) load(m *Model) error {
	all, err := m.snips.List()
	sd.all = all
	return err
}

// build lists the snippets the filter matches — a name match before a
// description match, an exact or prefix one first — with the cursor on sel
// when given.
func (sd *snippetsModal) build(sel string) {
	q := strings.ToLower(strings.TrimSpace(sd.filter.Text()))
	rank := func(sn userdata.Snippet) int {
		name := strings.ToLower(sn.Name)
		switch {
		case q == "":
			return 0
		case name == q:
			return 0
		case strings.HasPrefix(name, q):
			return 1
		case strings.Contains(name, q):
			return 2
		case strings.Contains(strings.ToLower(sn.Desc), q):
			return 3
		}
		return -1
	}
	var items []listItem
	nameW := 0
	for r := 0; r <= 3; r++ {
		for _, sn := range sd.all {
			if rank(sn) != r {
				continue
			}
			it := listItem{label: sn.Name, desc: sn.Desc, data: sn}
			if tpl, err := snippet.Parse(sn.Body); err != nil {
				it.mark, it.desc = "⚠", "does not parse: "+err.Error()
			} else if tpl.UsesSelection() {
				it.mark = "⇲"
			}
			items = append(items, it)
			nameW = max(nameW, width(sn.Name))
		}
	}
	sd.lst.descCol = nameW + 4
	sd.lst.set(items)
	if sel != "" {
		for i, it := range items {
			if it.label == sel {
				sd.lst.cur = i
			}
		}
	}
	sd.lst.ensureVisible()
}

// current is the snippet under the cursor.
func (sd *snippetsModal) current() (userdata.Snippet, bool) {
	it, ok := sd.lst.current()
	if !ok {
		return userdata.Snippet{}, false
	}
	sn, ok := it.data.(userdata.Snippet)
	return sn, ok
}

func (sd *snippetsModal) title() string {
	if sd.wraps {
		return "Snippets · a pick wraps the selection"
	}
	return "Snippets"
}

// size leaves room under the list for the body of the snippet under the
// cursor: what Enter is about to insert.
func (sd *snippetsModal) size(w, h int) (int, int) {
	return max(min(w-4, 100), min(64, w-4)), max(16, min(len(sd.lst.items)+14, h*3/4))
}

// previewRows is how many lines of the body the dialog shows.
const previewRows = 8

func (sd *snippetsModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	label := " + New "
	sd.newBtn = chip(s, s.W()-1-width(label), 0, label, pick(sd.hoverNew, m.st.buttonHover, m.st.button))
	field := s.Sub(Rect{3, 0, max(sd.newBtn.X-s.Rect().X-5, 10), 1})
	sd.filterRect = field.Rect()
	cx, cy, ok := sd.filter.Draw(field, m.st, m.st.raised, [2]int{}, true)

	prevH := min(previewRows, max(s.H()-8, 2))
	listH := s.H() - 4 - prevH
	sd.lst.draw(s.Sub(Rect{0, 2, s.W(), listH}), m.st, bg, true, "no snippet matches — ^N makes one")
	y := 2 + listH
	s.Put(1, y, strings.Repeat("─", s.W()-2), onBg(m.st.muted, bg))
	if sn, ok := sd.current(); ok {
		lines := strings.Split(sn.Body, "\n")
		for i := 0; i < prevH && i < len(lines); i++ {
			line := strings.ReplaceAll(lines[i], "\t", "    ")
			if i == prevH-1 && len(lines) > prevH {
				line = "…"
			}
			s.Put(2, y+1+i, truncate(line, s.W()-4), bg)
		}
	}
	s.Put(1, s.H()-1, truncate("Enter insert · ^N new · ^E edit · ^D delete · ⇲ uses the selection · Esc close", s.W()-2),
		onBg(m.st.muted, bg))
	if ok {
		return &caret{cx, cy}
	}
	return nil
}

func (sd *snippetsModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return nil, true
	case "enter":
		return sd.insert(m)
	case "ctrl+n":
		m.openSnippetForm("", sd.filterName())
		return nil, false
	case "ctrl+e", "f2":
		if sn, ok := sd.current(); ok {
			m.openSnippetForm(sn.Name, "")
		}
		return nil, false
	case "ctrl+d":
		sd.confirmDelete(m, sd.lst.view.X+2, sd.lst.view.Y+sd.lst.cur-sd.lst.top+1)
		return nil, false
	case "up", "down", "pgup", "pgdown":
		sd.lst.key(k)
		return nil, false
	}
	before := sd.filter.Text()
	sd.filter.HandleKey(k)
	if sd.filter.Text() != before {
		sd.lst.cur = 0 // the best match is the first row again
		sd.build("")
	}
	return nil, false
}

// filterName is the filter as a new snippet's name, when it would be one:
// ^N after a search that found nothing makes the snippet searched for.
func (sd *snippetsModal) filterName() string {
	q := strings.TrimSpace(sd.filter.Text())
	if snippet.ValidName(q) != nil {
		return ""
	}
	return q
}

// insert closes the dialog and inserts the snippet under the cursor.
func (sd *snippetsModal) insert(m *Model) (tea.Cmd, bool) {
	sn, ok := sd.current()
	if !ok {
		return nil, false
	}
	if m.modal == sd {
		m.modal = nil
	}
	m.insertPicked(sn)
	return nil, true
}

// confirmDelete asks, in a menu at (x, y), before deleting the snippet
// under the cursor: a delete cannot be undone, and ^D is one key away from
// ^E.
func (sd *snippetsModal) confirmDelete(m *Model, x, y int) {
	sn, ok := sd.current()
	if !ok {
		return
	}
	m.openMenu(x, y, []menuItem{
		heading("delete snippet " + sn.Name + "?"),
		{label: "✕ Delete " + sn.Name, act: func(m *Model) tea.Cmd {
			if err := m.snips.Delete(sn.Name); err != nil {
				m.logf(logErr, "snippets: %s", serr.StringFromErr(err))
				return nil
			}
			m.logf(logOk, "deleted snippet %s", sn.Name)
			if err := sd.load(m); err != nil {
				m.logf(logErr, "snippets: %s", serr.StringFromErr(err))
			}
			sd.build("")
			return nil
		}},
		{label: "Keep it", act: func(*Model) tea.Cmd { return nil }},
	})
}

func (sd *snippetsModal) paste(m *Model, s string) {
	sd.filter.Insert(s)
	sd.lst.cur = 0
	sd.build("")
}

func (sd *snippetsModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case sd.newBtn.Contains(x, y):
		m.openSnippetForm("", sd.filterName())
		return nil, false
	case sd.filterRect.Contains(x, y):
		sd.filter.Click(x, y, clicks, shift)
		return nil, false
	}
	i := sd.lst.indexAt(x, y)
	if i < 0 {
		return nil, false
	}
	sd.lst.cur = i
	if clicks >= 2 {
		return sd.insert(m)
	}
	return nil, false
}

// rightClick offers the row's actions, for a mouse.
func (sd *snippetsModal) rightClick(m *Model, x, y int) tea.Cmd {
	i := sd.lst.indexAt(x, y)
	if i < 0 {
		return nil
	}
	sd.lst.cur = i
	sn, ok := sd.current()
	if !ok {
		return nil
	}
	m.openMenu(x, y, []menuItem{
		heading(sn.Name),
		{label: "⇥ Insert", key: "Enter", act: func(m *Model) tea.Cmd { sd.insert(m); return nil }},
		{label: "✎ Edit…", key: "^E", act: func(m *Model) tea.Cmd { m.openSnippetForm(sn.Name, ""); return nil }},
		{label: "✕ Delete…", key: "^D", act: func(m *Model) tea.Cmd { sd.confirmDelete(m, x, y); return nil }},
		heading(""),
		{label: "+ New snippet…", key: "^N", act: func(m *Model) tea.Cmd { m.openSnippetForm("", ""); return nil }},
	})
	return nil
}

func (sd *snippetsModal) hover(m *Model, x, y int) {
	sd.lst.hover = sd.lst.indexAt(x, y)
	sd.hoverNew = sd.newBtn.Contains(x, y)
}

func (sd *snippetsModal) wheel(m *Model, x, y, dy int) { sd.lst.scroll(dy) }

// ---------------------------------------------------------------------------
// The form: one snippet's name, description and body
// ---------------------------------------------------------------------------

// snippetForm edits one snippet, or makes a new one. Saving or backing out
// returns to the dialog, the cursor on the snippet saved.
//
//	╭ Edit snippet cnt ─────────────────────────────────────── ✕ ╮
//	│ Name         [cnt                                        ] │
//	│ Description  [count rows per value                       ] │
//	│ Body                                                       │
//	│ ┊SELECT $VAR2:column$, count(*) AS n                       │
//	│ ┊FROM $VAR1:table$                                         │
//	│ $VAR1$ a stop (Tab visits them by number) · $VAR1:default$ │
//	│  ✓ Save    Cancel                 ^S · Tab next field · Esc│
//	╰────────────────────────────────────────────────────────────╯
type snippetForm struct {
	modalBase
	old    string // the name being edited; "" for a new snippet
	fields [3]*editor
	focus  int // which field has the keyboard: name, desc, body
	errMsg string

	rects          [3]Rect
	saveBtn, noBtn Rect
	hoverBtn       int
}

const (
	sfName = iota
	sfDesc
	sfBody
)

// openSnippetForm opens the form over the snippet called name, or empty
// for a new one (named suggest, when given).
func (m *Model) openSnippetForm(name, suggest string) {
	f := &snippetForm{old: name, hoverBtn: -1}
	f.fields = [3]*editor{newEditor(true), newEditor(true), newEditor(false)}
	f.fields[sfBody].placeholder = "SELECT $VAR2:*$ FROM $VAR1:table$"
	if name != "" {
		sn, ok, err := m.snips.Get(name)
		if err != nil || !ok {
			m.logf(logErr, "snippets: %s", orDefault(serr.StringFromErr(err), "no snippet "+name+" — deleted elsewhere?"))
			return
		}
		f.fields[sfName].SetText(sn.Name)
		f.fields[sfDesc].SetText(sn.Desc)
		f.fields[sfBody].SetText(sn.Body)
		f.focus = sfBody // an edit is usually to the body
	} else {
		f.fields[sfName].SetText(suggest)
		if suggest != "" {
			f.focus = sfDesc
		}
	}
	m.openModal(f)
}

func (f *snippetForm) title() string {
	if f.old == "" {
		return "New snippet"
	}
	return "Edit snippet " + f.old
}

func (f *snippetForm) size(w, h int) (int, int) {
	return max(min(w-4, 100), min(64, w-4)), max(16, min(h-4, 26))
}

func (f *snippetForm) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	var cr *caret
	put := func(i int, r Rect, fieldBg Style) {
		sub := s.Sub(r)
		f.rects[i] = sub.Rect()
		cx, cy, ok := f.fields[i].Draw(sub, m.st, fieldBg, [2]int{}, true)
		if ok && f.focus == i {
			cr = &caret{cx, cy}
		}
	}
	labelSt := func(i int) Style {
		if f.focus == i {
			return onBg(m.st.accent, bg).Bold()
		}
		return onBg(m.st.muted, bg)
	}
	const lw = 13
	s.Put(1, 0, "Name", labelSt(sfName))
	put(sfName, Rect{lw, 0, s.W() - lw - 1, 1}, m.st.raised)
	s.Put(1, 1, "Description", labelSt(sfDesc))
	put(sfDesc, Rect{lw, 1, s.W() - lw - 1, 1}, m.st.raised)
	s.Put(1, 3, "Body", labelSt(sfBody))
	bodyH := s.H() - 8
	put(sfBody, Rect{1, 4, s.W() - 2, bodyH}, m.st.raised)

	y := 4 + bodyH
	s.Put(1, y, truncate("$VAR1$ a stop — Tab visits them by number, a number used twice mirrors", s.W()-2), onBg(m.st.muted, bg))
	s.Put(1, y+1, truncate("$VAR1:default$ · $SELECTION$ the selected text · $END$ the caret at the end · $$ a $", s.W()-2),
		onBg(m.st.muted, bg))
	if f.errMsg != "" {
		s.Put(1, y+2, truncate(f.errMsg, s.W()-2), onBg(m.st.err, bg))
	}
	by := s.H() - 1
	f.saveBtn = chip(s, 1, by, " ✓ Save ", pick(f.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	f.noBtn = chip(s, f.saveBtn.X-s.Rect().X+f.saveBtn.W+2, by, " Cancel ", pick(f.hoverBtn == 1, m.st.buttonHover, m.st.button))
	if hint := "^S save · Tab next field · Esc"; f.noBtn.X-s.Rect().X+f.noBtn.W+2+width(hint) < s.W() {
		s.PutRight(s.W()-1, by, hint, onBg(m.st.muted, bg))
	}
	return cr
}

func (f *snippetForm) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return f.back(m, f.old)
	case "ctrl+s":
		return f.save(m)
	case "tab":
		f.focus = (f.focus + 1) % len(f.fields)
		return nil, false
	case "shift+tab":
		f.focus = (f.focus + len(f.fields) - 1) % len(f.fields)
		return nil, false
	case "enter":
		// a one-line field's Enter moves on, the way a form's would; the
		// body takes it as a new line
		if f.focus != sfBody {
			f.focus++
			return nil, false
		}
	}
	f.fields[f.focus].HandleKey(k)
	f.errMsg = "" // the error was about the text as it was
	return nil, false
}

// save writes the snippet and goes back to the dialog on it; a refusal
// (bad name, a name taken, a template that does not parse) stays in the
// form with the reason.
func (f *snippetForm) save(m *Model) (tea.Cmd, bool) {
	sn := userdata.Snippet{
		Name: strings.TrimSpace(f.fields[sfName].Text()),
		Desc: f.fields[sfDesc].Text(),
		Body: f.fields[sfBody].Text(),
	}
	if err := m.snips.Save(f.old, sn); err != nil {
		switch {
		case errors.Is(err, userdata.ErrSnippetExists):
			f.errMsg = "a snippet called " + sn.Name + " already exists"
			f.focus = sfName
		case errors.Is(err, userdata.ErrNoSnippet):
			f.errMsg = "snippet " + f.old + " is gone (deleted elsewhere) — Esc, then ^N to make it again"
		default:
			f.errMsg = serr.StringFromErr(err)
		}
		return nil, false
	}
	verb := "saved"
	if f.old == "" {
		verb = "made"
	}
	m.logf(logOk, "%s snippet %s — type %s then Tab in the editor, or pick it here", verb, sn.Name, sn.Name)
	return f.back(m, sn.Name)
}

// back returns to the dialog, the cursor on sel.
func (f *snippetForm) back(m *Model, sel string) (tea.Cmd, bool) {
	if m.modal == f {
		m.modal = nil
	}
	m.openSnippetsAt(sel)
	return nil, false // the dialog has replaced the form
}

func (f *snippetForm) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y), f.noBtn.Contains(x, y):
		return f.back(m, f.old)
	case f.saveBtn.Contains(x, y):
		return f.save(m)
	}
	for i, r := range f.rects {
		if r.Contains(x, y) {
			f.focus = i
			f.fields[i].Click(x, y, clicks, shift)
		}
	}
	return nil, false
}

func (f *snippetForm) drag(m *Model, x, y int) { f.fields[f.focus].Drag(x, y) }

func (f *snippetForm) wheel(m *Model, x, y, dy int) {
	if f.rects[sfBody].Contains(x, y) {
		f.fields[sfBody].Scroll(dy, 0)
	}
}

func (f *snippetForm) hover(m *Model, x, y int) {
	f.hoverBtn = -1
	if f.saveBtn.Contains(x, y) {
		f.hoverBtn = 0
	} else if f.noBtn.Contains(x, y) {
		f.hoverBtn = 1
	}
}

func (f *snippetForm) paste(m *Model, s string) {
	f.fields[f.focus].Insert(s)
	f.errMsg = ""
}
