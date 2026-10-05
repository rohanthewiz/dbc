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
