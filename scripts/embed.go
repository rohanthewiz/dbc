// Package scripts carries dbc's sample scripts and new-script templates
// inside the binary, so a fresh install has working examples without a
// checkout, and they never go stale against the sdb API of the binary
// that ships them.
//
// The files beside this one are the samples (named in the embed directive
// below). Each carries //go:build
// ignore and is package main, so `go build ./...` never meets them and
// this package (scripts) never mixes with theirs. The templates are in
// templates/, built the same way.
//
//	scripts/
//	    embed.go            ◄─ this package
//	    copy_table.go       ◄─ an Example: read-only, Duplicate copies it out
//	    loop_params.go
//	    …
//	    templates/
//	        blank.go        ◄─ a Template: what New starts from
//	        copy.go
//	        …
//
// Examples are listed after the user's own scripts and never written to
// (the scripts store, userdata/scripts.go, only touches scripts_dir).
// A template is filled with connection names on the way out (Fill): it
// says "{{conn}}" and "{{conn2}}" inside string literals, so the template
// file stays valid Go that Check passes as it is.
package scripts

import (
	"embed"
	"encoding/json"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/userdata"
)

// The samples, by name, and the templates. Named rather than *.go, which
// would also take this file and the package's _test.go files into the
// binary; TestSamplesAllEmbedded fails when a sample is added on disk but
// not here.
//
//go:embed copy_table.go export_report.go loop_params.go plan_check.go sweep_conns.go
//go:embed templates/*.go
//go:embed pipelines/*.json
//go:embed jobs/*.json
var files embed.FS

// Example is one built-in sample script.
type Example struct {
	Name string `json:"name"` // file name, with .go
	Desc string `json:"desc"` // as userdata.DescOf reads it, less the "Sample dbc script: " lead
	Text string `json:"-"`    // the source; large, so not in a listing
}

// samplePrefix opens every sample's description; in a list headed
// "Examples" it says nothing.
const samplePrefix = "Sample dbc script: "

// Examples lists the built-in samples by name.
func Examples() []Example {
	ents, _ := fs.ReadDir(files, ".")
	var out []Example
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		bs, err := files.ReadFile(e.Name())
		if err != nil {
			continue
		}
		out = append(out, Example{Name: e.Name(), Desc: exampleDesc(bs), Text: string(bs)})
	}
	slices.SortFunc(out, func(a, b Example) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ExampleByName is the sample called name, with its text.
func ExampleByName(name string) (Example, bool) {
	for _, ex := range Examples() {
		if ex.Name == name {
			return ex, true
		}
	}
	return Example{}, false
}

// exampleDesc is a sample's description: its first sentence, without the
// lead every sample shares, and capitalised again.
func exampleDesc(src []byte) string {
	d := strings.TrimPrefix(userdata.DescOf(src), samplePrefix)
	if d != "" {
		d = strings.ToUpper(d[:1]) + d[1:]
	}
	return d
}

// IsExample reports whether src is, byte for byte apart from line endings,
// one of the built-in samples: a copy that holds nothing the binary does
// not already carry. config's warning about the old ./scripts directory
// uses it, so a dbc checkout (whose ./scripts is these very samples) is
// not told to move them.
func IsExample(src []byte) bool {
	norm := func(b []byte) string { return strings.ReplaceAll(string(b), "\r\n", "\n") }
	s := norm(src)
	return slices.ContainsFunc(Examples(), func(ex Example) bool { return norm([]byte(ex.Text)) == s })
}

// Template is one starting point for a new script.
type Template struct {
	Name  string `json:"name"`  // its key, e.g. "copy"
	Title string `json:"title"` // a few words, for a menu
	Desc  string `json:"desc"`  // its description line: what the new script will say it does
}

// templates is the menu, in the order it is offered: the file under
// templates/ for each Name, and the Title shown for it. TestTemplates
// checks the two agree, so a file without a row (or a row without a file)
// fails the build's tests.
var templates = []Template{
	{Name: "blank", Title: "Blank script"},
	{Name: "query", Title: "Query and show"},
	{Name: "loop", Title: "Loop over parameters"},
	{Name: "copy", Title: "Copy a table"},
	{Name: "export", Title: "Export a report"},
}

// Templates lists the templates in menu order, each with its description.
func Templates() []Template {
	out := slices.Clone(templates)
	for i := range out {
		if bs, err := files.ReadFile(templatePath(out[i].Name)); err == nil {
			out[i].Desc = userdata.DescOf(bs)
		}
	}
	return out
}

func templatePath(name string) string { return "templates/" + name + ".go" }

// Placeholder connection names, for a config with fewer connections than
// a template wants. They fail a run with "unknown connection", which names
// the line to edit.
const (
	noConn  = "my-connection"
	noConn2 = "other-connection"
)

// Fill is template name's text with connection names put in: "{{conn}}"
// becomes the first of conns and "{{conn2}}" the second (the copy
// template's source and destination). Each is written as a Go string
// literal (strconv.Quote), so a name with a quote or backslash in it still
// makes a script that compiles. Missing names become placeholders.
func Fill(name string, conns []string) (string, bool) {
	if !slices.ContainsFunc(templates, func(t Template) bool { return t.Name == name }) {
		return "", false
	}
	bs, err := files.ReadFile(templatePath(name))
	if err != nil {
		return "", false
	}
	c1, c2 := noConn, noConn2
	if len(conns) > 0 {
		c1 = conns[0]
	}
	if len(conns) > 1 {
		c2 = conns[1]
	}
	r := strings.NewReplacer(`"{{conn}}"`, strconv.Quote(c1), `"{{conn2}}"`, strconv.Quote(c2))
	return r.Replace(string(bs)), true
}

// Pipeline is one built-in sample pipeline (pipelines/*.json): read-only,
// listed after the user's own as the script examples are, and run by name
// with `dbc pipeline run NAME` when no pipeline of the user's shadows it.
type Pipeline struct {
	Name string `json:"name"` // file name, with .json
	Desc string `json:"desc"` // the spec's own desc
	Text string `json:"-"`
}

// Pipelines lists the built-in sample pipelines by name.
func Pipelines() []Pipeline {
	ents, _ := fs.ReadDir(files, "pipelines")
	var out []Pipeline
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		bs, err := files.ReadFile("pipelines/" + e.Name())
		if err != nil {
			continue
		}
		var head struct {
			Desc string `json:"desc"`
		}
		_ = json.Unmarshal(bs, &head)
		out = append(out, Pipeline{Name: e.Name(), Desc: head.Desc, Text: string(bs)})
	}
	slices.SortFunc(out, func(a, b Pipeline) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// PipelineByName is the sample pipeline called name, with its text.
func PipelineByName(name string) (Pipeline, bool) {
	for _, p := range Pipelines() {
		if p.Name == name {
			return p, true
		}
	}
	return Pipeline{}, false
}

// Job is one built-in sample job (jobs/*.json): a DAG of the sample
// pipelines, read-only and runnable by name like the pipelines. Examples
// are never scheduled — only a job in jobs_dir is — so the sample's cron
// line is there to copy, not to fire on every machine that runs dbc web.
type Job struct {
	Name string `json:"name"` // file name, with .json
	Desc string `json:"desc"`
	Text string `json:"-"`
}

// Jobs lists the built-in sample jobs by name.
func Jobs() []Job {
	ents, _ := fs.ReadDir(files, "jobs")
	var out []Job
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		bs, err := files.ReadFile("jobs/" + e.Name())
		if err != nil {
			continue
		}
		var head struct {
			Desc string `json:"desc"`
		}
		_ = json.Unmarshal(bs, &head)
		out = append(out, Job{Name: e.Name(), Desc: head.Desc, Text: string(bs)})
	}
	slices.SortFunc(out, func(a, b Job) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// JobByName is the sample job called name, with its text.
func JobByName(name string) (Job, bool) {
	for _, j := range Jobs() {
		if j.Name == name {
			return j, true
		}
	}
	return Job{}, false
}
