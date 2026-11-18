package sqlcomplete

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/erd"
)

// bigShop is a catalog of n tables of cols columns each, spread over the
// given schemas in turn, each table after the first holding a foreign key to
// the one before it — n-1 relationships, so a pass over Rels per candidate
// shows up as it would on a large real catalog.
//
//	t_00000 ◄── t_00001 ◄── t_00002 ◄── …   (prev_id → id)
func bigShop(n, cols int, schemas ...string) *erd.Schema {
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}
	s := &erd.Schema{Driver: "postgres"}
	for i := range n {
		t := &erd.Table{Schema: schemas[i%len(schemas)], Name: fmt.Sprintf("t_%05d", i), PK: []string{"id"}}
		t.Cols = append(t.Cols, &erd.Column{Name: "id", Type: "bigint", PK: true})
		t.Cols = append(t.Cols, &erd.Column{Name: "prev_id", Type: "bigint", FK: i > 0, Nullable: true})
		for j := 2; j < cols; j++ {
			t.Cols = append(t.Cols, &erd.Column{Name: fmt.Sprintf("c%02d_%05d", j, i), Type: "text", Nullable: true})
		}
		if i > 0 {
			prev := s.Tables[i-1]
			s.Rels = append(s.Rels, &erd.Rel{Name: t.Name + "_prev_fk", Child: t, Parent: prev,
				ChildCols: []string{"prev_id"}, ParentCols: []string{"id"}})
		}
		s.Tables = append(s.Tables, t)
	}
	return s
}

// benchComplete times one request at buf's ▮ marker against sc. docB is
// the doc bytes the result carries as it comes back — what dbc web would
// send for it: since N-328 a catalog object's doc is held back (only a
// keyword's or function's own text is carried), so for the catalog cases
// it reads 0 where it read 177 KB.
func benchComplete(b *testing.B, sc *erd.Schema, path []string, buf string) {
	i := strings.Index(buf, "▮")
	req := Request{Schema: sc, Driver: "postgres", SearchPath: path, Buffer: buf[:i] + buf[i+len("▮"):], Caret: i}
	var n, docBytes int
	for b.Loop() {
		r := Complete(req)
		n, docBytes = len(r.Items), 0
		for _, it := range r.Items {
			docBytes += len(it.Doc)
		}
	}
	b.ReportMetric(float64(n), "items")
	b.ReportMetric(float64(docBytes), "docB")
}

// BenchmarkCompleteBig is completion on a 5,000-table, 20-column catalog
// (N-280): before, every table's DDL and every column's doc was built — each
// a pass over all Rels — before the typed prefix dropped it, 32–34 ms a
// request even when 5 items came back.
func BenchmarkCompleteBig(b *testing.B) {
	sc := bigShop(5000, 20)
	multi := bigShop(5000, 20, "app", "public", "audit")
	for _, c := range []struct {
		name string
		sc   *erd.Schema
		path []string
		buf  string
	}{
		// no FROM yet, a prefix no table has: every table is a candidate,
		// a few keywords come back
		{"select-prefix", sc, nil, "SELECT col▮"},
		// every table matches and the cap keeps 400
		{"from-empty", sc, nil, "SELECT * FROM ▮"},
		{"from-prefix", sc, nil, "SELECT * FROM t_012▮"},
		// a joined pair in scope: their columns, the FK join clauses
		{"scope", sc, nil, "SELECT ▮ FROM t_00100 a JOIN t_00101 b ON b.prev_id = a.id"},
		{"join", sc, nil, "SELECT * FROM t_00100 a JOIN ▮"},
		// three schemas on a reported search path: each table's bare-or-
		// qualified test
		{"from-schemas", multi, []string{"app", "public"}, "SELECT * FROM ▮"},
		{"select-schemas", multi, []string{"app", "public"}, "SELECT col▮"},
	} {
		b.Run(c.name, func(b *testing.B) { benchComplete(b, c.sc, c.path, c.buf) })
	}
}

// The docs are built after the cap, for the items kept (N-280), and still
// read as they did: a table's DDL with its foreign keys out, a join clause's
// the joined table's DDL.
func TestDocsOfKeptItems(t *testing.T) {
	r := at(t, "postgres", "SELECT * FROM ▮")
	for label, want := range map[string]string{
		"orders":      "  customer_id bigint NOT NULL,  -- → customers(id)\n",
		"order_items": "  order_id bigint NOT NULL,  -- → orders(id)\n",
	} {
		if it, ok := find(r, label); !ok || !strings.Contains(it.Documentation(), want) {
			t.Errorf("%s doc lacks %q:\n%s", label, want, it.Documentation())
		}
	}
	if it, _ := find(r, "customers"); !strings.HasPrefix(it.Documentation(), "CREATE TABLE public.customers (") || strings.Contains(it.Documentation(), "→") {
		t.Errorf("customers doc, no keys out:\n%s", it.Documentation())
	}
	r = at(t, "postgres", "SELECT * FROM orders o JOIN ▮")
	it, ok := find(r, "customers c ON c.id = o.customer_id")
	if !ok || !strings.HasPrefix(it.Documentation(), "CREATE TABLE public.customers (") {
		t.Errorf("join clause: %v %+v", ok, it)
	}
}

// A request costs what it keeps, not what the catalog holds (N-280): a table
// the prefix drops is a name compare, and of those it keeps only the
// MaxItems shown get a doc. Counted in allocations, which unlike time do not
// vary with the machine (nor, here, with -race). Before, every table's DDL
// was built whatever was typed: on this catalog 42,820 allocations for
// "SELECT col▮" (21.4 a table, 5 items back) and 46,118 for "SELECT * FROM ▮"
// (23.1); after, 1.3 and 7.1 a table. The bounds leave room for a Go
// release's maps to count differently and none for a doc per candidate.
func TestCompleteCostsWhatItKeeps(t *testing.T) {
	const n = 2000
	sc := bigShop(n, 10)
	for _, c := range []struct {
		buf      string
		perTable float64 // the bound, in allocations per table in the catalog
	}{
		// every table a candidate, none kept: index's own entries only
		{"SELECT col▮", 4},
		// every table kept as an item, 400 of them with a doc
		{"SELECT * FROM ▮", 12},
	} {
		i := strings.Index(c.buf, "▮")
		req := Request{Schema: sc, Driver: "postgres", Buffer: c.buf[:i], Caret: i}
		got := testing.AllocsPerRun(5, func() { Complete(req) })
		if got > c.perTable*n {
			t.Errorf("%q: %.0f allocations on %d tables, want at most %.0f", c.buf, got, n, c.perTable*n)
		}
		t.Logf("%q: %.0f allocations (%.1f a table)", c.buf, got, got/n)
	}
}

// N-328: a catalog object's doc is held back until it is read. The result
// carries none (DocPending), Documentation builds the one asked for — the
// same text as before — and a copy of the item reads it too. Reads from
// several goroutines at once share the book's one index (run under -race).
func TestDocsHeldBack(t *testing.T) {
	sc := bigShop(1000, 4)
	req := Request{Schema: sc, Driver: "postgres", Buffer: "SELECT * FROM ", Caret: len("SELECT * FROM ")}
	r := Complete(req)
	if len(r.Items) != MaxItems {
		t.Fatalf("%d items, want the cap", len(r.Items))
	}
	for _, it := range r.Items {
		if it.Kind == KindTable && (it.Doc != "" || !it.DocPending()) {
			t.Fatalf("%s: doc carried (%d bytes) or not pending", it.Label, len(it.Doc))
		}
	}
	it, ok := find(r, "t_00005")
	if !ok {
		t.Fatal("no t_00005")
	}
	doc := it.Documentation()
	if !strings.HasPrefix(doc, "CREATE TABLE public.t_00005 (") || !strings.Contains(doc, "→ t_00004(id)") {
		t.Fatalf("t_00005's doc:\n%s", doc)
	}
	if cp := it; cp.Documentation() != doc {
		t.Fatal("a copy of the item reads another doc")
	}

	done := make(chan string, len(r.Items))
	for _, it := range r.Items {
		go func() { done <- it.Documentation() }()
	}
	for range r.Items {
		if d := <-done; d == "" {
			t.Fatal("a doc read concurrently came back empty")
		}
	}
}
