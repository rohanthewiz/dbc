package workspace

import (
	"context"
	"strings"
	"testing"

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
	})
}
