// Run one parameterized query for each of a list of values.
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// conn is the connection to query: a name from your config.
const conn = "{{conn}}"

func Run(s *sdb.S) error {
	for _, n := range []int{1, 2, 3} {
		// Stop (Ctrl+K) cancels the running query; checking between
		// queries ends the loop promptly too.
		if s.Canceled() {
			return nil
		}
		// $1 on Postgres; write ? for MySQL and SQLite
		r, err := s.Query(conn, "SELECT $1 AS n", n)
		if err != nil {
			return err
		}
		s.Print("n=%d → %d row(s)", n, len(r.Rows))
		s.Show(r)
	}
	return nil
}
