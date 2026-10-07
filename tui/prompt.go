package tui

import (
	tea "charm.land/bubbletea/v2"
)

// promptModal asks for one line of text — a new name, mostly (renaming a
// console). It is the terminal's stand-in for the browser's small
// input-and-buttons dialog:
//
//	╭ Rename console console-2 ───────────── ✕ ╮
//	│ letters, digits, '.', '-' and '_'          │
//	│ [console-2▮                              ] │
//	│ name taken                                 │  ← accept's error, if any
//	│  ✓ Rename    Cancel        Enter · Esc     │
//	╰────────────────────────────────────────────╯
//
// accept does the work. Returning an error keeps the dialog open with the
// error shown under the field, so a refused name (invalid, taken, a file
// that would not move) can be corrected in place rather than retyped from a
// fresh dialog. The initial text starts selected: typing replaces it, an
// arrow key keeps it to edit.
type promptModal struct {
	modalBase
	head   string // the frame's title
	hint   string // the muted line above the field
	okText string // the accept button's words ("Rename")
	field  *editor
	errMsg string
	accept func(m *Model, text string) error
	// acceptCmd, when set, is used instead of accept: for an answer whose
	// work goes on in a command (the scripts browser's new script, which
	// then suspends the TUI for $EDITOR).
	acceptCmd func(m *Model, text string) (tea.Cmd, error)
	// cancel, when set, runs on Esc, Cancel or ✕: the scripts browser's
	// prompts reopen the browser, so backing out of a rename lands where
	// it started rather than on the bare screen.
	cancel func(m *Model)

	fieldRect    Rect
	okBtn, noBtn Rect
	hoverBtn     int
}

// openPrompt shows a promptModal over everything.
func (m *Model) openPrompt(title, hint, okText, initial string, accept func(m *Model, text string) error) {
	p := &promptModal{head: title, hint: hint, okText: okText, field: newEditor(true), accept: accept, hoverBtn: -1}
	p.field.SetText(initial)
	p.field.SelectAll()
	m.openModal(p)
}

// openPromptCmd is openPrompt for an accept that returns a command.
func (m *Model) openPromptCmd(title, hint, okText, initial string, accept func(m *Model, text string) (tea.Cmd, error)) *promptModal {
	p := &promptModal{head: title, hint: hint, okText: okText, field: newEditor(true), acceptCmd: accept, hoverBtn: -1}
	p.field.SetText(initial)
	p.field.SelectAll()
	m.openModal(p)
	return p
}

func (p *promptModal) title() string            { return p.head }
func (p *promptModal) size(w, h int) (int, int) { return max(50, min(70, width(p.head)+12)), 8 }

func (p *promptModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, p.hint, onBg(m.st.muted, bg))
	field := s.Sub(Rect{1, 1, s.W() - 2, 1})
	p.fieldRect = field.Rect()
	cx, cy, ok := p.field.Draw(field, m.st, m.st.raised, [2]int{}, true)
	if p.errMsg != "" {
		s.Put(1, 2, p.errMsg, onBg(m.st.err, bg))
	}
	y := s.H() - 1
	p.okBtn = chip(s, 1, y, " ✓ "+p.okText+" ", pick(p.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	p.noBtn = chip(s, p.okBtn.X-s.Rect().X+p.okBtn.W+2, y, " Cancel ", pick(p.hoverBtn == 1, m.st.buttonHover, m.st.button))
	if hint := "Enter · Esc"; p.noBtn.X-s.Rect().X+p.noBtn.W+2+width(hint) < s.W() {
		s.PutRight(s.W()-1, y, hint, onBg(m.st.muted, bg))
	}
	if ok {
		return &caret{cx, cy}
	}
	return nil
}

func (p *promptModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return p.dismiss(m)
	case "enter":
		return p.submit(m)
	}
	p.field.HandleKey(k)
	p.errMsg = "" // the error was about the text as it was
	return nil, false
}

// dismiss closes the dialog without accepting, running cancel if set.
func (p *promptModal) dismiss(m *Model) (tea.Cmd, bool) {
	if p.cancel != nil {
		p.cancel(m)
	}
	return nil, true
}

// submit hands the text to accept; an error keeps the dialog open.
func (p *promptModal) submit(m *Model) (tea.Cmd, bool) {
	var cmd tea.Cmd
	var err error
	if p.acceptCmd != nil {
		cmd, err = p.acceptCmd(m, p.field.Text())
	} else {
		err = p.accept(m, p.field.Text())
	}
	if err != nil {
		p.errMsg = err.Error()
		return nil, false
	}
	return cmd, true
}

func (p *promptModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y), p.noBtn.Contains(x, y):
		return p.dismiss(m)
	case p.okBtn.Contains(x, y):
		return p.submit(m)
	case p.fieldRect.Contains(x, y):
		p.field.Click(x, y, clicks, shift)
	}
	return nil, false
}

func (p *promptModal) hover(m *Model, x, y int) {
	p.hoverBtn = -1
	if p.okBtn.Contains(x, y) {
		p.hoverBtn = 0
	} else if p.noBtn.Contains(x, y) {
		p.hoverBtn = 1
	}
}

func (p *promptModal) paste(m *Model, s string) { p.field.Insert(s); p.errMsg = "" }
