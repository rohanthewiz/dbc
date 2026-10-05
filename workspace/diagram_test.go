package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/erd"
)

func TestDiagram(t *testing.T) {
	w := newTestWorkspace(t)
	run(t, w, `CREATE TABLE owners (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE pets (id INTEGER PRIMARY KEY, owner_id INTEGER REFERENCES owners(id))`)

	s, err := w.Diagram(context.Background(), erd.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	// the seeded cats plus the two new tables: the schema is read fresh,
	// not from the catalog the connect cached
	if len(s.Tables) != 3 || len(s.Rels) != 1 || s.Conn != demo {
		t.Errorf("diagram = %d tables, %d rels on %q", len(s.Tables), len(s.Rels), s.Conn)
	}

	s, err = w.Diagram(context.Background(), erd.Selection{Tables: []string{"pets"}, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tables) != 2 {
		t.Errorf("around pets = %d tables", len(s.Tables))
	}

	_, err = w.Diagram(context.Background(), erd.Selection{Tables: []string{"ghosts"}})
	refusal(t, err, Invalid)
	if !strings.Contains(err.Error(), "ghosts") {
		t.Errorf("the refusal should name the table: %v", err)
	}
}

// A diagram does not take the run slot: it can be drawn while a run holds it.
func TestDiagramWhileBusy(t *testing.T) {
	w := newTestWorkspace(t)
	st, err := w.RunStmts([]string{"SELECT 1"}, "query")
	if err != nil {
		t.Fatal(err)
	}
	// the run is started but its Job not yet run: the slot is taken
	if !w.Busy() {
		t.Fatal("expected the run slot taken")
	}
	if _, err = w.Diagram(context.Background(), erd.Selection{}); err != nil {
		t.Errorf("diagram while busy: %v", err)
	}
	st.Job()
}

func TestDiagramScope(t *testing.T) {
	for _, c := range []struct {
		name string
		sel  erd.Selection
		path []string
		want string
	}{
		{"one schema's everything", erd.Selection{Schema: "sales"}, []string{"app", "public"}, "sales"},
		{"everything, no schema shown", erd.Selection{}, []string{"app", "public"}, "app public"},
		{"everything, path unknown", erd.Selection{}, nil, "public"},
		{"bare name", erd.Selection{Tables: []string{"orders"}, Schema: "sales"}, nil, "sales public"},
		{"qualified names", erd.Selection{Tables: []string{"billing.invoices", "sales.orders", "orders"}, Schema: "sales"},
			[]string{"public"}, "sales public billing"},
		{"case folded too", erd.Selection{Tables: []string{"Billing.invoices"}}, []string{"public"}, "public Billing billing"},
		{"a leading dot is no schema", erd.Selection{Tables: []string{".orders"}}, []string{"public"}, "public"},
	} {
		if got := strings.Join(diagramScope(c.sel, c.path), " "); got != c.want {
			t.Errorf("%s: diagramScope = %q, want %q", c.name, got, c.want)
		}
	}
}

// A connection remembered as too big (by completion or an earlier diagram)
// is drawn from a scoped read. On SQLite, which is not navigable, the scope
// is ignored and the read is whole: the diagram is the same as before. (The
// scoped read on Postgres is in live_test.go.)
func TestDiagramRememberedTooBig(t *testing.T) {
	w := newTestWorkspace(t)
	w.mu.Lock()
	w.complScoped = map[string]bool{demo: true}
	w.mu.Unlock()
	s, err := w.Diagram(context.Background(), erd.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tables) != 1 || s.Tables[0].Name != "cats" {
		t.Errorf("diagram = %v", s.Tables)
	}
}
