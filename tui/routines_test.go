package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/clip"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// A routine's row: its name bare in a one-schema list, qualified when the
// database has several schemas or the list spans several; its kind short
// on the right; an overload's arguments after its name, a lone name's not.
func TestRoutineItems(t *testing.T) {
	rs := []model.Routine{
		{Schema: "public", Name: "total", Kind: model.RoutineFunction, Args: "o integer", ID: "1"},
		{Schema: "public", Name: "total", Kind: model.RoutineFunction, Args: "o integer, tax boolean", ID: "2"},
		{Schema: "public", Name: "archive", Kind: model.RoutineProcedure, Args: "IN before date", ID: "3"},
	}
	items := routineItems(rs, false)
	want := []struct{ label, sub, desc string }{
		{"total", "fn", "(o integer)"},
		{"total", "fn", "(o integer, tax boolean)"},
		{"archive", "proc", ""},
	}
	for i, w := range want {
		if it := items[i]; it.label != w.label || it.sub != w.sub || it.desc != w.desc {
			t.Errorf("row %d: %+v, want %+v", i, it, w)
		}
	}
	if r, ok := items[1].data.(model.Routine); !ok || r.ID != "2" {
		t.Errorf("row 1's data: %+v", items[1].data)
	}
	if items := routineItems(rs, true); items[2].label != "public.archive" {
		t.Errorf("many schemas: %q", items[2].label)
	}
	mixed := append(rs[:1:1], model.Routine{Schema: "audit", Name: "log", Kind: model.RoutineTrigger})
	if items := routineItems(mixed, false); items[0].label != "public.total" || items[1].label != "audit.log" || items[1].sub != "trigger" {
		t.Errorf("spanning schemas: %+v", items)
	}
	// a table and a routine never share a cursor key, nor two overloads
	if itemKey(items[0]) == itemKey(items[1]) || itemKey(listItem{data: "total"}) == itemKey(items[0]) {
		t.Error("itemKey collides")
	}
}

// The viewer cuts each line at the width, in runes, and the cuts index
// the expanded text (tabs as spaces), so the colors line up with it.
func TestDDLModalLines(t *testing.T) {
	d := newDDLModal(model.Routine{Name: "f"}, "abcdefg\n\té\n")
	if d.shown != "abcdefg\n    é\n" {
		t.Fatalf("shown %q", d.shown)
	}
	got := []string{}
	for _, l := range d.lines(3) {
		got = append(got, d.shown[l.from:l.to])
	}
	want := []string{"abc", "def", "g", "   ", " é", ""}
	if len(got) != len(want) {
		t.Fatalf("lines %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: %q, want %q", i, got[i], want[i])
		}
	}
}

// THE ROUTINES LIST ON A REAL POSTGRES, as a user drives it: f in the
// Tables pane lists the schema's routines in place of its tables, titled
// "Routines · n"; Enter on one opens its DDL in the viewer, y copies it,
// i puts it in the editor; f again brings the tables back.
func TestLiveRoutinesPanePostgres(t *testing.T) {
	dsn := os.Getenv("DBC_LIVE_PG_DSN")
	if dsn == "" {
		t.Skip("set DBC_LIVE_PG_DSN to run against a live Postgres")
	}
	for _, k := range []string{cats.EnvMarker, cats.EnvPaneID, cats.EnvControlSocket, cats.EnvHookSocket} {
		t.Setenv(k, "")
	}
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: "live",
		Connections:       []config.Connection{{Name: "live", Driver: "postgres", DSN: dsn}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	setup := []string{
		`DROP SCHEMA IF EXISTS tui_rt CASCADE`, `CREATE SCHEMA tui_rt`,
		`CREATE TABLE tui_rt.only_table (id int)`,
		`CREATE FUNCTION tui_rt.tui_rt_double(n integer) RETURNS integer LANGUAGE sql AS 'SELECT n * 2'`,
	}
	for _, s := range setup {
		if _, err := mgr.Run("live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = mgr.Run("live", `DROP SCHEMA tui_rt CASCADE`) })

	// the clipboard and browser stubbed, as newTestModel does: y must not
	// reach the real clipboard
	clipLog, openLog = nil, nil
	prevClip, prevOpen := clipWrite, openURL
	clipWrite = func(c clip.Content) (bool, error) { clipLog = append(clipLog, c); return c.HTML != "", nil }
	openURL = func(u string) { openLog = append(openLog, u) }
	t.Cleanup(func() { clipWrite, openURL = prevClip, prevOpen })
	m := New(cfg, mgr, Options{NoPersist: true})
	t.Cleanup(m.shutdown)
	m.loadPicks(filepath.Join(t.TempDir(), "schema-picks.json"))
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(t, m, nil, m.Init())
	drive(t, m, nil, m.pickSchema(workspace.SchemaPick{Name: "tui_rt"}))
	if m.ws.CatalogSchema() != "tui_rt" {
		t.Fatalf("schema %q\n%s", m.ws.CatalogSchema(), logText(m))
	}

	m.focus = focusTables
	key(t, m, "f")
	if !m.routinesShown() || len(m.tables.items) != 1 {
		t.Fatalf("after f: shown %v, rows %+v\n%s", m.routinesShown(), m.tables.items, logText(m))
	}
	f := frame(m)
	findText(t, f, "Routines · 1")
	// qualified, as the tables are on a many-schema database (and cut to
	// the narrow pane)
	findText(t, f, "tui_rt.tui_rt_")

	key(t, m, "enter")
	d, ok := m.modal.(*ddlModal)
	if !ok {
		t.Fatalf("no DDL viewer: %v\n%s", m.modal, logText(m))
	}
	if !strings.HasPrefix(d.ddl, "CREATE OR REPLACE FUNCTION tui_rt.tui_rt_double(n integer)") || !strings.HasSuffix(d.ddl, ";") {
		t.Errorf("ddl:\n%s", d.ddl)
	}
	findText(t, frame(m), "DDL · tui_rt.tui_rt_double")
	key(t, m, "y")
	if got := lastClip(t).Text; got != d.ddl {
		t.Errorf("copied %q", got)
	}
	key(t, m, "i")
	if m.modal != nil || !strings.Contains(m.editor.Text(), "SELECT n * 2") || m.focus != focusEditor {
		t.Errorf("after i: modal %v, focus %v, editor %q", m.modal, m.focus, m.editor.Text())
	}

	m.focus = focusTables
	key(t, m, "f")
	if m.routinesShown() || len(m.tables.items) != 1 || m.tables.items[0].data != "tui_rt.only_table" {
		t.Errorf("after f again: shown %v, rows %+v", m.routinesShown(), m.tables.items)
	}
}
