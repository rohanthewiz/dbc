package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/designer"
)

// shopDDL is a small store on the demo SQLite connection: enough keys for
// the designer to join by, and one index of its own.
const shopDDL = `CREATE TABLE customers (id INTEGER PRIMARY KEY, name TEXT NOT NULL, country TEXT);
CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER NOT NULL REFERENCES customers(id),
  total NUMERIC NOT NULL, status TEXT, created_at TIMESTAMP NOT NULL);
CREATE TABLE payments (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL REFERENCES orders(id), amount NUMERIC NOT NULL);
CREATE INDEX payments_order_idx ON payments (order_id);
INSERT INTO customers VALUES (1, 'Ana', 'NZ'), (2, 'Bo', 'AU'), (3, 'Cy', NULL);
INSERT INTO orders VALUES (10, 1, 50, 'paid', '2024-03-01 10:00'), (11, 1, 70, NULL, '2025-01-02 09:00'), (12, 2, 20, 'void', '2024-07-07 12:00');
INSERT INTO payments VALUES (100, 10, 20), (101, 10, 30), (102, 11, 70)`

// The designer's routes against a real engine: the palette, a table added
// and joined by its foreign key, an analysis whose SQL then runs, the
// indexes read, the plan painted, a refusal, and the design saved with its
// tab.
func TestDesignRoutes(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(shopDDL, 0, true), 200)
	s.await(t, "run")
	path := "/api/v1/ws/" + id + "/design"

	type palette struct {
		Tables []struct {
			Name string
			Keys int
		}
		Catalog struct{ Indexes, Estimates bool }
	}
	pal := decodeData[palette](t, e.api("GET", path+"/tables?fresh=1", "", 200))
	keys := map[string]int{}
	for _, tb := range pal.Tables {
		keys[tb.Name] = tb.Keys
	}
	if keys["orders"] != 2 || keys["customers"] != 1 {
		t.Fatalf("palette = %+v", pal)
	}

	type added struct {
		Analysis designer.Analysis
		Notes    []string
	}
	add := func(d designer.Design, name string) added {
		b, _ := json.Marshal(map[string]any{"design": d, "schema": "main", "name": name, "x": 10, "y": 10})
		return decodeData[added](t, e.api("POST", path+"/add", string(b), 200))
	}
	a := add(designer.Design{}, "customers")
	a = add(a.Analysis.Design, "orders")
	if len(a.Notes) != 1 || !strings.Contains(a.Notes[0], "c.id = o.customer_id") {
		t.Fatalf("notes = %q", a.Notes)
	}
	a = add(a.Analysis.Design, "payments")
	d := a.Analysis.Design
	if len(d.Joins) != 2 {
		t.Fatalf("joins = %+v", d.Joins)
	}

	// totals per customer: orders' totals summed over payments is the
	// classic inflated sum — the designer says so, and its fix is a
	// statement that runs and returns the right numbers
	c, o, p := d.Tables[0].ID, d.Tables[1].ID, d.Tables[2].ID
	d.Columns = []designer.Output{
		{Ref: designer.Ref{Table: c, Column: "name"}},
		{Ref: designer.Ref{Table: o, Column: "total", Agg: designer.AggSum}},
		{Ref: designer.Ref{Table: p, Column: "amount", Agg: designer.AggSum}},
	}
	d.Sorts = []designer.Sort{{Ref: designer.Ref{Table: c, Column: "name"}}}
	analyze := func(d designer.Design) designer.Analysis {
		b, _ := json.Marshal(map[string]any{"design": d, "fresh": true})
		return decodeData[struct{ Analysis designer.Analysis }](t, e.api("POST", path, string(b), 200)).Analysis
	}
	an := analyze(d)
	var fan *designer.Lint
	for i := range an.Lints {
		if an.Lints[i].Rule == "fanout-aggregate" {
			fan = &an.Lints[i]
		}
	}
	if fan == nil || !strings.Contains(fan.Title, "sum(o.total)") {
		t.Fatalf("lints = %+v", an.Lints)
	}
	var fixed *designer.Design
	for _, f := range fan.Fixes {
		if strings.HasPrefix(f.Label, "Summarize p") {
			fixed = f.Design
		}
	}
	if fixed == nil {
		t.Fatalf("fixes = %+v", fan.Fixes)
	}
	an = analyze(*fixed)
	if !an.Valid || !strings.Contains(an.SQL, "LEFT JOIN (") {
		t.Fatalf("fixed = %v\n%s", an.Lints, an.SQL)
	}
	e.api("POST", "/api/v1/ws/"+id+"/run", runBody(an.SQL, 0, false), 200)
	ev, logs := s.await(t, "run")
	if r := decodeData[runEvent](t, testEnvelope{Data: ev.Data}); !r.OK || !r.HasResult {
		t.Fatalf("the designed statement failed: %+v\n%s\nlogs %q", r, an.SQL, logs)
	}
	res := decodeData[resultPage](t, e.api("GET", "/api/v1/ws/"+id+"/result?from=0&count=10", "", 200))
	got := []string{}
	for _, r := range res.Cells {
		cells := make([]string, len(r))
		for i, c := range r {
			if c != nil {
				cells[i] = *c
			}
		}
		got = append(got, strings.Join(cells, "|"))
	}
	// Ana: orders 50 + 70 = 120 (not 50·2 + 70), payments 20 + 30 + 70;
	// Bo: 20, no payments (NULL, as a sum of none is)
	if want := "Ana|120|120 Bo|20|"; strings.Join(got, " ") != want {
		t.Fatalf("rows = %q, want %q\n%s", got, want, an.SQL)
	}

	// the index of payments is read, and its key column marked indexed
	for _, tv := range an.Tables {
		if tv.Alias != "p" {
			continue
		}
		for _, cv := range tv.Cols {
			if cv.Name == "order_id" && !cv.Indexed {
				t.Errorf("payments.order_id not marked indexed: %+v", tv)
			}
		}
	}

	// the plan, painted onto the tables
	b, _ := json.Marshal(map[string]any{"design": an.Design})
	pv := decodeData[designer.PlanView](t, e.api("POST", path+"/plan", string(b), 200))
	if pv.Engine != "sqlite" || pv.Tables[c].Op == "" {
		t.Fatalf("plan view = %+v", pv)
	}
	// a design with errors is not planned
	bad := an.Design
	bad.Filters = []designer.Filter{{Ref: designer.Ref{Table: o, Column: "total"}, Op: designer.OpGt, Values: []string{"lots"}}}
	b, _ = json.Marshal(map[string]any{"design": bad})
	if env := e.api("POST", path+"/plan", string(b), 409); !strings.Contains(env.Error, "errors to fix") {
		t.Fatalf("refusal = %+v", env)
	}

	// the design is saved with its tab, and a broken one is refused
	dj, _ := json.Marshal(an.Design)
	body, _ := json.Marshal(Tab{Title: "Design 1", Conn: "demo-sqlite", Buffer: an.SQL, Design: string(dj)})
	e.api("PUT", "/api/v1/tabs/d1?win=", string(body), 200)
	body, _ = json.Marshal(Tab{Title: "Design 2", Design: "{nope"})
	if env := e.api("PUT", "/api/v1/tabs/d2?win=", string(body), 400); !strings.Contains(env.Error, "not a design") {
		t.Fatalf("broken design = %+v", env)
	}
	tabs, err := e.srv.store.Tabs()
	if err != nil {
		t.Fatal(err)
	}
	var saved string
	for _, tb := range tabs {
		if tb.ID == "d1" {
			saved = tb.Design
		}
	}
	if saved != string(dj) {
		t.Fatalf("saved design = %q", saved)
	}
}
