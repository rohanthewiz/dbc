package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/designer"
)

// The query designer against live Postgres and MySQL: the catalog it reads
// (keys, indexes, estimates), and — the point of the whole package — the
// statements it writes, each run on the real engine and checked for the
// rows a person would expect. The shapes chosen are the ones the designer
// writes instead of the obvious-but-wrong SQL: a summarized child, EXISTS
// and NOT EXISTS, a filter moved into a left join, a sargable year, an
// escaped LIKE, a count of a repeated table's rows, MySQL's OFFSET.
func TestLiveDesigner(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := []string{`DROP TABLE IF EXISTS qd_payments`, `DROP TABLE IF EXISTS qd_orders`, `DROP TABLE IF EXISTS qd_customers`}
		obsExec(t, obs, drop...)
		obsExec(t, obs,
			`CREATE TABLE qd_customers (id INTEGER PRIMARY KEY, name VARCHAR(40) NOT NULL, country VARCHAR(2))`,
			// table-level FOREIGN KEY: MySQL 8.4 parses a column's inline
			// REFERENCES and ignores it, so no key would exist there
			`CREATE TABLE qd_orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL,
			  total INTEGER NOT NULL, ref VARCHAR(40), created_at TIMESTAMP NOT NULL,
			  FOREIGN KEY (customer_id) REFERENCES qd_customers(id))`,
			`CREATE TABLE qd_payments (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL, amount INTEGER NOT NULL,
			  FOREIGN KEY (order_id) REFERENCES qd_orders(id))`,
			`CREATE INDEX qd_payments_order_idx ON qd_payments (order_id)`,
			`INSERT INTO qd_customers VALUES (1, 'Ana', 'NZ'), (2, 'Bo', 'AU'), (3, 'Cy', 'NZ')`,
			`INSERT INTO qd_orders VALUES (10, 1, 50, '50%_off', '2024-03-01 10:00:00'), (11, 1, 70, 'x5000off', '2025-01-02 09:00:00'),
			  (12, 2, 20, NULL, '2024-07-07 12:00:00')`,
			`INSERT INTO qd_payments VALUES (100, 10, 20), (101, 10, 30), (102, 11, 70)`)
		t.Cleanup(func() {
			for _, s := range drop {
				_, _ = obs.Run("live", s)
			}
		})

		ctx := context.Background()
		var d designer.Design
		add := func(name string) {
			t.Helper()
			keys := []designer.TableKey{}
			for _, x := range d.Tables {
				keys = append(keys, designer.TableKey{Schema: x.Schema, Name: x.Name})
			}
			schema := ""
			cat0, err := w.DesignCatalog(ctx, keys)
			if err != nil {
				t.Fatal(err)
			}
			for _, et := range cat0.Schema.Tables {
				if et.Name == name {
					schema = et.Schema
				}
			}
			cat, err := w.DesignCatalog(ctx, append(keys, designer.TableKey{Schema: schema, Name: name}))
			if err != nil {
				t.Fatal(err)
			}
			if d, _, err = designer.AddTable(d, cat, schema, name, 0, 0); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
		}
		add("qd_customers")
		add("qd_orders")
		add("qd_payments")
		if len(d.Joins) != 2 || d.Joins[0].FK == "" {
			t.Fatalf("not joined by the foreign keys: %+v", d.Joins)
		}
		id := func(name string) string {
			for _, x := range d.Tables {
				if x.Name == name {
					return x.ID
				}
			}
			t.Fatalf("no %s", name)
			return ""
		}
		c, o, p := id("qd_customers"), id("qd_orders"), id("qd_payments")
		ref := func(tb, col string, a designer.Agg) designer.Ref { return designer.Ref{Table: tb, Column: col, Agg: a} }

		analyze := func(d designer.Design) *designer.Analysis {
			t.Helper()
			keys := []designer.TableKey{}
			for _, x := range d.Tables {
				keys = append(keys, designer.TableKey{Schema: x.Schema, Name: x.Name})
			}
			cat, err := w.DesignCatalog(ctx, keys)
			if err != nil {
				t.Fatal(err)
			}
			if !cat.IndexesKnown {
				t.Fatalf("%s: the indexes were not read", e.driver)
			}
			a := designer.Analyze(d, cat)
			if !a.Valid {
				t.Fatalf("not valid: %+v\n%s", a.Lints, a.SQL)
			}
			return a
		}
		rows := func(sql string) string {
			t.Helper()
			res, err := obs.Run("live", sql)
			if err != nil {
				t.Fatalf("%s:\n%s", err, sql)
			}
			var out []string
			for _, r := range res.Rows {
				out = append(out, strings.Join(r, "|"))
			}
			return strings.Join(out, " ")
		}
		check := func(what string, d designer.Design, want string) *designer.Analysis {
			t.Helper()
			a := analyze(d)
			if got := rows(a.SQL); got != want {
				t.Errorf("%s: got %q, want %q\n%s", what, got, want, a.SQL)
			}
			return a
		}

		// the payments' join column is indexed, and the card says so
		a := analyze(d)
		for _, tv := range a.Tables {
			for _, cv := range tv.Cols {
				if tv.ID == p && cv.Name == "order_id" && !cv.Indexed {
					t.Errorf("qd_payments.order_id not marked indexed")
				}
			}
		}

		// totals per customer: orders summed beside summarized payments
		s := d
		s.Columns = []designer.Output{{Ref: ref(c, "name", "")}, {Ref: ref(o, "total", designer.AggSum)},
			{Ref: ref(p, "amount", designer.AggSum)}, {Ref: ref(p, "", designer.AggCount)}}
		s.Joins = append([]designer.Join(nil), d.Joins...)
		for i := range s.Joins {
			if s.Joins[i].Right == p {
				s.Joins[i].Kind = designer.JoinSummary
			}
		}
		s.Sorts = []designer.Sort{{Ref: ref(c, "name", "")}}
		// Bo's one order has no payment: a sum of none is NULL, a count 0
		check("summarized payments", s, "Ana|120|120|3 Bo|20|NULL|0")

		// customers who have an order over 60 (EXISTS, with its filter
		// inside) and customers with no order at all (NOT EXISTS)
		x := d
		x.Tables = d.Tables[:2]
		x.Joins = []designer.Join{d.Joins[0]}
		x.Joins[0].Kind = designer.JoinExists
		x.Columns = []designer.Output{{Ref: ref(c, "name", "")}}
		x.Filters = []designer.Filter{{Ref: ref(o, "total", ""), Op: designer.OpGt, Values: []string{"60"}}}
		check("has an order over 60", x, "Ana")
		x.Joins = []designer.Join{d.Joins[0]}
		x.Joins[0].Kind = designer.JoinNotExists
		x.Filters = nil
		check("has no order", x, "Cy")

		// every customer, with only their 2024 orders counted: the year in
		// the join (a range), the count of orders by key (not count(*))
		l := x
		l.Joins = []designer.Join{d.Joins[0]}
		l.Joins[0].Kind = designer.JoinLeft
		l.Columns = []designer.Output{{Ref: ref(c, "name", "")}, {Ref: ref(o, "", designer.AggCount)}}
		l.Filters = []designer.Filter{{Ref: ref(o, "created_at", ""), Op: designer.OpYear, Values: []string{"2024"}, InJoin: true}}
		l.Sorts = []designer.Sort{{Ref: ref(c, "name", "")}}
		check("2024 orders per customer", l, "Ana|1 Bo|1 Cy|0")

		// a LIKE whose value holds % and _: only the literal match
		k := x
		k.Joins = []designer.Join{d.Joins[0]}
		k.Joins[0].Kind = designer.JoinInner
		k.Columns = []designer.Output{{Ref: ref(o, "id", "")}}
		k.Filters = []designer.Filter{{Ref: ref(o, "ref", ""), Op: designer.OpContains, Values: []string{"50%_"}}}
		check("escaped LIKE", k, "10")

		// ignoring case: ILIKE on Postgres, lower() LIKE lower() on MySQL —
		// the same rows on both, where a plain LIKE's case rule differs
		// (case-sensitive on Postgres, the column's _ci collation on MySQL)
		k.Sorts = []designer.Sort{{Ref: ref(o, "id", "")}}
		k.Filters = []designer.Filter{{Ref: ref(o, "ref", ""), Op: designer.OpEnds, Values: []string{"OFF"}, IgnoreCase: true}}
		check("ends with, ignoring case", k, "10 11")
		k.Filters[0] = designer.Filter{Ref: ref(o, "ref", ""), Op: designer.OpContains, Values: []string{"50%_OFF"}, IgnoreCase: true}
		check("escaped, ignoring case", k, "10")

		// a backslash in the value, matched as itself: Postgres reads the
		// pattern's \\ by its default escape, with no ESCAPE clause, MySQL
		// by its ESCAPE '\\'
		lit := `'p\q'`
		if e.driver == "mysql" {
			lit = `'p\\q'`
		}
		obsExec(t, obs, `UPDATE qd_orders SET ref = `+lit+` WHERE id = 12`)
		k.Filters[0] = designer.Filter{Ref: ref(o, "ref", ""), Op: designer.OpContains, Values: []string{`p\q`}}
		check("a backslash, escaped", k, "12")
		obsExec(t, obs, `UPDATE qd_orders SET ref = NULL WHERE id = 12`)
		k.Sorts = nil

		// customers per country, counted once each through their orders
		g := k
		g.Columns = []designer.Output{{Ref: ref(c, "country", "")}, {Ref: ref(c, "", designer.AggCount)}}
		g.Filters = nil
		g.Sorts = []designer.Sort{{Ref: ref(c, "country", "")}}
		a = check("customers per country", g, "AU|1 NZ|1")
		if !strings.Contains(a.SQL, "count(DISTINCT c.id)") && !strings.Contains(a.SQL, "count(DISTINCT qc.id)") {
			t.Errorf("not counted distinct:\n%s", a.SQL)
		}

		// an offset without a limit, and the last N days: they run
		k.Filters = []designer.Filter{{Ref: ref(o, "created_at", ""), Op: designer.OpLastDays, Values: []string{"7"}}}
		k.Sorts = []designer.Sort{{Ref: ref(o, "id", "")}}
		k.Offset = 1
		check("offset and last days", k, "")

		// a query written by hand, opened as a design: the design's own
		// statement returns the rows the hand-written one does
		for _, q := range []string{
			`SELECT c.name, sum(o.total) AS spent FROM qd_customers c JOIN qd_orders o ON o.customer_id = c.id
			  WHERE o.ref LIKE '%off' GROUP BY c.name ORDER BY c.name`,
			`SELECT c.name FROM qd_customers c WHERE c.id IN (SELECT customer_id FROM qd_orders WHERE total > 60)`,
			`SELECT c.name, o.id FROM qd_orders o RIGHT JOIN qd_customers c ON c.id = o.customer_id ORDER BY c.name, o.id`,
			`SELECT c.name FROM qd_customers c LEFT JOIN qd_orders o ON o.customer_id = c.id
			  AND o.created_at >= '2024-01-01' AND o.created_at < '2025-01-01' WHERE o.id IS NULL ORDER BY 1`,
			`SELECT country, count(*) FROM qd_customers c, qd_orders o WHERE o.customer_id = c.id
			  AND (o.total BETWEEN 10 AND 30 OR o.ref IS NULL) GROUP BY country HAVING count(*) >= 1 ORDER BY 2 DESC, 1`,
			`SELECT o.id FROM qd_orders o WHERE NOT EXISTS (SELECT 1 FROM qd_payments p WHERE p.order_id = o.id) ORDER BY o.id`,
			// summaries, as the designer writes them but named by hand:
			// per order, and re-aggregated per customer
			`SELECT o.id, s.paid, COALESCE(s.n, 0) AS n FROM qd_orders o
			  LEFT JOIN (SELECT order_id, sum(amount) AS paid, count(*) AS n FROM qd_payments WHERE amount > 0 GROUP BY order_id) s
			  ON s.order_id = o.id WHERE (s.paid > 10 OR s.paid IS NULL) ORDER BY o.id`,
			`SELECT c.name, count(*) AS orders, COALESCE(sum(s.n), 0) AS payments, sum(s.paid) AS paid,
			  sum(s.paid) * 1.0 / NULLIF(sum(s.cnt), 0) AS avg_paid
			  FROM qd_customers c LEFT JOIN qd_orders o ON o.customer_id = c.id
			  LEFT JOIN (SELECT order_id, sum(amount) AS paid, count(amount) AS cnt, count(*) AS n FROM qd_payments GROUP BY order_id) s
			  ON s.order_id = o.id GROUP BY c.name ORDER BY c.name`,
			// nothing ticked beside a summary: the design writes o.* too, so
			// only the orders' own columns, never the summary's key or count
			`SELECT o.* FROM qd_orders o
			  LEFT JOIN (SELECT order_id, sum(amount) AS paid FROM qd_payments GROUP BY order_id) s
			  ON s.order_id = o.id ORDER BY 1`,
		} {
			cat, err := w.DesignCatalog(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			d, _, err := designer.Import(q, cat)
			if err != nil {
				t.Errorf("%s: import refused: %v\n%s", e.driver, err, q)
				continue
			}
			a := analyze(d)
			if want, got := rows(q), rows(a.SQL); got != want {
				t.Errorf("%s: imported, it returns %q, the query %q\n%s\n→\n%s", e.driver, got, want, q, a.SQL)
			}
		}

		// the plan, painted on the tables
		a = analyze(s)
		pl, err := w.DesignPlan(ctx, a.SQL)
		if err != nil {
			t.Fatalf("plan: %v\n%s", err, a.SQL)
		}
		v := designer.PaintPlan(pl, a.Design)
		if v.Tables[c].Op == "" || v.Tables[o].Op == "" {
			t.Errorf("plan not painted: %+v", v.Tables)
		}

		// a starts-with and the indexes that can serve it. MySQL's b-tree
		// finds a prefix in any collation, so a plain index is all of it
		k.Filters = []designer.Filter{{Ref: ref(o, "ref", ""), Op: designer.OpStarts, Values: []string{"r1999"}}}
		k.Sorts, k.Offset = nil, 0
		refCard := func() designer.ColView {
			t.Helper()
			w.ForgetDesignCatalog()
			for _, tv := range analyze(k).Tables {
				for _, cv := range tv.Cols {
					if tv.ID == o && cv.Name == "ref" {
						return cv
					}
				}
			}
			t.Fatal("no qd_orders.ref on the card")
			return designer.ColView{}
		}
		if e.driver == "mysql" {
			obsExec(t, obs, `CREATE INDEX qd_orders_ref_plain ON qd_orders (ref)`)
			if cv := refCard(); !cv.Indexed || cv.NoPrefix || cv.NoRange {
				t.Errorf("mysql, a plain index on ref: %+v", cv)
			}
			return
		}
		livePrefixIndex(t, w, obs, k, o, analyze, refCard)
	})
}

// livePrefixIndex: on Postgres a b-tree finds a starts-with only in byte
// order (a pattern class, or a C-ordered collation). The designer's
// listing reads each key column's class and collation; this checks that
// what it concludes is what the planner does — on whatever collation the
// server's database has — and that the index it suggests runs and serves.
//
// qd_orders is grown past the designer's size threshold (bigTable) first,
// so the index rule speaks; the planner is held off the sequential scan
// (a session's enable_seqscan) so a usable index always shows as an Index
// Cond, and an unusable one never does.
func livePrefixIndex(t *testing.T, w *Workspace, obs *db.Manager, k designer.Design, o string,
	analyze func(designer.Design) *designer.Analysis, refCard func() designer.ColView) {
	ctx := context.Background()
	obsExec(t, obs,
		`INSERT INTO qd_orders SELECT g, 1, g, 'r' || g, '2024-01-01' FROM generate_series(1000, 21000) g`,
		`ANALYZE qd_orders`)
	sess, err := obs.Session(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if _, err := sess.Run(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	indexCond := func(where string) bool {
		t.Helper()
		res, err := sess.Run(ctx, `EXPLAIN (COSTS OFF) SELECT id FROM qd_orders WHERE `+where)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Rows {
			if strings.Contains(r[0], "Index Cond") && strings.Contains(r[0], "ref") {
				return true
			}
		}
		return false
	}
	lint := func(rule string) *designer.Lint {
		t.Helper()
		w.ForgetDesignCatalog()
		a := analyze(k)
		for i := range a.Lints {
			if a.Lints[i].Rule == rule {
				return &a.Lints[i]
			}
		}
		return nil
	}

	// no index on ref: the rule speaks, and its fix is a pattern index
	l := lint("filter-no-index")
	if l == nil || len(l.Fixes) == 0 || !strings.Contains(l.Fixes[0].SQL, "(ref text_pattern_ops)") {
		t.Fatalf("no index: %+v", l)
	}
	obsExec(t, obs, l.Fixes[0].SQL)
	if l := lint("filter-no-index"); l != nil {
		t.Errorf("after its own fix: %+v", l)
	}
	if cv := refCard(); !cv.Indexed || cv.NoPrefix || !cv.NoRange {
		t.Errorf("the pattern index's card: %+v", cv)
	}
	if !indexCond(`ref LIKE 'r1999%'`) || indexCond(`ref >= 'r5'`) {
		t.Errorf("the pattern index should find the prefix and not the range")
	}

	// a plain index in the database's own collation: the card says
	// whether it finds a prefix, and the planner must agree
	obsExec(t, obs, `DROP INDEX qd_orders_ref_idx`, `CREATE INDEX qd_orders_ref_plain ON qd_orders (ref)`)
	cv := refCard()
	if !cv.Indexed || cv.NoRange {
		t.Errorf("the plain index's card: %+v", cv)
	}
	if uses := indexCond(`ref LIKE 'r1999%'`); uses == cv.NoPrefix {
		t.Errorf("the card says NoPrefix=%v, the planner uses the plain index for a prefix: %v", cv.NoPrefix, uses)
	}
	if l := lint("filter-no-index"); (l != nil) != cv.NoPrefix {
		t.Errorf("NoPrefix=%v but the rule %+v", cv.NoPrefix, l)
	}
	// and under COLLATE "C" a plain index finds it, whatever the database's
	obsExec(t, obs, `DROP INDEX qd_orders_ref_plain`, `CREATE INDEX qd_orders_ref_c ON qd_orders (ref COLLATE "C")`)
	if cv := refCard(); cv.NoPrefix || cv.NoRange || !indexCond(`ref LIKE 'r1999%'`) {
		t.Errorf("a C-collated index: %+v", cv)
	}
	if l := lint("filter-no-index"); l != nil {
		t.Errorf("a C-collated index: %+v", l)
	}
}
