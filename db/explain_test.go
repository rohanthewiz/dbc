package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/explain"
)

// sqliteExplainMgr is an in-memory SQLite database with a users table (500
// rows) and an orders table (20,000 rows, indexed only on status) — big
// enough that a full scan of orders is worth a warning and one of users is not.
func sqliteExplainMgr(t *testing.T) *Manager {
	t.Helper()
	cfg := &config.Config{
		MaxRows: 1000,
		Connections: []config.Connection{{Name: "sq", Driver: "sqlite",
			DSN: fmt.Sprintf("file:%s?mode=memory&cache=shared", memName(t))}},
	}
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	for _, s := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, city TEXT)",
		"CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INT, total REAL, status TEXT)",
		"CREATE INDEX orders_status ON orders(status)",
		`WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < 500)
		 INSERT INTO users SELECT n, 'user'||n, 'city'||(n%7) FROM g`,
		`WITH RECURSIVE g(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM g WHERE n < 20000)
		 INSERT INTO orders SELECT n, 1+n%500, n%300, CASE n%4 WHEN 0 THEN 'paid' ELSE 'new' END FROM g`,
	} {
		if _, err := mgr.Run("sq", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return mgr
}

// insightTitles lists a plan's findings, for failure messages and asserts.
func insightTitles(p *explain.Plan) string {
	var b strings.Builder
	for _, in := range p.Insights {
		fmt.Fprintf(&b, "[%s] %s\n", in.Severity, in.Title)
	}
	return b.String()
}

// The SQLite plan is only a list of steps; the db layer adds what makes it
// judgeable — the size of each table read — and the rules then tell a full
// scan of the big orders table from a lookup.
func TestExplainSQLite(t *testing.T) {
	mgr := sqliteExplainMgr(t)
	p, err := mgr.Explain(context.Background(), "sq",
		"SELECT o.id, u.name FROM orders o JOIN users u ON u.id = o.user_id WHERE o.total > 250", ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != explain.SQLite || p.Analyzed || p.Measured {
		t.Fatalf("plan = %+v", p)
	}
	var scan *explain.Node
	for _, n := range p.Nodes() {
		if n.Kind == explain.KindScan {
			scan = n
		}
	}
	if scan == nil || scan.Relation != "orders" || scan.TableRows != 20000 {
		t.Fatalf("expected a sized scan of orders (alias resolved):\n%s", p.Text(explain.TextOptions{}))
	}
	if !strings.Contains(insightTitles(p), "Full scan of orders (20,000 rows)") {
		t.Errorf("insights:\n%s", insightTitles(p))
	}
	if p.Root.Op != "Nested loop" {
		t.Errorf("two table accesses should be gathered under a nested loop:\n%s", p.Text(explain.TextOptions{}))
	}
}

// Analyze on SQLite runs a read and times it as a whole; a write is not run
// at all — the table is untouched and a note says why.
func TestExplainSQLiteAnalyze(t *testing.T) {
	mgr := sqliteExplainMgr(t)
	ctx := context.Background()
	p, err := mgr.Explain(ctx, "sq", "SELECT * FROM orders WHERE status = 'paid'", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Measured || p.ResultRows != 5000 || p.ExecutionMs <= 0 {
		t.Errorf("measured=%v rows=%v ms=%v", p.Measured, p.ResultRows, p.ExecutionMs)
	}

	zeros := func() string {
		res, err := mgr.Run("sq", "SELECT count(*) FROM orders WHERE total = 0")
		if err != nil {
			t.Fatal(err)
		}
		return res.Rows[0][0]
	}
	before := zeros()
	p, err = mgr.Explain(ctx, "sq", "UPDATE orders SET total = 0", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Measured || len(p.Notes) == 0 || !strings.Contains(p.Notes[0], "not analyzed") {
		t.Errorf("a write must not be analyzed: measured=%v notes=%v", p.Measured, p.Notes)
	}
	if after := zeros(); after != before {
		t.Fatalf("explaining the UPDATE ran it: rows with total 0 went %s → %s", before, after)
	}
}

// A statement the user already wrote as an EXPLAIN is unwrapped, and its
// ANALYZE honored.
func TestExplainUnwrapsUserExplain(t *testing.T) {
	mgr := sqliteExplainMgr(t)
	p, err := mgr.Explain(context.Background(), "sq", "EXPLAIN QUERY PLAN SELECT * FROM users WHERE id = 3;", ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Statement != "SELECT * FROM users WHERE id = 3" {
		t.Errorf("statement = %q", p.Statement)
	}
	if n := p.Nodes()[len(p.Nodes())-1]; n.Kind != explain.KindIndex || n.Index != "primary key" {
		t.Errorf("want a primary-key search:\n%s", p.Text(explain.TextOptions{}))
	}
}

// bytdb speaks Postgres-style text EXPLAIN; it goes through the text parser
// and gets table sizes and a measured run like SQLite.
func TestExplainBytdb(t *testing.T) {
	mgr := bytdbMgr(t)
	for _, s := range []string{
		"CREATE TABLE users (id int PRIMARY KEY, name text, city text)",
		"CREATE INDEX by_city ON users (city)",
		"INSERT INTO users VALUES (1, 'a', 'x'), (2, 'b', 'y'), (3, 'c', 'x')",
	} {
		if _, err := mgr.Run("bd", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	ctx := context.Background()
	p, err := mgr.Explain(ctx, "bd", "SELECT name FROM users WHERE city = 'x' ORDER BY name", ExplainOptions{Analyze: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != explain.Bytdb || !p.Measured || p.ResultRows != 2 {
		t.Errorf("engine=%s measured=%v rows=%v", p.Engine, p.Measured, p.ResultRows)
	}
	text := p.Text(explain.TextOptions{})
	for _, want := range []string{"Sort", "Index Scan · users using by_city", "(city = 'x')"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan text lacks %q:\n%s", want, text)
		}
	}
	for _, n := range p.Nodes() {
		if n.Relation == "users" && n.TableRows != 3 {
			t.Errorf("users should be sized at 3 rows, got %v", n.TableRows)
		}
	}
}

// A statement with an error fails as a statement error, not a parse error.
func TestExplainBadStatement(t *testing.T) {
	mgr := sqliteExplainMgr(t)
	_, err := mgr.Explain(context.Background(), "sq", "SELECT * FROM no_such_table", ExplainOptions{})
	if err == nil || !strings.Contains(err.Error(), "no_such_table") {
		t.Errorf("err = %v", err)
	}
}
