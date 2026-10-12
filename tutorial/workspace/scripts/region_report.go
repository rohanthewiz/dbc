//go:build ignore

// Export one CSV per region, plus an HTML overview.
package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

// Knobs live at the top of the file: a script takes no arguments.
const minTotal = 20.0

func Run(s *sdb.S) error {
	regions, err := s.Query("shop", "SELECT DISTINCT coalesce(region, 'none') FROM customers ORDER BY 1")
	if err != nil {
		return err
	}
	for _, row := range regions.Rows {
		if s.Canceled() { // Ctrl+K in the TUI, Ctrl+C headless
			return s.Ctx().Err()
		}
		region := row[0]
		r, err := s.Query("shop", `SELECT o.id, c.email, o.total, o.status
			FROM orders o JOIN customers c ON c.id = o.customer_id
			WHERE coalesce(c.region, 'none') = ? AND o.total >= ?
			ORDER BY o.id`, region, minTotal)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("reports/orders_%s.csv", region)
		if err := s.Export(r, "csv", name); err != nil { // relative → files_dir
			return err
		}
		s.Print("%-5s %d orders → %s", region, len(r.Rows), s.Path(name))
	}

	all, err := s.Query("shop", "SELECT coalesce(c.region,'none') AS region, count(*) AS orders, sum(o.total) AS revenue FROM orders o JOIN customers c ON c.id = o.customer_id GROUP BY 1 ORDER BY 1")
	if err != nil {
		return err
	}
	s.Show(all)
	return s.Export(all, "html", "reports/overview.html")
}
