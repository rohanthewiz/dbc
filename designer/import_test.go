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

	// summaries (a derived table per child): every aggregate per row, a
	// filter inside, filters and a sort on the totals in WHERE
	d = build(t, cat, "orders", "payments")
	d.Joins[0].Kind = JoinSummary
	d.Columns = outs(col(d, "o", "id"), agg(d, AggSum, "p", "amount"), agg(d, AggCount, "p", ""),
		agg(d, AggCount, "p", "amount"), agg(d, AggMin, "p", "paid_on"), agg(d, AggMax, "p", "amount"),
		agg(d, AggAvg, "p", "amount"), agg(d, AggCountDistinct, "p", "paid_on"))
	d.Filters = []Filter{
		{Ref: col(d, "p", "amount"), Op: OpGt, Values: []string{"0"}},
		{Ref: agg(d, AggSum, "p", "amount"), Op: OpGt, Values: []string{"10"}, OrNull: true},
		{Ref: agg(d, AggCount, "p", ""), Op: OpGe, Values: []string{"1"}},
	}
	d.Sorts = []Sort{{Ref: agg(d, AggSum, "p", "amount"), Desc: true}, {Ref: col(d, "o", "id")}}
	designs = append(designs, d)

	// grouped outside too: each re-aggregated, avg from its parts, a
	// HAVING on a total
	d = build(t, cat, "customers", "orders", "payments")
	d.Joins[1].Kind = JoinSummary
	d.Columns = outs(col(d, "c", "country"), agg(d, AggAvg, "p", "amount"), agg(d, AggCount, "p", ""),
		agg(d, AggSum, "p", "amount"), agg(d, AggMin, "p", "amount"), agg(d, AggMax, "p", "paid_on"), Ref{Agg: AggCount})
	d.Filters = []Filter{{Ref: agg(d, AggSum, "p", "amount"), Op: OpGt, Values: []string{"100"}}}
	d.Sorts = []Sort{{Ref: agg(d, AggCount, "p", ""), Desc: true}, {Ref: agg(d, AggAvg, "p", "amount")}}
	designs = append(designs, d)

	// two children summarized under one parent; one asked for nothing,
	// so its derived table holds the count the generator puts there
	d = build(t, cat, "orders", "payments", "order_lines")
	d.Joins[0].Kind, d.Joins[1].Kind = JoinSummary, JoinSummary
	d.Columns = outs(col(d, "o", "id"), agg(d, AggSum, "p", "amount"))
	d.Filters = []Filter{{Ref: col(d, "ol", "qty"), Op: OpGt, Values: []string{"1"}}}
	designs = append(designs, d)

	// a summary and nothing ticked: o.* (one table) and c.*, o.* (two),
	// each read back as nothing ticked, not as each of the columns
	d = build(t, cat, "orders", "payments")
	d.Joins[0].Kind = JoinSummary
	designs = append(designs, d)
	d = build(t, cat, "customers", "orders", "payments")
	d.Joins[1].Kind = JoinSummary
	d.Sorts = []Sort{{Ref: col(d, "o", "id")}}
	designs = append(designs, d)

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

	// a summary written by hand: its own names for the derived table and
	// its columns, a bare key, GROUP BY 1, an ordering inside that orders
	// nothing; read as summarize, and written back in the designer's terms
	cat.Driver = "postgres"
	d, notes = importOK(t, cat, `SELECT o.id, s.paid, COALESCE(s.n, 0) AS payments
		FROM orders o
		LEFT OUTER JOIN (
		  SELECT order_id, sum(amount) AS paid, count(*) AS n
		  FROM payments
		  WHERE paid_on >= '2024-01-01' AND paid_on < '2025-01-01'
		  GROUP BY 1
		  ORDER BY 1
		) AS s ON o.id = s.order_id
		WHERE s.paid > 10
		ORDER BY paid DESC`)
	if j := d.Joins[0]; j.Kind != JoinSummary || j.FK != "payments_order_id_fkey" || d.Tables[1].Alias != "s" ||
		!strings.Contains(strings.Join(notes, "\n"), "ORDER BY inside a summary") {
		t.Fatalf("summary: %+v %+v %q", d.Joins, d.Tables, notes)
	}
	a = Analyze(d, cat)
	wantSQL(t, a, "SELECT o.id, s.sum_amount AS paid, COALESCE(s.count, 0) AS payments\nFROM orders o\nLEFT JOIN (\n"+
		"    SELECT s.order_id, sum(s.amount) AS sum_amount, count(*) AS count\n    FROM payments s\n"+
		"    WHERE s.paid_on >= '2024-01-01' AND s.paid_on < '2025-01-01'\n    GROUP BY s.order_id\n) s ON s.order_id = o.id\n"+
		"WHERE s.sum_amount > 10\nORDER BY s.sum_amount DESC")

	// grouped outside, so the summary re-aggregated: orders and payments
	// per customer, counted without the payments multiplying the orders
	d, _ = importOK(t, cat, `select c.name, count(*) as orders, coalesce(sum(t.cnt), 0) as payments
		from customers c join orders o on o.customer_id = c.id
		left join (select pay.order_id, count(*) cnt from payments pay group by pay.order_id) t on t.order_id = o.id
		group by c.name`)
	if a = Analyze(d, cat); !a.Valid || !a.Grouped || d.Columns[2].Ref != (Ref{Table: d.Tables[2].ID, Agg: AggCount}) {
		t.Fatalf("grouped summary: %v %+v\n%s", rules(a), d.Columns, a.SQL)
	}

	// the parent's star beside a summary is nothing ticked, and ORDER BY 1
	// is the parent's first column (the summary's key is not among them)
	d, _ = importOK(t, cat, `SELECT o.* FROM orders o
		LEFT JOIN (SELECT order_id, sum(amount) AS paid FROM payments GROUP BY order_id) s ON s.order_id = o.id
		ORDER BY 1 DESC`)
	if len(d.Columns) != 0 || d.Joins[0].Kind != JoinSummary || d.Sorts[0].Ref != (Ref{Table: d.Tables[0].ID, Column: "id"}) {
		t.Fatalf("o.* over a summary: cols %+v joins %+v sorts %+v", d.Columns, d.Joins, d.Sorts)
	}
	wantSQL(t, Analyze(d, cat), "SELECT o.*\nFROM orders o\nLEFT JOIN (\n    SELECT s.order_id, count(*) AS count\n    FROM payments s\n"+
		"    GROUP BY s.order_id\n) s ON s.order_id = o.id\nORDER BY o.id DESC")
}

// TestImportRefuses: what a design cannot say is refused, with the reason
// and where — never imported as something else.
func TestImportRefuses(t *testing.T) {
	cat := shopCat()
	cases := []struct{ sql, want string }{
		{"WITH x AS (SELECT 1) SELECT * FROM x", "WITH clause"},
		{"SELECT id FROM orders UNION SELECT id FROM payments", "UNION"},
		{"SELECT id, row_number() OVER (ORDER BY id) FROM orders", "window function"},
		{"SELECT * FROM (SELECT id FROM orders) s", "derived table first in FROM"},
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

	// derived tables: only the summary the generator writes, and its
	// columns only in the forms it writes them
	const pay = "(SELECT order_id, sum(amount) AS s, count(*) AS n, avg(amount) AS a FROM payments GROUP BY order_id) p"
	for _, c := range []struct{ sql, want string }{
		{"SELECT o.id FROM orders o JOIN " + pay + " ON p.order_id = o.id", "LEFT JOIN"},
		{"SELECT o.id FROM orders o LEFT JOIN (SELECT order_id, amount FROM payments) p ON p.order_id = o.id", "no GROUP BY"},
		{"SELECT o.id FROM orders o LEFT JOIN (SELECT order_id, paid_on, sum(amount) AS s FROM payments GROUP BY order_id, paid_on) p " +
			"ON p.order_id = o.id", "not joined on it"},
		{"SELECT o.id FROM orders o LEFT JOIN (SELECT order_id, sum(amount) AS s FROM payments GROUP BY order_id HAVING sum(amount) > 5) p " +
			"ON p.order_id = o.id", "HAVING inside a summary"},
		{"SELECT o.id FROM orders o LEFT JOIN (SELECT p.order_id, sum(ol.amount) AS s FROM payments p JOIN order_lines ol " +
			"ON ol.order_id = p.order_id GROUP BY p.order_id) x ON x.order_id = o.id", "more than one table"},
		{"SELECT o.id FROM orders o LEFT JOIN (SELECT order_id, sum(amount) AS s FROM payments GROUP BY order_id) ON order_id = o.id", "no alias"},
		{"SELECT o.id, p.n FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "COALESCE(p.n, 0)"},
		{"SELECT o.id, COALESCE(p.s, 0) FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "COALESCE of a summary's sum"},
		{"SELECT p.order_id FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "the summary's key"},
		{"SELECT * FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "over the summary `p`: write o.* for the other tables'"},
		{"SELECT p.* FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "over the summary"},
		{"SELECT o.*, p.* FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "over the summary"},
		{"SELECT * FROM customers c JOIN orders o ON o.customer_id = c.id LEFT JOIN " + pay + " ON p.order_id = o.id",
			"write c.*, o.* for"},
		{"SELECT o.id FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id AND p.s > 5", "in the summary's ON"},
		{"SELECT o.id FROM orders o LEFT JOIN " + pay + " ON p.s = o.total", "aggregate"},
		{"SELECT o.id FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id JOIN order_lines ol ON ol.order_id = p.order_id",
			"is a column of the summary"},
		{"SELECT o.status, sum(p.s) FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id GROUP BY o.status", "nothing else is aggregated"},
		{"SELECT o.status, count(*), p.s FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id GROUP BY o.status", "the query is grouped"},
		{"SELECT o.status, count(*), avg(p.a) FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id GROUP BY o.status", "average re-aggregated"},
		{"SELECT o.status, count(*), sum(p.n) FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id GROUP BY o.status", "no COALESCE"},
		{"SELECT o.id FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id WHERE sum(p.s) > 5", "aggregate in WHERE"},
		{"SELECT o.id, p.s + 1 FROM orders o LEFT JOIN " + pay + " ON p.order_id = o.id", "as it writes them"},
	} {
		refused(t, cat, c.sql, c.want)
	}
	// where: the line of what was refused, in the buffer given
	ie := refused(t, cat, "-- totals\nSELECT id\nFROM orders\nWHERE ref LIKE 'a%b'", "inside it")
	if ie.Line != 4 || !strings.HasPrefix(ie.Error(), "line 4: ") {
		t.Fatalf("line %d: %v", ie.Line, ie)
	}
}
