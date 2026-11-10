package sqlcomplete

import (
	"testing"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// FuzzComplete runs completion, symbol lookup and rename at a caret in a
// fuzzed buffer, for each driver, and wants no panic and no more than
// MaxItems items (N-302). A typed "." as a statement's first token
// panicked here once (N-250), on the TUI's event loop. Run at length by
// hand:
//
//	go test -run '^$' -fuzz '^FuzzComplete$' -fuzztime 5m ./sqlcomplete
func FuzzComplete(f *testing.F) {
	for _, s := range []string{
		".", "SELECT 1;.", "SELECT c. FROM cats c", "SELECT * FROM ", "SELECT * FROM public.",
		"INSERT INTO orders (", "UPDATE orders SET ", "SELECT o.id FROM orders o JOIN customers c ON ",
		"WITH a AS (SELECT id FROM orders) SELECT a. FROM a", "SELECT x::", "CALL ", "DROP FUNCTION ",
		"SELECT \"ord", "SELECT * FROM \"public\".\"or", "SELECT `a", "-- c.\nSELECT ", "SELECT 'a.",
		"CREATE TRIGGER t AFTER DELETE ON orders BEGIN DELETE FROM customers WHERE id = old.",
		"DELIMITER //\nSELECT o. FROM orders o //", "SELECT * FROM t # c.\nWHERE ",
	} {
		for d := range uint8(4) {
			f.Add(s, uint16(len(s)), d)
			f.Add(s, uint16(len(s)/2), d)
		}
	}
	drivers := []string{"postgres", "mysql", "sqlite", "bytdb"}
	schema := shop()
	f.Fuzz(func(t *testing.T, buf string, at uint16, dn uint8) {
		caret := int(at) % (len(buf) + 1)
		driver := drivers[int(dn)%len(drivers)]
		r := Complete(Request{Schema: schema, Routines: shopRoutines(), Driver: driver, Buffer: buf, Caret: caret})
		if len(r.Items) > MaxItems {
			t.Fatalf("%s: %d items, over MaxItems", driver, len(r.Items))
		}
		Resolve(buf, caret, sqlsplit.DialectOf(driver))
		Rename(buf, caret, "renamed", driver) //nolint:errcheck // only a panic matters here
	})
}
