package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The results title's switcher between the results one script run showed.
// A script that calls s.Show more than once (a loop over parameters, a
// before/after pair) used to leave only the last on the grid; the workspace
// keeps the newest MaxScriptResults of them (Workspace.ScriptResults), and
// dbc web has long offered "Result 1 · 2 · 3" on its results bar. This is
// the same switcher, drawn right-aligned on the results pane's top border so
// it never competes with the title (or the Results │ ◈ Plan tabs) for the
// left-hand cells:
//
//	╭ Results · 4 rows · 2ms · right-click to copy ───── Result 1 · 2 · 3 ╮
//	                                                            └─┘ └─┘ └─┘  setChips: a click shows that one
//
// Where the numbered form does not fit (twenty results in a narrow pane), a
// compact one steps instead:
//
//	╭ Results · 4 rows · 2ms ─ Result ‹ 7/20 › ╮
//	                                 └─┘    └─┘  ‹ the previous, › the next
//
// The pick is the workspace's (ShowScriptResult makes it the last result),
// so the grid, copies, exports and the assistant all follow the one shown —
// exactly as in dbc web, which talks to the same Workspace. Keys: [ and ]
// in the results grid step back and forth; neither is otherwise bound
// there, and both read as "previous / next" in vim-flavoured tools.

// setChip is one clickable part of the switcher, as drawn: a click on r
// shows the run's result i (0-based among the kept ones).
type setChip struct {
	i int
	r Rect
}

// setPart is one run of text in the switcher before it is placed: a chip
// (to >= 0, the result it shows), the current result (on), or plain text.
type setPart struct {
	text string
	to   int  // the result a click shows; -1 for text that is not a chip
	on   bool // the result on the grid now
}

// setsReserve is how many of the results pane's title cells the switcher
// leaves to the title on its left: enough for "Results · 12 rows · …" to
// still say what is on the grid.
const setsReserve = 24

// resultSetParts lays out the switcher for a results pane w cells wide that
// must also leave room for left (the title, or the Results │ Plan tabs'
// minimum): the numbered form if it fits, else the compact one, else
// nothing. Nothing too when the last script run showed fewer than two
// results, or the grid has moved on to a result of its own (at < 0).
func (m *Model) resultSetParts(w, left int) []setPart {
	n, at, cut := m.ws.ScriptResults()
	if n < 2 || at < 0 {
		return nil
	}
	room := w - 2 - left // the two corners, then what the left side keeps

	// the numbered form, as dbc web draws it: each number is a chip padded
	// by a space either side (a bigger target than a digit), with "·"
	// between, so it reads "Result 1 · 2 · 3". Numbers are the script's own
	// s.Show count: past the cap the dropped first shows are skipped (cut).
	full := []setPart{{text: " Result", to: -1}}
	for i := range n {
		if i > 0 {
			full = append(full, setPart{text: "·", to: -1})
		}
		full = append(full, setPart{text: " " + itoa(cut+i+1) + " ", to: i, on: i == at})
	}
	if partsWidth(full) <= room {
		return full
	}

	// the compact form: ‹ and › step one either way; at an end the arrow is
	// drawn but is no chip, so a click there does nothing rather than wrap
	prev, next := at-1, at+1
	if next >= n {
		next = -1
	}
	compact := []setPart{
		{text: " Result", to: -1},
		{text: " ‹ ", to: prev},
		{text: itoa(cut+at+1) + "/" + itoa(cut+n), to: -1, on: true},
		{text: " › ", to: next},
	}
	if partsWidth(compact) <= room {
		return compact
	}
	return nil
}

// partsWidth is the switcher's width in cells.
func partsWidth(parts []setPart) int {
	w := 0
	for _, p := range parts {
		w += width(p.text)
	}
	return w
}

// drawResultSets draws the switcher on the results pane r's top border,
// ending one cell left of the ╮ corner, and records its chips for clicks.
// parts comes from resultSetParts, computed before the title was drawn so
// the title could be cut to leave it room.
func (m *Model) drawResultSets(c *Canvas, r Rect, parts []setPart) {
	m.lay.setChips = m.lay.setChips[:0]
	if len(parts) == 0 {
		return
	}
	bg := m.st.base
	off, on := onBg(m.st.muted, bg), onBg(m.st.title, bg).Bold().Underline()
	if m.focus == focusGrid {
		on = onBg(m.st.titleFocus, bg).Underline()
	}
	s := c.Sub(Rect{r.X, r.Y, r.W, 1})
	x := r.W - 1 - partsWidth(parts)
	for _, p := range parts {
		st := off
		if p.on {
			st = on
		}
		// the dead end of the compact form (‹ on the first, › on the last)
		// is drawn dim, so it does not read as a control
		if p.to < 0 && !p.on && (p.text == " ‹ " || p.text == " › ") {
			st = st.Dim()
		}
		start := x
		x = s.Put(x, 0, p.text, st)
		if p.to >= 0 && !p.on {
			m.lay.setChips = append(m.lay.setChips, setChip{i: p.to, r: Rect{r.X + start, r.Y, x - start, 1}})
		}
	}
}

// resultSetAt reports which of the switcher's chips is at (x, y).
func (m *Model) resultSetAt(x, y int) (int, bool) {
	for _, ch := range m.lay.setChips {
		if ch.r.Contains(x, y) {
			return ch.i, true
		}
	}
	return 0, false
}

// showScriptResult puts the last script run's result i on the grid: the
// switcher's click, and [ / ] through stepScriptResult. The workspace
// refuses while a run is in flight (a script still showing results would
// move the grid under the pick); that refusal is said in the log.
func (m *Model) showScriptResult(i int) tea.Cmd {
	if err := m.ws.ShowScriptResult(i); err != nil {
		m.refused(err)
		return nil
	}
	m.showResult(m.ws.LastResult())
	m.focus = focusGrid
	return nil
}

// stepScriptResult is [ (d = -1) and ] (d = +1) in the results grid. It
// stops at either end rather than wrapping, as the switcher's arrows do —
// the numbers on the border already say where the grid is.
func (m *Model) stepScriptResult(d int) tea.Cmd {
	n, at, _ := m.ws.ScriptResults()
	if n < 2 || at < 0 {
		m.log(logWarn, "no script results to step through — [ and ] switch between the results a script showed (s.Show)")
		return nil
	}
	i := at + d
	if i < 0 || i >= n {
		return nil
	}
	return m.showScriptResult(i)
}
