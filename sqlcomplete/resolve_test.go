package sqlcomplete

import (
	"strings"
	"testing"
)

// resolveAt resolves buf at the ▮ marker, and returns the buffer without it.
func resolveAt(t *testing.T, buf string) (Symbol, string) {
	t.Helper()
	i := strings.Index(buf, "▮")
	if i < 0 {
		t.Fatal("no ▮ in buffer")
	}
	buf = buf[:i] + buf[i+len("▮"):]
	return Resolve(buf, i), buf
}

// bracket marks each span of buf with [ ], so a test reads the uses as
// the editor would highlight them.
func bracket(buf string, spans []Span) string {
	var b strings.Builder
	at := 0
	for _, s := range spans {
		b.WriteString(buf[at:s.From] + "[" + buf[s.From:s.To] + "]")
		at = s.To
	}
	return b.String() + buf[at:]
}

func TestResolveUses(t *testing.T) {
	cases := []struct {
		name, buf string
		kind      SymbolKind
		want      string // the buffer with every use bracketed
	}{
		{"alias from a qualifier",
			"SELECT o.id, ▮o.status FROM orders o WHERE o.id = 1",
			SymAlias, "SELECT [o].id, [o].status FROM orders [o] WHERE [o].id = 1"},
		{"alias where it is given",
			"SELECT o.id FROM orders AS ▮o",
			SymAlias, "SELECT [o].id FROM orders AS [o]"},
		{"a subquery's own alias shadows the outer one",
			"SELECT ▮o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items o WHERE o.order_id = 1) AND o.status = 'x'",
			SymAlias, "SELECT [o].id FROM orders [o] WHERE EXISTS (SELECT 1 FROM order_items o WHERE o.order_id = 1) AND [o].status = 'x'"},
		{"and is its own inside",
			"SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items o WHERE ▮o.order_id = 1) AND o.status = 'x'",
			SymAlias, "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items [o] WHERE [o].order_id = 1) AND o.status = 'x'"},
		{"a correlated reference resolves outward",
			"SELECT 1 FROM orders o WHERE EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = ▮o.id)",
			SymAlias, "SELECT 1 FROM orders [o] WHERE EXISTS (SELECT 1 FROM order_items i WHERE i.order_id = [o].id)"},
		{"each UNION arm has its own aliases",
			"SELECT ▮o.id FROM orders o UNION SELECT o.id FROM customers o",
			SymAlias, "SELECT [o].id FROM orders [o] UNION SELECT o.id FROM customers o"},
		{"a function's FROM is not a table list",
			"SELECT extract(year FROM o.created_at), o.id IS DISTINCT FROM o.status FROM orders ▮o",
			SymAlias, "SELECT extract(year FROM [o].created_at), [o].id IS DISTINCT FROM [o].status FROM orders [o]"},
		{"strings and comments are not uses",
			"SELECT o.id, 'o.id' -- o.id\nFROM orders ▮o",
			SymAlias, "SELECT [o].id, 'o.id' -- o.id\nFROM orders [o]"},
		{"only the caret's statement",
			"SELECT o.id FROM orders o;\nSELECT ▮o.id FROM customers o",
			SymAlias, "SELECT o.id FROM orders o;\nSELECT [o].id FROM customers [o]"},
		{"a quoted alias, quotes and all",
			`SELECT "O".id FROM orders ▮"O"`,
			SymAlias, `SELECT ["O"].id FROM orders ["O"]`},
		{"a derived table's alias",
			"SELECT ▮d.n FROM (SELECT count(*) n FROM orders) d",
			SymAlias, "SELECT [d].n FROM (SELECT count(*) n FROM orders) [d]"},
		{"UPDATE's alias",
			"UPDATE orders ▮o SET status = 'x' WHERE o.id = 1",
			SymAlias, "UPDATE orders [o] SET status = 'x' WHERE [o].id = 1"},
		{"a CTE: its name, a table, an unaliased qualifier",
			"WITH ▮big AS (SELECT * FROM orders) SELECT big.id FROM big JOIN customers c ON c.id = big.customer_id",
			SymCTE, "WITH [big] AS (SELECT * FROM orders) SELECT [big].id FROM [big] JOIN customers c ON c.id = [big].customer_id"},
		{"a CTE from where it is used",
			"WITH big AS (SELECT * FROM orders), top AS (SELECT * FROM big) SELECT * FROM ▮big",
			SymCTE, "WITH [big] AS (SELECT * FROM orders), top AS (SELECT * FROM [big]) SELECT * FROM [big]"},
		{"an aliased CTE: the alias is not the CTE",
			"WITH big AS (SELECT * FROM orders) SELECT b.id FROM ▮big b",
			SymCTE, "WITH [big] AS (SELECT * FROM orders) SELECT b.id FROM [big] b"},
		{"a recursive CTE, through its UNION",
			"WITH RECURSIVE ▮t(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 5) SELECT n FROM t",
			SymCTE, "WITH RECURSIVE [t](n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM [t] WHERE n < 5) SELECT n FROM [t]"},
		{"a table without an alias",
			"SELECT orders.id FROM ▮orders",
			SymTable, "SELECT [orders].id FROM [orders]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sym, buf := resolveAt(t, c.buf)
			if sym.Kind != c.kind {
				t.Fatalf("kind = %q, want %q", sym.Kind, c.kind)
			}
			if got := bracket(buf, sym.Uses); got != c.want {
				t.Errorf("uses:\n got %s\nwant %s", got, c.want)
			}
			if !strings.Contains(c.want, "["+buf[sym.Def.From:sym.Def.To]+"]") {
				t.Errorf("def %q is not a use", buf[sym.Def.From:sym.Def.To])
			}
		})
	}
}

// The definition is where the name is declared, wherever the caret is.
func TestResolveDefinition(t *testing.T) {
	sym, buf := resolveAt(t, "WITH big AS (SELECT 1 AS n) SELECT ▮big.n FROM big")
	if got := buf[sym.Def.From:sym.Def.To]; got != "big" || sym.Def.From != 5 {
		t.Errorf("def = %q at %d, want big at 5", got, sym.Def.From)
	}
	if got := buf[sym.At.From:sym.At.To]; got != "big" || sym.At.From != 35 {
		t.Errorf("at = %q at %d, want the caret's big at 35", got, sym.At.From)
	}
}

// Columns, keywords, schemas and blank space resolve to nothing.
func TestResolveNothing(t *testing.T) {
	for _, buf := range []string{
		"SELECT o.▮id FROM orders o",
		"SEL▮ECT 1",
		"SELECT ▮ 1",
		"SELECT * FROM ▮sales.invoices",
		"SELECT now()::timestamp with ▮time zone",
		"SELECT * FROM unnest(xs) WITH ▮ordinality",
		"SELECT 'o▮' FROM orders o",
	} {
		if sym, _ := resolveAt(t, buf); sym.Kind != "" {
			t.Errorf("%s: got %q %q, want nothing", buf, sym.Kind, sym.Name)
		}
	}
}

func TestRename(t *testing.T) {
	apply := func(buf string, edits []Edit) string {
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			buf = buf[:e.From] + e.Text + buf[e.To:]
		}
		return buf
	}
	cases := []struct{ driver, buf, name, want string }{
		{"postgres", "SELECT ▮o.id FROM orders o", "ord", "SELECT ord.id FROM orders ord"},
		// a capital needs quotes on Postgres, which folds names to lower
		// case, and not on MySQL; a reserved word needs them on both
		{"postgres", "SELECT ▮o.id FROM orders o", "Ord", `SELECT "Ord".id FROM orders "Ord"`},
		{"mysql", "SELECT ▮o.id FROM orders o", "Ord", "SELECT Ord.id FROM orders Ord"},
		{"mysql", "SELECT ▮o.id FROM orders o", "order", "SELECT `order`.id FROM orders `order`"},
		// a name the user quoted is theirs
		{"postgres", "SELECT ▮o.id FROM orders o", `"my o"`, `SELECT "my o".id FROM orders "my o"`},
		// a quoted original is replaced quotes and all
		{"postgres", `SELECT "O".id FROM orders ▮"O"`, "o", "SELECT o.id FROM orders o"},
		{"postgres", "WITH ▮big AS (SELECT 1 AS n) SELECT big.n FROM big", "  small ",
			"WITH small AS (SELECT 1 AS n) SELECT small.n FROM small"},
		// the same alias in an outer block is shadowed, not a collision
		{"postgres", "SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items ▮i WHERE i.order_id = o.id)", "o",
			"SELECT o.id FROM orders o WHERE EXISTS (SELECT 1 FROM order_items o WHERE o.order_id = o.id)"},
	}
	for _, c := range cases {
		i := strings.Index(c.buf, "▮")
		buf := c.buf[:i] + c.buf[i+len("▮"):]
		edits, err := Rename(buf, i, c.name, c.driver)
		if err != nil {
			t.Errorf("%s → %s: %v", c.buf, c.name, err)
			continue
		}
		if got := apply(buf, edits); got != c.want {
			t.Errorf("%s → %s:\n got %s\nwant %s", c.buf, c.name, got, c.want)
		}
	}
}

func TestRenameRefused(t *testing.T) {
	cases := []struct{ buf, name, want string }{
		{"SELECT o.▮id FROM orders o", "x", "nothing to rename"},
		{"SELECT orders.id FROM ▮orders", "x", "is a table"},
		{"SELECT ▮o.id FROM orders o JOIN customers c ON c.id = o.customer_id", "C", "already a name"},
		{"WITH a AS (SELECT 1), ▮b AS (SELECT 2) SELECT * FROM a, b", "a", "already a name"},
		{"SELECT ▮o.id FROM orders o", " ", "empty"},
	}
	for _, c := range cases {
		i := strings.Index(c.buf, "▮")
		buf := c.buf[:i] + c.buf[i+len("▮"):]
		_, err := Rename(buf, i, c.name, "postgres")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s → %q: err = %v, want %q", c.buf, c.name, err, c.want)
		}
	}
}

// Columns the statement names: a CTE's or derived table's output columns,
// and select-list aliases in ORDER BY.
func TestResolveColumns(t *testing.T) {
	cases := []struct {
		name, buf string
		want      string // the buffer with every use bracketed
		fixed     bool   // found, but a table's column: not renamable
	}{
		{"a CTE's alias, from a qualified use",
			"WITH t AS (SELECT count(*) AS n FROM orders) SELECT t.▮n FROM t WHERE t.n > 1",
			"WITH t AS (SELECT count(*) AS [n] FROM orders) SELECT t.[n] FROM t WHERE t.[n] > 1", false},
		{"from its declaration",
			"WITH t AS (SELECT count(*) AS ▮n FROM orders) SELECT t.n FROM t",
			"WITH t AS (SELECT count(*) AS [n] FROM orders) SELECT t.[n] FROM t", false},
		{"an alias without AS",
			"WITH t AS (SELECT count(*) n FROM orders) SELECT ▮n FROM t",
			"WITH t AS (SELECT count(*) [n] FROM orders) SELECT [n] FROM t", false},
		{"bare uses, the CTE the only thing in FROM",
			"WITH t AS (SELECT count(*) AS n FROM orders) SELECT n FROM t WHERE ▮n > 1 GROUP BY n ORDER BY n",
			"WITH t AS (SELECT count(*) AS [n] FROM orders) SELECT [n] FROM t WHERE [n] > 1 GROUP BY [n] ORDER BY [n]", false},
		{"through an aliased CTE",
			"WITH t AS (SELECT 1 AS n) SELECT x.▮n FROM t x",
			"WITH t AS (SELECT 1 AS [n]) SELECT x.[n] FROM t x", false},
		{"a CTE's column list wins over its body's names",
			"WITH t(▮n) AS (SELECT count(*) AS c FROM orders ORDER BY c) SELECT t.n FROM t",
			"WITH t([n]) AS (SELECT count(*) AS c FROM orders ORDER BY c) SELECT t.[n] FROM t", false},
		{"a derived table's column",
			"SELECT d.▮n FROM (SELECT count(*) AS n FROM orders) d ORDER BY d.n",
			"SELECT d.[n] FROM (SELECT count(*) AS [n] FROM orders) d ORDER BY d.[n]", false},
		{"a derived table's column list",
			"SELECT d.▮k FROM (SELECT 1 AS n) AS d(k)",
			"SELECT d.[k] FROM (SELECT 1 AS n) AS d([k])", false},
		{"passed through another CTE, by name and by star",
			"WITH a AS (SELECT 1 AS n), b AS (SELECT n FROM a), c AS (SELECT * FROM b) SELECT c.▮n FROM c",
			"WITH a AS (SELECT 1 AS [n]), b AS (SELECT [n] FROM a), c AS (SELECT * FROM b) SELECT c.[n] FROM c", false},
		{"t.* passes through too",
			"WITH a AS (SELECT 1 AS n) SELECT s.▮n FROM (SELECT a.* FROM a) s",
			"WITH a AS (SELECT 1 AS [n]) SELECT s.[n] FROM (SELECT a.* FROM a) s", false},
		{"a correlated bare use resolves outward",
			"WITH t AS (SELECT 1 AS n), u AS (SELECT 2 AS m) SELECT 1 FROM t WHERE EXISTS (SELECT 1 FROM u WHERE m = ▮n)",
			"WITH t AS (SELECT 1 AS [n]), u AS (SELECT 2 AS m) SELECT 1 FROM t WHERE EXISTS (SELECT 1 FROM u WHERE m = [n])", false},
		{"a LATERAL body sees the FROM before it",
			"WITH t AS (SELECT 1 AS n) SELECT * FROM t, LATERAL (SELECT ▮n + 1 AS m) l",
			"WITH t AS (SELECT 1 AS [n]) SELECT * FROM t, LATERAL (SELECT [n] + 1 AS m) l", false},
		{"inside a function's parentheses",
			"WITH t AS (SELECT 1 AS n) SELECT coalesce(▮n, 0), sum(t.n) FROM t",
			"WITH t AS (SELECT 1 AS [n]) SELECT coalesce([n], 0), sum(t.[n]) FROM t", false},
		{"a select alias in ORDER BY",
			"SELECT status, count(*) AS n FROM orders GROUP BY status ORDER BY ▮n DESC",
			"SELECT status, count(*) AS [n] FROM orders GROUP BY status ORDER BY [n] DESC", false},
		{"a UNION's ORDER BY names the first arm's columns",
			"SELECT id AS k FROM orders UNION SELECT id FROM archive ORDER BY ▮k",
			"SELECT id AS [k] FROM orders UNION SELECT id FROM archive ORDER BY [k]", false},
		{"a CTE's table column: found, not renamed",
			"WITH t AS (SELECT o.id FROM orders o) SELECT t.▮id FROM t",
			"WITH t AS (SELECT o.[id] FROM orders o) SELECT t.[id] FROM t", true},
		{"a plain column in ORDER BY: the select item",
			"SELECT id FROM orders ORDER BY ▮id",
			"SELECT [id] FROM orders ORDER BY [id]", true},
		{"a recursive CTE's column list, in both arms",
			"WITH RECURSIVE t(▮n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM t WHERE n < 5) SELECT n FROM t",
			"WITH RECURSIVE t([n]) AS (SELECT 1 UNION ALL SELECT [n] + 1 FROM t WHERE [n] < 5) SELECT [n] FROM t", false},
		{"a recursive CTE's first-arm alias",
			"WITH RECURSIVE t AS (SELECT 1 AS ▮n UNION ALL SELECT n + 1 FROM t WHERE n < 5) SELECT n FROM t",
			"WITH RECURSIVE t AS (SELECT 1 AS [n] UNION ALL SELECT [n] + 1 FROM t WHERE [n] < 5) SELECT [n] FROM t", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sym, buf := resolveAt(t, c.buf)
			if sym.Kind != SymColumn {
				t.Fatalf("kind = %q, want column", sym.Kind)
			}
			if got := bracket(buf, sym.Uses); got != c.want {
				t.Errorf("uses:\n got %s\nwant %s", got, c.want)
			}
			if (sym.Fixed != "") != c.fixed {
				t.Errorf("fixed = %q, want fixed %v", sym.Fixed, c.fixed)
			}
		})
	}
}

// A bare column is left alone wherever it could be a catalog table's.
func TestResolveColumnsNothing(t *testing.T) {
	for _, buf := range []string{
		// orders is in scope: n could be its
		"WITH t AS (SELECT 1 AS n) SELECT ▮n FROM t, orders",
		// a CTE's body does not see the statement's FROM (u is not in
		// FROM, so only the seal stops n reaching t)
		"WITH t AS (SELECT 1 AS n), u AS (SELECT ▮n) SELECT * FROM t",
		// nor does a derived table's, unless LATERAL
		"WITH t AS (SELECT 1 AS n) SELECT * FROM t WHERE EXISTS (SELECT 1 FROM (SELECT ▮n) d)",
		// ambiguous between two CTEs
		"WITH a AS (SELECT 1 AS n), b AS (SELECT 2 AS n) SELECT ▮n FROM a, b",
		// a column no CTE has
		"WITH t AS (SELECT 1 AS n) SELECT t.▮m FROM t",
		// an expression in ORDER BY is not an output name
		"SELECT x AS n FROM orders ORDER BY ▮n + 1",
		// keywords are not columns, even one a CTE has
		"WITH t AS (SELECT 1 AS first) SELECT * FROM t ORDER BY 1 NULLS ▮first",
		// a star over a catalog table: the column might be there
		"WITH t AS (SELECT * FROM orders) SELECT t.▮id FROM t",
		// a table's column the select list names, used nowhere else
		"SELECT o.▮id FROM orders o",
	} {
		if sym, _ := resolveAt(t, buf); sym.Kind != "" {
			t.Errorf("%s: got %q %q, want nothing", buf, sym.Kind, sym.Name)
		}
	}
}

func TestRenameColumns(t *testing.T) {
	cases := []struct{ buf, name, want string }{
		{"WITH t AS (SELECT count(*) AS n FROM orders) SELECT t.▮n FROM t ORDER BY n",
			"total", "WITH t AS (SELECT count(*) AS total FROM orders) SELECT t.total FROM t ORDER BY total"},
		{"SELECT status, count(*) n FROM orders GROUP BY status ORDER BY ▮n",
			"Total", `SELECT status, count(*) "Total" FROM orders GROUP BY status ORDER BY "Total"`},
		// a pass-through follows its origin, so b's column stays a's
		{"WITH a AS (SELECT 1 AS ▮n), b AS (SELECT n FROM a) SELECT b.n FROM b",
			"m", "WITH a AS (SELECT 1 AS m), b AS (SELECT m FROM a) SELECT b.m FROM b"},
		{"SELECT d.▮n FROM (SELECT 1 AS n) d", "k", "SELECT d.k FROM (SELECT 1 AS k) d"},
	}
	for _, c := range cases {
		i := strings.Index(c.buf, "▮")
		buf := c.buf[:i] + c.buf[i+len("▮"):]
		edits, err := Rename(buf, i, c.name, "postgres")
		if err != nil {
			t.Errorf("%s → %s: %v", c.buf, c.name, err)
			continue
		}
		for j := len(edits) - 1; j >= 0; j-- {
			buf = buf[:edits[j].From] + edits[j].Text + buf[edits[j].To:]
		}
		if buf != c.want {
			t.Errorf("%s → %s:\n got %s\nwant %s", c.buf, c.name, buf, c.want)
		}
	}
}

func TestRenameColumnsRefused(t *testing.T) {
	cases := []struct{ buf, name, want string }{
		// a sibling column already has the name
		{"WITH t AS (SELECT 1 AS ▮n, 2 AS m) SELECT t.n FROM t", "m", "already a name"},
		// t.n would become ambiguous with u's m in the bare use
		{"WITH t AS (SELECT 1 AS ▮n), u AS (SELECT 2 AS m) SELECT n, m FROM t, u", "m", "already a name"},
		// the subquery's bare m would be captured by t's renamed column
		{"WITH t AS (SELECT 1 AS ▮n), u AS (SELECT 2 AS m) SELECT (SELECT m FROM t) FROM u", "m", "already a name"},
		{"WITH t AS (SELECT o.id FROM orders o) SELECT t.▮id FROM t", "k", "table's column"},
	}
	for _, c := range cases {
		i := strings.Index(c.buf, "▮")
		buf := c.buf[:i] + c.buf[i+len("▮"):]
		_, err := Rename(buf, i, c.name, "postgres")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s → %q: err = %v, want %q", c.buf, c.name, err, c.want)
		}
	}
}
