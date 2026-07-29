// Sample dbc script: loop over params and export each result to CSV,
// plus one combined HTML report of all adopted cats.
//go:build ignore

package main

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	for _, breed := range []string{"Tabby", "Siamese", "Maine Coon"} {
		r, err := s.Query("demo",
			"SELECT id, name, age, adopted FROM cats WHERE breed = ? ORDER BY name", breed)
		if err != nil {
			return err
		}
		file := fmt.Sprintf("out_%s.csv", strings.ReplaceAll(strings.ToLower(breed), " ", "_"))
		if err = s.Export(r, "csv", file); err != nil {
			return err
		}
		s.Print("wrote %s (%d rows)", file, len(r.Rows))
	}

	adopted, err := s.Query("demo",
		"SELECT name, breed, age FROM cats WHERE adopted = ? ORDER BY breed, name", 1)
	if err != nil {
		return err
	}
	if err = s.Export(adopted, "html", "adopted_report.html"); err != nil {
		return err
	}
	s.Print("wrote adopted_report.html (%d rows)", len(adopted.Rows))
	s.Show(adopted)
	return nil
}
