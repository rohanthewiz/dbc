package designer

import "testing"

// The designer quotes each engine's own reserved words (N-299): MySQL 8
// reserves rank and groups, which it wrote bare (`FROM rank r`), and
// SQLite's `SELECT r.transaction` was a syntax error.
func TestIdentEngineReserved(t *testing.T) {
	for _, c := range []struct{ driver, name, want string }{
		{"mysql", "rank", "`rank`"},
		{"mysql", "groups", "`groups`"},
		{"mysql", "condition", "`condition`"},
		{"postgres", "rank", "rank"},
		{"sqlite", "transaction", `"transaction"`},
		{"sqlite", "commit", `"commit"`},
		{"sqlite", "orders", "orders"},
		{"bytdb", "foreign", `"foreign"`},
	} {
		if got := dialectOf(c.driver).ident(c.name); got != c.want {
			t.Errorf("%s ident(%q) = %q, want %q", c.driver, c.name, got, c.want)
		}
	}
}
