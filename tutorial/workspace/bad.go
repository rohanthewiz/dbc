//go:build ignore

// A script with the interpreter pitfall dbc script --check warns about.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	byID := map[string]string{}
	r, err := s.Query("shop", "SELECT id, email FROM customers")
	if err != nil {
		return err
	}
	for _, row := range r.Rows {
		byID[row[0]], _ = any(row[1]).(string) // dropped by the interpreter
	}
	s.Print("%d emails kept of %d", len(byID), len(r.Rows))
	return nil
}
