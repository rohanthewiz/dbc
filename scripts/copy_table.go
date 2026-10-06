// Sample dbc script: copy a Postgres table from one connection to another,
// then a small extract-transform-load across the two.
//
// Edit the constants to name two connections from your config, then:
//
//	dbc script scripts/copy_table.go     (or Ctrl+O in the TUI)
//
// Postgres to Postgres with no Transform streams COPY to COPY — the rows are
// never decoded, so it runs at the speed of the servers and the network. The
// destination is loaded in one transaction with its CREATE and TRUNCATE, so
// a failed or stopped copy (Ctrl+K) leaves it exactly as it was.
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import (
	"strings"
	"time"

	"github.com/rohanthewiz/dbc/sdb"
)

const (
	from  = "prod-pg"       // source connection
	to    = "local-pg"      // destination connection
	table = "public.orders" // schema-qualified is fine
)

func Run(s *sdb.S) error {
	// 1. The whole table. Create makes it on the destination when missing,
	//    with the source's column types, NOT NULLs and primary key; Truncate
	//    replaces whatever a previous run left there.
	st, err := s.Copy(from, to, table, sdb.CopyOpts{
		Create:        true,
		Truncate:      true,
		ProgressEvery: 250_000, // a progress line per 250k rows
	})
	if err != nil {
		if sdb.IsCanceled(err) {
			s.Print("stopped — %s is unchanged", to)
			return nil
		}
		return err
	}
	s.Print("%s", st)

	// 2. A slice of it, reshaped on the way: Where (with Args) picks the
	//    rows, Transform edits each one in Go — or drops it by returning nil.
	//    A Transform makes the copy go row by row instead of COPY to COPY.
	st, err = s.Copy(from, to, table, sdb.CopyOpts{
		To:      "public.orders_recent",
		Columns: []string{"id", "customer_email", "total", "created_at"},
		Where:   "created_at >= now() - $1::interval",
		Args:    []any{"30 days"},
		Create:  true, Truncate: true,
		Transform: func(row []any) ([]any, error) {
			if email, ok := row[1].(string); ok {
				row[1] = strings.ToLower(strings.TrimSpace(email))
			}
			return row, nil
		},
	})
	if err != nil {
		return err
	}
	s.Print("%s", st)

	// 3. Anything Copy can't express: stream rows with a Reader, do what you
	//    like with them in Go, load them with a Writer. Here, a join across
	//    the two connections — no single server could run it: each order on
	//    the source gets its customer's region from a table on the
	//    destination. Readers aren't capped at max_rows the way s.Query is.
	regions := map[string]string{}
	lk, err := s.Reader(to, "SELECT lower(email), region FROM public.customers")
	if err != nil {
		return err
	}
	for lk.Next() {
		row := lk.Row()
		// Two steps on purpose: the interpreter (yaegi) silently drops
		// `regions[k], _ = v.(string)` — a comma-ok assertion assigned
		// straight into a map element (traefik/yaegi#1655; any two-value
		// form into a map index is affected).
		email, _ := row[0].(string)
		region, _ := row[1].(string)
		regions[email] = region
	}
	if err := lk.Err(); err != nil {
		return err
	}

	rd, err := s.Reader(from, "SELECT id, customer_email, total FROM "+table+
		" WHERE created_at >= $1", time.Now().AddDate(0, 0, -30))
	if err != nil {
		return err
	}
	defer rd.Close()

	if _, err = s.Exec(to, `CREATE TABLE IF NOT EXISTS public.orders_by_region
		(id bigint PRIMARY KEY, region text NOT NULL, total numeric)`); err != nil {
		return err
	}
	// Nothing is visible on the destination until Close commits; the
	// deferred Abort rolls back on any early return (and is a no-op after
	// Close). A Writer left open when Run returns is rolled back too.
	w, err := s.Writer(to, "public.orders_by_region", []string{"id", "region", "total"},
		sdb.WriteOpts{Truncate: true})
	if err != nil {
		return err
	}
	defer w.Abort()
	for rd.Next() {
		row := rd.Row()
		email, _ := row[1].(string)
		region, ok := regions[strings.ToLower(strings.TrimSpace(email))]
		if !ok {
			region = "unknown"
		}
		if err := w.Write([]any{row[0], region, row[2]}); err != nil {
			return err
		}
	}
	if err := rd.Err(); err != nil {
		return err
	}
	n, err := w.Close()
	if err != nil {
		return err
	}
	s.Print("orders_by_region: %d rows", n)
	return nil
}
