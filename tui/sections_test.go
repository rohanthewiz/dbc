package tui

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/workspace"
)

// tableNames is the Tables pane's rows that name something — a table,
// view or routine — without the sections' headers and notes: what tests
// written against the list's names index.
func tableNames(m *Model) []listItem {
	var out []listItem
	for _, it := range m.tables.items {
		if !it.head && !isSectionHeader(it) {
			out = append(out, it)
		}
	}
	return out
}

// nameIndex is the index in m.tables.items of the i'th name row
// (tableNames), for putting the cursor on it.
func nameIndex(m *Model, i int) int {
	for j, it := range m.tables.items {
		if !it.head && !isSectionHeader(it) {
			if i == 0 {
				return j
			}
			i--
		}
	}
	return -1
}

// THE SECTIONS, as a user drives them on SQLite: Tables open and Views
// folded as a tab starts, with no Routines section on an engine that
// stores none; v unfolds the views, D on one opens its DDL in the viewer;
// a click on a header folds it; the find keeps a header only while its
// section has a match.
func TestTablesPaneSections(t *testing.T) {
	m := newTestModel(t)
	for _, q := range []string{"CREATE VIEW old_cats AS SELECT * FROM cats WHERE 1 = 1"} {
		if _, err := m.ws.Manager().Run(m.ws.Active(), q); err != nil {
			t.Fatal(err)
		}
	}
	drive(t, m, nil, job(m.ws.Connect(m.ws.Active()).Job))
	if got := strings.Join(labelsAll(m), ","); got != "Tables,cats,Views" {
		t.Fatalf("as a tab starts: %q", got)
	}
	if it := m.tables.items[2]; it.mark != "▸" || it.sub != "1" {
		t.Errorf("the folded Views header: %+v", it)
	}
	if name, _ := m.currentTable(); name != "cats" {
		t.Errorf("cursor on %q, want the first table", name)
	}

	m.focus = focusTables
	key(t, m, "v")
	if got := strings.Join(labelsAll(m), ","); got != "Tables,cats,Views,old_cats" {
		t.Fatalf("after v: %q", got)
	}
	if s, ok := m.currentSection(); !ok || s != workspace.SectionViews {
		t.Errorf("v left the cursor on %+v, want the Views header", m.tables.items[m.tables.cur])
	}
	key(t, m, "down")
	key(t, m, "D")
	d, ok := m.modal.(*ddlModal)
	if !ok {
		t.Fatalf("no DDL viewer: %v\n%s", m.modal, logText(m))
	}
	if d.ddl != "CREATE VIEW old_cats AS SELECT * FROM cats WHERE 1 = 1;" || d.qname != "main.old_cats" && d.qname != "old_cats" {
		t.Errorf("viewer %q: %q", d.qname, d.ddl)
	}
	key(t, m, "esc")
	// D on a table says so, and opens nothing
	m.focus = focusTables
	m.tables.cur = nameIndex(m, 0)
	key(t, m, "D")
	if m.modal != nil || !strings.Contains(logText(m), "Show DDL is for a view or a routine") {
		t.Errorf("D on a table: modal %v\n%s", m.modal, logText(m))
	}

	// one click on the Tables header folds it
	frame(m)
	click(t, m, m.lay.tables.X+3, m.lay.tables.Y+1)
	if m.ws.SectionOpen(workspace.SectionTables) {
		t.Fatal("a click on the Tables header did not fold it")
	}
	if got := strings.Join(labelsAll(m), ","); got != "Tables,Views,old_cats" {
		t.Errorf("tables folded: %q", got)
	}
	key(t, m, "t")

	// the find: a header stays while its section has a match
	key(t, m, "/")
	typeText(t, m, "old")
	if got := strings.Join(labelsAll(m), ","); got != "Views,old_cats" {
		t.Errorf("found: %q", got)
	}
	if name, _ := m.currentTable(); name != "old_cats" {
		t.Errorf("find cursor on %q, want the match, past its header", name)
	}
}

// labelsAll is every row of the tables list by label, headers included.
func labelsAll(m *Model) []string {
	out := make([]string, 0, len(m.tables.items))
	for _, it := range m.tables.items {
		out = append(out, it.label)
	}
	return out
}
