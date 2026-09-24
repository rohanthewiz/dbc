package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/userdata"
)

// modal is a floating dialog that owns the keyboard until it closes: export,
// history, scripts, the cell inspector, the cats agent picker.
//
// Each is drawn inside a frame the Model provides (title, border, a ✕ that
// closes it), and gets its clicks, drags and wheel in canvas coordinates.
// Methods returning closed=true ask the Model to drop the modal.
type modal interface {
	title() string
	size(w, h int) (int, int) // desired outer size for a w×h screen
	draw(m *Model, s Surface) *caret
	key(m *Model, k tea.KeyPressMsg) (cmd tea.Cmd, closed bool)
	click(m *Model, x, y, clicks int, shift bool) (cmd tea.Cmd, closed bool)
	rightClick(m *Model, x, y int) tea.Cmd
	drag(m *Model, x, y int)
	hover(m *Model, x, y int)
	wheel(m *Model, x, y, dy int)
	paste(m *Model, s string)
}

// modalBase supplies do-nothing defaults so each modal implements only what
// it uses.
type modalBase struct{}

func (modalBase) rightClick(*Model, int, int) tea.Cmd { return nil }
func (modalBase) drag(*Model, int, int)               {}
func (modalBase) hover(*Model, int, int)              {}
func (modalBase) wheel(*Model, int, int, int)         {}
func (modalBase) paste(*Model, string)                {}

// modalRect is where the open modal sits: centered, clamped to the screen.
func (m *Model) modalRect() Rect {
	if m.modal == nil {
		return Rect{}
	}
	w, h := m.modal.size(m.w, m.h)
	w, h = min(w, m.w-2), min(h, m.h-2)
	return Rect{(m.w - w) / 2, (m.h - h) / 2, w, h}
}

// modalClose is the ✕ in the frame's top-right corner.
func (m *Model) modalClose() Rect {
	r := m.modalRect()
	return Rect{r.X + r.W - 4, r.Y, 3, 1}
}

// drawModal draws the frame and the modal's content.
func (m *Model) drawModal(c *Canvas) *caret {
	r := m.modalRect()
	// a one-cell shadow separates the dialog from what is behind it
	c.Sub(Rect{r.X + 1, r.Y + 1, r.W, r.H}).Restyle(func(s Style) Style { return s.Dim() })
	s := c.Sub(r)
	bg := m.st.panel
	s.Fill(bg)
	s.Box(onBg(m.st.borderFocus, bg), m.modal.title(), onBg(m.st.titleFocus, bg))
	s.Put(r.W-4, 0, " ✕ ", onBg(m.st.err, bg).Bold())
	return m.modal.draw(m, c.Sub(r.Inset(1)))
}

// openModal shows md.
func (m *Model) openModal(md modal) {
	m.menu = nil
	m.modal = md
}

// chip draws a clickable button label and returns its rect.
func chip(s Surface, x, y int, label string, st Style) Rect {
	end := s.Put(x, y, label, st)
	r := s.Rect()
	return Rect{r.X + x, r.Y + y, end - x, 1}
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// exportModal saves the result to a file or copies it, in a chosen format.
type exportModal struct {
	modalBase
	formats []string
	fmtIdx  int
	path    *editor
	onPath  bool // keyboard is in the path field (else on the format row)

	fmtRects         []Rect
	pathRect         Rect
	copyBtn, saveBtn Rect
	hoverBtn         int
}

func (m *Model) openExport() {
	if m.lastRes == nil {
		m.log(logWarn, "no result to export — run a query first")
		return
	}
	md := &exportModal{formats: export.Names(), path: newEditor(true), hoverBtn: -1}
	md.path.placeholder = "e.g. results.csv — leave empty to copy instead"
	m.openModal(md)
}

func (e *exportModal) title() string            { return "Export result" }
func (e *exportModal) size(w, h int) (int, int) { return 70, 11 }

func (e *exportModal) format() export.Format {
	f, _ := export.ParseFormat(e.formats[e.fmtIdx])
	return f
}

func (e *exportModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "Format", onBg(m.st.muted, bg))
	e.fmtRects = e.fmtRects[:0]
	x := 1
	for i, f := range e.formats {
		st := m.st.button
		if i == e.fmtIdx {
			st = m.st.buttonHover
			if e.onPath {
				st = m.st.sel
			}
		}
		r := chip(s, x, 1, " "+f+" ", st)
		e.fmtRects = append(e.fmtRects, r)
		x += r.W + 1
	}
	if e.format() == export.HTML {
		s.Put(1, 2, "copies as a formatted table (Teams, Outlook, Docs); saves as a styled page", onBg(m.st.muted, bg).Italic())
	}

	s.Put(1, 4, "File", onBg(m.st.muted, bg))
	field := s.Sub(Rect{1, 5, s.W() - 2, 1})
	e.pathRect = field.Rect()
	fb := m.st.raised
	cx, cy, ok := e.path.Draw(field, m.st, fb, [2]int{}, true)

	y := s.H() - 1
	e.copyBtn = chip(s, 1, y, " ⧉ Copy to clipboard ", pick(e.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	e.saveBtn = chip(s, e.copyBtn.X-s.Rect().X+e.copyBtn.W+2, y, " ⤓ Save to file ", pick(e.hoverBtn == 1, m.st.buttonHover, m.st.button))
	// the key hint only where it fits beside the buttons — never over them
	if hint := "Tab: format ↔ file · Enter: go"; e.saveBtn.X-s.Rect().X+e.saveBtn.W+2+width(hint) < s.W() {
		s.PutRight(s.W()-1, y, hint, onBg(m.st.muted, bg))
	}
	if ok && e.onPath {
		return &caret{cx, cy}
	}
	return nil
}

func pick[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

func (e *exportModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return nil, true
	case "tab", "shift+tab":
		e.onPath = !e.onPath
		return nil, false
	case "enter":
		return e.run(m, strings.TrimSpace(e.path.Text()) == "")
	}
	if !e.onPath {
		switch k.String() {
		case "left", "h":
			e.fmtIdx = (e.fmtIdx + len(e.formats) - 1) % len(e.formats)
		case "right", "l":
			e.fmtIdx = (e.fmtIdx + 1) % len(e.formats)
		default:
			// typing starts a path: the likely intent of a letter here
			if k.Text != "" {
				e.onPath = true
				e.path.HandleKey(k)
			}
		}
		return nil, false
	}
	e.path.HandleKey(k)
	return nil, false
}

func (e *exportModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	for i, r := range e.fmtRects {
		if r.Contains(x, y) {
			e.fmtIdx, e.onPath = i, false
			return nil, false
		}
	}
	switch {
	case e.pathRect.Contains(x, y):
		e.onPath = true
		e.path.Click(x, y, clicks, shift)
	case e.copyBtn.Contains(x, y):
		return e.run(m, true)
	case e.saveBtn.Contains(x, y):
		return e.run(m, false)
	}
	return nil, false
}

func (e *exportModal) hover(m *Model, x, y int) {
	e.hoverBtn = -1
	if e.copyBtn.Contains(x, y) {
		e.hoverBtn = 0
	} else if e.saveBtn.Contains(x, y) {
		e.hoverBtn = 1
	}
}

func (e *exportModal) paste(m *Model, s string) { e.onPath = true; e.path.Insert(s) }

// run performs the export: to the clipboard, or to the file named.
func (e *exportModal) run(m *Model, toClipboard bool) (tea.Cmd, bool) {
	f := e.format()
	if toClipboard {
		r, what := m.grid.Selected(true)
		return m.copyResult(r, what, copyFormatFor(f)), true
	}
	path := strings.TrimSpace(e.path.Text())
	if path == "" {
		m.log(logWarn, "type a file name to save to, or use ⧉ Copy")
		e.onPath = true
		return nil, false
	}
	if err := export.ToFile(m.lastRes, f, path); err != nil {
		m.logf(logErr, "export failed: %s", serr.StringFromErr(err))
		return nil, false
	}
	m.logf(logOk, "exported %d rows as %s to %s", len(m.lastRes.Rows), f, path)
	return nil, true
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

// historyModal lists past statements, newest first, with a live filter.
// Enter (or a double-click) inserts the pick into the editor at the caret —
// it never runs it: a recalled query is usually one you want to edit, and
// auto-running someone's remembered DELETE is not a thing to do.
type historyModal struct {
	modalBase
	all    []userdata.Entry
	shown  []userdata.Entry
	filter *editor
	lst    *list
}

func (m *Model) openHistory() {
	entries := m.hist.Recent()
	if len(entries) == 0 {
		m.log(logWarn, "no query history yet — it fills up as you run queries")
		return
	}
	md := &historyModal{all: entries, filter: newEditor(true), lst: newList()}
	md.filter.placeholder = "filter by SQL or connection…"
	md.refilter()
	m.openModal(md)
}

func (h *historyModal) refilter() {
	h.shown = userdata.MatchHistory(h.all, h.filter.Text())
	items := make([]listItem, len(h.shown))
	for i, e := range h.shown {
		items[i] = listItem{label: preview(e.SQL), sub: e.At.Local().Format("Jan 2 15:04") + " · " + e.Conn}
	}
	h.lst.cur = 0
	h.lst.set(items)
}

func (h *historyModal) title() string { return "History · Enter inserts · double-click inserts" }
func (h *historyModal) size(w, hh int) (int, int) {
	return max(60, w*3/4), max(12, hh*2/3)
}

func (h *historyModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	cx, cy, ok := h.filter.Draw(s.Sub(Rect{3, 0, s.W() - 4, 1}), m.st, m.st.raised, [2]int{}, true)
	h.lst.draw(s.Sub(Rect{0, 2, s.W(), s.H() - 2}), m.st, bg, true, "no match")
	if ok {
		return &caret{cx, cy}
	}
	return nil
}

func (h *historyModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return nil, true
	case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
		h.lst.key(k)
		return nil, false
	case "enter":
		return h.insert(m)
	}
	before := h.filter.Text()
	h.filter.HandleKey(k)
	if h.filter.Text() != before {
		h.refilter()
	}
	return nil, false
}

func (h *historyModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	if i := h.lst.indexAt(x, y); i >= 0 {
		h.lst.cur = i
		if clicks >= 2 {
			return h.insert(m)
		}
	}
	return nil, false
}

func (h *historyModal) hover(m *Model, x, y int)     { h.lst.hover = h.lst.indexAt(x, y) }
func (h *historyModal) wheel(m *Model, x, y, dy int) { h.lst.scroll(dy) }
func (h *historyModal) paste(m *Model, s string)     { h.filter.Insert(s); h.refilter() }

func (h *historyModal) insert(m *Model) (tea.Cmd, bool) {
	if h.lst.cur < 0 || h.lst.cur >= len(h.shown) {
		return nil, false
	}
	sql := h.shown[h.lst.cur].SQL
	m.editor.Insert(sql)
	m.focus = focusEditor
	m.drag.follow = true
	m.logf(logOk, "recalled — %s", preview(sql))
	return nil, true
}

// ---------------------------------------------------------------------------
// Scripts
// ---------------------------------------------------------------------------

// scriptsModal lists the Go scripts in scripts_dir; picking one runs it.
type scriptsModal struct {
	modalBase
	files []string
	lst   *list
}

func (m *Model) openScripts() {
	files, err := filepath.Glob(filepath.Join(m.cfg.ScriptsDir, "*.go"))
	if err != nil || len(files) == 0 {
		m.logf(logWarn, "no scripts found in %s — add .go files with func Run(s *sdb.S) error", m.cfg.ScriptsDir)
		return
	}
	sort.Strings(files)
	md := &scriptsModal{files: files, lst: newList()}
	items := make([]listItem, len(files))
	for i, f := range files {
		items[i] = listItem{label: filepath.Base(f), sub: "▶ run"}
	}
	md.lst.set(items)
	m.openModal(md)
}

func (sm *scriptsModal) title() string { return "Scripts · Enter or click runs" }
func (sm *scriptsModal) size(w, h int) (int, int) {
	return 56, min(len(sm.files)+4, 20)
}
func (sm *scriptsModal) draw(m *Model, s Surface) *caret {
	sm.lst.draw(s.Sub(Rect{0, 1, s.W(), s.H() - 1}), m.st, m.st.panel, true, "")
	return nil
}
func (sm *scriptsModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	if k.String() == "esc" {
		return nil, true
	}
	if _, picked := sm.lst.key(k); picked {
		return m.runScript(sm.files[sm.lst.cur]), true
	}
	return nil, false
}
func (sm *scriptsModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	if i := sm.lst.indexAt(x, y); i >= 0 {
		return m.runScript(sm.files[i]), true
	}
	return nil, false
}
func (sm *scriptsModal) hover(m *Model, x, y int)     { sm.lst.hover = sm.lst.indexAt(x, y) }
func (sm *scriptsModal) wheel(m *Model, x, y, dy int) { sm.lst.scroll(dy) }

// ---------------------------------------------------------------------------
// Cell inspector
// ---------------------------------------------------------------------------

// inspectModal shows one value in full — the grid truncates at 40 cells and
// flattens newlines, which is right for scanning and wrong for reading a
// JSON document or a long text. JSON is pretty-printed; everything else is
// wrapped as it is.
type inspectModal struct {
	modalBase
	col, row int
	value    string
	null     bool
	pretty   bool // value was JSON and is shown indented
	top      int
	lines    []string
	view     Rect
	copyBtn  Rect
	askBtn   Rect
	hoverBtn int
}

func (m *Model) openInspect() {
	g := m.grid
	val, _, ok := g.value(g.cur.row, g.cur.col)
	if !ok {
		m.log(logWarn, "no cell selected")
		return
	}
	md := &inspectModal{col: g.cur.col, row: g.cur.row, value: val, null: g.isNull(g.cur.row, g.cur.col), hoverBtn: -1}
	if t := strings.TrimSpace(val); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			if b, err := json.MarshalIndent(v, "", "  "); err == nil {
				md.value, md.pretty = string(b), true
			}
		}
	}
	m.openModal(md)
}

func (in *inspectModal) title() string {
	return "Inspect"
}

// size fits the dialog to the value: a short value gets a small dialog, and a
// long one grows up to two thirds of the screen and scrolls from there.
func (in *inspectModal) size(w, h int) (int, int) {
	widest := 0
	for _, l := range strings.Split(in.value, "\n") {
		widest = max(widest, width(l))
	}
	mw := max(50, min(widest+6, w*2/3))
	rows := 0
	for _, l := range strings.Split(in.value, "\n") {
		rows += len(wrap(l, mw-4))
	}
	return mw, max(9, min(rows+6, h*2/3))
}

func (in *inspectModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	col := m.lastRes.Columns[in.col]
	head := fmt.Sprintf("%s · row %d", col, in.row+1)
	if in.pretty {
		head += " · JSON, formatted"
	}
	s.Put(1, 0, head, onBg(m.st.accent, bg).Bold())
	body := s.Sub(Rect{1, 2, s.W() - 2, s.H() - 4})
	in.view = body.Rect()
	if in.null {
		body.Put(0, 0, "NULL", onBg(m.st.null, bg))
	} else {
		in.lines = in.lines[:0]
		for _, l := range strings.Split(in.value, "\n") {
			in.lines = append(in.lines, wrap(l, body.W())...)
		}
		in.top = max(0, min(in.top, len(in.lines)-body.H()))
		for y := 0; y < body.H() && in.top+y < len(in.lines); y++ {
			body.Put(0, y, in.lines[in.top+y], onBg(m.st.base, bg))
		}
		if len(in.lines) > body.H() {
			drawVBar(s.Sub(Rect{s.W() - 1, 2, 1, s.H() - 4}), m.st, in.top, body.H(), len(in.lines))
		}
	}
	y := s.H() - 1
	in.copyBtn = chip(s, 1, y, " ⧉ Copy value ", pick(in.hoverBtn == 0, m.st.buttonHover, m.st.buttonHot))
	in.askBtn = chip(s, in.copyBtn.X-s.Rect().X+in.copyBtn.W+2, y, " ✦ Ask about it ", pick(in.hoverBtn == 1, m.st.buttonHover, m.st.button))
	s.PutRight(s.W()-1, y, "y copies · Esc closes", onBg(m.st.muted, bg))
	return nil
}

func (in *inspectModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "enter", "q":
		return nil, true
	case "y":
		return in.copy(m), false
	case "up", "k":
		in.top = max(in.top-1, 0)
	case "down", "j":
		in.top++
	case "pgup":
		in.top = max(in.top-in.view.H, 0)
	case "pgdown":
		in.top += in.view.H
	}
	return nil, false
}

func (in *inspectModal) copy(m *Model) tea.Cmd {
	raw, _, _ := m.grid.value(in.row, in.col)
	return m.copyString(raw, m.lastRes.Columns[in.col]+" of row "+itoa(in.row+1))
}

func (in *inspectModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case in.copyBtn.Contains(x, y):
		return in.copy(m), false
	case in.askBtn.Contains(x, y):
		col := m.lastRes.Columns[in.col]
		return m.askAbout(fmt.Sprintf("Explain the %s value in row %d of this result.", col, in.row+1)), true
	}
	return nil, false
}

func (in *inspectModal) hover(m *Model, x, y int) {
	in.hoverBtn = -1
	if in.copyBtn.Contains(x, y) {
		in.hoverBtn = 0
	} else if in.askBtn.Contains(x, y) {
		in.hoverBtn = 1
	}
}
func (in *inspectModal) wheel(m *Model, x, y, dy int) { in.top = max(in.top+dy, 0) }
