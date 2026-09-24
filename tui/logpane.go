package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// logPane is the scrolling message log under the grid.
//
// Lines are stored whole and WRAPPED AT DRAW TIME to the pane's width, so a
// resize (or dragging the border) rewraps everything instead of leaving
// lines chopped at the width they were written at. An error message is the
// line most likely to be long and the one that most needs to be read in
// full, which is why this wraps where the grid truncates.
type logPane struct {
	lines  []logLine
	top    int  // first visual row shown, when not following
	follow bool // pinned to the newest line
	view   Rect
	total  int // visual rows at the last draw
}

// logLine is one message.
type logLine struct {
	at   time.Time
	kind logKind
	text string
}

// logKind picks a line's color.
type logKind int

const (
	logInfo logKind = iota
	logOk
	logWarn
	logErr
	logMuted
	logAccent
)

// logMaxLines bounds the log so a chatty script cannot grow it forever.
const logMaxLines = 2000

func newLogPane() *logPane { return &logPane{follow: true} }

func (l *logPane) add(kind logKind, text string) {
	// one message per line: a multi-line error keeps its shape
	for i, part := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		k := kind
		if i > 0 && kind == logInfo {
			k = logMuted
		}
		l.lines = append(l.lines, logLine{at: time.Now(), kind: k, text: part})
	}
	if over := len(l.lines) - logMaxLines; over > 0 {
		l.lines = l.lines[over:]
	}
}

// Text returns the whole log as plain text, for "copy log".
func (l *logPane) Text() string {
	var b strings.Builder
	for _, ln := range l.lines {
		b.WriteString(ln.at.Format("15:04:05"))
		b.WriteByte(' ')
		b.WriteString(ln.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// scroll moves the view by n visual rows; scrolling back to the bottom
// re-pins it to the newest line.
func (l *logPane) scroll(n int) {
	maxTop := max(l.total-l.view.H, 0)
	if l.follow {
		l.top = maxTop
	}
	l.top = max(0, min(l.top+n, maxTop))
	l.follow = l.top >= maxTop
}

func (l *logPane) key(k tea.KeyPressMsg) {
	switch k.String() {
	case "up", "k":
		l.scroll(-1)
	case "down", "j":
		l.scroll(1)
	case "pgup":
		l.scroll(-max(l.view.H-1, 1))
	case "pgdown":
		l.scroll(max(l.view.H-1, 1))
	case "home", "g":
		l.scroll(-l.total)
	case "end", "G":
		l.scroll(l.total)
	}
}

// visual is one wrapped row of a line.
type visual struct {
	kind  logKind
	stamp string // only on a line's first row
	text  string
}

// draw paints the log into s.
func (l *logPane) draw(s Surface, st styles, bg Style) {
	s.Fill(bg)
	l.view = s.Rect()
	const stampW = 9 // "15:04:05 "
	textW := max(s.W()-stampW-1, 10)

	var rows []visual
	for _, ln := range l.lines {
		for i, part := range wrap(ln.text, textW) {
			v := visual{kind: ln.kind, text: part}
			if i == 0 {
				v.stamp = ln.at.Format("15:04:05")
			}
			rows = append(rows, v)
		}
	}
	l.total = len(rows)
	maxTop := max(len(rows)-s.H(), 0)
	if l.follow {
		l.top = maxTop
	}
	l.top = min(l.top, maxTop)

	for y := 0; y < s.H() && l.top+y < len(rows); y++ {
		v := rows[l.top+y]
		if v.stamp != "" {
			s.Put(0, y, v.stamp, onBg(st.muted, bg))
		}
		s.Put(stampW, y, v.text, onBg(logStyle(st, v.kind), bg))
	}
	if len(rows) > s.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H()}), st, l.top, s.H(), len(rows))
	}
}

func logStyle(st styles, k logKind) Style {
	switch k {
	case logOk:
		return st.ok
	case logWarn:
		return st.warn
	case logErr:
		return st.err
	case logMuted:
		return st.muted
	case logAccent:
		return st.accent
	}
	return st.base
}

// wrap breaks text into rows of at most w cells, at spaces where it can and
// mid-word where a single word is longer than the row.
func wrap(text string, w int) []string {
	if w <= 0 {
		return []string{text}
	}
	if width(text) <= w {
		return []string{text}
	}
	var out []string
	var line strings.Builder
	lw := 0
	flush := func() {
		out = append(out, line.String())
		line.Reset()
		lw = 0
	}
	for _, word := range strings.SplitAfter(text, " ") {
		ww := width(word)
		if lw+ww > w && lw > 0 {
			flush()
		}
		for ww > w { // a word longer than a whole row: hard-break it
			head, rest := cutWidth(word, w-lw)
			line.WriteString(head)
			flush()
			word, ww = rest, width(rest)
		}
		line.WriteString(word)
		lw += ww
	}
	if lw > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// cutWidth splits s after at most w cells.
func cutWidth(s string, w int) (head, rest string) {
	used := 0
	for i, r := range s {
		rw := runeWidth(r)
		if used+rw > w {
			return s[:i], s[i:]
		}
		used += rw
	}
	return s, ""
}
