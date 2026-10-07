// Run a report query and export it as CSV and as an HTML page.
//
// A relative path is relative to the directory dbc runs in; write an
// absolute one to be sure where the files land.
//
// The ignore tag keeps `go build` off script files; dbc runs them fine.
//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// conn is the connection to query: a name from your config.
const conn = "{{conn}}"

func Run(s *sdb.S) error {
	r, err := s.Query(conn, "SELECT 1 AS id, 'example' AS name")
	if err != nil {
		return err
	}
	// formats: csv, tsv, markdown, html, json, text
	for _, f := range []struct{ format, path string }{
		{"csv", "report.csv"},
		{"html", "report.html"},
	} {
		if err := s.Export(r, f.format, f.path); err != nil {
			return err
		}
		s.Print("wrote %s (%d rows)", f.path, len(r.Rows))
	}
	s.Show(r)
	return nil
}
