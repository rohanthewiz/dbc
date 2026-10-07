//go:build ignore

// gen writes api.json: the sdb package described for the script editor
// (see sdbapi.go). Run through go generate, from this directory:
//
//	go generate ./sdb/sdbapi
package main

import (
	"fmt"
	"os"

	"github.com/rohanthewiz/dbc/sdb/sdbapi"
)

func main() {
	api, err := sdbapi.Build("../..")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdbapi:", err)
		os.Exit(1)
	}
	b, err := sdbapi.Encode(api)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sdbapi:", err)
		os.Exit(1)
	}
	if err = os.WriteFile("api.json", b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "sdbapi:", err)
		os.Exit(1)
	}
}
