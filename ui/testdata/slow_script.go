// Test fixture: a script that keeps the connection busy until it is stopped.
// Files under testdata are invisible to the go tool, so no build tag is needed.

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for i := 0; i < 100; i++ {
		s.Print("pass %d", i)
		_, err := s.Query("demo", `WITH RECURSIVE c(x) AS (
			SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 2000000000
		) SELECT count(*) AS n FROM c`)
		if err != nil {
			if sdb.IsCanceled(err) {
				s.Print("canceled cleanly")
			}
			return err
		}
	}
	return nil
}
