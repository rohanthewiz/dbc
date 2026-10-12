//go:build ignore

// Summarize orders by status, then list the big ones.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	r, err := s.Query("shop", `SELECT status, count(*) AS orders, sum(total) AS revenue
		FROM orders GROUP BY status ORDER BY revenue DESC`)
	if err != nil {
		return err
	}
	s.Print("%d statuses in %s", len(r.Rows), r.Duration)
	s.Show(r)

	// Placeholders follow the driver: ? for SQLite/MySQL, $1 for Postgres/bytdb.
	big, err := s.Query("shop", "SELECT id, customer_id, total FROM orders WHERE total >= ? ORDER BY total DESC", 40)
	if err != nil {
		return err
	}
	s.Show(big)
	return nil
}
