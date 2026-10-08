package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// labels is the tables list's rows, by label.
func labels(m *Model) []string {
	out := make([]string, 0, len(m.tables.items))
	for _, it := range m.tables.items {
		out = append(out, it.label)
	}
	return out
}

// THE TABLES PANE'S FIND, as a user drives it: / opens the filter line,
// typing narrows the list (the pane's letter keys are text meanwhile),
// Enter previews the row under the cursor and leaves the filter applied,
// / goes back into the line with its text, and Esc clears it, keeping the
// cursor on the table found.
func TestTablesFind(t *testing.T) {
	m := newTestModel(t)
	for _, q := range []string{
		"CREATE TABLE orders (x INT)", "CREATE TABLE order_items (x INT)",
		"CREATE TABLE backorders (x INT)", "CREATE TABLE zebra (x INT)",
	} {
		if _, err := m.ws.Manager().Run(m.ws.Active(), q); err != nil {
			t.Fatal(err)
		}
	}
	drive(t, m, nil, job(m.ws.Connect(m.ws.Active()).Job))
	if n := len(m.tables.items); n != 5 {
		t.Fatalf("tables %v", labels(m))
	}

	m.focus = focusTables
	key(t, m, "/")
	if !m.findEditing() {
		t.Fatal("/ did not open the filter line")
	}
	if c := frame(m); !strings.Contains(c.Text(), "⌕ find a table") {
		t.Errorf("no filter line with its placeholder:\n%s", c.Text())
	}
	// d and e are the pane's database picker and diagram keys; ? opens the
	// keys. In the line they are text.
	typeText(t, m, "Ord")
	if got := strings.Join(labels(m), ","); got != "backorders,order_items,orders" {
		t.Fatalf("narrowed to %q", got)
	}
	if m.modal != nil {
		t.Fatalf("a letter acted as a key: %T", m.modal)
	}
	if c := frame(m); !strings.Contains(c.Text(), "Tables · 3 of 5") {
		t.Errorf("the title should count the matches:\n%s", c.Text())
	}
	if _, cur := m.render(); cur == nil {
		t.Error("no caret in the filter line")
	}

	// ↓ moves the list under the line; Enter previews that row
	key(t, m, "down")
	key(t, m, "enter")
	if r := m.ws.LastResult(); r == nil || !strings.Contains(r.Query, "FROM order_items LIMIT 100") {
		t.Fatalf("enter did not preview order_items: %+v", r)
	}
	frame(m)
	if m.findEditing() || len(m.tables.items) != 3 {
		t.Fatalf("after the preview: editing %v, rows %v", m.findEditing(), labels(m))
	}

	// back in the pane its letters are keys again: c lists the columns
	m.focus = focusTables
	key(t, m, "c")
	if r := m.ws.LastResult(); r == nil || !strings.Contains(r.Query, "order_items") {
		t.Fatalf("c on the filtered list: %+v", r)
	}

	// / goes back into the line with its text; Esc clears it and the
	// cursor stays on the table found
	m.focus = focusTables
	key(t, m, "/")
	if !m.findEditing() || m.tfind.input.Text() != "Ord" {
		t.Fatalf("/ again: editing %v, text %q", m.findEditing(), m.tfind.input.Text())
	}
	key(t, m, "esc")
	if m.tfind != nil || len(m.tables.items) != 5 {
		t.Fatalf("esc: filter %v, rows %v", m.tfind, labels(m))
	}
	if name, _ := m.currentTable(); name != "order_items" {
		t.Errorf("cursor on %q after the clear, want order_items", name)
	}
	if frame(m); !m.lay.findRow.Empty() {
		t.Error("the filter line outlived its clear")
	}
}

// No match says so, and Enter on it picks nothing; a paste narrows as
// typing does; a click on the line goes back into it; and Esc off the
// line clears a filter left applied.
func TestTablesFindEdges(t *testing.T) {
	m := newTestModel(t)
	m.focus = focusTables
	key(t, m, "/")
	typeText(t, m, "nope")
	if !strings.Contains(frame(m).Text(), "no match") {
		t.Errorf("an empty narrowing should say no match:\n%s", frame(m).Text())
	}
	before := m.ws.LastResult()
	key(t, m, "enter")
	if m.ws.LastResult() != before {
		t.Error("enter on no match ran something")
	}

	// the line holds text, so it stays up off the typing; a click on it
	// goes back in
	frame(m)
	if m.lay.findRow.Empty() {
		t.Fatal("the filter line is down while it holds text")
	}
	click(t, m, m.lay.findRow.X+5, m.lay.findRow.Y)
	if !m.findEditing() {
		t.Fatal("a click on the line did not go back into it")
	}
	for range "nope" {
		key(t, m, "backspace")
	}
	drive(t, m, tea.PasteMsg{Content: "CAT\n"})
	if got := strings.Join(labels(m), ","); got != "cats" {
		t.Fatalf("after the paste: %q", got)
	}

	// Tab away ends the typing, the filter stays; Esc in the pane clears
	key(t, m, "tab")
	frame(m)
	if m.tfind == nil || m.tfind.editing {
		t.Fatalf("tab away: %+v", m.tfind)
	}
	m.focus = focusTables
	key(t, m, "esc")
	if m.tfind != nil {
		t.Error("esc in the pane left the filter")
	}
}

// A new catalog is a new list: the filter's text goes with the old one.
// A re-read of the same catalog keeps it, and the cursor on its row.
func TestTablesFindAcrossCatalogs(t *testing.T) {
	m := newTestModel(t)
	if _, err := m.ws.Manager().Run(m.ws.Active(), "CREATE TABLE catnip (x INT)"); err != nil {
		t.Fatal(err)
	}
	drive(t, m, nil, job(m.ws.Connect(m.ws.Active()).Job))
	m.focus = focusTables
	key(t, m, "/")
	typeText(t, m, "cat")
	key(t, m, "down")
	key(t, m, "tab")                                 // off the line, filter kept
	if name, _ := m.currentTable(); name != "cats" { // catnip sorts first
		t.Fatalf("cursor on %q, want cats", name)
	}

	// the same catalog re-read (a Refresh): filter and cursor stay
	m.relistTables()
	if m.tfind == nil || len(m.tables.items) != 2 {
		t.Fatalf("relist dropped the filter: %v", labels(m))
	}
	if name, _ := m.currentTable(); name != "cats" {
		t.Errorf("relist moved the cursor to %q", name)
	}

	// a new catalog: the filter goes
	m.refreshTables()
	if m.tfind != nil {
		t.Errorf("a new catalog kept the filter %q", m.tfind.input.Text())
	}
	if len(m.tables.items) != 2 || m.tableRowsTotal() != 2 {
		t.Errorf("rows %v", labels(m))
	}
}
