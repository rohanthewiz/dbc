package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The keys dialog (F1, or ? outside a text field): every key the TUI
// answers, grouped by where it works — dbc web's "Keys" dialog (app.js
// KEYS), in the terminal's spellings.
//
// The list is data, not generated from the key switch: the switch is spread
// over app.go, the panes and the modals, and what a user needs is the
// phrase ("run the statement under the caret"), which the code does not
// hold. So a key added elsewhere is added here by hand, as it is to the
// README's Keys table and to dbc web's KEYS.
//
// The dialog is a read-only scroller: the rows are laid out once per draw
// into lines (a key column, then the description wrapped beside it), and
// the wheel, the arrows and PgUp/PgDn move a window over them.
//
//	┌ Keys · F1 or ? ───────────────────────── ✕ ┐
//	│ Editor                                       │
//	│   ^R              run the statement under …  │
//	│   ^⇧R · ⌥R        run every statement        │
//	│ …                                            │
//	│                     ↑↓ scroll · Esc closes   │
//	└──────────────────────────────────────────────┘

// keyGroup is one section of the dialog: a heading and its rows, each a key
// (or gesture) and what it does.
type keyGroup struct {
	name string
	rows [][2]string
}

// keyGroups is the dialog's content. Keep it in step with the README's
// "Keys" and "Mouse" tables.
var keyGroups = []keyGroup{
	{"Editor", [][2]string{
		{"^R", "run the statement under the caret (or the selection)"},
		{"^⇧R · ⌥R", "run every statement in the buffer"},
		{"^X · ⌥X / ^⇧X", "explain · explain analyze (runs it to time each step)"},
		{"^K", "stop the run, a connect still dialing, or the assistant's answer"},
		{"^Space", "suggestions from the schema (also as you type, and after . and ::)"},
		{"Tab · Enter · Esc", "in the suggestions: pick · pick · close"},
		{"F12", "on a table alias, a CTE name or a column the query names: go to where it is declared"},
		{"⇧F12", "highlight its uses and list them; Enter goes to one"},
		{"F2", "rename it everywhere in the statement"},
		{"^Z · ⌥Z", "undo · redo"},
		{"⌥N · ⌥C", "new console · next console of the database"},
		{"right-click", "run, copy, consoles (new, rename, delete), ask the assistant"},
	}},
	{"Anywhere", [][2]string{
		{"^P", "history — insert a past statement (never runs it)"},
		{"^E", "export the result (file or clipboard)"},
		{"^O", "scripts and plugin files — run, edit ($EDITOR), make from a template, examples, trash"},
		{"^J", "pipelines & jobs — run, edit the JSON ($EDITOR), new, examples, trash"},
		{"⌥J", "the runs: every pipeline and job run, live and recorded; Enter opens one"},
		{"^T", "list the tables and views"},
		{"^A", "the assistant — and back"},
		{"^B", "hide the sidebar — and back (or click ‹ / ›)"},
		{"^L", "the connections list"},
		{"^G", "(inside Cats) hand the statement to an agent pane"},
		{"Tab · ⇧Tab", "next · previous pane"},
		{"F1 · ?", "this list (? outside the editor and the assistant)"},
		{"^C · ^Q", "stop what runs, else quit · quit"},
	}},
	{"Query tabs", [][2]string{
		{"⌥T · ⌥W", "new tab (its own session and console) · close the tab"},
		{"⌥1 … ⌥9", "go to tab N"},
		{"click · double-click a tab", "switch to it · rename it"},
		{"right-click a tab", "rename, close, new tab, its group, its consoles"},
		{"● · • · ◆", "running · finished in the background · may hold a transaction"},
		{"⌥G", "tab groups: add the tab to one, remove it, each group's menu"},
		{"click · right-click a group's chip", "fold or unfold it · its menu (new tab in it, rename, ungroup…)"},
		{"[name] · right-click +", "a connection group with no tab: click opens one · a new tab in any group"},
	}},
	{"Scripts browser (^O)", [][2]string{
		{"Enter · double-click", "run it · edit it (an example: copy it; a trashed one: restore it)"},
		{"e", "edit in $VISUAL / $EDITOR (else vi); dbc checks it when the editor exits"},
		{"n · d", "new from a template · duplicate (an example: copy into your scripts)"},
		{"r · F2 · Del · x", "rename · rename · move to the trash · the same"},
		{"y · t", "copy its path · show or hide the Trash"},
		{"/ · Esc", "filter by name and description · back to the list, then close"},
		{"right-click", "everything above for the row"},
		{"Plugins section", "your pipeline plugins (plugins_dir): Enter edits one; on return it is checked and loaded (n: new from an example)"},
	}},
	{"Pipelines & jobs (^J)", [][2]string{
		{"Enter · p", "run it (asks for a parameter with no default) · run it, asking for every parameter"},
		{"e · double-click", "edit the JSON in $VISUAL / $EDITOR; dbc checks it when the editor exits (an example: copy it first)"},
		{"n · d", "new pipeline or job · duplicate (an example: copy it into yours)"},
		{"r · F2 · Del · x", "rename · rename · move to the trash · the same"},
		{"h · ⌥J", "its runs · every run"},
		{"y · t · /", "copy its path · show or hide the Trash · filter"},
		{"right-click", "everything above for the row"},
	}},
	{"Runs (⌥J) and a run", [][2]string{
		{"Enter · ⌫", "open the run: its pipelines, fragments and nodes, live · back to the list"},
		{"↑↓ · Enter · ← →", "move · fold or unfold · fold (or go up) · unfold"},
		{"^K", "stop the run (a run another process runs: says where to stop it)"},
		{"y · Y", "copy the run's id · the run as text (as dbc run show prints it)"},
		{"a · /", "(the list) every run, not only one job's · filter"},
		{"● nightly 3m12s", "(status bar) a run going here — click it to open the run"},
	}},
	{"Results grid", [][2]string{
		{"arrows · ⇧arrows", "move · extend the range"},
		{"Enter · double-click", "inspect the value"},
		{"y · Y · c", "copy the value or range · the row · the copy menu"},
		{"< · > · =", "narrow · widen · fit the column"},
		{"- · +", "hide the column (or the range's) · show every hidden one"},
		{"t", "transpose — each row a column; copies and exports follow"},
		{"click a header", "sort: ascending, descending, off"},
		{"p", "the plan, when there is one"},
		{"[ · ]", "a script's previous · next shown result (or click a number on the title)"},
		{"{ · }", "the connection's previous · next result tab (or click one on the bottom border)"},
		{"P · x", "pin the result tab — the next run opens a new one — or unpin it · close it"},
		{"r", "rerun the result tab's query into that tab, pinned or not (a write asks first) — or click its ↻"},
		{"S", "share the result tab with the assistant (✦: it goes with every question; needs ai_rows) — or stop"},
		{"right-click a result tab", "rerun, pin or unpin, share, close, close the unpinned ones"},
		{"z", "give the results the whole column — and back"},
	}},
	{"Plan", [][2]string{
		{"1 · 2 · 3 · v", "tree · flame graph · insights · next view"},
		{"m", "size by the next metric (time, cost, rows)"},
		{"↑↓ · Enter", "walk the steps · fold"},
		{"e · a", "explain again (compares) · explain analyze"},
		{"y · Y", "copy as text · the engine's own output"},
		{"b · c", "open in the browser · the plan menu (PDF, PNG, JPEG, Mermaid)"},
		{"p", "back to the results"},
		{"{ · } · P · s · x", "result tabs, as in the grid"},
	}},
	{"Log", [][2]string{
		{"y · x", "copy the connection's log · clear it (or ⧉ copy / ✕ clear on its title)"},
		{"↑↓ · PgUp/PgDn · g/G", "scroll · to the top / the newest line"},
	}},
	{"Sidebar", [][2]string{
		{"Enter · click", "connect · preview a table's rows"},
		{"x", "(connections) disconnect"},
		{"r", "(connections) refresh: read its databases, schemas and tables again"},
		{"a · + · e", "(connections) add a connection · add one, or start Postgres in Docker · edit the one under the cursor"},
		{"right-click", "(connections) connect, refresh, edit, remove, add; dump a Postgres database; stop a Postgres in Docker container"},
		{"● · ○", "(connections) this tab's connection · another tab's"},
		{"d · s", "(tables) pick a database · a schema"},
		{"c · e", "(tables) show its columns · diagram it (ERD)"},
		{"#", "(tables) show or hide row counts (an exact count(*) per table)"},
		{"f", "(tables) list the schema's functions and procedures in place of its tables, and back"},
		{"/", "(tables) find: type to narrow the list · Enter previews · Esc clears"},
		{"Enter · 2×click", "(routines) show its DDL — y copies it, i inserts it in the editor"},
	}},
	{"Assistant", [][2]string{
		{"Enter · ⇧Enter", "send · new line"},
		{"^K", "stop the answer"},
		{"Esc", "back to the editor"},
	}},
}

// helpModal is the keys dialog.
type helpModal struct {
	modalBase
	top   int
	lines []helpLine
	view  Rect
}

// helpLine is one laid-out line: a group heading, or a row's key (on its
// first line only) and a slice of its wrapped description.
type helpLine struct {
	head, key, what string
}

// keyColW is the key column's width — wide enough for the longest key in
// keyGroups, so the descriptions line up.
const keyColW = 28

func (m *Model) openHelp() { m.openModal(&helpModal{}) }

func (h *helpModal) title() string { return "Keys · F1 or ?" }

func (h *helpModal) size(w, hh int) (int, int) {
	return min(max(64, w*2/3), 100), max(12, hh*4/5)
}

// layout wraps the groups for a body of width w. Done per draw: it is a few
// dozen rows, and a resize changes the wrap.
func (h *helpModal) layout(w int) {
	h.lines = h.lines[:0]
	descW := max(10, w-keyColW-2)
	for gi, g := range keyGroups {
		if gi > 0 {
			h.lines = append(h.lines, helpLine{})
		}
		h.lines = append(h.lines, helpLine{head: g.name})
		for _, r := range g.rows {
			for i, part := range wrap(r[1], descW) {
				l := helpLine{what: part}
				if i == 0 {
					l.key = r[0]
				}
				h.lines = append(h.lines, l)
			}
		}
	}
}

func (h *helpModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	body := s.Sub(Rect{1, 0, s.W() - 3, s.H() - 1})
	h.view = body.Rect()
	h.layout(body.W())
	h.top = max(0, min(h.top, len(h.lines)-body.H()))
	for y := 0; y < body.H() && h.top+y < len(h.lines); y++ {
		l := h.lines[h.top+y]
		if l.head != "" {
			body.Put(0, y, l.head, onBg(m.st.accent, bg).Bold())
			continue
		}
		body.Put(2, y, truncate(l.key, keyColW-1), onBg(m.st.titleFocus, bg))
		body.Put(keyColW+2, y, l.what, onBg(m.st.base, bg))
	}
	if len(h.lines) > body.H() {
		drawVBar(s.Sub(Rect{s.W() - 1, 0, 1, s.H() - 1}), m.st, h.top, body.H(), len(h.lines))
	}
	s.PutRight(s.W()-1, s.H()-1, "↑↓ PgUp PgDn scroll · Esc closes", onBg(m.st.muted, bg))
	return nil
}

func (h *helpModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "q", "f1", "?", "enter":
		return nil, true
	case "up", "k":
		h.top = max(h.top-1, 0)
	case "down", "j":
		h.top++ // clamped by the next draw
	case "pgup":
		h.top = max(h.top-h.view.H, 0)
	case "pgdown", "space":
		h.top += h.view.H
	case "home", "g":
		h.top = 0
	case "end", "G":
		h.top = len(h.lines)
	}
	return nil, false
}

func (h *helpModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	return nil, m.modalClose().Contains(x, y)
}

func (h *helpModal) wheel(m *Model, x, y, dy int) { h.top = max(h.top+dy, 0) }
