// Package script runs user Go scripts with the yaegi interpreter, exposing
// the sdb API plus the Go standard library.
package script

import (
	"fmt"
	"reflect"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/serr"
)

// Run interprets the Go script at path and invokes its Run(s *sdb.S) error.
func Run(path string, s *sdb.S) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = serr.New("script panicked", "script", path, "panic", fmt.Sprint(r))
		}
	}()

	i := interp.New(interp.Options{})
	if err = i.Use(stdlib.Symbols); err != nil {
		return serr.Wrap(err, "phase", "load stdlib symbols")
	}
	if err = i.Use(interp.Exports{
		"github.com/rohanthewiz/dbc/sdb/sdb": {
			"S":          reflect.ValueOf((*sdb.S)(nil)),
			"Result":     reflect.ValueOf((*model.Result)(nil)),
			"IsCanceled": reflect.ValueOf(sdb.IsCanceled),
		},
	}); err != nil {
		return serr.Wrap(err, "phase", "load sdb symbols")
	}

	if _, err = i.EvalPath(path); err != nil {
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
	if err = fn(s); err != nil {
		return serr.Wrap(err, "script", path, "phase", "run")
	}
	return nil
}
