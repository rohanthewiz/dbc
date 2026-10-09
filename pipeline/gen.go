package pipeline

import (
	"fmt"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Gen writes a spec as a dbc script that builds and runs the same
// pipeline through the Builder — `dbc pipeline export`, dbc web's Export
// as Go. It is the proof that the canvas draws nothing a script cannot
// do, and the way out of the UI when a pipeline outgrows it: the script
// runs as any other, and can be edited from there on.
//
//	//go:build ignore
//
//	// Yesterday's orders into the warehouse.
//	package main
//
//	import "github.com/rohanthewiz/dbc/sdb"
//
//	func Run(s *sdb.S) error {
//		p := sdb.NewPipeline("orders-nightly").Desc("Yesterday's orders into the warehouse")
//		p.Param("days", "1", "How many days back")
//
//		orders := p.Fragment("orders").Batch(1000)
//		src := orders.Named("src", "sql.read", sdb.Cfg{
//			"conn":  "prod",
//			"query": "SELECT …",
//		})
//		…
//		orders.Wire(src, dst)
//
//		st, err := s.RunPipeline(p, sdb.PipelineOpts{})
//		…
//
// Node ids are kept (Named), so the script's run stats name the same
// nodes the spec's do. A spec holding a Go func (Builder.Func) has no
// text for it and cannot be exported.
func Gen(s *Spec) (string, error) {
	var sb strings.Builder
	names := newNamer("s", "p", "st", "err")
	desc := s.Desc
	if desc == "" {
		desc = "Pipeline " + s.Name + ", exported from dbc."
	}
	fmt.Fprintf(&sb, "//go:build ignore\n\n// %s\n//\n// Written by dbc from the pipeline's spec: an ordinary dbc script, to run\n// or edit as one.\npackage main\n\n", strings.ReplaceAll(desc, "\n", "\n// "))
	sb.WriteString("import \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n")
	fmt.Fprintf(&sb, "\tp := sdb.NewPipeline(%s)", strconv.Quote(s.Name))
	if s.Desc != "" {
		fmt.Fprintf(&sb, ".Desc(%s)", strconv.Quote(s.Desc))
	}
	sb.WriteString("\n")
	params := make([]string, 0, len(s.Params))
	for name := range s.Params {
		params = append(params, name)
	}
	sort.Strings(params)
	for _, name := range params {
		p := s.Params[name]
		if p.Doc != "" {
			fmt.Fprintf(&sb, "\tp.Param(%s, %s, %s)\n", strconv.Quote(name), strconv.Quote(p.Default), strconv.Quote(p.Doc))
		} else {
			fmt.Fprintf(&sb, "\tp.Param(%s, %s)\n", strconv.Quote(name), strconv.Quote(p.Default))
		}
	}
	for _, f := range s.Fragments {
		fv := names.name(f.Name)
		fmt.Fprintf(&sb, "\n\t%s := p.Fragment(%s)", fv, strconv.Quote(f.Name))
		if f.Batch > 0 {
			fmt.Fprintf(&sb, ".Batch(%d)", f.Batch)
		}
		if f.OnError != "" {
			fmt.Fprintf(&sb, ".OnError(%s)", strconv.Quote(f.OnError))
		}
		sb.WriteString("\n")
		vars := map[string]string{}
		for _, n := range f.Nodes {
			if n.Fn != nil {
				return "", serr.New("a pipeline with a Go func node cannot be exported", "fragment", f.Name, "node", n.ID)
			}
			used := len(f.Children(n.ID)) > 0 || f.Parent(n.ID) != ""
			nv := names.name(n.ID)
			vars[n.ID] = nv
			if used {
				fmt.Fprintf(&sb, "\t%s := %s.Named(%s, %s, %s)\n", nv, fv, strconv.Quote(n.ID), strconv.Quote(n.Plugin), cfgLiteral(n.Cfg, 1))
			} else {
				fmt.Fprintf(&sb, "\t%s.Named(%s, %s, %s)\n", fv, strconv.Quote(n.ID), strconv.Quote(n.Plugin), cfgLiteral(n.Cfg, 1))
			}
		}
		for _, e := range f.Edges {
			fmt.Fprintf(&sb, "\t%s.Wire(%s, %s)\n", fv, vars[e.From], vars[e.To])
		}
	}
	sb.WriteString("\n\tst, err := s.RunPipeline(p, sdb.PipelineOpts{})\n\tif err != nil {\n\t\treturn err\n\t}\n\ts.Print(\"%s\", st)\n\treturn nil\n}\n")
	return sb.String(), nil
}

// cfgLiteral is a Config as a Go composite literal, keys sorted, one per
// line, a multi-line value as a raw string when it can be.
func cfgLiteral(cfg Config, indent int) string {
	if len(cfg) == 0 {
		return "nil"
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tab := strings.Repeat("\t", indent)
	var sb strings.Builder
	sb.WriteString("sdb.Cfg{\n")
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s\t%s: %s,\n", tab, strconv.Quote(k), goString(cfg[k]))
	}
	sb.WriteString(tab + "}")
	return sb.String()
}

// goString quotes a value: a raw string for text with newlines and no
// backquote (SQL reads as written), else an interpreted one.
func goString(s string) string {
	if strings.Contains(s, "\n") && !strings.Contains(s, "`") && !strings.Contains(s, "\r") {
		return "`" + s + "`"
	}
	return strconv.Quote(s)
}

// namer makes Go identifiers from spec names, unique in the script.
type namer struct{ taken []string }

func newNamer(reserved ...string) *namer { return &namer{taken: reserved} }

func (n *namer) name(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	id := b.String()
	if id == "" || (id[0] >= '0' && id[0] <= '9') {
		id = "n" + id
	}
	if token.IsKeyword(id) {
		id += "_"
	}
	base := id
	for i := 2; slices.Contains(n.taken, id); i++ {
		id = base + strconv.Itoa(i)
	}
	n.taken = append(n.taken, id)
	return id
}
