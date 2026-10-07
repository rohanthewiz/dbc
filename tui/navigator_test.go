package tui

import (
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/userdata"
)

// x in the Connections pane leaves the connection without picking another:
// nothing active, an empty sidebar saying so, "not connected" in the status
// bar and on the toolbar's chip, and runs refused. A click on the row
// connects again.
func TestDisconnectFromTheConnectionsPane(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusConns
	key(t, m, "x")

	if a := m.ws.Active(); a != "" {
		t.Fatalf("active = %q after x", a)
	}
	f := frame(m)
	findText(t, f, "○ not connected ▾")  // the toolbar chip
	findText(t, f, " ○ not connected │") // the status bar
	findText(t, f, "disconnected from demo-sqlite")
	if len(m.tables.items) != 0 {
		t.Errorf("tables still listed: %d", len(m.tables.items))
	}
	findText(t, f, "not connected") // the Tables pane's empty text

	key(t, m, "ctrl+r")
	if !strings.Contains(logText(m), "no active connection") {
		t.Errorf("a run was not refused:\n%s", logText(m))
	}

	// the menu offers Disconnect only while connected
	x, y := findText(t, frame(m), "demo-sqlite")
	rightClick(t, m, x, y)
	if m.menu == nil || strings.Contains(m.menu.items[0].label, "Disconnect") {
		t.Fatalf("menu while disconnected: %+v", m.menu)
	}
	m.menu = nil

	click(t, m, x, y)
	if m.ws.Active() != config.DemoSQLite || len(m.tables.items) == 0 {
		t.Fatalf("reconnect: active %q, %d tables", m.ws.Active(), len(m.tables.items))
	}
	rightClick(t, m, x, y)
	if m.menu == nil || m.menu.items[0].label != "⏏ Disconnect demo-sqlite" {
		t.Fatalf("menu while connected: %+v", m.menu)
	}
}

// r in the Connections pane re-reads the sidebar's connection: a table
// another client created is listed, the tables cursor stays on the table it
// was on though the new one shifted every row below it, and the log says
// what the list holds now. The connections menu offers the same as its
// second row, under Disconnect.
func TestRefreshFromTheConnectionsPane(t *testing.T) {
	m := newTestModel(t)
	if len(m.tables.items) == 0 {
		t.Fatalf("tables: %d", len(m.tables.items))
	}
	last := len(m.tables.items) - 1
	m.tables.cur = last
	was := m.tables.items[last].data.(string)
	// "aaa" sorts first, so every listed table moves down a row
	if _, err := m.mgr.Run(config.DemoSQLite, "CREATE TABLE aaa_refreshed (id INTEGER)"); err != nil {
		t.Fatal(err)
	}

	m.focus = focusConns
	key(t, m, "r")
	if len(m.tables.items) != last+2 || m.tables.items[0].data.(string) != "aaa_refreshed" {
		t.Fatalf("after r: %d tables, first %+v", len(m.tables.items), m.tables.items[0])
	}
	if it, _ := m.tables.current(); it.data.(string) != was {
		t.Errorf("cursor on %q, want it kept on %q", it.data, was)
	}
	logs := logText(m)
	for _, want := range []string{"refreshing demo-sqlite…", fmt.Sprintf("refreshed demo-sqlite: %d tables", last+2)} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}

	// the Connections row, not the toolbar's chip above it
	x, y := findText(t, frame(m), "demo-sqlite    sqlite")
	rightClick(t, m, x, y)
	if m.menu == nil || len(m.menu.items) < 2 || m.menu.items[1].label != "↻ Refresh demo-sqlite" || m.menu.items[1].why != "" {
		t.Fatalf("menu while connected: %+v", m.menu)
	}
}

// A DDL run relists the sidebar on its own, as r would: the new table is
// listed and the cursor stays on its table. The status bar keeps the run's
// summary — the relist is not what the user asked for — and the log says
// what the list holds now.
func TestDDLRunRelistsTheSidebar(t *testing.T) {
	m := newTestModel(t)
	last := len(m.tables.items) - 1
	m.tables.cur = last
	was := m.tables.items[last].data.(string)

	m.editor.SetText("CREATE TABLE aaa_relisted (id INTEGER)")
	key(t, m, "ctrl+r")
	if len(m.tables.items) != last+2 || m.tables.items[0].data.(string) != "aaa_relisted" {
		t.Fatalf("after the CREATE: %d tables, first %+v", len(m.tables.items), m.tables.items[0])
	}
	if it, _ := m.tables.current(); it.data.(string) != was {
		t.Errorf("cursor on %q, want it kept on %q", it.data, was)
	}
	if strings.Contains(m.status, "refresh") || strings.Contains(m.status, "relist") || m.status == "" {
		t.Errorf("status = %q, want the run's summary", m.status)
	}
	if want := fmt.Sprintf("relisted demo-sqlite after the DDL: %d tables", last+2); !strings.Contains(logText(m), want) {
		t.Errorf("log lacks %q:\n%s", want, logText(m))
	}
	if _, connecting := m.ws.Connecting(); connecting {
		t.Error("the relist never landed: the workspace is still mid-connect")
	}
}

// A session that may hold a transaction asks first, with a menu, and
// "Stay connected" keeps it. Disconnecting from there rolls the work back.
func TestDisconnectAsksWhenTheSessionHoldsState(t *testing.T) {
	m := newTestModel(t)
	m.editor.SetText("BEGIN")
	key(t, m, "ctrl+r")
	m.editor.SetText("DELETE FROM cats")
	key(t, m, "ctrl+r")

	m.focus = focusConns
	key(t, m, "x")
	if m.menu == nil || m.menu.items[1].label != "Stay connected" {
		t.Fatalf("no confirm menu: %+v", m.menu)
	}
	key(t, m, "enter") // the cursor opens on Stay connected
	if m.ws.Active() != config.DemoSQLite {
		t.Fatalf("Stay connected disconnected")
	}

	key(t, m, "x")
	key(t, m, "down")
	key(t, m, "enter")
	if m.ws.Active() != "" {
		t.Fatalf("still on %q", m.ws.Active())
	}
	if !strings.Contains(logText(m), "any open transaction was rolled back") {
		t.Errorf("no rollback note:\n%s", logText(m))
	}
	// the in-memory demo's pool is kept (its contents live there), so the
	// rollback is visible through it
	res, err := m.mgr.Run(config.DemoSQLite, "SELECT count(*) FROM cats")
	if err != nil || res.Rows[0][0] == "0" {
		t.Errorf("the DELETE survived the disconnect: %v %v", res, err)
	}
}

// Off Postgres there is nothing to navigate: no picker rows, and d and s
// say why rather than opening an empty picker.
func TestNoNavigatorOffPostgres(t *testing.T) {
	m := newTestModel(t)
	if dbRow, schemaRow := m.navRows(); dbRow || schemaRow {
		t.Fatal("picker rows on SQLite")
	}
	m.focus = focusTables
	key(t, m, "s")
	if m.modal != nil || !strings.Contains(logText(m), "lists its tables whole") {
		t.Errorf("s on SQLite: modal %v\n%s", m.modal, logText(m))
	}
}

// The picker filters anywhere in a name, case-insensitively, and opens on
// the row in use.
func TestPickModalFilters(t *testing.T) {
	var got string
	p := newPickModal("t", "", []listItem{{label: "public"}, {label: "Billing_v2"}, {label: "audit"}}, 2,
		nil)
	if it, _ := p.lst.current(); it.label != "audit" {
		t.Errorf("opens on %q", it.label)
	}
	p.filter.SetText("BILL")
	p.refilter()
	if len(p.shown) != 1 || p.shown[0].label != "Billing_v2" {
		t.Fatalf("shown = %+v", p.shown)
	}
	p.pick = func(_ *Model, it listItem) tea.Cmd { got = it.label; return nil }
	if _, closed := p.choose(nil); !closed || got != "Billing_v2" {
		t.Errorf("choose: %v %q", closed, got)
	}
	if commas(1234567) != "1,234,567" || commas(12) != "12" {
		t.Error("commas")
	}
}

// TestLiveNavigatorPostgres drives the Tables pane's pickers against a real
// Postgres (opt-in, DBC_LIVE_PG_DSN, as db's live tests): the rows, a
// schema pick and its tables, a database switch onto a derived connection
// with the base's row still marked, the schema pick restored on a switch
// back, and the derived pool closed once the sidebar has left it.
func TestLiveNavigatorPostgres(t *testing.T) {
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	for _, k := range []string{cats.EnvMarker, cats.EnvPaneID, cats.EnvControlSocket, cats.EnvHookSocket} {
		t.Setenv(k, "")
	}
	const other = "dbc_tui_nav_other"
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: "live",
		Connections:       []config.Connection{{Name: "live", Driver: "postgres", DSN: dsn}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	setup := []string{
		`DROP SCHEMA IF EXISTS tui_nav_a CASCADE`, `DROP SCHEMA IF EXISTS tui_nav_b CASCADE`,
		`CREATE SCHEMA tui_nav_a`, `CREATE SCHEMA tui_nav_b`,
		`CREATE TABLE tui_nav_a.alpha (id int)`, `CREATE TABLE tui_nav_b.beta (id int)`,
		`CREATE TABLE tui_nav_b.gamma (id int)`,
		`DROP DATABASE IF EXISTS ` + other + ` WITH (FORCE)`, `CREATE DATABASE ` + other,
	}
	for _, s := range setup {
		if _, err := mgr.Run("live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		mgr.Drop("live")
		for _, s := range []string{`DROP SCHEMA tui_nav_a CASCADE`, `DROP SCHEMA tui_nav_b CASCADE`,
			`DROP DATABASE IF EXISTS ` + other + ` WITH (FORCE)`} {
			_, _ = mgr.Run("live", s)
		}
	})
	if _, err := mgr.Run(config.DerivedName("live", other), `CREATE TABLE only_there (id int)`); err != nil {
		t.Fatalf("create on %s: %v", other, err)
	}
	mgr.Disconnect(config.DerivedName("live", other))

	clipLog, openLog = nil, nil
	m := New(cfg, mgr, Options{NoPersist: true})
	t.Cleanup(m.shutdown)
	picks := filepath.Join(t.TempDir(), "schema-picks.json")
	m.loadPicks(picks)
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(t, m, nil, m.Init())

	if dbRow, schemaRow := m.navRows(); !dbRow || !schemaRow {
		t.Fatalf("rows: db %v schema %v", dbRow, schemaRow)
	}
	f := frame(m)
	findText(t, f, "⛁ "+m.currentDatabase())
	findText(t, f, "◫ ")

	// s, filter, Enter: the schema's tables, and the row names it
	m.focus = focusTables
	key(t, m, "s")
	if _, ok := m.modal.(*pickModal); !ok {
		t.Fatalf("no schema picker: %v\n%s", m.modal, logText(m))
	}
	typeText(t, m, "nav_b")
	key(t, m, "enter")
	if s := m.ws.CatalogSchema(); s != "tui_nav_b" || len(m.tables.items) != 2 {
		t.Fatalf("schema %q, %d tables:\n%s", s, len(m.tables.items), logText(m))
	}
	findText(t, frame(m), "◫ tui_nav_b · 2")

	// a click on the database row, then the other database
	x, y := findText(t, frame(m), "⛁ ")
	click(t, m, x, y)
	if _, ok := m.modal.(*pickModal); !ok {
		t.Fatalf("no database picker")
	}
	typeText(t, m, other)
	key(t, m, "enter")
	derived := config.DerivedName("live", other)
	if m.ws.Active() != derived {
		t.Fatalf("active = %q:\n%s", m.ws.Active(), logText(m))
	}
	if it := m.conns.items[m.conns.cur]; it.label != "live" || it.mark != "●" {
		t.Errorf("connections row marked: %+v", it)
	}
	if len(m.tables.items) != 1 || !strings.Contains(m.tables.items[0].label, "only_there") {
		t.Errorf("tables on %s: %+v", other, m.tables.items)
	}

	// back to the base: the schema picked there is listed again, and the
	// derived pool — left, and no longer anyone's — is closed
	key(t, m, "d")
	typeText(t, m, baseDatabase(m))
	key(t, m, "enter")
	if m.ws.Active() != "live" || m.ws.CatalogSchema() != "tui_nav_b" {
		t.Fatalf("back: active %q schema %q", m.ws.Active(), m.ws.CatalogSchema())
	}
	if mgr.Disconnect(derived) {
		t.Errorf("%s's pool was still open after the switch away", derived)
	}

	// all schemas lists both schemas' tables
	key(t, m, "s")
	for range 50 { // to the top: the first row is "all schemas"
		key(t, m, "up")
	}
	key(t, m, "enter")
	if m.ws.CatalogSchema() != "" {
		t.Errorf("all schemas: listed %q", m.ws.CatalogSchema())
	}

	// the picks outlive the run: a new dbc's first connect opens on the
	// last one (all schemas), not the default schema
	if got := userdata.LoadPicks(picks); got["live"] != (userdata.SchemaPick{All: true}) {
		t.Fatalf("saved picks = %+v", got)
	}
	m.shutdown()
	next := New(cfg, mgr, Options{NoPersist: true})
	t.Cleanup(next.shutdown)
	next.loadPicks(picks)
	drive(t, next, tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(t, next, nil, next.Init())
	if next.ws.Active() != "live" || next.ws.CatalogSchema() != "" {
		t.Errorf("restart: active %q schema %q", next.ws.Active(), next.ws.CatalogSchema())
	}
}

// TestLiveNavigatorMySQL: on MySQL (opt-in, DBC_LIVE_MYSQL_DSN) the Tables
// pane has the database row only — a MySQL database is its one schema, so
// s says there is nothing to pick — and d switches onto a derived
// connection and back, as on Postgres.
func TestLiveNavigatorMySQL(t *testing.T) {
	dsn := os.Getenv("DBC_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_MYSQL_DSN to run against a live MySQL")
	}
	for _, k := range []string{cats.EnvMarker, cats.EnvPaneID, cats.EnvControlSocket, cats.EnvHookSocket} {
		t.Setenv(k, "")
	}
	const other = "dbc_tui_nav_other"
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: "live",
		Connections:       []config.Connection{{Name: "live", Driver: "mysql", DSN: dsn}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	for _, s := range []string{`DROP DATABASE IF EXISTS ` + other, `CREATE DATABASE ` + other} {
		if _, err := mgr.Run("live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		mgr.Drop("live")
		_, _ = mgr.Run("live", `DROP DATABASE IF EXISTS `+other)
	})
	derived := config.DerivedName("live", other)
	if _, err := mgr.Run(derived, `CREATE TABLE only_there (id int)`); err != nil {
		t.Fatalf("create on %s: %v", other, err)
	}
	mgr.Disconnect(derived)

	clipLog, openLog = nil, nil
	m := New(cfg, mgr, Options{NoPersist: true})
	t.Cleanup(m.shutdown)
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(t, m, nil, m.Init())

	if dbRow, schemaRow := m.navRows(); !dbRow || schemaRow {
		t.Fatalf("rows: db %v schema %v (want the database row only)", dbRow, schemaRow)
	}
	findText(t, frame(m), "⛁ "+baseDatabase(m))

	m.focus = focusTables
	key(t, m, "s")
	if m.modal != nil || !strings.Contains(logText(m), "no schemas to pick") {
		t.Errorf("s on MySQL: modal %v\n%s", m.modal, logText(m))
	}

	key(t, m, "d")
	if _, ok := m.modal.(*pickModal); !ok {
		t.Fatalf("no database picker:\n%s", logText(m))
	}
	typeText(t, m, other)
	key(t, m, "enter")
	if m.ws.Active() != derived {
		t.Fatalf("active = %q:\n%s", m.ws.Active(), logText(m))
	}
	if it := m.conns.items[m.conns.cur]; it.label != "live" || it.mark != "●" {
		t.Errorf("connections row marked: %+v", it)
	}
	if len(m.tables.items) != 1 || !strings.Contains(m.tables.items[0].label, "only_there") {
		t.Errorf("tables on %s: %+v", other, m.tables.items)
	}
	findText(t, frame(m), "⛁ "+other)

	// back to the DSN's own database: the configured connection itself,
	// and the derived pool closed once left
	key(t, m, "d")
	typeText(t, m, baseDatabase(m))
	key(t, m, "enter")
	if m.ws.Active() != "live" {
		t.Fatalf("back: active %q:\n%s", m.ws.Active(), logText(m))
	}
	if mgr.Disconnect(derived) {
		t.Errorf("%s's pool was still open after the switch away", derived)
	}
}

// baseDatabase is the live DSN's own database, for picking it back.
func baseDatabase(m *Model) string {
	cc, _ := m.cfg.ConnByName("live")
	return db.DefaultDatabase(cc)
}
