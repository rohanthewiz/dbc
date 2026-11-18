package designer

import (
	"errors"
	"testing"
	"time"
)

// FuzzImport reads fuzzed SQL as a design on the shop schema, per driver
// (N-302). It wants:
//
//   - no panic, and no fatal stack overflow (N-259's depth cap);
//   - a refusal that is an *ImportError, its position inside the SQL;
//   - an accepted design that Analyze writes without panicking, and whose
//     statement reads back to a design writing that same statement — the
//     round trip TestImportRoundTrip holds for the generator's own
//     statements, here for whatever SQL got in.
//
// Run at length by hand:
//
//	go test -run '^$' -fuzz '^FuzzImport$' -fuzztime 5m ./designer
func FuzzImport(f *testing.F) {
	for _, s := range []string{
		"SELECT id FROM orders",
		"select c.name, sum(o.total) as spent from customers c join orders o on o.customer_id = c.id group by c.name",
		"SELECT c.name FROM customers c WHERE c.id IN (SELECT customer_id FROM tickets WHERE subject LIKE 'refund%')",
		"SELECT c.name FROM customers c LEFT OUTER JOIN orders o ON o.customer_id = c.id WHERE o.id IS NULL",
		"SELECT c.name, o.id FROM orders o RIGHT JOIN customers c ON c.id = o.customer_id",
		"SELECT `o`.`id` FROM orders o WHERE lower(o.ref) LIKE lower('%x\\_y%') AND o.created_at >= NOW() - INTERVAL 30 DAY LIMIT 5, 10",
		"SELECT status, count(1) FROM orders WHERE ref ILIKE '%ab' AND created_at >= now() - interval '7 days' GROUP BY 1 ORDER BY 2 DESC",
		"SELECT country FROM customers GROUP BY country",
		"SELECT * FROM payments WHERE paid_on >= date('now', '-14 days') LIMIT -1 OFFSET 3",
		"SELECT o.* FROM orders o WHERE NOT EXISTS (SELECT 1 FROM payments p WHERE p.order_id = o.id)",
		"SELECT id FROM orders WHERE (total > 1 OR total IS NULL) AND created_at >= '2024-01-01' AND created_at < '2025-01-01'",
		`SELECT "User" FROM "order" o`,
		"SELECT id FROM orders WHERE id = ((((1))))",
		"WITH a AS (SELECT 1) SELECT * FROM a",
		"SELECT id FROM orders; SELECT 2",
		"",
	} {
		for d := range uint8(4) {
			f.Add(s, d)
		}
	}
	drivers := []string{"postgres", "mysql", "sqlite", "bytdb"}
	// relative dates ("in the last 7 days") are written from now; a fixed
	// now keeps a read-back from straddling a midnight
	now = func() time.Time { return time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC) }
	f.Cleanup(func() { now = time.Now })
	f.Fuzz(func(t *testing.T, sql string, dn uint8) {
		cat := shopCat()
		cat.Driver = drivers[int(dn)%len(drivers)]
		d, _, err := Import(sql, cat)
		if err != nil {
			var ie *ImportError
			if !errors.As(err, &ie) {
				t.Fatalf("%s: a refusal that is not an *ImportError: %T %v", cat.Driver, err, err)
			}
			if ie.At > len(sql) || (ie.At >= 0 && ie.Line < 1) {
				t.Fatalf("%s: refusal at %d (line %d) of %d bytes", cat.Driver, ie.At, ie.Line, len(sql))
			}
			return
		}
		want := Analyze(d, cat).SQL
		if want == "" {
			return
		}
		back, _, err := Import(want, cat)
		if err != nil {
			t.Fatalf("%s: the statement written from an accepted import is refused: %v\nin:  %q\nout: %s",
				cat.Driver, err, sql, want)
		}
		if got := Analyze(back, cat).SQL; got != want {
			t.Fatalf("%s: read back, it writes another statement\nin:   %q\nfirst:\n%s\nagain:\n%s",
				cat.Driver, sql, want, got)
		}
	})
}
