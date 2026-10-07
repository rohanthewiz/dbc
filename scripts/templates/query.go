// Run a query and show its rows in the grid.
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// conn is the connection to query: a name from your config.
const conn = "{{conn}}"

func Run(s *sdb.S) error {
	// Arguments bind as the driver's parameters: $1 on Postgres, ? on
	// MySQL and SQLite. A result is capped at max_rows, as in the editor.
	r, err := s.Query(conn, "SELECT 1 AS answer")
	if err != nil {
		return err
	}
	s.Print("%d row(s) in %s", len(r.Rows), r.Duration)
	s.Show(r)
	return nil
}
