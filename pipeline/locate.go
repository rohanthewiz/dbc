package pipeline

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// Placing a check's findings in the spec's text. A Diag names WHERE in the
// spec it is ("clean/dst.table"), which is what the canvas needs; an editor
// needs a line and a column. Both hosts that show the text want them: dbc
// web's JSON view puts a marker there, and the TUI prints the finding as a
// compiler does ("orders.json:12:9: …") after $EDITOR, so the editor can
// jump to it.
//
// It is a text search, not a JSON parse with positions: encoding/json keeps
// none, and a spec is small and in the shape Spec.JSON writes, where each
// name, id and key is on a line of its own. A search that misses returns
// (0, 0), which every host takes as "no place": the finding is still shown,
// without one.

// LineCol is the 1-based line and column of byte offset off in text.
func LineCol(text string, off int) (int, int) {
	off = min(max(off, 0), len(text))
	before := text[:off]
	line := strings.Count(before, "\n") + 1
	col := off - strings.LastIndex(before, "\n")
	return line, col
}

// Locate places a Diag's where in the spec's text: the fragment's "name",
// then within it the node's "id", then the field's key.
//
//	where "clean/dst.table" → "name": "clean" … "id": "dst" … "table":
func Locate(text, where string) (int, int) {
	if where == "" {
		return 0, 0
	}
	at := 0
	find := func(needle *regexp.Regexp) bool {
		loc := needle.FindStringIndex(text[at:])
		if loc == nil {
			return false
		}
		at += loc[0]
		return true
	}
	quote := func(s string) string { return regexp.QuoteMeta(strings.ReplaceAll(s, `"`, `\"`)) }
	switch {
	case where == "name" || where == "fragments":
		if !find(regexp.MustCompile(`"` + where + `"\s*:`)) {
			return 0, 0
		}
		return LineCol(text, at)
	case strings.HasPrefix(where, "params."):
		if !find(regexp.MustCompile(`"params"\s*:`)) || !find(regexp.MustCompile(`"`+quote(strings.TrimPrefix(where, "params."))+`"\s*:`)) {
			return 0, 0
		}
		return LineCol(text, at)
	}
	frag, rest, _ := strings.Cut(where, "/")
	frag, edge, isEdge := strings.Cut(frag, ":")
	if !find(regexp.MustCompile(`"name"\s*:\s*"` + quote(frag) + `"`)) {
		return 0, 0
	}
	switch {
	case isEdge:
		// "edge a→b": the fragment's edges
		if from, to, ok := strings.Cut(strings.TrimPrefix(edge, "edge "), "→"); ok &&
			find(regexp.MustCompile(`\[\s*"`+quote(from)+`"\s*,\s*"`+quote(to)+`"\s*\]`)) {
			return LineCol(text, at)
		}
	case rest != "":
		node, field, _ := strings.Cut(rest, ".")
		if find(regexp.MustCompile(`"id"\s*:\s*"` + quote(node) + `"`)) {
			if field != "" {
				_ = find(regexp.MustCompile(`"` + quote(field) + `"\s*:`))
			}
		}
	}
	return LineCol(text, at)
}

// unknownFieldRe finds the key encoding/json names when it refuses one
// (Parse disallows unknown fields).
var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

// ParseErrorAt places an error from Parse (or any strict JSON decode of
// text): a syntax or type error at its offset, an unknown key where the key
// is written. Anything else is put at the start, (1, 1): a parse error
// always has a place to be shown, unlike a check's finding.
func ParseErrorAt(text string, err error) (int, int) {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return LineCol(text, int(se.Offset))
	case errors.As(err, &te):
		return LineCol(text, int(te.Offset))
	}
	if m := unknownFieldRe.FindStringSubmatch(err.Error()); m != nil {
		if i := strings.Index(text, `"`+m[1]+`"`); i >= 0 {
			return LineCol(text, i)
		}
	}
	return 1, 1
}
