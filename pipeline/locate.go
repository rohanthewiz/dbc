package pipeline

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
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
//	where "clean/dst.table" → "fragments": [ … "name": "clean" … "id": "dst" … "table":
//
// The fragment and the node are matched against the names the text
// declares (Parse), not cut out of where at its first dot: a name may hold
// dots (ValidName), and Check names an element whose name is invalid by
// its place instead (fragmentAt, nodeAt).
//
//	where                    the text has                found at
//	"w/my.src.query"         nodes my, my.src            my.src's "query":
//	"w/my.query"             nodes my, my.src            my's "query":
//	"w/nodes[3].query"       a 4th node "a b"            that node's "query":
//	"fragments[1]/src"       a 2nd fragment ""           its node src
//
// A fragment's "name" is looked for inside "fragments": [ … ], so a
// pipeline called like one of its fragments ("orders" and "orders") does
// not take the fragment's findings onto its own name.
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
	// findName moves to the nth `"key": "…"` from at on whose string,
	// decoded, is s. Decoding, rather than searching for s as written,
	// finds an invalid name however the file spells it: "" as `""`, a
	// quote or backslash escaped. nth counts the elements before this one
	// that share its name, so a duplicate is found as itself.
	findName := func(key, s string, nth int) bool {
		re := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*("(?:[^"\\]|\\.)*")`)
		for _, m := range re.FindAllStringSubmatchIndex(text[at:], -1) {
			var v string
			if json.Unmarshal([]byte(text[at+m[2]:at+m[3]]), &v) != nil || v != s {
				continue
			}
			if nth--; nth < 0 {
				at += m[0]
				return true
			}
		}
		return false
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
	// a name never holds "/" or ":", so the fragment is what precedes them
	part, rest, _ := strings.Cut(where, "/")
	part, edge, isEdge := strings.Cut(part, ":")
	// The names to match where against. Every host checks a text that
	// parses, so spec is nil only for a caller with some other text; the
	// names in where are then taken as written.
	spec, _ := Parse(text)
	f, name, nth := fragmentAt(spec, part)
	if !find(regexp.MustCompile(`"fragments"\s*:\s*\[`)) || !findName("name", name, nth) {
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
		// the node's "id" after the fragment's "name" (Spec.JSON writes a
		// fragment's name before its nodes); a field's key after that
		id, nth, field := nodeAt(f, rest)
		if findName("id", id, nth) && field != "" {
			_ = find(regexp.MustCompile(`"` + quote(field) + `"\s*:`))
		}
	}
	return LineCol(text, at)
}

// fragmentAt is the fragment a where's first part names, the name to look
// for in the text, and how many fragments before it are called the same.
// Check names a fragment by its name, or as "fragments[i]" when the name is
// invalid ("", "a b"): the spec's i-th, as Parse keeps the file's order.
// The index is tried first: no valid name holds "[", so a where shaped
// like one is always one. Without a spec, or naming none of its fragments
// (a check older than a rename), it is part as written.
func fragmentAt(spec *Spec, part string) (*Fragment, string, int) {
	if spec == nil {
		return nil, part, 0
	}
	i := indexIn(part, "fragments", len(spec.Fragments))
	if i < 0 {
		i = slices.IndexFunc(spec.Fragments, func(f Fragment) bool { return f.Name == part })
	}
	if i < 0 {
		return nil, part, 0
	}
	f := &spec.Fragments[i]
	nth := 0
	for _, g := range spec.Fragments[:i] {
		if g.Name == f.Name {
			nth++
		}
	}
	return f, f.Name, nth
}

// nodeAt reads what follows a fragment's "/" in a where: the node's id to
// look for, how many nodes before it in f share that id, and the field
// after it ("" for the node itself). Check names a node by its id, or as
// "nodes[i]" when the id is invalid. An id may hold dots but a field never
// does, so the node is the longest of f's ids that is the whole of rest or
// is followed by a dot:
//
//	rest            f has         node     field
//	"my.src.query"  my, my.src    my.src   query
//	"my.src"        my, my.src    my.src   —
//	"my.query"      my, my.src    my       query
//	"nodes[3].sql"  4 nodes       the 4th  sql
//
// Without f, or naming none of its nodes, rest is cut at its first dot,
// which is as near as the text allows.
func nodeAt(f *Fragment, rest string) (id string, nth int, field string) {
	i := -1
	if f != nil {
		head, tail, _ := strings.Cut(rest, ".")
		if i = indexIn(head, "nodes", len(f.Nodes)); i >= 0 {
			field = tail
		} else {
			for j, n := range f.Nodes {
				if n.ID != "" && (i < 0 || len(n.ID) > len(f.Nodes[i].ID)) &&
					(rest == n.ID || strings.HasPrefix(rest, n.ID+".")) {
					i = j
				}
			}
			if i >= 0 && len(rest) > len(f.Nodes[i].ID) {
				field = rest[len(f.Nodes[i].ID)+1:]
			}
		}
	}
	if i < 0 {
		id, field, _ = strings.Cut(rest, ".")
		return id, 0, field
	}
	id = f.Nodes[i].ID
	for _, n := range f.Nodes[:i] {
		if n.ID == id {
			nth++
		}
	}
	return id, nth, field
}

// indexIn reads "<list>[i]" — Check's name for the i-th element of a list
// when the element's own name is invalid — as i, when the list has more
// than i elements; -1 for anything else.
func indexIn(s, list string, n int) int {
	if !strings.HasPrefix(s, list+"[") || !strings.HasSuffix(s, "]") {
		return -1
	}
	i, err := strconv.Atoi(s[len(list)+1 : len(s)-1])
	if err != nil || i < 0 || i >= n {
		return -1
	}
	return i
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
