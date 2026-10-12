//go:build ignore

// Adds a tier column (small, medium, large) from a numeric column's value.
package main

import (
	"fmt"
	"strconv"

	"github.com/rohanthewiz/dbc/sdb"
)

var Plugin = sdb.Plugin{
	Name:  "order.tier",
	Kind:  sdb.KindTransform,
	Label: "Order tier",
	Fields: []sdb.Field{
		{Name: "column", Type: sdb.FieldString, Required: true, Doc: "The numeric column to grade."},
		{Name: "medium", Type: sdb.FieldString, Default: "50", Doc: "Lowest value that counts as medium."},
		{Name: "large", Type: sdb.FieldString, Default: "200", Doc: "Lowest value that counts as large."},
	},
}

// Package-level state is per node: every node gets its own interpreter.
var medium, large float64

// Open runs once per node before the first batch: parse settings here.
func Open(e *sdb.Env) error {
	var err error
	if medium, err = strconv.ParseFloat(e.Cfg.Str("medium", "50"), 64); err != nil {
		return fmt.Errorf("medium: %w", err)
	}
	if large, err = strconv.ParseFloat(e.Cfg.Str("large", "200"), 64); err != nil {
		return fmt.Errorf("large: %w", err)
	}
	return nil
}

// Apply runs once per batch.
func Apply(e *sdb.Env, b *sdb.Batch) (*sdb.Batch, error) {
	col := e.Cfg.Str("column", "")
	if b.Col(col) < 0 {
		return nil, fmt.Errorf("no column %q", col)
	}
	b.AddCol("tier", "text", func(row int) any {
		var v float64
		switch n := b.Get(row, col).(type) {
		case float64:
			v = n
		case int64:
			v = float64(n)
		default:
			return nil // NULL or not a number
		}
		switch {
		case v >= large:
			return "large"
		case v >= medium:
			return "medium"
		}
		return "small"
	})
	return b, nil
}
