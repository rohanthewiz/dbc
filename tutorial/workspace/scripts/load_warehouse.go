//go:build ignore

// Load the shop into the warehouse: clean customers, then a per-region order fact table.
package main

import (
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	// Level 2: Copy with a Transform. Each row arrives as []any in the
	// Columns order; return the edited row, or nil to skip it.
	st, err := s.Copy("shop", "warehouse", "customers", sdb.CopyOpts{
		Columns:  []string{"id", "email", "region"},
		Create:   true,
		Truncate: true,
		Transform: func(row []any) ([]any, error) {
			email, _ := row[1].(string)
			email = strings.ToLower(strings.TrimSpace(email))
			row[1] = email
			if row[2] == nil {
				row[2] = "unknown"
			}
			return row, nil
		},
	})
	if err != nil {
		return err
	}
	s.Print("%s", st)

	// Level 3: Reader + Writer — a join no single server can run. The
	// lookup comes from the warehouse, the facts from the shop.
	region := map[int64]string{}
	lk, err := s.Reader("warehouse", "SELECT id, region FROM customers")
	if err != nil {
		return err
	}
	for lk.Next() {
		row := lk.Row()
		id, _ := row[0].(int64)
		r, _ := row[1].(string) // into a local first: see "yaegi pitfalls"
		region[id] = r
	}
	if err := lk.Err(); err != nil {
		return err
	}

	// The Writer's table must exist; bytdb wants a primary key.
	if _, err := s.Exec("warehouse", `CREATE TABLE IF NOT EXISTS order_facts
		(order_id bigint PRIMARY KEY, region text NOT NULL, total double precision, paid boolean)`); err != nil {
		return err
	}
	w, err := s.Writer("warehouse", "order_facts",
		[]string{"order_id", "region", "total", "paid"}, sdb.WriteOpts{Truncate: true})
	if err != nil {
		return err
	}
	defer w.Abort() // rolls back on any early return; a no-op after Close

	rd, err := s.Reader("shop", "SELECT id, customer_id, total, status FROM orders ORDER BY id")
	if err != nil {
		return err
	}
	defer rd.Close()
	for rd.Next() {
		row := rd.Row() // typed: int64, float64, string, time.Time, nil, ...
		cust, _ := row[1].(int64)
		paid := row[3] == "paid"
		if err := w.Write([]any{row[0], region[cust], row[2], paid}); err != nil {
			return err
		}
	}
	if err := rd.Err(); err != nil {
		return err
	}
	n, err := w.Close() // the commit: nothing is visible until here
	if err != nil {
		return err
	}
	s.Print("order_facts: %d rows", n)
	return nil
}
