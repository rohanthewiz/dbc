// Copy a table from one connection to another.
//
// Create makes the table on the destination when it is missing, with the
// source's column types, NOT NULLs and primary key; Truncate replaces what
// a previous run left there. The destination is loaded in one transaction,
// so a failed or stopped copy leaves it as it was.
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// The source and destination connections (names from your config), and
// the table to copy: schema-qualified is fine.
const (
	from  = "{{conn}}"
	to    = "{{conn2}}"
	table = "public.orders"
)

func Run(s *sdb.S) error {
	st, err := s.Copy(from, to, table, sdb.CopyOpts{
		Create:        true,
		Truncate:      true,
		ProgressEvery: 100_000, // a progress line per 100k rows
	})
	if err != nil {
		if sdb.IsCanceled(err) {
			s.Print("stopped: %s is unchanged", to)
			return nil
		}
		return err
	}
	s.Print("%s", st)
	return nil
}
