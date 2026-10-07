package sdbapi

import (
	"encoding/json"
	"fmt"
	"go/doc"
	"strings"
	"sync"
)

// Summary is the sdb API as plain text for the assistant: what a script
// can call and the fields of what it gets back, each with the first
// sentence of its doc. It goes with the first question a conversation asks
// from a script tab, so the model writes s.Query(conn, …) against the real
// signatures instead of guessing at a database/sql-shaped API.
//
// Why a summary and not api.json: the JSON is ~35 KB, most of it full doc
// comments written for hover, and the model is sent this once per
// conversation on the user's tokens. Signatures plus one sentence each are
// what writing a call needs; the rest is a `go doc` away.
//
//	package sdb — …
//
//	func IsCanceled(err error) bool — reports whether …
//
//	type S — …
//	  func (s *S) Query(conn, query string, args ...any) (*Result, error) — …
//
//	type CopyOpts = etl.CopyOptions — …
//	  To string — names the destination table; …
//	  func (r *Reader) Next() …            (methods of the other types: signature only)
//
// It is built once from the embedded JSON, which is generated from the
// source (see the package comment), so it cannot drift either.
func Summary() string { return summary() }

var summary = sync.OnceValue(func() string {
	var api API
	if err := json.Unmarshal(JSON(), &api); err != nil {
		// the embedded file is checked by TestAPIUpToDate; this cannot
		// happen in a built binary, and an empty summary only costs the
		// model its guide
		return ""
	}
	return render(&api)
})

// render writes api as Summary describes.
func render(api *API) string {
	var sb strings.Builder
	line := func(indent, head, docText string) {
		sb.WriteString(indent)
		sb.WriteString(head)
		if s := synopsis(docText); s != "" {
			sb.WriteString(" — ")
			sb.WriteString(s)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "package %s — %s\n", api.Package, synopsis(api.Doc))
	sb.WriteString("A script is a Go file (package main) with func Run(s *sdb.S) error; " +
		"it imports \"github.com/rohanthewiz/dbc/sdb\" and the standard library only.\n\n")
	for _, f := range api.Funcs {
		line("", f.Sig, f.Doc)
	}
	for _, t := range api.Types {
		sb.WriteString("\n")
		head := "type " + t.Name
		if t.Of != "" {
			head += " = " + t.Of
		}
		line("", head, t.Doc)
		for _, f := range t.Fields {
			line("  ", f.Name+" "+f.Type, f.Doc)
		}
		for _, m := range t.Methods {
			// S's methods are the API itself and keep their sentence; the
			// other types' (Plan alone has nineteen) are listed by
			// signature, which says what they do well enough to call them
			if t.Name == "S" {
				line("  ", m.Sig, m.Doc)
			} else {
				line("  ", m.Sig, "")
			}
		}
	}
	return sb.String()
}

// synopsis is a doc comment's first sentence, on one line — go/doc's rule,
// so "e.g." and the like do not cut it short.
func synopsis(text string) string {
	return new(doc.Package).Synopsis(text)
}
