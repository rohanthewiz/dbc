package sqlcomplete

import "testing"

// Per-engine reserved words are quoted when inserted (N-299): MySQL's rank
// and groups, SQLite's transaction.
func TestQuoteEngineReserved(t *testing.T) {
	for _, c := range []struct{ driver, name, want string }{
		{"mysql", "rank", "`rank`"},
		{"mysql", "groups", "`groups`"},
		{"mysql", "interval", "`interval`"},
		{"postgres", "rank", "rank"},
		{"sqlite", "transaction", `"transaction"`},
		{"sqlite", "foreign", `"foreign"`},
		{"postgres", "foreign", `"foreign"`},
		{"mysql", "orders", "orders"},
	} {
		if got := dialectFor(c.driver).quote(c.name); got != c.want {
			t.Errorf("%s quote(%q) = %q, want %q", c.driver, c.name, got, c.want)
		}
	}
}
