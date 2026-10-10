// Run a report query and export it as CSV and as an HTML page.
//
// A relative path is in files_dir — your home directory unless the
// config sets it — whether a shell, dbc web or a schedule runs the
// script; s.Path says where. A file the script opens itself goes through
// s.Path too: os.Create(s.Path("notes.txt")).
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
		s.Print("wrote %s (%d rows)", s.Path(f.path), len(r.Rows))
	}
	s.Show(r)
	return nil
}
