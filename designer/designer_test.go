package designer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/sqlsplit"
	_ "modernc.org/sqlite"
)

// shop is the fixture schema, a small store:
//
//	customers ◄── orders ◄── order_lines ──► products
//	    ▲           ▲  └──► coupons (optional)
//	 tickets     payments
//	employees.manager_id ──► employees (optional, self)
//
// plus a view with no keys, and a table named with a reserved word.
func shop() *erd.Schema {
	s := &erd.Schema{Conn: "shop", Driver: "postgres"}
	tbl := func(name string, pk []string, cols ...string) *erd.Table {
		t := &erd.Table{Schema: "public", Name: name, Label: name, PK: pk}
		for _, c := range cols {
			nm, typ, _ := strings.Cut(c, " ")
			col := &erd.Column{Name: nm, Type: typ}
			if strings.HasSuffix(typ, "?") {
				col.Type, col.Nullable = strings.TrimSuffix(typ, "?"), true
			}
			col.PK = slices.Contains(pk, nm)
			t.Cols = append(t.Cols, col)
		}
		s.Tables = append(s.Tables, t)
		return t
	}
	customers := tbl("customers", []string{"id"}, "id integer", "name text", "country text", "email text?", "credit numeric")
	orders := tbl("orders", []string{"id"}, "id integer", "customer_id integer", "coupon_id integer?",
		"total numeric", "status text?", "created_at timestamp", "ref text")
	lines := tbl("order_lines", []string{"order_id", "line_no"}, "order_id integer", "line_no integer",
		"product_id integer", "qty integer", "amount numeric")
	products := tbl("products", []string{"id"}, "id integer", "name text", "price numeric")
	payments := tbl("payments", []string{"id"}, "id integer", "order_id integer", "amount numeric", "paid_on date")
	coupons := tbl("coupons", []string{"id"}, "id integer", "code text")
	coupons.Uniques = [][]string{{"code"}}
	coupons.Col("code").Unique = true
	tickets := tbl("tickets", []string{"id"}, "id integer", "customer_id integer", "subject text")
	employees := tbl("employees", []string{"id"}, "id integer", "manager_id integer?", "name text")
	v := tbl("big_spenders", nil, "customer_id integer", "spent numeric")
	v.View = true
	tbl("order", []string{"id"}, "id integer", "User Name text")

	rel := func(nm string, child *erd.Table, cc []string, parent *erd.Table, pc []string) {
		for _, c := range cc {
			child.Col(c).FK = true
		}
		s.Rels = append(s.Rels, &erd.Rel{Name: nm, Child: child, Parent: parent, ChildCols: cc, ParentCols: pc})
	}
	rel("orders_customer_id_fkey", orders, []string{"customer_id"}, customers, []string{"id"})
	rel("orders_coupon_id_fkey", orders, []string{"coupon_id"}, coupons, []string{"id"})
	rel("order_lines_order_id_fkey", lines, []string{"order_id"}, orders, []string{"id"})
	rel("order_lines_product_id_fkey", lines, []string{"product_id"}, products, []string{"id"})
	rel("payments_order_id_fkey", payments, []string{"order_id"}, orders, []string{"id"})
	rel("tickets_customer_id_fkey", tickets, []string{"customer_id"}, customers, []string{"id"})
	rel("employees_manager_id_fkey", employees, []string{"manager_id"}, employees, []string{"id"})
	return s
}

func shopCat() *Catalog { return &Catalog{Driver: "postgres", Schema: shop()} }

// build adds tables in order through AddTable, as the page does.
func build(t *testing.T, cat *Catalog, names ...string) Design {
	t.Helper()
	var d Design
	for i, n := range names {
		var err error
		d, _, err = AddTable(d, cat, "public", n, float64(i*200), 0)
		if err != nil {
			t.Fatalf("add %s: %v", n, err)
		}
	}
	return d
}

// id finds a table's ID by alias.
func id(d Design, alias string) string {
	for _, t := range d.Tables {
		if t.Alias == alias {
			return t.ID
		}
	}
	panic("no alias " + alias)
}

func col(d Design, alias, column string) Ref { return Ref{Table: id(d, alias), Column: column} }

func agg(d Design, a Agg, alias, column string) Ref {
	r := Ref{Agg: a, Column: column}
	if alias != "" {
		r.Table = id(d, alias)
	}
	return r
}

func outs(refs ...Ref) []Output {
	var o []Output
	for _, r := range refs {
		o = append(o, Output{Ref: r})
	}
	return o
}

func lintOf(a *Analysis, rule string) *Lint {
	for i := range a.Lints {
		if a.Lints[i].Rule == rule {
			return &a.Lints[i]
		}
	}
	return nil
}

func fixOf(t *testing.T, l *Lint, prefix string) Design {
	t.Helper()
	if l == nil {
		t.Fatal("no finding to fix")
	}
	for _, f := range l.Fixes {
		if strings.HasPrefix(f.Label, prefix) && f.Design != nil {
			return *f.Design
		}
	}
	t.Fatalf("no fix %q among %+v", prefix, l.Fixes)
	return Design{}
}

func wantSQL(t *testing.T, a *Analysis, want string) {
	t.Helper()
	if got := a.SQL; got != want {
		t.Fatalf("sql:\n%s\n\nwant:\n%s", got, want)
	}
}

func rules(a *Analysis) []string {
	var r []string
	for _, l := range a.Lints {
		r = append(r, l.Rule)
	}
	return r
}

func TestOneTable(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers")
	a := Analyze(d, cat)
	wantSQL(t, a, "SELECT *\nFROM customers c")
	if !a.Valid || a.Tables[0].Role != "main" || a.Story[0] != "Start from `customers` (`c`)." {
		t.Fatalf("valid %v role %s story %q", a.Valid, a.Tables[0].Role, a.Story)
	}
	if lintOf(a, "select-all") == nil {
		t.Fatalf("no select-all hint: %v", rules(a))
	}
}

func TestAddTableJoinsByForeignKey(t *testing.T) {
	cat := shopCat()
	var d Design
	d, _, _ = AddTable(d, cat, "public", "customers", 0, 0)
	d, notes, err := AddTable(d, cat, "public", "orders", 200, 0)
	if err != nil || len(d.Joins) != 1 {
		t.Fatalf("err %v joins %+v", err, d.Joins)
	}
	j := d.Joins[0]
	if j.Kind != JoinInner || j.FK != "orders_customer_id_fkey" || !slices.Equal(j.On, []Pair{{L: "id", R: "customer_id"}}) {
		t.Fatalf("join %+v", j)
	}
	if !strings.Contains(notes[0], "c.id = o.customer_id") {
		t.Fatalf("notes %q", notes)
	}
	a := Analyze(d, cat)
	wantSQL(t, a, "SELECT c.*, o.*\nFROM customers c\nJOIN orders o ON o.customer_id = c.id")
	if a.Joins[0].Card != "1:N" || !a.Joins[0].Fans {
		t.Fatalf("join view %+v", a.Joins[0])
	}
	if !strings.Contains(a.Story[1], "from here on there is a row per `o`") || !strings.Contains(a.Story[2], "one `orders` row") {
		t.Fatalf("story %q", a.Story)
	}

	// an optional key followed from the child is a left join, and says why
	d, notes, _ = AddTable(d, cat, "public", "coupons", 400, 0)
	if k := d.Joins[1].Kind; k != JoinLeft || !strings.Contains(notes[0], "left join") {
		t.Fatalf("coupons joined %s: %q", k, notes)
	}
	if _, _, err = AddTable(d, cat, "public", "nope", 0, 0); err != ErrNoSuchTable {
		t.Fatalf("unknown table: %v", err)
	}
}

func TestSelfJoinAndAliases(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "employees", "employees")
	if d.Tables[1].Alias != "e2" || d.Joins[0].Kind != JoinLeft {
		t.Fatalf("tables %+v joins %+v", d.Tables, d.Joins)
	}
	d.Columns = outs(col(d, "e", "name"), col(d, "e2", "name"))
	a := Analyze(d, cat)
	wantSQL(t, a, "SELECT e.name, e2.name AS e2_name\nFROM employees e\nLEFT JOIN employees e2 ON e2.id = e.manager_id")

	// a reserved initialism is numbered, a reserved or odd name quoted
	taken := map[string]bool{}
	if got := makeAlias("orders_new", taken); got != "on2" {
		t.Fatalf("alias %q", got)
	}
	d = build(t, cat, "order")
	d.Columns = outs(col(d, "o", "User Name"))
	wantSQL(t, Analyze(d, cat), `SELECT o."User Name"`+"\nFROM \"order\" o")
}

func TestFanoutAggregateAndFix(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders", "order_lines")
	d.Columns = outs(col(d, "o", "status"), agg(d, AggSum, "o", "total"))
	a := Analyze(d, cat)
	l := lintOf(a, "fanout-aggregate")
	if l == nil || l.Sev != SevWarn || !strings.Contains(l.Title, "sum(o.total)") {
		t.Fatalf("lints %v", rules(a))
	}
	// order_lines' columns are unused: test it instead
	fixed := Analyze(fixOf(t, l, "Only test ol"), cat)
	wantSQL(t, fixed, "SELECT o.status, sum(o.total) AS sum_total\nFROM orders o\nWHERE EXISTS (\n"+
		"    SELECT 1 FROM order_lines ol\n    WHERE ol.order_id = o.id\n)\nGROUP BY o.status")
	if lintOf(fixed, "fanout-aggregate") != nil || !fixed.Valid {
		t.Fatalf("after the fix: %v", rules(fixed))
	}
}

func TestSummaryJoin(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders", "payments")
	d.Columns = outs(col(d, "o", "id"), agg(d, AggSum, "p", "amount"), agg(d, AggCount, "p", ""))
	// before: payments repeat orders, but nothing is aggregated over orders
	// — fine; summarizing is the fix the fan-out rule offers when it is
	d.Joins[0].Kind = JoinSummary
	d.Filters = []Filter{{Ref: col(d, "p", "amount"), Op: OpGt, Values: []string{"0"}}}
	a := Analyze(d, cat)
	wantSQL(t, a, "SELECT o.id, p.sum_amount, COALESCE(p.count, 0) AS count_payments\nFROM orders o\nLEFT JOIN (\n"+
		"    SELECT p.order_id, sum(p.amount) AS sum_amount, count(*) AS count\n    FROM payments p\n"+
		"    WHERE p.amount > 0\n    GROUP BY p.order_id\n) p ON p.order_id = o.id")
	if a.Grouped || !a.Valid {
		t.Fatalf("grouped %v valid %v %v", a.Grouped, a.Valid, rules(a))
	}

	// grouped outside too: the summaries are re-aggregated, avg from its parts
	d.Columns = outs(col(d, "o", "status"), agg(d, AggAvg, "p", "amount"), agg(d, AggCount, "p", ""))
	d.Filters = nil
	d.Columns = append(d.Columns, Output{Ref: agg(d, AggCount, "", "")})
	a = Analyze(d, cat)
	wantSQL(t, a, "SELECT\n    o.status,\n    sum(p.sum_amount) * 1.0 / NULLIF(sum(p.count_amount), 0) AS avg_amount,\n"+
		"    COALESCE(sum(p.count), 0) AS count_payments,\n    count(*) AS count\nFROM orders o\nLEFT JOIN (\n"+
		"    SELECT p.order_id, sum(p.amount) AS sum_amount, count(p.amount) AS count_amount, count(*) AS count\n"+
		"    FROM payments p\n    GROUP BY p.order_id\n) p ON p.order_id = o.id\nGROUP BY o.status")

	// a plain column of a summarized table cannot be shown
	d.Columns = outs(col(d, "p", "amount"))
	if a = Analyze(d, cat); a.Valid || lintOf(a, "invalid") == nil {
		t.Fatalf("plain summary column: %v", rules(a))
	}
}

func TestChasmTrap(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders", "tickets")
	d.Columns = outs(col(d, "c", "name"), col(d, "o", "id"), col(d, "t", "subject"))
	a := Analyze(d, cat)
	l := lintOf(a, "chasm")
	if l == nil || !strings.Contains(l.Title, "o and t") {
		t.Fatalf("lints %v", rules(a))
	}
	if !strings.Contains(strings.Join(a.Story, "\n"), "a combination of rows") {
		t.Fatalf("story %q", a.Story)
	}
}

func TestLeftJoinFilter(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	d.Joins[0].Kind = JoinLeft
	d.Columns = outs(col(d, "c", "name"), col(d, "o", "total"))
	d.Filters = []Filter{{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"paid"}}}
	a := Analyze(d, cat)
	l := lintOf(a, "left-filter")
	if l == nil || l.Filter != 0 {
		t.Fatalf("lints %v", rules(a))
	}
	moved := Analyze(fixOf(t, l, "Move the filter"), cat)
	wantSQL(t, moved, "SELECT c.name, o.total\nFROM customers c\nLEFT JOIN orders o ON o.customer_id = c.id AND o.status = 'paid'")
	if lintOf(moved, "left-filter") != nil {
		t.Fatalf("still: %v", rules(moved))
	}
	inner := Analyze(fixOf(t, l, "Make it an inner"), cat)
	if inner.Design.Joins[0].Kind != JoinInner || lintOf(inner, "left-filter") != nil {
		t.Fatalf("inner fix: %+v %v", inner.Design.Joins, rules(inner))
	}

	// LEFT JOIN … IS NULL is "has none"
	d.Columns = outs(col(d, "c", "name"))
	d.Filters = []Filter{{Ref: col(d, "o", "id"), Op: OpNull}}
	a = Analyze(d, cat)
	anti := Analyze(fixOf(t, lintOf(a, "anti-join"), "Use has none"), cat)
	wantSQL(t, anti, "SELECT c.name\nFROM customers c\nWHERE NOT EXISTS (\n    SELECT 1 FROM orders o\n    WHERE o.customer_id = c.id\n)")
}

func TestFiltersAreWrittenSafely(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	d.Filters = []Filter{
		{Ref: col(d, "o", "created_at"), Op: OpYear, Values: []string{"2024"}},
		{Ref: col(d, "o", "ref"), Op: OpContains, Values: []string{"50%_o'k"}},
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"a"}, Group: "g"},
		{Ref: col(d, "o", "total"), Op: OpBetween, Values: []string{"1", "2"}, Group: "g"},
		{Ref: col(d, "o", "status"), Op: OpNe, Values: []string{"void"}, OrNull: true},
		{Ref: col(d, "o", "created_at"), Op: OpOn, Values: []string{"2024-02-29"}},
		{Ref: col(d, "o", "customer_id"), Op: OpIn, Values: []string{"1", "2", "3"}},
	}
	a := Analyze(d, cat)
	wantSQL(t, a, "SELECT o.id\nFROM orders o\nWHERE o.created_at >= '2024-01-01' AND o.created_at < '2025-01-01'\n"+
		`  AND o.ref LIKE '%50\%\_o''k%'`+"\n"+
		"  AND (o.status = 'a' OR (o.total BETWEEN 1 AND 2))\n"+
		"  AND (o.status <> 'void' OR o.status IS NULL)\n"+
		"  AND o.created_at >= '2024-02-29' AND o.created_at < '2024-03-01'\n"+
		"  AND o.customer_id IN (1, 2, 3)")
	if !a.Valid {
		t.Fatalf("lints %v", rules(a))
	}

	// MySQL: backticks, doubled backslashes, its own date arithmetic
	cat.Driver = "mysql"
	d.Filters = []Filter{
		{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{`a\b`}},
		{Ref: col(d, "o", "created_at"), Op: OpLastDays, Values: []string{"7"}},
	}
	d.Offset = 5
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE o.ref LIKE 'a\\\\\\\\b%' ESCAPE '\\\\'\n"+
		"  AND o.created_at >= NOW() - INTERVAL 7 DAY\nLIMIT 18446744073709551615\nOFFSET 5")

	// bytdb has no date arithmetic to rely on: the instant itself
	cat.Driver = "bytdb"
	now = func() time.Time { return time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	d.Offset = 0
	d.Filters = []Filter{{Ref: col(d, "o", "created_at"), Op: OpLastDays, Values: []string{"7"}}}
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE o.created_at >= '2026-10-21 09:30:00'")
}

// TestLikeEscapeScans: each engine's escaped LIKE, exactly, and the
// statement still ending where it does for sqlsplit's scanner. That scanner
// once honored \' in every dialect, so an ESCAPE '\' on Postgres or SQLite
// read as an open string and ran on into the next statement: Split saw one
// where there were two. It reads each engine's rule now (N-238), and the
// split here is the engine's, as the editor's and the run's are; the
// Generic split is checked too, since the LIKEs were written to survive it.
func TestLikeEscapeScans(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	d.Filters = []Filter{
		{Ref: col(d, "o", "ref"), Op: OpContains, Values: []string{`5!0%_\`}},
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"a"}},
	}
	for _, c := range []struct{ driver, like string }{
		{"postgres", `o.ref LIKE '%5!0\%\_\\%'`},
		{"bytdb", `o.ref LIKE '%5!0\%\_\\%'`},
		{"mysql", `o.ref LIKE '%5!0\\%\\_\\\\%' ESCAPE '\\'`},
		{"sqlite", `o.ref LIKE '%5!!0!%!_\%' ESCAPE '!'`},
	} {
		cat.Driver = c.driver
		sql := Analyze(d, cat).SQL
		if want := "WHERE " + c.like + "\n  AND o.status = 'a'"; !strings.HasSuffix(sql, want) {
			t.Errorf("%s:\n%s\n\nwant it to end\n%s", c.driver, sql, want)
		}
		for _, sd := range []sqlsplit.Dialect{sqlsplit.DialectOf(c.driver), sqlsplit.Generic} {
			stmts := sd.Split(sql + ";\nSELECT 2")
			if len(stmts) != 2 || stmts[0].Text != sql || stmts[1].Text != "SELECT 2" {
				t.Errorf("%s (%v split): split as %d statements: %+v", c.driver, sd, len(stmts), stmts)
			}
		}
	}

	// the escape character alone is escaped only where a clause names it:
	// SQLite's ! is plain text without ESCAPE '!', and so is its \
	cat.Driver = "sqlite"
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{`a!\b`}}}
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE o.ref LIKE 'a!!\\b%' ESCAPE '!'")
	d.Filters[0].Values = []string{`a\b`}
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE o.ref LIKE 'a\\b%'")
}

// TestBackslashValuesSplit is N-238's own cases: a value ending in a
// backslash is written as a literal ending in one — `= 'a\'` on every
// engine but MySQL (which doubles it), and on SQLite an ends-with's
// `LIKE '%a\' ESCAPE '!'` too. The engine reads each whole, and so does its
// dialect's split: the statement after it is one of its own, where the
// Generic split (MySQL's rule) ran the literal on into it. Opening such a
// buffer as a design sees the two statements, as running it does.
func TestBackslashValuesSplit(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	for _, c := range []struct{ driver, op, value, where string }{
		{"postgres", OpEq, `a\`, `o.ref = 'a\'`},
		{"bytdb", OpEq, `a\`, `o.ref = 'a\'`},
		{"sqlite", OpEq, `a\`, `o.ref = 'a\'`},
		{"mysql", OpEq, `a\`, `o.ref = 'a\\'`},
		// SQLite's ends-with, with an ESCAPE clause (the value has a %) and
		// without one (it has nothing to escape)
		{"sqlite", OpEnds, `5%\`, `o.ref LIKE '%5!%\' ESCAPE '!'`},
		{"sqlite", OpEnds, `a\`, `o.ref LIKE '%a\'`},
	} {
		cat.Driver = c.driver
		d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: c.op, Values: []string{c.value}}}
		sql := Analyze(d, cat).SQL
		if want := "WHERE " + c.where; !strings.HasSuffix(sql, want) {
			t.Errorf("%s %s:\n%s\n\nwant it to end\n%s", c.driver, c.op, sql, want)
		}
		stmts := sqlsplit.DialectOf(c.driver).Split(sql + ";\nSELECT 2")
		if len(stmts) != 2 || stmts[0].Text != sql || stmts[1].Text != "SELECT 2" {
			t.Errorf("%s %s: split as %d statements: %+v", c.driver, c.op, len(stmts), stmts)
		}
	}

	cat.Driver = "postgres"
	_, _, err := Import("SELECT o.id FROM orders o WHERE o.ref = 'a\\';\nSELECT 2", cat)
	var ie *ImportError
	if !errors.As(err, &ie) || !strings.HasPrefix(ie.Reason, "more than one statement") || ie.Line != 2 {
		t.Errorf("Import of two statements on postgres: %v, want more than one statement at line 2", err)
	}
}

// TestLikeEscapeRunsOnSQLite: SQLite itself runs the patterns written with
// ESCAPE '!', and each matches its value literally — % and _ as
// themselves, the ! escape and the \ (no escape there) too — the rows Go's
// own contains, starts with and ends with pick.
func TestLikeEscapeRunsOnSQLite(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// one connection: each :memory: connection is a database of its own
	db.SetMaxOpenConns(1)
	refs := []string{`50%`, `50x`, `a_b`, `axb`, `x!y`, `x!!y`, `xy`, `p\q`, `p\\q`, `!%`, `%!`, `\`, `ab\`, `ab`}
	if _, err := db.Exec(`CREATE TABLE orders (id INTEGER PRIMARY KEY, ref TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range refs {
		if _, err := db.Exec(`INSERT INTO orders VALUES (?, ?)`, i, r); err != nil {
			t.Fatal(err)
		}
	}

	cat := shopCat()
	cat.Driver = "sqlite"
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	matches := map[string]func(s, v string) bool{
		OpContains: strings.Contains, OpStarts: strings.HasPrefix, OpEnds: strings.HasSuffix,
	}
	for _, v := range []string{`50%`, `_`, `!`, `!!`, `\`, `!%`, `b\`, `x!y`} {
		for op, match := range matches {
			for _, icase := range []bool{false, true} {
				d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: op, Values: []string{v}, IgnoreCase: icase}}
				q := Analyze(d, cat).SQL
				rows, err := db.Query(q)
				if err != nil {
					t.Fatalf("%s\n%s", err, q)
				}
				var got, want []int
				for rows.Next() {
					var id int
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					got = append(got, id)
				}
				rows.Close()
				slices.Sort(got)
				for i, r := range refs {
					if match(r, v) {
						want = append(want, i)
					}
				}
				if !slices.Equal(got, want) {
					t.Errorf("%s %q: rows %v, want %v\n%s", op, v, got, want, q)
				}
			}
		}
	}
}

// TestIgnoreCase: contains, starts with and ends with can ignore case,
// written in each engine's own way, and the index rules follow — an
// ordinary index on the column no longer serves a starts-with.
func TestIgnoreCase(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	d.Filters = []Filter{
		{Ref: col(d, "o", "ref"), Op: OpContains, Values: []string{"50%"}, IgnoreCase: true},
		{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{"AB"}, IgnoreCase: true},
		// not a LIKE: the flag has nothing to do
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"Open"}, IgnoreCase: true},
	}
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE o.ref ILIKE '%50\\%%'\n"+
		"  AND o.ref ILIKE 'AB%'\n  AND o.status = 'Open'")
	cat.Driver = "bytdb"
	if a := Analyze(d, cat); !strings.Contains(a.SQL, "o.ref ILIKE 'AB%'") {
		t.Fatalf("bytdb: %s", a.SQL)
	}
	// MySQL and SQLite: both sides lowered by the engine, the escape kept
	cat.Driver = "mysql"
	wantSQL(t, Analyze(d, cat), "SELECT o.id\nFROM orders o\nWHERE lower(o.ref) LIKE lower('%50\\\\%%') ESCAPE '\\\\'\n"+
		"  AND lower(o.ref) LIKE lower('AB%')\n  AND o.status = 'Open'")
	cat.Driver = "sqlite"
	a := Analyze(d, cat)
	if !strings.Contains(a.SQL, "lower(o.ref) LIKE lower('AB%')") {
		t.Fatalf("sqlite: %s", a.SQL)
	}
	if !slices.ContainsFunc(a.Story, func(s string) bool { return strings.Contains(s, "starts with `AB`, ignoring the case of A–Z") }) {
		t.Fatalf("sqlite story: %q", a.Story)
	}

	// the rules, on a big Postgres table with an index on ref — a pattern
	// one: a plain b-tree finds no prefix in Postgres's usual collation
	// (TestPrefixIndex)
	cat.Driver, cat.IndexesKnown = "postgres", true
	cat.Rows = map[TableKey]int64{{"public", "orders"}: 2_000_000}
	cat.Indexes = map[TableKey][]Index{{"public", "orders"}: {{Name: "o_ref", Cols: []string{"ref"}, Order: []string{orderPattern}}}}
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{"AB"}}}
	if a = Analyze(d, cat); lintOf(a, "ignore-case-like") != nil || lintOf(a, "filter-no-index") != nil {
		t.Fatalf("plain starts with, indexed: %v", rules(a))
	}
	d.Filters[0].IgnoreCase = true
	a = Analyze(d, cat)
	l := lintOf(a, "ignore-case-like")
	if l == nil || !strings.Contains(l.Fixes[0].SQL, "gin_trgm_ops") || lintOf(a, "filter-no-index") != nil {
		t.Fatalf("folded starts with: %v %+v", rules(a), l)
	}
	if back := fixOf(t, l, "Match case"); back.Filters[0].IgnoreCase {
		t.Fatal("the fix still ignores case")
	}
	// a GIN index on the column is taken for a trigram one: quiet, and
	// for contains too
	cat.Indexes[TableKey{"public", "orders"}] = append(cat.Indexes[TableKey{"public", "orders"}],
		Index{Name: "o_ref_trgm", Cols: []string{"ref"}, Method: "gin"})
	if a = Analyze(d, cat); lintOf(a, "ignore-case-like") != nil {
		t.Fatalf("with a trigram index: %v", rules(a))
	}
	d.Filters[0].Op = OpContains
	if a = Analyze(d, cat); lintOf(a, "leading-wildcard") != nil {
		t.Fatalf("contains with a trigram index: %v", rules(a))
	}

	// MySQL: the functional index, and silence beside an expression index
	// the listing cannot name
	cat.Driver = "mysql"
	cat.Indexes = map[TableKey][]Index{{"public", "orders"}: {{Name: "o_ref", Cols: []string{"ref"}}}}
	d.Filters[0].Op = OpStarts
	l = lintOf(Analyze(d, cat), "ignore-case-like")
	if l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_ref_lower_idx ON orders ((lower(ref)));" || !strings.Contains(l.Why, "…_ci") {
		t.Fatalf("mysql: %+v", l)
	}
	cat.Indexes[TableKey{"public", "orders"}] = append(cat.Indexes[TableKey{"public", "orders"}], Index{Name: "o_lower", Cols: []string{""}})
	if a = Analyze(d, cat); lintOf(a, "ignore-case-like") != nil {
		t.Fatalf("mysql with an expression index: %v", rules(a))
	}
}

// On Postgres a b-tree serves a starts-with only in byte order: a pattern
// class (text_pattern_ops), or a C-ordered collation. In any other — the
// usual en_US.UTF-8 — a plain index on the column finds = and ranges but
// no prefix, so the rule speaks and suggests a pattern index; and a
// pattern index, in turn, finds no range and serves no sort.
func TestPrefixIndex(t *testing.T) {
	cat := shopCat()
	cat.IndexesKnown = true
	cat.Rows = map[TableKey]int64{{"public", "orders"}: 2_000_000}
	k := TableKey{"public", "orders"}
	d := build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{"AB"}}}
	refCol := func(a *Analysis) ColView {
		t.Helper()
		for _, c := range a.Tables[0].Cols {
			if c.Name == "ref" {
				return c
			}
		}
		t.Fatal("no ref column")
		return ColView{}
	}

	// a plain index in a linguistic collation: it starts with ref, but
	// cannot find the prefix
	cat.Indexes = map[TableKey][]Index{k: {{Name: "o_ref", Cols: []string{"ref"}}}}
	a := Analyze(d, cat)
	l := lintOf(a, "filter-no-index")
	if l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_ref_idx ON orders (ref text_pattern_ops);" {
		t.Fatalf("plain index: %v %+v", rules(a), l)
	}
	for _, w := range []string{"o_ref, which starts with ref, cannot find a starts-with: it keeps the collation's order",
		"An index on (ref text_pattern_ops) lets",
		"not <, BETWEEN or a sort", "CONCURRENTLY"} {
		if !strings.Contains(l.Why, w) {
			t.Errorf("why lacks %q: %s", w, l.Why)
		}
	}
	if c := refCol(a); !c.Indexed || !c.NoPrefix || c.NoRange {
		t.Fatalf("plain index card: %+v", c)
	}
	// the fix's index, as the listing reads it back, quiets the rule
	cat.Indexes[k] = append(cat.Indexes[k], Index{Name: "orders_ref_idx", Cols: []string{"ref"}, Order: []string{orderPattern}})
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil {
		t.Fatalf("with the pattern index: %v", rules(a))
	}
	if c := refCol(a); c.NoPrefix || c.NoRange {
		t.Fatalf("plain and pattern card: %+v", c)
	}
	// under a C collation the plain index finds the prefix itself
	cat.Indexes[k] = []Index{{Name: "o_ref", Cols: []string{"ref"}, Order: []string{orderC}}}
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil {
		t.Fatalf("C collation: %v", rules(a))
	}
	if c := refCol(a); c.NoPrefix || c.NoRange {
		t.Fatalf("C collation card: %+v", c)
	}
	// so does a trigram (GIN) or an SP-GiST index; a hash one does not
	for _, m := range []string{"gin", "spgist"} {
		cat.Indexes[k] = []Index{{Name: "o_ref_x", Cols: []string{"ref"}, Method: m}}
		if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil {
			t.Fatalf("%s: %v", m, rules(a))
		}
	}
	cat.Indexes[k] = []Index{{Name: "o_ref_h", Cols: []string{"ref"}, Method: "hash"}}
	a = Analyze(d, cat)
	if l = lintOf(a, "filter-no-index"); l == nil || !strings.Contains(l.Why, "o_ref_h, which starts with ref, is a hash index") {
		t.Fatalf("hash: %v %+v", rules(a), l)
	}
	if c := refCol(a); !c.Indexed || !c.NoPrefix || !c.NoRange {
		t.Fatalf("hash card: %+v", c)
	}

	// an equality goes first, the starts-with last in its pattern class
	cat.Indexes[k] = nil
	d.Filters = append(d.Filters, Filter{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"open"}})
	l = lintOf(Analyze(d, cat), "filter-no-index")
	if l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_status_ref_idx ON orders (status, ref text_pattern_ops);" ||
		!strings.Contains(l.Why, "equalities first, then the starts-with") {
		t.Fatalf("status and prefix: %+v", l)
	}
	// a range on the same column wants the collation's order: plain
	d.Filters = []Filter{d.Filters[0], {Ref: col(d, "o", "ref"), Op: OpLt, Values: []string{"M"}}}
	if l = lintOf(Analyze(d, cat), "filter-no-index"); l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_ref_idx ON orders (ref);" {
		t.Fatalf("prefix and range: %+v", l)
	}

	// a pattern index finds no range, and serves no sort
	cat.Indexes[k] = []Index{{Name: "o_ref_p", Cols: []string{"ref"}, Order: []string{orderPattern}}}
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpGe, Values: []string{"M"}}}
	a = Analyze(d, cat)
	if l = lintOf(a, "filter-no-index"); l == nil || !strings.Contains(l.Why, "o_ref_p, which starts with ref, cannot find a range") ||
		l.Fixes[0].SQL != "CREATE INDEX orders_ref_idx ON orders (ref);" {
		t.Fatalf("range on a pattern index: %v %+v", rules(a), l)
	}
	if c := refCol(a); !c.Indexed || c.NoPrefix || !c.NoRange {
		t.Fatalf("pattern card: %+v", c)
	}
	d.Filters = nil
	d.Sorts = []Sort{{Ref: col(d, "o", "ref")}}
	d.Limit = 20
	if a = Analyze(d, cat); lintOf(a, "sort-no-index") == nil {
		t.Fatalf("sort on a pattern index: %v", rules(a))
	}
	cat.Indexes[k][0].Order = nil
	if a = Analyze(d, cat); lintOf(a, "sort-no-index") != nil {
		t.Fatalf("sort on a plain index: %v", rules(a))
	}

	// MySQL's b-tree finds a prefix in any collation: as before
	cat.Driver = "mysql"
	d.Sorts, d.Limit = nil, 0
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpStarts, Values: []string{"AB"}}}
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil {
		t.Fatalf("mysql: %v", rules(a))
	}
	if c := refCol(a); !c.Indexed || c.NoPrefix || c.NoRange {
		t.Fatalf("mysql card: %+v", c)
	}
	cat.Indexes[k] = nil
	if l = lintOf(Analyze(d, cat), "filter-no-index"); l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_ref_idx ON orders (ref);" {
		t.Fatalf("mysql fix: %+v", l)
	}

	// char(n) takes its own pattern class
	for typ, want := range map[string]string{"text": "text_pattern_ops", "character varying(40)": "text_pattern_ops",
		"character(3)": "bpchar_pattern_ops", "bpchar": "bpchar_pattern_ops", "CHAR(2)": "bpchar_pattern_ops"} {
		if got := patternOps(typ); got != want {
			t.Errorf("patternOps(%q) = %s, want %s", typ, got, want)
		}
	}
}

func TestBadValues(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	for _, f := range []Filter{
		{Ref: col(d, "o", "total"), Op: OpGt, Values: []string{"lots"}},
		{Ref: col(d, "o", "created_at"), Op: OpGe, Values: []string{"yesterday"}},
		{Ref: col(d, "o", "created_at"), Op: OpYear, Values: []string{"24x"}},
		{Ref: col(d, "o", "id"), Op: OpIn},
		{Ref: col(d, "o", "nope"), Op: OpEq, Values: []string{"1"}},
	} {
		d.Filters = []Filter{f}
		a := Analyze(d, cat)
		l := lintOf(a, "invalid")
		if a.Valid || l == nil || l.Filter != 0 || len(l.Fixes) != 1 {
			t.Fatalf("%+v: valid %v %v", f, a.Valid, rules(a))
		}
	}
}

func TestGroupingByConstruction(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	d.Columns = outs(col(d, "c", "country"), agg(d, AggCount, "c", ""), agg(d, AggSum, "o", "total"))
	d.Filters = []Filter{
		{Ref: agg(d, AggCount, "", ""), Op: OpGt, Values: []string{"5"}},
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"paid"}},
	}
	d.Sorts = []Sort{{Ref: agg(d, AggSum, "o", "total"), Desc: true}}
	d.Limit = 10
	a := Analyze(d, cat)
	// customers are repeated by their orders: counted DISTINCT
	wantSQL(t, a, "SELECT\n    c.country,\n    count(DISTINCT c.id) AS count_customers,\n    sum(o.total) AS sum_total\n"+
		"FROM customers c\nJOIN orders o ON o.customer_id = c.id\nWHERE o.status = 'paid'\nGROUP BY c.country\n"+
		"HAVING count(*) > 5\nORDER BY sum(o.total) DESC\nLIMIT 10")
	if !a.Valid || lintOf(a, "fanout-aggregate") != nil {
		t.Fatalf("lints %v", rules(a))
	}
	if !strings.Contains(strings.Join(a.Story, "\n"), "counts each once") {
		t.Fatalf("story %q", a.Story)
	}

	// a left-joined table's rows are counted by its key (count(*) counts
	// the empty match as one)
	d.Joins[0].Kind = JoinLeft
	d.Columns = outs(col(d, "c", "name"), agg(d, AggCount, "o", ""))
	d.Filters, d.Sorts, d.Limit = nil, nil, 0
	wantSQL(t, Analyze(d, cat), "SELECT c.name, count(o.id) AS count_orders\nFROM customers c\n"+
		"LEFT JOIN orders o ON o.customer_id = c.id\nGROUP BY c.name")

	// a sort the grouping cannot answer is refused, with a way out
	d.Sorts = []Sort{{Ref: col(d, "o", "created_at")}}
	a = Analyze(d, cat)
	l := lintOf(a, "invalid")
	if a.Valid || l == nil || l.Sort != 0 {
		t.Fatalf("sort not grouped: %v", rules(a))
	}
	if fixed := Analyze(fixOf(t, l, "Show it"), cat); !fixed.Valid {
		t.Fatalf("after showing it: %v", rules(fixed))
	}
}

func TestCartesianFindsAPath(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "products")
	d.Columns = outs(col(d, "c", "name"), col(d, "p", "name"))
	a := Analyze(d, cat)
	l := lintOf(a, "cartesian")
	if l == nil || !strings.Contains(a.SQL, "CROSS JOIN products p") {
		t.Fatalf("lints %v sql %s", rules(a), a.SQL)
	}
	if l.Fixes[0].Label != "Join through orders, order_lines" {
		t.Fatalf("fixes %+v", l.Fixes)
	}
	fixed := Analyze(fixOf(t, l, "Join through"), cat)
	wantSQL(t, fixed, "SELECT c.name, p.name AS p_name\nFROM customers c\nJOIN orders o ON o.customer_id = c.id\n"+
		"JOIN order_lines ol ON ol.order_id = o.id\nJOIN products p ON p.id = ol.product_id")
	if lintOf(fixed, "cartesian") != nil {
		t.Fatalf("still: %v", rules(fixed))
	}
}

func TestPerformanceRules(t *testing.T) {
	cat := shopCat()
	cat.IndexesKnown = true
	cat.Rows = map[TableKey]int64{{"public", "orders"}: 2_000_000, {"public", "customers"}: 50_000}
	d := build(t, cat, "customers", "orders")
	d.Columns = outs(col(d, "c", "name"), col(d, "o", "total"))
	d.Filters = []Filter{
		{Ref: col(d, "c", "country"), Op: OpEq, Values: []string{"NZ"}},
		{Ref: col(d, "o", "status"), Op: OpEq, Values: []string{"open"}},
		{Ref: col(d, "o", "created_at"), Op: OpYear, Values: []string{"2025"}},
	}
	a := Analyze(d, cat)
	var idx []string
	for _, l := range a.Lints {
		if l.Rule == "filter-no-index" || l.Rule == "join-no-index" {
			idx = append(idx, l.Rule+" "+l.Fixes[0].SQL)
		}
	}
	want := []string{
		"filter-no-index CREATE INDEX customers_country_idx ON customers (country);",
		"filter-no-index CREATE INDEX orders_status_created_at_idx ON orders (status, created_at);",
		"join-no-index CREATE INDEX orders_customer_id_idx ON orders (customer_id);",
	}
	if !slices.Equal(idx, want) {
		t.Fatalf("index findings:\n%s", strings.Join(idx, "\n"))
	}
	if l := lintOf(a, "join-no-index"); l.Sev != SevWarn || !strings.Contains(l.Why, "does not index a foreign key") {
		t.Fatalf("join finding %+v", l)
	}

	// with the indexes, silence
	cat.Indexes = map[TableKey][]Index{
		{"public", "orders"}:    {{Name: "o_cust", Cols: []string{"customer_id"}}, {Name: "o_st", Cols: []string{"status", "created_at"}}},
		{"public", "customers"}: {{Name: "c_country", Cols: []string{"country"}}},
	}
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil || lintOf(a, "join-no-index") != nil {
		t.Fatalf("with indexes: %v", rules(a))
	}
	// a partial index serves nothing in general
	cat.Indexes[TableKey{"public", "customers"}][0].Partial = true
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") == nil {
		t.Fatalf("partial index counted: %v", rules(a))
	}
	// no indexes known: no index findings at all (evidence or silence)
	cat.IndexesKnown, cat.Indexes = false, nil
	if a = Analyze(d, cat); lintOf(a, "filter-no-index") != nil || lintOf(a, "join-no-index") != nil {
		t.Fatalf("unknown indexes: %v", rules(a))
	}

	// top-N without an index in that order, and without a tie-breaker
	cat.IndexesKnown = true
	d = build(t, cat, "orders")
	d.Columns = outs(col(d, "o", "id"))
	d.Sorts = []Sort{{Ref: col(d, "o", "created_at"), Desc: true}}
	d.Limit = 20
	a = Analyze(d, cat)
	if l := lintOf(a, "sort-no-index"); l == nil || l.Fixes[0].SQL != "CREATE INDEX orders_created_at_idx ON orders (created_at);" {
		t.Fatalf("sort: %v", rules(a))
	}
	tie := Analyze(fixOf(t, lintOf(a, "order-not-unique"), "Break ties"), cat)
	if !strings.HasSuffix(tie.SQL, "ORDER BY o.created_at DESC, o.id\nLIMIT 20") {
		t.Fatalf("tie-break: %s", tie.SQL)
	}

	// contains on a big table, with Postgres's trigram index
	d.Filters = []Filter{{Ref: col(d, "o", "ref"), Op: OpContains, Values: []string{"x"}}}
	if l := lintOf(Analyze(d, cat), "leading-wildcard"); l == nil || !strings.Contains(l.Fixes[0].SQL, "gin_trgm_ops") {
		t.Fatalf("contains: %+v", l)
	}
	// unbounded
	d.Filters, d.Sorts, d.Limit = nil, nil, 0
	if a = Analyze(d, cat); lintOf(a, "unbounded") == nil {
		t.Fatalf("unbounded: %v", rules(a))
	}
}

func TestLimitWithoutOrder(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "orders")
	d.Limit = 5
	a := Analyze(d, cat)
	fixed := Analyze(fixOf(t, lintOf(a, "limit-no-order"), "Sort by o.id"), cat)
	wantSQL(t, fixed, "SELECT *\nFROM orders o\nORDER BY o.id\nLIMIT 5")
}

func TestUnusedFanoutAndDistinct(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	d.Columns = outs(col(d, "c", "name"))
	d.Filters = []Filter{{Ref: col(d, "o", "total"), Op: OpGt, Values: []string{"100"}}}
	a := Analyze(d, cat)
	fixed := Analyze(fixOf(t, lintOf(a, "fanout-unused"), "Only test o"), cat)
	wantSQL(t, fixed, "SELECT c.name\nFROM customers c\nWHERE EXISTS (\n    SELECT 1 FROM orders o\n"+
		"    WHERE o.customer_id = c.id\n      AND o.total > 100\n)")
	if !strings.Contains(fixed.Story[1], "where `o.total` is more than `100`") {
		t.Fatalf("story %q", fixed.Story)
	}

	d.Distinct = true
	if a = Analyze(d, cat); lintOf(a, "distinct-fanout") == nil || lintOf(a, "fanout-unused") != nil {
		t.Fatalf("distinct: %v", rules(a))
	}
	// DISTINCT with a sort on a column it does not show
	d.Sorts = []Sort{{Ref: col(d, "c", "country")}}
	if a = Analyze(d, cat); a.Valid {
		t.Fatalf("distinct sort accepted: %v", rules(a))
	}
}

func TestOtherCorrectnessRules(t *testing.T) {
	cat := shopCat()
	// a timestamp BETWEEN two days misses the last one
	d := build(t, cat, "orders")
	d.Filters = []Filter{{Ref: col(d, "o", "created_at"), Op: OpBetween, Values: []string{"2024-01-01", "2024-01-31"}}}
	a := Analyze(d, cat)
	r := Analyze(fixOf(t, lintOf(a, "between-day"), "Include all"), cat)
	if !strings.Contains(r.SQL, "o.created_at >= '2024-01-01' AND o.created_at < '2024-02-01'") {
		t.Fatalf("range fix: %s", r.SQL)
	}
	// <> drops NULLs
	d.Filters = []Filter{{Ref: col(d, "o", "status"), Op: OpNe, Values: []string{"void"}}}
	if l := lintOf(Analyze(d, cat), "null-dropped"); l == nil || !fixOf(t, l, "Keep").Filters[0].OrNull {
		t.Fatalf("null drop: %+v", l)
	}
	// a full join on MySQL; a join on no key; a type mismatch; a guess
	d = build(t, cat, "customers", "orders")
	d.Joins[0].Kind = JoinFull
	cat.Driver = "mysql"
	if a = Analyze(d, cat); lintOf(a, "full-join") == nil || a.Valid {
		t.Fatalf("mysql full join: %v", rules(a))
	}
	cat.Driver = "postgres"
	d.Joins[0] = Join{ID: "j1", Left: id(d, "c"), Right: id(d, "o"), Kind: JoinInner, On: []Pair{{L: "name", R: "ref"}}}
	if a = Analyze(d, cat); lintOf(a, "many-to-many") == nil {
		t.Fatalf("many-to-many: %v", rules(a))
	}
	d.Joins[0].On = []Pair{{L: "name", R: "total"}}
	if a = Analyze(d, cat); lintOf(a, "type-mismatch") == nil {
		t.Fatalf("type mismatch: %v", rules(a))
	}
	d.Joins[0].On = nil
	a = Analyze(d, cat)
	nc := lintOf(a, "join-no-condition")
	if a.Valid || nc == nil || nc.Fixes[0].Label != "Match on c.id = o.customer_id" {
		t.Fatalf("no condition: %+v", nc)
	}
	// an EXISTS table cannot be shown, nor joined on
	d = build(t, cat, "customers", "orders", "order_lines")
	d.Joins[0].Kind = JoinExists
	if a = Analyze(d, cat); lintOf(a, "subquery-leaf") == nil {
		t.Fatalf("exists not a leaf: %v", rules(a))
	}
	// a table the catalog lacks
	d.Tables = append(d.Tables, Table{ID: "zz", Schema: "public", Name: "gone"})
	if a = Analyze(d, cat); lintOf(a, "missing-table") == nil {
		t.Fatalf("missing: %v", rules(a))
	}
}

func TestRightJoinSuggestsTheKeptTable(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	// keep every order: the left side of the join is orders, but the query
	// starts from customers
	d.Joins[0] = Join{ID: "j1", Left: id(d, "o"), Right: id(d, "c"), Kind: JoinLeft, On: []Pair{{L: "customer_id", R: "id"}}}
	a := Analyze(d, cat)
	if !strings.Contains(a.SQL, "RIGHT JOIN orders o") {
		t.Fatalf("sql %s", a.SQL)
	}
	fixed := Analyze(fixOf(t, lintOf(a, "right-join"), "Start from o"), cat)
	if !strings.Contains(fixed.SQL, "FROM orders o\nLEFT JOIN customers c ON c.id = o.customer_id") {
		t.Fatalf("sql %s", fixed.SQL)
	}
}

func TestDesignRoundTripsAsJSON(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	d.Columns = outs(col(d, "c", "name"))
	a := Analyze(d, cat)
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var back struct{ Design Design }
	if err = json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if again := Analyze(back.Design, cat); again.SQL != a.SQL {
		t.Fatalf("round trip:\n%s\n%s", again.SQL, a.SQL)
	}
	// views for the page: a card's flags
	if c := a.Tables[0]; c.Cols[0].PK != true || !c.Cols[0].Indexed || c.Repeated != true || a.Tables[1].Grain != true {
		t.Fatalf("cards %+v", a.Tables)
	}
}

func TestPaintPlan(t *testing.T) {
	cat := shopCat()
	d := build(t, cat, "customers", "orders")
	a := Analyze(d, cat)
	p := &explain.Plan{Engine: explain.Postgres, Root: &explain.Node{Op: "Hash Join", HasEst: true, EstRows: 900, HasCost: true, TotalCost: 410,
		Children: []*explain.Node{
			{Op: "Seq Scan", Relation: "orders", Alias: "o", HasEst: true, EstRows: 900},
			{Op: "Hash", Children: []*explain.Node{{Op: "Index Scan", Relation: "customers", Alias: "c", Index: "customers_pkey", HasEst: true, EstRows: 50}}},
		}}}
	p.Finalize()
	v := PaintPlan(p, a.Design)
	o, c := v.Tables[id(a.Design, "o")], v.Tables[id(a.Design, "c")]
	if !o.Full || o.EstRows != 900 || c.Full || c.Index != "customers_pkey" || v.EstRows != 900 || v.Cost != 410 {
		t.Fatalf("plan view %+v", v)
	}
}
