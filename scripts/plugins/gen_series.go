//go:build ignore

// Makes a column of whole numbers, from one value to another: rows to load,
// to join against, or to test a pipeline with before the real source is
// ready.
//
// A pipeline plugin: copy it into plugins_dir (~/.config/dbc/plugins) and
// gen.series joins the palette under Yours, and `dbc plugins`.
package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{
	Name:  "gen.series",
	Kind:  sdb.KindSource,
	Label: "Number series",
	Fields: []sdb.Field{
		{Name: "from", Type: sdb.FieldInt, Default: "1", Doc: "The first number."},
		{Name: "to", Type: sdb.FieldInt, Required: true, Doc: "The last number (included)."},
		{Name: "step", Type: sdb.FieldInt, Default: "1", Doc: "The gap between two numbers; positive."},
		{Name: "column", Type: sdb.FieldString, Default: "n", Doc: "The column's name."},
	},
}

// A plugin keeps its state in package-level vars: every node gets an
// interpreter of its own, so two gen.series nodes never share them.
var next, to, step int64

// Open is called once, before the first Next.
func Open(e *sdb.Env) error {
	from, err := e.Cfg.Int("from", 1)
	if err != nil {
		return err
	}
	last, err := e.Cfg.Int("to", 0)
	if err != nil {
		return err
	}
	by, err := e.Cfg.Int("step", 1)
	if err != nil {
		return err
	}
	if by <= 0 {
		return fmt.Errorf("step must be positive, not %d", by)
	}
	next, to, step = int64(from), int64(last), int64(by)
	return nil
}

// Next returns the next batch — at most e.Batch rows — or nil at the end.
func Next(e *sdb.Env) (*sdb.Batch, error) {
	if next > to {
		return nil, nil
	}
	// INT8 tells a sink creating the table that this is a whole number
	b := sdb.NewBatch(sdb.ColsOf([]string{e.Cfg.Str("column", "n")}, []string{"INT8"}), nil)
	for len(b.Rows) < e.Batch && next <= to {
		b.Rows = append(b.Rows, []any{next})
		next += step
	}
	return b, nil
}
