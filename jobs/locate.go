package jobs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/pipeline"
)

// Locate places a CheckJob finding's where in the job's text, as
// pipeline.Locate does for a pipeline's — for the TUI, which prints a
// finding as "nightly.json:7:5: …" after $EDITOR, so the editor can jump
// there, and for dbc web's JSON view, which marks the line. A text search
// over the shape Spec.JSON writes; (0, 0) when it cannot place it.
//
// The where strings CheckJob writes are of two families:
//
//	a step's    "copy", "copy.after", "copy.params", "copy.params.day",
//	            "pipelines[2]", "pipelines[2].after"
//	            → the step's "id": "copy" inside "pipelines": [ … ],
//	            then each key after it
//	the job's   "root", "pipelines", "triggers.tz", "triggers.schedule[1]",
//	            "policy.overlap", "params.day" → each key in turn; [i] is
//	            the array's i-th string
//
// Which family is decided by the step ids the text declares (stepAt): an
// id may hold dots, so a where is a step's when it is "pipelines[i]" or
// starts with one of the ids. (A step named like a top-level key, "root",
// would shadow the key; CheckJob's root findings then land on that step,
// which is near enough.)
func Locate(text, where string) (int, int) {
	if where == "" {
		return 0, 0
	}
	at := 0
	find := func(re *regexp.Regexp) bool {
		loc := re.FindStringIndex(text[at:])
		if loc == nil {
			return false
		}
		at += loc[0]
		return true
	}
	quote := func(s string) string { return regexp.QuoteMeta(strings.ReplaceAll(s, `"`, `\"`)) }
	// Hosts check only text that parses, so spec is nil only for a caller
	// with some other text; every where is then taken as the job's.
	spec, _ := ParseJob(text)
	keys := keysOf(where)
	if i, rest := stepAt(spec, where); i >= 0 {
		// the step's "id" inside the pipelines list (a job's own "name"
		// or a param's "doc" cannot be taken for it), counting the steps
		// before it that share its id, so a duplicate is found as itself
		id, nth := spec.Pipelines[i].ID, 0
		for _, st := range spec.Pipelines[:i] {
			if st.ID == id {
				nth++
			}
		}
		if !find(regexp.MustCompile(`"pipelines"\s*:\s*\[`)) {
			return 0, 0
		}
		j := pipeline.FindName(text, at, "id", id, nth)
		if j < 0 {
			// a step whose "id" is left out: the list is as near as it gets
			return pipeline.LineCol(text, at)
		}
		at = j
		keys = keysOf(rest)
		if rest == "" {
			return pipeline.LineCol(text, at)
		}
	}
	start := at
	for i, p := range keys {
		key, idx := p, -1
		// "schedule[1]": the key, then its array's element 1
		if open := strings.IndexByte(p, '['); open > 0 && strings.HasSuffix(p, "]") {
			if n, err := strconv.Atoi(p[open+1 : len(p)-1]); err == nil {
				key, idx = p[:open], n
			}
		}
		if !find(regexp.MustCompile(`"` + quote(key) + `"\s*:`)) {
			if i == 0 && start == 0 {
				return 0, 0 // not even the first key: nothing to point at
			}
			break // the closest place found so far
		}
		if idx >= 0 {
			// past the key's own string, then over idx strings of the
			// array to the one wanted (each skipped whole, so a string's
			// closing quote is never taken for the next one's opening)
			at += len(key) + 2
			for n := 0; n <= idx; n++ {
				loc := jsonString.FindStringIndex(text[at:])
				if loc == nil {
					break
				}
				if n == idx {
					at += loc[0]
					break
				}
				at += loc[1]
			}
		}
	}
	return pipeline.LineCol(text, at)
}

// keysOf splits a where (or what follows a step in one) into the keys to
// find in turn. A param's name may hold dots, so "params.a.b" is the key
// "params", then the key "a.b"; anything else is split at every dot.
func keysOf(where string) []string {
	if where == "" {
		return nil
	}
	if p, ok := strings.CutPrefix(where, "params."); ok {
		return []string{"params", p}
	}
	return strings.Split(where, ".")
}

// stepAt is the step of spec a where names and what follows it ("",
// "after", "params", "params.<p>"); -1 when the where is the job's own.
// CheckJob names a step by its id, or as "pipelines[i]" when the id is
// invalid ("", "a b"): the spec's i-th, as ParseJob keeps the file's
// order. An id may hold dots but nothing after one does, apart from a
// param's name, so the step is the longest of the spec's ids that is the
// whole where or is followed by a dot:
//
//	where                  the spec has            step           rest
//	"nightly.copy.after"   nightly, nightly.copy   nightly.copy   after
//	"nightly.after"        nightly, nightly.copy   nightly        after
//	"pipelines[2].after"   3 steps                 the 3rd        after
//	"triggers.tz"          a step "triggers"       —              (tz is no step key)
//
// What follows must be a step's key, so a step named like a top-level key
// does not take that key's findings, only the bare name's.
func stepAt(spec *Spec, where string) (int, string) {
	if spec == nil {
		return -1, ""
	}
	head, tail, _ := strings.Cut(where, ".")
	if i := pipeline.IndexIn(head, "pipelines", len(spec.Pipelines)); i >= 0 {
		return i, tail
	}
	best := -1
	for j, st := range spec.Pipelines {
		if st.ID == "" || (best >= 0 && len(st.ID) <= len(spec.Pipelines[best].ID)) {
			continue
		}
		if rest, ok := strings.CutPrefix(where, st.ID); ok && (rest == "" || rest[0] == '.' && isStepKey(rest[1:])) {
			best = j
		}
	}
	if best < 0 {
		return -1, ""
	}
	return best, strings.TrimPrefix(where[len(spec.Pipelines[best].ID):], ".")
}

// isStepKey says whether rest is what CheckJob writes after a step's id.
func isStepKey(rest string) bool {
	return rest == "after" || rest == "params" || strings.HasPrefix(rest, "params.")
}

// jsonString is a JSON string literal, escapes included.
var jsonString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
