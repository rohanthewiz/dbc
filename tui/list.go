package tui

import tea "charm.land/bubbletea/v2"

// list is a scrollable, clickable list: the connections and tables in the
// sidebar, and the rows of the history picker and the scripts browser.
type list struct {
	items []listItem
	cur   int
	top   int
	hover int // item under the mouse, -1 when none

	// descCol is the column, from the row's left edge, where every row's
	// desc starts, so descriptions line up under each other (the scripts
	// browser sets it to its widest name). 0 puts each desc two cells
	// after its own label.
	descCol int

	view Rect // item area as last drawn, for hit-testing
}

// listItem is one row. mark is a one-cell glyph drawn before the label (the
// ● on the active connection); sub is right-aligned and muted.
type listItem struct {
	label string
	sub   string
	mark  string
	desc  string // muted text after the label (at list.descCol), cut before sub
	muted bool   // drawn dim — e.g. a view among tables
	// head is a row that cannot be picked: a section heading or a note
	// between the rows (the scripts browser's "Examples", "Trash (2) ▸").
	// The cursor steps over it, and it never lights up under the mouse.
	head bool
	data any // what picking it means, for the caller
}

func newList() *list { return &list{hover: -1} }

// set replaces the items, keeping the cursor in range and off a heading.
func (l *list) set(items []listItem) {
	l.items = items
	l.cur = max(0, min(l.cur, len(items)-1))
	l.top = max(0, min(l.top, max(len(items)-1, 0)))
	l.settle(1)
}

// settle moves the cursor off a heading: onward in direction d to the
// next pickable row, else back the other way. A list of nothing but
// headings leaves it where it is, and current() then reports no pick.
func (l *list) settle(d int) {
	if d == 0 {
		d = 1
	}
	for _, dir := range []int{d, -d} {
		for i := l.cur; i >= 0 && i < len(l.items); i += dir {
			if !l.items[i].head {
				l.cur = i
				return
			}
		}
	}
}

// current returns the item under the cursor.
func (l *list) current() (listItem, bool) {
	if l.cur < 0 || l.cur >= len(l.items) || l.items[l.cur].head {
		return listItem{}, false
	}
	return l.items[l.cur], true
}

// move shifts the cursor by d, clamped.
func (l *list) move(d int) {
	if len(l.items) == 0 {
		return
	}
	l.cur = max(0, min(l.cur+d, len(l.items)-1))
	l.settle(d)
	if l.cur == l.firstPick() {
		// at the top, show a heading above the first row rather than
		// scrolling it out of view: it names the section the row is in
		l.top = 0
	}
	l.ensureVisible()
}

// firstPick is the index of the first pickable row, or -1.
func (l *list) firstPick() int {
	for i, it := range l.items {
		if !it.head {
			return i
		}
	}
	return -1
}

func (l *list) ensureVisible() {
	h := l.view.H
	if h <= 0 {
		return
	}
	if l.cur < l.top {
		l.top = l.cur
	} else if l.cur >= l.top+h {
		l.top = l.cur - h + 1
	}
}

// indexAt returns the item at canvas (x, y), or -1.
func (l *list) indexAt(x, y int) int {
	if !l.view.Contains(x, y) {
		return -1
	}
	i := l.top + (y - l.view.Y)
	if i >= len(l.items) {
		return -1
	}
	return i
}

// scroll moves the view by n rows without moving the cursor.
func (l *list) scroll(n int) {
	l.top = max(0, min(l.top+n, max(len(l.items)-l.view.H, 0)))
}

// key handles navigation. It reports enter separately so the caller decides
// what picking means.
func (l *list) key(k tea.KeyPressMsg) (used, picked bool) {
	switch k.String() {
	case "up", "k", "ctrl+p":
		l.move(-1)
	case "down", "j", "ctrl+n":
		l.move(1)
	case "pgup":
		l.move(-max(l.view.H-1, 1))
	case "pgdown":
		l.move(max(l.view.H-1, 1))
	case "home", "g":
		l.move(-len(l.items))
	case "end", "G":
		l.move(len(l.items))
	case "enter":
		return true, len(l.items) > 0
	default:
		return false, false
	}
	return true, false
}

// draw paints the items into s. focused lights the cursor row; an unfocused
// list still shows where its cursor is, one step quieter.
func (l *list) draw(s Surface, st styles, bg Style, focused bool, empty string) {
	s.Fill(bg)
	l.view = s.Rect()
	if len(l.items) == 0 {
		s.Put(1, 0, empty, onBg(st.muted, bg))
		return
	}
	l.top = max(0, min(l.top, max(len(l.items)-s.H(), 0)))
	for y := 0; y < s.H(); y++ {
		i := l.top + y
		if i >= len(l.items) {
			break
		}
		it := l.items[i]
		row := s.Sub(Rect{0, y, s.W(), 1})
		if it.head {
			// a heading: muted, flush left, never a cursor or hover row
			row.Fill(bg)
			row.Put(1, 0, truncate(it.label, row.W()-2), onBg(st.muted, bg).Bold())
			continue
		}
		rs := bg
		switch {
		case i == l.cur && focused:
			rs = st.sel
		case i == l.cur:
			rs = bg.WithBg(st.selRange.Bg)
		case i == l.hover:
			rs = bg.WithBg(st.hover.Bg)
		}
		row.Fill(rs)
		ls := rs
		if it.muted {
			ls = rs.WithFg(st.muted.Fg)
		}
		x := 1
		if it.mark != "" {
			x = row.Put(0, 0, it.mark, rs.WithFg(st.accent.Fg))
			x++
		}
		subW := 0
		if it.sub != "" {
			subW = width(it.sub) + 1
			row.PutRight(row.W()-1, 0, it.sub, rs.WithFg(st.muted.Fg))
		}
		end := row.Put(x, 0, truncate(it.label, row.W()-x-subW-1), ls)
		if it.desc != "" {
			dx := max(end+2, x+l.descCol)
			if room := row.W() - subW - 1 - dx; room > 3 {
				row.Put(dx, 0, truncate(it.desc, room), rs.WithFg(st.muted.Fg))
			}
		}
	}
	if len(l.items) > s.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H()}), st, l.top, s.H(), len(l.items))
	}
}
