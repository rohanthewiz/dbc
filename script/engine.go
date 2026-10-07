// Package script runs user Go scripts with the yaegi interpreter, exposing
// the sdb API plus the Go standard library.
package script

import (
	"fmt"
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/serr"
)

// newInterp is the interpreter every script gets: the standard library plus
// the sdb API, and nothing else. Run and Check (check.go) both start from
// it, so a script Check passes imports exactly what Run will give it; two
// hand-kept copies of this list would drift the first time sdb grew a type.
func newInterp() (*interp.Interpreter, error) {
	i := interp.New(interp.Options{})
	if err := i.Use(stdlib.Symbols); err != nil {
		return nil, serr.Wrap(err, "phase", "load stdlib symbols")
	}
	if err := i.Use(interp.Exports{
		"github.com/rohanthewiz/dbc/sdb/sdb": {
			"S":          reflect.ValueOf((*sdb.S)(nil)),
			"Result":     reflect.ValueOf((*model.Result)(nil)),
			"Plan":       reflect.ValueOf((*explain.Plan)(nil)),
			"PlanText":   reflect.ValueOf((*explain.TextOptions)(nil)),
			"IsCanceled": reflect.ValueOf(sdb.IsCanceled),
			// ETL across connections (sdb/etl.go)
			"CopyOpts":  reflect.ValueOf((*sdb.CopyOpts)(nil)),
			"CopyStats": reflect.ValueOf((*sdb.CopyStats)(nil)),
			"Reader":    reflect.ValueOf((*sdb.Reader)(nil)),
			"Writer":    reflect.ValueOf((*sdb.Writer)(nil)),
			"WriteOpts": reflect.ValueOf((*sdb.WriteOpts)(nil)),
		},
	}); err != nil {
		return nil, serr.Wrap(err, "phase", "load sdb symbols")
	}
	return i, nil
}

// Run interprets the Go script at path and invokes its Run(s *sdb.S) error.
func Run(path string, s *sdb.S) error {
	return run(path, s, func(i *interp.Interpreter) error {
		_, err := i.EvalPath(path)
		return err
	})
}

// RunSource is Run for a script that is text rather than a file: a sample
// built into the binary, run by `dbc script NAME` without being copied out
// first. name stands in for the path in errors.
func RunSource(name, src string, s *sdb.S) error {
	return run(name, s, func(i *interp.Interpreter) error {
		_, err := i.Eval(src)
		return err
	})
}

// run is Run and RunSource after the one step they differ in: load
// evaluates the script's file or text into a fresh interpreter, then
// main.Run is looked up, checked and called.
func run(path string, s *sdb.S, load func(*interp.Interpreter) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = serr.New("script panicked", "script", path, "panic", fmt.Sprint(r))
		}
	}()

	i, err := newInterp()
	if err != nil {
		return err
	}

	if err = load(i); err != nil {
		return serr.Wrap(err, "script", path, "phase", "compile")
	}
	v, err := i.Eval("main.Run")
	if err != nil {
		return serr.Wrap(err, "script", path,
			"hint", "script must define: func Run(s *sdb.S) error")
	}
	fn, ok := v.Interface().(func(*sdb.S) error)
	if !ok {
		return serr.New("Run has the wrong signature", "script", path,
			"want", "func Run(s *sdb.S) error", "got", v.Type().String())
	}
	// Registered after the recover above, so it runs first on a panic too:
	// whatever the script left open is rolled back / closed before the
	// panic becomes an error.
	defer s.Release()
	if err = fn(s); err != nil {
		return serr.Wrap(err, "script", path, "phase", "run")
	}
	return nil
}
