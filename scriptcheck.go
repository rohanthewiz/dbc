package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/script"
)

// `dbc script --check NAME|PATH…` checks scripts without running them
// (script.Check: parse, Run's signature, the yaegi#1655 lint, and yaegi's
// compile pass), the same check the editor will run as the user types. It
// is meant for CI over a scripts repo as much as for a shell:
//
//	dbc script --check copy_mytable            one script by name
//	dbc script --check scripts/*.go            every file the shell expands
//	dbc script --check -t json nightly.go      diags as JSON, for tooling
//
// Output is a compiler's, one line per problem, on stdout:
//
//	/home/me/.config/dbc/scripts/nightly.go:12:5: undefined: foo
//	/home/me/.config/dbc/scripts/nightly.go:20:3: warning: the script interpreter drops …
//
// and nothing for a clean script, as go vet. Exit 1 when any script has an
// error; warnings alone exit 0, since a warning is legal Go that might be
// exactly what was meant. A name that finds no script is a usage error
// (exit 2), before anything is checked.
//
// Only the config is read, for scripts_dir: nothing is connected.

// checkedDiag is one diag with the file it is in, the JSON form's row.
type checkedDiag struct {
	File string `json:"file"`
	script.Diag
}

// checkScriptFlags refuses the root flags --check would ignore, as
// checkCopyFlags does for copy: a flag that does nothing reads as a bug.
func checkScriptFlags() error {
	switch {
	case flagConn != "":
		return errors.New("--check runs nothing, so -c does not apply: a script names its own connections")
	case flagOut != "":
		return errors.New("--check prints diagnostics; --out does not apply (redirect stdout)")
	case flagFormat != "" && flagFormat != "text" && flagFormat != "json":
		return errors.New("--check prints text (the default) or json; --format " + flagFormat + " does not apply")
	}
	return nil
}

// scriptCheckAction is `dbc script --check`.
func scriptCheckAction(args []string) {
	refuseQueryFlags("script --check")
	if err := checkScriptFlags(); err != nil {
		usage(err.Error())
	}
	if len(args) == 0 {
		usage("usage: dbc script --check <file.go|NAME>...")
	}
	demo, err := config.ParseDemoEngine(flagDemo)
	if err != nil {
		fail(err, "bad --demo engine")
	}
	cfg, err := config.LoadDemo(flagConfig, demo)
	if err != nil {
		fail(err, "could not load config")
	}
	warnConfig(cfg)
	// every argument is found before any is checked, so a typo in the
	// fifth name does not come after four scripts' worth of output
	paths := make([]string, len(args))
	for i, a := range args {
		if paths[i], err = cfg.FindScript(a); err != nil {
			usage(err.Error())
		}
	}
	bad, err := checkScripts(paths, flagFormat == "json", os.Stdout)
	if err != nil {
		fail(err, "check failed")
	}
	if bad {
		os.Exit(1)
	}
}

// checkScripts checks each file and writes what it finds to w, as compiler
// lines or (asJSON) one JSON array of every file's diags — [] when all are
// clean, so a consumer always gets a document. It reports whether any diag
// is an error. A file that cannot be read is an error for the run (err).
func checkScripts(paths []string, asJSON bool, w io.Writer) (bad bool, err error) {
	all := []checkedDiag{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			return false, err
		}
		diags := script.Check(p, string(src))
		bad = bad || script.HasError(diags)
		for _, d := range diags {
			if asJSON {
				all = append(all, checkedDiag{File: p, Diag: d})
			} else {
				fmt.Fprintf(w, "%s:%s\n", p, d)
			}
		}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return bad, enc.Encode(all)
	}
	return bad, nil
}
