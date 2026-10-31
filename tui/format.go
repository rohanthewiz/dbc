package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// formatQuery lays out the selection, or the whole buffer when nothing is
// selected, with sqlsplit.Format (⌥⇧F, the editor menu's "Format SQL"). It
// also unwraps a query pasted from a log line — the label in front, the
// string-literal quotes and the \n escapes come off — so the paste-then-
// format round trip is one key.
//
// It is one Replace, so one undo step. The selection's surrounding blanks
// are kept (FormatSpan), and with the whole buffer formatted the caret goes to the start of the statement it was
// in — rather than to the top or the end, which would lose the reader's
// place. When the statement count changes (a quoted log string unwrapping
// into several statements) there is no "same statement", and it goes to the
// top.
func (m *Model) formatQuery() tea.Cmd {
	e := m.editor
	text := e.Text()
	sel, from, to := e.Selection()
	what := "the selection"
	if sel == "" {
		sel, from, to, what = text, 0, len(text), "the buffer"
	}
	if strings.TrimSpace(sel) == "" {
		m.log(logWarn, "format: nothing to format — type or paste a query first")
		return nil
	}
	// read as the connection's engine reads strings, as the run would
	d := m.ws.Dialect()
	out := d.FormatSpan(sel)
	if out == sel {
		m.logf(logInfo, "format: %s is already formatted", what)
		return nil
	}

	cursor := -1 // after the formatted text: Replace's default
	if from == 0 && to == len(text) {
		before, after := d.Split(text), d.Split(out)
		cursor = 0
		if i := sqlsplit.IndexAt(before, e.Caret()); i >= 0 && len(before) == len(after) {
			cursor = after[i].CodeStart
		}
	}
	e.Replace(from, to, out, cursor)
	m.focus = focusEditor
	m.drag.follow = true
	m.logf(logOk, "formatted %s · ^Z undoes it", what)
	return nil
}

// formatLabel names the editor menu's row by what it will format.
func formatLabel(sel string) string {
	if sel != "" {
		return "Format selection"
	}
	return "Format SQL"
}
