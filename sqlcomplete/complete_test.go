package sqlcomplete

import (
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/erd"
)

// shop is a small Postgres-shaped catalog: three linked tables in public,
// a view, and a table in a second schema.
//
//	customers ◄── orders ◄── order_items
func shop() *erd.Schema {
	col := func(name, typ string, pk, fk, null bool) *erd.Column {
		return &erd.Column{Name: name, Type: typ, PK: pk, FK: fk, Nullable: null}
	}
	customers := &erd.Table{Schema: "public", Name: "customers", PK: []string{"id"}, Cols: []*erd.Column{
		col("id", "bigint", true, false, false), col("name", "text", false, false, false), col("email", "text", false, false, true),
	}}
	orders := &erd.Table{Schema: "public", Name: "orders", PK: []string{"id"}, Cols: []*erd.Column{
		col("id", "bigint", true, false, false), col("customer_id", "bigint", false, true, false),
		col("status", "text", false, false, false), col("created_at", "timestamptz", false, false, false),
	}}
	items := &erd.Table{Schema: "public", Name: "order_items", Cols: []*erd.Column{
		col("id", "bigint", true, false, false), col("order_id", "bigint", false, true, false), col("qty", "integer", false, false, false),
	}}
	view := &erd.Table{Schema: "public", Name: "big_orders", View: true, Cols: []*erd.Column{col("id", "bigint", false, false, true)}}
	inv := &erd.Table{Schema: "sales", Name: "Invoices", Cols: []*erd.Column{col("Amount", "numeric", false, false, true)}}
	return &erd.Schema{
		Driver: "postgres",
		Tables: []*erd.Table{customers, orders, items, view, inv},
		Rels: []*erd.Rel{
			{Name: "orders_customer_fk", Child: orders, Parent: customers, ChildCols: []string{"customer_id"}, ParentCols: []string{"id"}},
			{Name: "items_order_fk", Child: items, Parent: orders, ChildCols: []string{"order_id"}, ParentCols: []string{"id"}},
		},
	}
}

// at completes buf at the ▮ marker.
func at(t *testing.T, driver, buf string) Result {
	t.Helper()
	i := strings.Index(buf, "▮")
	if i < 0 {
		t.Fatal("no ▮ in buffer")
	}
	return Complete(Request{Schema: shop(), Driver: driver, Buffer: buf[:i] + buf[i+len("▮"):], Caret: i})
}

func labels(r Result) []string {
	out := make([]string, len(r.Items))
	for i, it := range r.Items {
		out[i] = it.Label
	}
	return out
}

func find(r Result, label string) (Item, bool) {
	for _, it := range r.Items {
		if it.Label == label {
			return it, true
		}
	}
	return Item{}, false
}

// before reports whether a is listed before b (both present).
func before(t *testing.T, r Result, a, b string) {
	t.Helper()
	ls := labels(r)
	ia, ib := slices.Index(ls, a), slices.Index(ls, b)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("want %q before %q; got %d and %d in %v", a, b, ia, ib, head(ls))
	}
}

func head(ls []string) []string { return ls[:min(len(ls), 15)] }

func TestColumnsOfAlias(t *testing.T) {
	r := at(t, "postgres", "SELECT o.▮ FROM orders o")
	if got := head(labels(r)); !slices.Equal(got, []string{"created_at", "customer_id", "id", "status"}) {
		t.Fatalf("o. → %v", got)
	}
	it, _ := find(r, "customer_id")
	if it.Detail != "bigint FK NOT NULL · orders" || !strings.Contains(it.Doc, "references customers(id)") {
		t.Errorf("detail %q doc %q", it.Detail, it.Doc)
	}
}

func TestPrefixAndRange(t *testing.T) {
	r := at(t, "postgres", "select * from orders o where o.cu▮")
	if r.Prefix != "cu" || r.To-r.From != 2 {
		t.Fatalf("prefix %q range %d..%d", r.Prefix, r.From, r.To)
	}
	if got := labels(r); !slices.Equal(got, []string{"customer_id"}) {
		t.Errorf("got %v", got)
	}
}

func TestTablesAfterFrom(t *testing.T) {
	r := at(t, "postgres", "SELECT * FROM ord▮")
	if got := labels(r); len(got) < 2 || got[0] != "order_items" || got[1] != "orders" {
		t.Fatalf("FROM ord → %v", got)
	}
	if it, _ := find(r, "orders"); !strings.HasPrefix(it.Doc, "CREATE TABLE public.orders (") {
		t.Errorf("table doc:\n%s", it.Doc)
	}
	// a table outside public is inserted qualified, and quoted where its
	// name has capitals
	r = at(t, "postgres", "SELECT * FROM inv▮")
	if it, ok := find(r, "Invoices"); !ok || it.Insert != `sales."Invoices"` {
		t.Errorf("Invoices → %+v", it)
	}
}

func TestSchemaQualifier(t *testing.T) {
	r := at(t, "postgres", "SELECT * FROM sales.▮")
	if it, ok := find(r, "Invoices"); !ok || it.Insert != `"Invoices"` {
		t.Errorf("sales. → %v", labels(r))
	}
	r = at(t, "postgres", "SELECT sales.Invoices.▮ FROM sales.Invoices")
	if _, ok := find(r, "Amount"); !ok {
		t.Errorf("schema.table. → %v", labels(r))
	}
}

func TestExpressionScope(t *testing.T) {
	// columns of the tables in scope come first, even before FROM is
	// reached by the caret; an ambiguous name is qualified
	r := at(t, "postgres", "SELECT ▮ FROM orders o JOIN customers c ON c.id = o.customer_id")
	before(t, r, "status", "count")
	before(t, r, "name", "SELECT")
	it, ok := find(r, "o.id")
	if !ok || it.Insert != "o.id" || it.Filter != "id" {
		t.Errorf("ambiguous id → %+v (%v)", it, head(labels(r)))
	}
	if _, ok := find(r, "id"); ok {
		t.Error("a bare id is ambiguous here")
	}
}

func TestJoinFromForeignKeys(t *testing.T) {
	r := at(t, "postgres", "SELECT * FROM orders o JOIN ▮")
	ls := labels(r)
	if len(ls) == 0 || ls[0] != "customers c ON c.id = o.customer_id" && ls[0] != "order_items oi ON oi.order_id = o.id" {
		t.Fatalf("JOIN → %v", head(ls))
	}
	if _, ok := find(r, "order_items oi ON oi.order_id = o.id"); !ok {
		t.Errorf("no join clause for order_items: %v", head(ls))
	}
	// ON offers the condition for the table just joined
	r = at(t, "postgres", "SELECT * FROM orders o JOIN customers cu ON ▮")
	if ls := labels(r); len(ls) == 0 || ls[0] != "cu.id = o.customer_id" {
		t.Errorf("ON → %v", head(ls))
	}
}

func TestCTEColumns(t *testing.T) {
	buf := "WITH recent AS (SELECT o.id, o.status AS state, count(*) n FROM orders o) SELECT r.▮ FROM recent r"
	if got := labels(at(t, "postgres", buf)); !slices.Equal(got, []string{"id", "n", "state"}) {
		t.Errorf("CTE columns → %v", got)
	}
	buf = "WITH x AS (SELECT * FROM customers) SELECT x.▮ FROM x"
	if got := labels(at(t, "postgres", buf)); !slices.Equal(got, []string{"email", "id", "name"}) {
		t.Errorf("star CTE → %v", got)
	}
}

func TestInsertColumns(t *testing.T) {
	r := at(t, "postgres", "INSERT INTO orders (status, ▮")
	if _, ok := find(r, "customer_id"); !ok || len(r.Items) != 4 {
		t.Errorf("INSERT cols → %v", labels(r))
	}
}

func TestNothingInLiterals(t *testing.T) {
	for _, buf := range []string{
		"SELECT 'ord▮",
		"SELECT 'a b▮c' FROM orders",
		"SELECT 1 -- from ord▮",
		"SELECT /* ord▮ */ 1",
		`SELECT "ord▮`,
		"SELECT 12▮",
	} {
		if r := at(t, "postgres", buf); len(r.Items) != 0 {
			t.Errorf("%q → %v", buf, head(labels(r)))
		}
	}
}

func TestStatementBoundaries(t *testing.T) {
	// the second statement does not see the first's tables
	r := at(t, "postgres", "SELECT * FROM orders o;\nSELECT o.▮")
	if len(r.Items) != 0 {
		t.Errorf("o. across a semicolon → %v", labels(r))
	}
	r = at(t, "postgres", "SELECT 1;\nsel▮")
	if ls := labels(r); len(ls) == 0 || ls[0] != "select" {
		t.Errorf("statement start, lower case → %v", head(ls))
	}
}

func TestPostgresVocabulary(t *testing.T) {
	r := at(t, "postgres", "SELECT jsonb_ag▮ FROM orders")
	it, ok := find(r, "jsonb_agg")
	if !ok || it.Insert != "jsonb_agg()" || it.Cursor != len("jsonb_agg(") || !strings.Contains(it.Detail, "jsonb") {
		t.Errorf("jsonb_agg → %+v", it)
	}
	r = at(t, "postgres", "SELECT created_at::timest▮ FROM orders")
	if ls := labels(r); !slices.Equal(ls, []string{"timestamp", "timestamptz"}) {
		t.Errorf(":: → %v", ls)
	}
	r = at(t, "postgres", "SELECT CAST(id AS big▮")
	if ls := labels(r); !slices.Equal(ls, []string{"bigint", "bigserial"}) {
		t.Errorf("CAST AS → %v", ls)
	}
	r = at(t, "postgres", "SELECT * FROM orders WHERE status IL▮")
	if ls := labels(r); len(ls) == 0 || ls[0] != "ILIKE" {
		t.Errorf("clause → %v", head(ls))
	}
	// a keyword lands the caret after itself, with no space added (Enter
	// after a keyword typed in full must stay a new line in both UIs)
	r = at(t, "postgres", "SELECT count(*) FIL▮")
	if it, ok := find(r, "FILTER (WHERE"); !ok || it.Cursor != -1 || it.Insert != "FILTER (WHERE" {
		t.Errorf("FILTER (WHERE → %+v", it)
	}
	// no Postgres functions on SQLite
	if _, ok := find(at(t, "sqlite", "SELECT jsonb_ag▮"), "jsonb_agg"); ok {
		t.Error("jsonb_agg offered on sqlite")
	}
}

func TestClauseAfterTable(t *testing.T) {
	r := at(t, "postgres", "SELECT * FROM orders o ▮")
	before(t, r, "WHERE", "SELECT")
	if _, ok := find(r, "status"); ok {
		t.Error("a column offered where a clause is expected")
	}
}

func TestQuoting(t *testing.T) {
	pg, my := dialectFor("postgres"), dialectFor("mysql")
	for in, want := range map[string]string{"orders": "orders", "Orders": `"Orders"`, "user": `"user"`, "a b": `"a b"`, "x$1": "x$1"} {
		if got := pg.quote(in); got != want {
			t.Errorf("pg %q → %q, want %q", in, got, want)
		}
	}
	if got := my.quote("Orders"); got != "Orders" {
		t.Errorf("mysql Orders → %q", got)
	}
	if got := my.quote("order"); got != "`order`" {
		t.Errorf("mysql order → %q", got)
	}
}

func TestNoSchema(t *testing.T) {
	r := Complete(Request{Driver: "postgres", Buffer: "SELECT coa", Caret: 10})
	if _, ok := find(r, "coalesce"); !ok {
		t.Errorf("no schema → %v", labels(r))
	}
}

func TestMatchQuality(t *testing.T) {
	for _, c := range []struct {
		w, p string
		q    int
		ok   bool
	}{
		{"customer_id", "cus", 0, true},
		{"customer_id", "id", 1, true},
		{"customer_id", "cid", 2, true},
		{"status", "id", 0, false},
		{"Orders", "ord", 0, true},
	} {
		q, ok := matchQuality(c.w, c.p)
		if q != c.q || ok != c.ok {
			t.Errorf("%q/%q → %d %v", c.w, c.p, q, ok)
		}
	}
}
