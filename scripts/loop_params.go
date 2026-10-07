// Sample dbc script: run a parameterized query in a loop.
//
// Run headless:  dbc script loop_params   (the built-in copy, by name)
// Or in the TUI: Ctrl+O, Examples, Enter makes your own copy
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	for _, minAge := range []int{1, 3, 5} {
		r, err := s.Query("demo-sqlite",
			"SELECT id, name, breed, age FROM cats WHERE age >= ? ORDER BY age", minAge)
		if err != nil {
			return err
		}
		s.Print("min age %d → %d cats", minAge, len(r.Rows))
		s.Show(r)
	}
	return nil
}
