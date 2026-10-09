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
// there. A text search over the shape Spec.JSON writes; (0, 0) when it
// cannot place it.
//
// The where strings CheckJob writes are of two families:
//
//	a step's    "copy", "copy.after", "copy.params", "copy.params.day"
//	            → the step's "id": "copy", then each key after it
//	the job's   "root", "pipelines", "triggers.tz", "triggers.schedule[1]",
//	            "policy.overlap" → each key in turn; [i] is the array's
//	            i-th string
//
// Which family is decided by the step ids the text declares: a where
// whose first part is a step's id is that step's. (A step named like a
// top-level key, "root", would shadow the key; CheckJob's root findings
// then land on that step, which is near enough.)
func Locate(text, where string) (int, int) {
	if where == "" {
		return 0, 0
	}
	parts := strings.Split(where, ".")
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
	start := 0
	if spec, err := ParseJob(text); err == nil && spec.Step(parts[0]) != nil {
		if !find(regexp.MustCompile(`"id"\s*:\s*"` + quote(parts[0]) + `"`)) {
			return 0, 0
		}
		start = 1
	}
	for i, p := range parts[start:] {
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

// jsonString is a JSON string literal, escapes included.
var jsonString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
