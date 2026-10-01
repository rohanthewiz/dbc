package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
)

// The sidebar's navigator: on a server whose catalog is loaded a level at a
// time (db.Navigable — Postgres), the Tables pane opens with two picker rows
// above its list, the web sidebar's two levels:
//
//	┌ Tables · 40 ──────────┐
//	│ ⛁ analytics         ▾ │  d — the server's databases
//	│ ◫ sales · 40        ▾ │  s — this database's schemas, "all schemas" first
//	│ orders        ~1.2M   │
//	│ …                     │
//
// A click on a row, or its key in the Tables pane, opens a type-to-filter
// picker (pickModal): a server can hold hundreds of schemas, and a list that
// long wants a filter, not a scroll.
//
// Picking a database switches the connection, as the web's picker does: a
// Postgres connection is bound to one database, so another database is
// another connection, "<conn>/<database>" (config.DatabaseSep), derived
// from the configured one and never stored. The DSN's own database maps
// back to the configured connection itself, so it never gets a second pool.
//
// Picking a schema is workspace.PickSchema on the same connection. The pick
// is remembered per connection (schemaPicks) and saved to a small file
// (userdata.SavePick), so going back to a database — in this run or the
// next — reopens the schema the user was in, as the web does from its
// saved layout.

// navigable reports whether the active connection's sidebar is loaded a
// level at a time, and so has the picker rows.
func (m *Model) navigable() bool {
	cc, ok := m.cfg.ConnByName(m.ws.Active())
	return ok && db.Navigable(cc.Driver)
}

// baseOf is the configured connection a connection is, or is derived from:
// the Connections list's row to mark while the sidebar is on another of its
// server's databases.
func (m *Model) baseOf(name string) string {
	if cc, ok := m.cfg.ConnByName(name); ok && cc.Base != "" {
		return cc.Base
	}
	return name
}

// currentDatabase is the database the active connection is open on, as the
// server named it in the database list ("" when there is no list).
func (m *Model) currentDatabase() string {
	for _, d := range m.ws.Databases() {
		if d.Current {
			return d.Name
		}
	}
	return ""
}

// schemaTotal is the table count of every schema of the active database.
func (m *Model) schemaTotal() int {
	n := 0
	for _, s := range m.ws.Schemas() {
		n += s.Tables
	}
	return n
}

// navRows lists the picker rows the Tables pane draws above its list: the
// database row when the server listed its databases, the schema row when
// the database has more than one schema — with one, every table is in it,
// and the workspace lists them without a pick (see resolvePick).
func (m *Model) navRows() (dbRow, schemaRow bool) {
	if !m.navigable() {
		return false, false
	}
	return len(m.ws.Databases()) > 0, len(m.ws.Schemas()) > 1
}

// schemaLabel is what the schema row says: the schema listed and its table
// count, "all schemas" and theirs, or the pick still loading.
func (m *Model) schemaLabel() string {
	if m.schemaLoading != "" {
		return m.schemaLoading + " · loading…"
	}
	if s := m.ws.CatalogSchema(); s != "" {
		for _, si := range m.ws.Schemas() {
			if si.Name == s {
				return fmt.Sprintf("%s · %s", s, commas(si.Tables))
			}
		}
		return s
	}
	return "all schemas · " + commas(m.schemaTotal())
}

// drawTablesPane is the Tables pane's body: the navigator's rows, when the
// connection has them, then the list. The rows' rects go into the layout
// for clicks, as the toolbar's buttons do.
func (m *Model) drawTablesPane(s Surface) {
	m.lay.dbRow, m.lay.schemaRow = Rect{}, Rect{}
	bg := m.st.panel
	y := 0
	row := func(icon, label string) Rect {
		r := s.Sub(Rect{0, y, s.W(), 1})
		st := m.st.raised
		r.Fill(st)
		x := r.Put(1, 0, icon+" ", st.WithFg(m.st.accent.Fg))
		r.Put(x, 0, truncate(label, max(r.W()-x-3, 1)), st.Bold())
		r.PutRight(r.W()-1, 0, "▾", st.WithFg(m.st.muted.Fg))
		y++
		return r.Rect()
	}
	dbRow, schemaRow := m.navRows()
	if dbRow {
		m.lay.dbRow = row("⛁", orDefault(m.currentDatabase(), m.ws.Active()))
	}
	if schemaRow && s.H()-y > 2 {
		m.lay.schemaRow = row("◫", m.schemaLabel())
	}
	empty := "(none yet)"
	switch {
	case m.ws.Active() == "":
		empty = "not connected"
	case m.ws.Catalog() != nil:
		empty = "no tables"
	}
	m.tables.draw(s.Sub(Rect{0, y, s.W(), s.H() - y}), m.st, bg, m.focus == focusTables, empty)
}

// commas formats n with thousands separators, as the web's picker does.
func commas(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// ---------------------------------------------------------------------------
// The pickers
// ---------------------------------------------------------------------------

// openDatabasePicker lists the server's databases. Picking one switches to
// its connection: the configured one for the DSN's own database, a derived
// "<base>/<database>" for every other (see the web's sidebar, which maps
// them the same way).
func (m *Model) openDatabasePicker() {
	dbs := m.ws.Databases()
	if !m.navigable() || len(dbs) == 0 {
		m.log(logWarn, m.pickWhy("databases"))
		return
	}
	base := m.baseOf(m.ws.Active())
	baseDB := ""
	if cc, ok := m.cfg.ConnByName(base); ok {
		baseDB = db.DefaultDatabase(cc)
	}
	items := make([]listItem, 0, len(dbs))
	cur := 0
	for _, d := range dbs {
		conn := config.DerivedName(base, d.Name)
		if d.Name == baseDB {
			conn = base
		}
		it := listItem{label: d.Name, data: conn}
		if d.Current {
			it.mark, cur = "●", len(items)
		}
		if conn == base {
			it.sub = "configured"
		}
		items = append(items, it)
	}
	m.openModal(newPickModal(fmt.Sprintf("Databases on %s · Enter switches", base), "filter databases…", items, cur,
		func(m *Model, it listItem) tea.Cmd { return m.setActive(it.data.(string)) }))
}

// schemaChoice is a schema picker row's meaning: a schema, or every schema
// (all), or a row that cannot be picked and says why.
type schemaChoice struct {
	pick workspace.SchemaPick
	why  string
}

// openSchemaPicker lists the active database's schemas, with their table
// counts, after an "all schemas" row. That row is dim past
// db.AllSchemasLimit tables — the workspace would list the default schema
// instead — and picking it then says so rather than doing something else.
func (m *Model) openSchemaPicker() {
	schemas := m.ws.Schemas()
	if !m.navigable() || len(schemas) < 2 {
		m.log(logWarn, m.pickWhy("schemas"))
		return
	}
	listed := m.ws.CatalogSchema()
	total := m.schemaTotal()
	all := listItem{label: "all schemas", sub: commas(total), data: schemaChoice{pick: workspace.SchemaPick{All: true}}}
	if total > db.AllSchemasLimit {
		all.muted = true
		all.sub = commas(total) + " — too many"
		all.data = schemaChoice{why: fmt.Sprintf("%s tables are too many to list at once (the limit is %s) — pick a schema",
			commas(total), commas(db.AllSchemasLimit))}
	}
	if listed == "" {
		all.mark = "●"
	}
	items := []listItem{all}
	cur := 0
	for _, s := range schemas {
		it := listItem{label: s.Name, sub: commas(s.Tables), data: schemaChoice{pick: workspace.SchemaPick{Name: s.Name}}}
		if s.Default {
			it.sub = "default · " + it.sub
		}
		if s.Tables == 0 {
			it.muted = true
		}
		if s.Name == listed {
			it.mark, cur = "●", len(items)
		}
		items = append(items, it)
	}
	title := "Schemas · Enter lists its tables"
	if d := m.currentDatabase(); d != "" {
		title = "Schemas of " + d + " · Enter lists its tables"
	}
	m.openModal(newPickModal(title, "filter schemas…", items, cur, func(m *Model, it listItem) tea.Cmd {
		c := it.data.(schemaChoice)
		if c.why != "" {
			m.log(logWarn, c.why)
			return nil
		}
		return m.pickSchema(c.pick)
	}))
}

// pickWhy says why a picker has nothing to offer.
func (m *Model) pickWhy(what string) string {
	switch {
	case m.ws.Active() == "":
		return "not connected — pick a connection first"
	case !m.navigable():
		return m.ws.Active() + " lists its tables whole — there are no " + what + " to pick"
	case what == "schemas":
		return "this database has one schema — its tables are all listed"
	}
	return "the server's databases could not be listed"
}

// pickSchema lists another schema's tables, remembering the pick for the
// connection so a later switch back reopens it. The schema row says
// "loading…" until the tables land (schemaLoaded).
func (m *Model) pickSchema(pick workspace.SchemaPick) tea.Cmd {
	st, err := m.ws.PickSchema(pick)
	if err != nil {
		m.refused(err)
		return nil
	}
	m.schemaPicks[m.ws.Active()] = pick
	// Saved now rather than at quit: a pick is rare and the file is small,
	// and a dbc killed rather than quit (a closed terminal) keeps it.
	if err := userdata.SavePick(m.picksFile, m.ws.Active(),
		userdata.SchemaPick{Name: pick.Name, All: pick.All}); err != nil {
		m.logf(logWarn, "the schema pick is not saved for next time: %v", err)
	}
	m.schemaLoading = pick.Name
	if pick.All {
		m.schemaLoading = "all schemas"
	}
	m.notes(st.Notes)
	return job(st.Job)
}

// schemaLoaded draws a schema pick's tables, then counts their rows. A load
// that failed leaves the old list up (the workspace kept it, and its notes
// say why).
func (m *Model) schemaLoaded(ev *workspace.SchemaLoaded) tea.Cmd {
	if ev.Stale {
		return nil // a later pick or connect owns the sidebar
	}
	m.schemaLoading = ""
	m.notes(ev.Notes)
	if ev.Catalog != nil {
		m.refreshTables()
	}
	return job(ev.Counts)
}

// ---------------------------------------------------------------------------
// The picker modal
// ---------------------------------------------------------------------------

// pickModal is a type-to-filter list: the database and schema pickers. The
// filter matches anywhere in a row's name, case-insensitively, since a
// schema is remembered by a fragment ("bill" for billing_v2) as often as by
// its start. Enter or a click picks; the cursor opens on the row in use.
type pickModal struct {
	modalBase
	head   string
	all    []listItem
	shown  []listItem
	filter *editor
	lst    *list
	pick   func(*Model, listItem) tea.Cmd
}

func newPickModal(head, placeholder string, items []listItem, cur int, pick func(*Model, listItem) tea.Cmd) *pickModal {
	p := &pickModal{head: head, all: items, filter: newEditor(true), lst: newList(), pick: pick}
	p.filter.placeholder = placeholder
	p.refilter()
	p.lst.cur = cur
	return p
}

func (p *pickModal) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.filter.Text()))
	p.shown = p.shown[:0]
	for _, it := range p.all {
		if q == "" || strings.Contains(strings.ToLower(it.label), q) {
			p.shown = append(p.shown, it)
		}
	}
	p.lst.cur, p.lst.top = 0, 0
	p.lst.set(p.shown)
}

func (p *pickModal) title() string { return p.head }
func (p *pickModal) size(w, h int) (int, int) {
	return min(max(48, w/2), 72), min(len(p.all)+5, max(12, h*2/3))
}

func (p *pickModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	cx, cy, ok := p.filter.Draw(s.Sub(Rect{3, 0, s.W() - 4, 1}), m.st, m.st.raised, [2]int{}, true)
	p.lst.draw(s.Sub(Rect{0, 2, s.W(), s.H() - 2}), m.st, bg, true, "no match")
	p.lst.ensureVisible() // the opening cursor may sit below the first screen
	if ok {
		return &caret{cx, cy}
	}
	return nil
}

func (p *pickModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		return nil, true
	case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
		p.lst.key(k)
		return nil, false
	case "enter":
		return p.choose(m)
	}
	before := p.filter.Text()
	p.filter.HandleKey(k)
	if p.filter.Text() != before {
		p.refilter()
	}
	return nil, false
}

func (p *pickModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	if m.modalClose().Contains(x, y) {
		return nil, true
	}
	if i := p.lst.indexAt(x, y); i >= 0 {
		p.lst.cur = i
		return p.choose(m)
	}
	return nil, false
}

func (p *pickModal) hover(m *Model, x, y int)     { p.lst.hover = p.lst.indexAt(x, y) }
func (p *pickModal) wheel(m *Model, x, y, dy int) { p.lst.scroll(dy) }
func (p *pickModal) paste(m *Model, s string)     { p.filter.Insert(s); p.refilter() }

// choose picks the row under the cursor and closes the picker.
func (p *pickModal) choose(m *Model) (tea.Cmd, bool) {
	it, ok := p.lst.current()
	if !ok {
		return nil, false
	}
	return p.pick(m, it), true
}

// ---------------------------------------------------------------------------
// Disconnect
// ---------------------------------------------------------------------------

// connOn is the connection the sidebar is on or connecting to — what a
// Disconnect would leave — or "".
func (m *Model) connOn() string {
	if a := m.ws.Active(); a != "" {
		return a
	}
	if name, ok := m.ws.Connecting(); ok {
		return name
	}
	return ""
}

// disconnect leaves the connection without picking another (x in the
// Connections pane, or the connection menu's first row). A session that may
// hold a transaction, SET values or temp tables asks first, with a menu at
// (x, y) — the closing rolls them back, as the web asks before it does the
// same. Busy is left to the workspace to refuse: Session waits on a run in
// flight, and the refusal says how to stop it.
func (m *Model) disconnect(x, y int) tea.Cmd {
	if !m.ws.Busy() {
		if conn, stateful := m.ws.Session(); stateful && conn != "" {
			m.openMenu(x, y, []menuItem{
				heading("the session on " + conn + " may hold a transaction"),
				{label: "Stay connected", act: func(m *Model) tea.Cmd { return nil }},
				{label: "⏏ Disconnect — roll it back", act: func(m *Model) tea.Cmd { return m.disconnectNow() }},
			})
			return nil
		}
	}
	return m.disconnectNow()
}

// disconnectNow is workspace.Disconnect, then — off the UI goroutine, since
// it waits on the session lock — the session closed and the pool with it.
// The TUI's workspace is the only one on its Manager, so the pool has no
// other user to keep it open for (the web counts its tabs first). An
// in-memory SQLite pool stays open regardless: db.Manager.Disconnect keeps
// it, since its contents live there.
func (m *Model) disconnectNow() tea.Cmd {
	left, st, err := m.ws.Disconnect()
	if err != nil {
		m.refused(err)
		return nil
	}
	m.notes(st.Notes)
	m.setStatus("disconnected")
	m.schemaLoading = ""
	m.refreshConns()
	m.refreshTables()
	m.catsAfterTransition()
	mgr, release := m.mgr, st.Job
	return func() tea.Msg {
		var out tea.Msg
		if ev := release(); ev != nil {
			out = ev
		}
		mgr.Disconnect(left)
		return out
	}
}

// releaseThenClose is a switch's Release job, followed — when the switch
// left a derived connection ("<conn>/<database>") — by closing that pool,
// so browsing a server's databases does not leave one open per database
// visited (see workspace.Derived). The close is skipped when the sidebar is
// back on that connection, or dialing it, by the time the release is done.
func (m *Model) releaseThenClose(ev *workspace.Connected) tea.Cmd {
	if ev.Left == "" || !m.ws.Derived(ev.Left) {
		return job(ev.Release)
	}
	ws, mgr, left, release := m.ws, m.mgr, ev.Left, ev.Release
	return func() tea.Msg {
		var out tea.Msg
		if release != nil {
			if e := release(); e != nil {
				out = e
			}
		}
		connecting, dialing := ws.Connecting()
		if ws.Active() != left && (!dialing || connecting != left) {
			mgr.Disconnect(left)
		}
		return out
	}
}
