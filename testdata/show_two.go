//go:build ignore

package main

import "github.com/rohanthewiz/dbc/sdb"

// Run shows two results and prints a line between them, so a test can tell
// the data stream apart from the progress stream.
func Run(s *sdb.S) error {
	for _, breed := range []string{"Tabby", "Siamese"} {
		r, err := s.Query("demo",
			"SELECT name FROM cats WHERE breed = ? ORDER BY name", breed)
		if err != nil {
			return err
		}
		s.Print("breed %s -> %d cats", breed, len(r.Rows))
		s.Show(r)
	}
	return nil
}
