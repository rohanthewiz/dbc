package designer

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// importOK imports sql, failing the test on a refusal.
func importOK(t *testing.T, cat *Catalog, sql string) (Design, []string) {
	t.Helper()
	d, notes, err := Import(sql, cat)
	if err != nil {
		t.Fatalf("import refused: %v\n%s", err, sql)
	}
	return d, notes
}

// refused imports sql and wants a refusal whose reason holds want.
func refused(t *testing.T, cat *Catalog, sql, want string) *ImportError {
	t.Helper()
	_, _, err := Import(sql, cat)
	var ie *ImportError
	if !errors.As(err, &ie) {
		t.Fatalf("not refused (%v):\n%s", err, sql)
	}
	if !strings.Contains(ie.Reason, want) {
		t.Fatalf("refused for %q, want %q:\n%s", ie.Reason, want, sql)
	}
	return ie
}

// TestImportRoundTrip: the generator's own statements read back as
// designs that write the very same statement — the strongest check that
// reading is the generator's inverse, on every engine's spelling.
func TestImportRoundTrip(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	cat := shopCat()
	var designs []Design

	// filters of every kind, one table
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"), col(d, "o", "total"))
	d.Filters = []Filter{
		{Ref: col(d, "o", "created_at"), Op: OpYear, Values: []string{"2024"}},
		{Ref: col(d, "o", "ref"), Op: OpContains, Values: []string{"50%_o'k"}},
		// each engine's escape character in a value, and a backslash
		{Ref: col(d, "o", "ref"), Op: OpEnds, Values: []string{`x!\y`}},
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"a"}, Group: "A"},
		{Ref: col(d, "o", "total"), Op: OpBetween, Values: []string{"1", "2"}, Group: "A"},
		{Ref: col(d, "o", "status"), Op: OpNe, Values: []string{"void"}, OrNull: true},
		{Ref: col(d, "o", "created_at"), Op: OpOn, Values: []string{"2024-02-29"}},
		{Ref: col(d, "o", "customer_id"), Op: OpIn, Values: []string{"1", "2", "3"}},
		{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{"AB"}, IgnoreCase: true},
		{Ref: col(d, "o", "created_at"), Op: OpLastDays, Values: []string{"7"}},
		{Ref: col(d, "o", "coupon_id"), Op: OpNull},
		{Ref: col(d, "o", "created_at"), Op: OpRange, Values: []string{"2024-01-05", "2024-03-01"}},
	}
	d.Sorts = []Sort{{Ref: col(d, "o", "created_at"), Desc: true}, {Ref: col(d, "o", "id")}}
	d.Limit, d.Offset = 20, 40
	designs = append(designs, d)

	// joins: inner by FK, a left join with its filter in the join, has
	// any with a filter inside, has none, a self-join, grouping with HAVING
	d = build(t, cat, "customers", "orders")
	d.Joins[0].Kind = JoinLeft
	d.Columns = outs(col(d, "c", "name"), col(d, "o", "total"))
	d.Filters = []Filter{{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"paid"}, InJoin: true}}
	designs = append(designs, d)

	d = build(t, cat, "customers", "orders", "tickets")
	d.Joins[0].Kind, d.Joins[1].Kind = JoinExists, JoinNotExists
	d.Columns = outs(col(d, "c", "name"), col(d, "c", "country"))
	d.Filters = []Filter{
		{Ref: col(d, "o", "created_at"), Op: OpYear, Values: []string{"2025"}},
		{Ref: col(d, "c", "country"), Op: OpIn, Values: []string{"NZ", "AU"}},
	}
	d.Distinct = true
	designs = append(designs, d)

	d = build(t, cat, "employees", "employees")
	d.Columns = outs(col(d, "e", "name"), col(d, "e2", "name"))
	designs = append(designs, d)

	d = build(t, cat, "customers", "orders", "order_lines")
	d.Columns = outs(col(d, "c", "country"), agg(d, AggCountDistinct, "o", "id"), agg(d, AggSum, "ol", "amount"), Ref{Agg: AggCount})
	d.Filters = []Filter{
		{Ref: agg(d, AggSum, "ol", "amount"), Op: OpGt, Values: []string{"100"}},
		{Ref: col(d, "o", "status"), Op: OpNotNull},
	}
	d.Sorts = []Sort{{Ref: agg(d, AggSum, "ol", "amount"), Desc: true}}
	designs = append(designs, d)

	// a reserved table name and a capitalized column: both quoted
	d = build(t, cat, "order")
	d.Columns = outs(col(d, "o", "User"))
	designs = append(designs, d)

	// nothing ticked over two tables: a.*, b.*
	designs = append(designs, build(t, cat, "orders", "payments"))

	for _, driver := range []string{"postgres", "mysql", "sqlite", "bytdb"} {
		cat.Driver = driver
		for i, d := range designs {
			want := Analyze(d, cat).SQL
			back, _ := importOK(t, cat, want)
			if got := Analyze(back, cat).SQL; got != want {
				t.Errorf("%s design %d: read back, it writes\n%s\n\nwas\n%s", driver, i, got, want)
			}
		}
	}
}

// TestImportHandWritten: queries as people write them, read into the
// designer's terms, and judged.
func TestImportHandWritten(t *testing.T) {
	cat := shopCat()

	// the classic inflated sum: the guide says so at once
	d, _ := importOK(t, cat, `select c.name, sum(o.total) as spent
		from customers c
		join orders o on c.id = o.customer_id
		join payments p on p.order_id = o.id
		group by c.name
		order by spent desc
		limit 10`)
	a := Analyze(d, cat)
	if lintOf(a, "fanout-aggregate") == nil || d.Joins[0].FK != "orders_customer_id_fkey" || d.Columns[1].Alias != "spent" {
		t.Fatalf("lints %v joins %+v cols %+v", rules(a), d.Joins, d.Columns)
	}
	wantSQL(t, a, "SELECT c.name, sum(o.total) AS spent\nFROM customers c\nJOIN orders o ON o.customer_id = c.id\n"+
		"JOIN payments p ON p.order_id = o.id\nGROUP BY c.name\nORDER BY sum(o.total) DESC\nLIMIT 10")

	// old-style comma joins, a range on dates, a bare column resolved
	d, notes := importOK(t, cat, `SELECT name, o.total FROM customers c, orders o
		WHERE o.customer_id = c.id AND o.created_at >= '2024-01-01' AND o.created_at < '2025-01-01' AND 1 = 1`)
	if len(d.Joins) != 1 || d.Joins[0].Kind != JoinInner || d.Filters[0].Op != OpYear || len(notes) == 0 {
		t.Fatalf("joins %+v filters %+v notes %q", d.Joins, d.Filters, notes)
	}

	// IN (SELECT …) is has any; LEFT JOIN … IS NULL is flagged as the
	// anti-join it is
	d, _ = importOK(t, cat, `SELECT c.name FROM customers c WHERE c.id IN (SELECT customer_id FROM tickets WHERE subject LIKE 'refund%')`)
	if d.Joins[0].Kind != JoinExists || d.Filters[0].Op != OpStarts {
		t.Fatalf("in-subquery: %+v %+v", d.Joins, d.Filters)
	}
	d, _ = importOK(t, cat, `SELECT c.name FROM customers c LEFT OUTER JOIN orders o ON o.customer_id = c.id WHERE o.id IS NULL`)
	if a = Analyze(d, cat); lintOf(a, "anti-join") == nil {
		t.Fatalf("anti-join not seen: %v", rules(a))
	}

	// RIGHT JOIN, read as a left join from the kept table; USING
	d, _ = importOK(t, cat, `SELECT c.name, o.id FROM orders o RIGHT JOIN customers c ON c.id = o.customer_id`)
	if a = Analyze(d, cat); !strings.Contains(a.SQL, "RIGHT JOIN customers c ON c.id = o.customer_id") || lintOf(a, "right-join") == nil {
		t.Fatalf("right join: %s %v", a.SQL, rules(a))
	}
	d, _ = importOK(t, cat, `SELECT o.id, line_no FROM orders o JOIN order_lines ol ON ol.order_id = o.id JOIN products p ON p.id = ol.product_id`)
	if len(d.Joins) != 2 {
		t.Fatalf("joins %+v", d.Joins)
	}

	// spellings: ILIKE, lower() LIKE lower(), MySQL's LIMIT m, n, the last
	// N days, count(1), GROUP BY with no aggregate, ordinals, IS TRUE
	cat.Driver = "mysql"
	d, _ = importOK(t, cat, "SELECT `o`.`id` FROM orders o WHERE lower(o.ref) LIKE lower('%x\\_y%') AND o.created_at >= NOW() - INTERVAL 30 DAY LIMIT 5, 10")
	if f := d.Filters[0]; f.Op != OpContains || !f.IgnoreCase || f.Values[0] != "x_y" || d.Filters[1].Op != OpLastDays || d.Limit != 10 || d.Offset != 5 {
		t.Fatalf("mysql: %+v limit %d offset %d", d.Filters, d.Limit, d.Offset)
	}
	cat.Driver = "postgres"
	d, notes = importOK(t, cat, `SELECT status, count(1) FROM orders WHERE ref ILIKE '%ab' AND created_at >= now() - interval '7 days' GROUP BY 1 ORDER BY 2 DESC`)
	if f := d.Filters[0]; f.Op != OpEnds || !f.IgnoreCase || d.Filters[1].Values[0] != "7" || d.Sorts[0].Agg != AggCount || !d.Sorts[0].Desc {
		t.Fatalf("postgres: %+v %+v", d.Filters, d.Sorts)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "count(1)") {
		t.Fatalf("notes %q", notes)
	}
	d, _ = importOK(t, cat, `SELECT country FROM customers GROUP BY country`)
	if !d.Distinct {
		t.Fatal("GROUP BY with no aggregate is not DISTINCT")
	}
	cat.Driver = "sqlite"
	d, _ = importOK(t, cat, `SELECT * FROM payments WHERE paid_on >= date('now', '-14 days') LIMIT -1 OFFSET 3`)
	if d.Filters[0].Op != OpLastDays || d.Filters[0].Values[0] != "14" || d.Limit != 0 || d.Offset != 3 || len(d.Columns) != 0 {
		t.Fatalf("sqlite: %+v", d)
	}
}

// TestImportRefuses: what a design cannot say is refused, with the reason
// and where — never imported as something else.
func TestImportRefuses(t *testing.T) {
	cat := shopCat()
	cases := []struct{ sql, want string }{
		{"WITH x AS (SELECT 1) SELECT * FROM x", "WITH clause"},
		{"SELECT id FROM orders UNION SELECT id FROM payments", "UNION"},
		{"SELECT id, row_number() OVER (ORDER BY id) FROM orders", "window function"},
		{"SELECT * FROM (SELECT id FROM orders) s", "derived table"},
		{"SELECT CASE WHEN total > 5 THEN 1 END FROM orders", "CASE"},
		{"SELECT upper(name) FROM customers", "not other functions"},
		{"SELECT id FROM orders WHERE id NOT IN (SELECT order_id FROM payments)", "NOT IN (SELECT"},
		{"SELECT id FROM orders WHERE customer_id = $1", "parameter"},
		{"SELECT id FROM orders WHERE total > credit", "no column `credit`"},
		{"SELECT o.id FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.total > c.credit", "joins on equal columns"},
		{"SELECT id FROM nosuch", "no table or view `nosuch`"},
		{"SELECT id FROM orders o JOIN payments p ON p.order_id = o.id", "say which"},
		{"SELECT 1 FROM orders; SELECT 2 FROM orders", "more than one statement"},
		{"SELECT status FROM orders GROUP BY status, total", "not shown"},
		{"SELECT id FROM orders WHERE ref LIKE 'a%b'", "inside it"},
		{"SELECT id FROM orders WHERE ref NOT LIKE 'a%'", "NOT LIKE"},
		{"SELECT id FROM orders WHERE status = NULL", "IS NULL is meant"},
		{"SELECT c.name FROM customers c LEFT JOIN orders o ON o.customer_id = c.id WHERE o.coupon_id = c.id", "outer-joined"},
		{"UPDATE orders SET total = 0", "not a SELECT"},
		{"SELECT id FROM orders LIMIT 0", "LIMIT 0"},
		{"SELECT id FROM orders WHERE EXISTS (SELECT count(*) FROM payments p WHERE p.order_id = orders.id)", "always true"},
		{"SELECT o.id FROM orders o JOIN order_lines ol ON ol.order_id = o.id RIGHT JOIN products p ON p.id = ol.product_id JOIN payments pa ON pa.order_id = o.id",
			"RIGHT or FULL JOIN"},
	}
	for _, c := range cases {
		refused(t, cat, c.sql, c.want)
	}
	// where: the line of what was refused, in the buffer given
	ie := refused(t, cat, "-- totals\nSELECT id\nFROM orders\nWHERE ref LIKE 'a%b'", "inside it")
	if ie.Line != 4 || !strings.HasPrefix(ie.Error(), "line 4: ") {
		t.Fatalf("line %d: %v", ie.Line, ie)
	}
}
