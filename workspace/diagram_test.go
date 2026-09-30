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
