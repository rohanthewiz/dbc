package sqlcomplete

import (
	"strings"
	"testing"
)

// kindsAndInserts lists r's items as kind:insert.
func kindsAndInserts(r Result) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, string(it.Kind)+":"+it.Insert)
	}
	return out
}

// Inside a quoted name being typed, names are offered (N-298): the quote is
// replaced with the name as it needs writing, the vocabulary is left out,
// and the scope is read as though the name were closed at the caret.
func TestCompleteInsideQuotedName(t *testing.T) {
	cases := []struct {
		driver, buf string
		want        []string
		from        int // where the replacement starts: the opening quote
	}{
		{"postgres", `SELECT * FROM "Inv▮`, []string{`table:sales."Invoices"`}, 14},
		{"postgres", `SELECT * FROM sales."Inv▮`, []string{`table:"Invoices"`}, 20},
		{"postgres", `SELECT * FROM orders WHERE "sta▮`, []string{"column:status"}, 27},
		{"postgres", `SELECT i."Am▮ FROM sales."Invoices" i`, []string{`column:"Amount"`}, 9},
		{"mysql", "SELECT * FROM `ord▮", []string{"table:order_items", "table:orders", "view:big_orders"}, 14},
	}
	for _, c := range cases {
		r := at(t, c.driver, c.buf)
		if got := kindsAndInserts(r); strings.Join(got, " ") != strings.Join(c.want, " ") || r.From != c.from {
			t.Errorf("%s %q: %v from %d, want %v from %d", c.driver, c.buf, got, r.From, c.want, c.from)
		}
		for _, it := range r.Items {
			if !strings.HasPrefix(it.Filter, r.Prefix[:1]) {
				t.Errorf("%q: filter %q lacks the quote", c.buf, it.Filter)
			}
		}
	}
	// after a closed quoted name, and inside one with an escaped quote:
	// as before (the clause after it; nothing)
	if r := at(t, "postgres", `SELECT * FROM "Invoices"▮`); r.From != 24 {
		t.Errorf("after a closed name: from %d", r.From)
	}
	if r := at(t, "postgres", `SELECT "a""b▮`); len(r.Items) != 0 {
		t.Errorf("an escaped quote: %v", kindsAndInserts(r))
	}
}

// A table's own columns where a statement names one (N-298): ALTER TABLE
// t … COLUMN ▮, CREATE INDEX ON t (▮, REFERENCES t (▮.
func TestCompleteDDLColumnLists(t *testing.T) {
	orders := "column:created_at column:customer_id column:id column:status"
	for buf, want := range map[string]string{
		`ALTER TABLE orders DROP COLUMN ▮`:                          orders,
		`ALTER TABLE IF EXISTS public.orders ALTER COLUMN st▮`:      "column:status",
		`ALTER TABLE orders RENAME COLUMN ▮`:                        orders,
		`CREATE INDEX ON orders (▮`:                                 orders,
		`CREATE UNIQUE INDEX ix ON public.orders (status, ▮`:        orders,
		`ALTER TABLE x ADD FOREIGN KEY (a) REFERENCES customers (▮`: "column:email column:id column:name",
	} {
		if got := strings.Join(kindsAndInserts(at(t, "postgres", buf)), " "); got != want {
			t.Errorf("%q:\n got %s\nwant %s", buf, got, want)
		}
	}
	// a join's ON ( is no column list
	if r := at(t, "postgres", `SELECT * FROM orders o JOIN customers c ON (▮`); len(r.Items) < 10 {
		t.Errorf("ON ( in a join: %v", kindsAndInserts(r))
	}
}
