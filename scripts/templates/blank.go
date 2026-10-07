// Describe what this script does in one sentence: it shows in the scripts list.
//
// Run it from the scripts list (Ctrl+O), or `dbc script NAME` from a shell.
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// Run is what dbc calls. Return an error to fail the run; s.Print writes to
// the log, s.Show puts a result in the grid.
func Run(s *sdb.S) error {
	s.Print("connections: %v", s.Conns())
	return nil
}
