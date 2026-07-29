// Sample dbc script: run the same health-check query on every configured
// connection — handy for comparing hosts.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for _, conn := range s.Conns() {
		r, err := s.Query(conn, "SELECT 1 AS ok")
		if err != nil {
			s.Print("%s: ERROR %v", conn, err)
			continue
		}
		s.Print("%s: ok (%s)", conn, r.Duration)
	}
	return nil
}
