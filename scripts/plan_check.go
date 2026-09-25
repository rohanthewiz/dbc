// Sample dbc script: a query-plan check. Explain the statements that matter,
// print each plan with dbc's findings, and fail when any finding is a warning
// or worse — run it in CI (`dbc script scripts/plan_check.go`) to catch a
// query that starts scanning a big table before it reaches production.
//go:build ignore

package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

// the queries whose plans are guarded, and the connection they run on
const conn = "demo-sqlite"

var queries = []string{
	"SELECT name, breed FROM cats WHERE id = 3",
	"SELECT breed, count(*) AS n FROM cats GROUP BY breed ORDER BY n DESC",
}

func Run(s *sdb.S) error {
	bad := 0
	for _, q := range queries {
		// false: explain only — true would also run and time each query
		p, err := s.Explain(conn, q, false)
		if err != nil {
			return err
		}
		s.Print("%s\n%s", q, p.Text(sdb.PlanText{Width: 100, Insights: true}))
		for _, in := range p.Insights {
			if in.Severity != "info" {
				bad++
			}
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d plan finding(s) need a look", bad)
	}
	s.Print("all %d plans look fine", len(queries))
	return nil
}
