package jobs

import (
	"strings"
	"testing"
)

// Locate puts each family of CheckJob's where on the line that holds it, in
// a job written the way Spec.JSON writes one.
func TestLocate(t *testing.T) {
	text := `{
  "name": "nightly",
  "root": "copy",
  "pipelines": [
    { "id": "copy", "pipeline": "copy-cats" },
    {
      "id": "clean",
      "pipeline": "clean-and-load",
      "after": ["copy"],
      "params": { "min_age": "${nope}" }
    }
  ],
  "triggers": { "schedule": ["0 2 * * *", "61 * * * *"], "tz": "Mars/Base" },
  "policy": { "overlap": "sometimes" }
}
`
	lineOf := func(needle string) int {
		for i, l := range strings.Split(text, "\n") {
			if strings.Contains(l, needle) {
				return i + 1
			}
		}
		t.Fatalf("%q is not in the text", needle)
		return 0
	}
	for _, c := range []struct{ where, line string }{
		{"root", `"root"`},
		{"pipelines", `"pipelines"`},
		{"copy", `"id": "copy"`},
		{"clean", `"id": "clean"`},
		{"clean.after", `"after"`},
		{"clean.params.min_age", `"min_age"`},
		{"triggers.tz", `"tz"`},
		{"triggers.schedule[1]", `"61 * * * *"`},
		{"policy.overlap", `"overlap"`},
	} {
		line, col := Locate(text, c.where)
		if want := lineOf(c.line); line != want || col == 0 {
			t.Errorf("Locate(%q) = %d:%d, want line %d", c.where, line, col, want)
		}
	}
	// the second schedule entry, exactly: its opening quote
	line, col := Locate(text, "triggers.schedule[1]")
	if got := strings.Split(text, "\n")[line-1][col-1:]; !strings.HasPrefix(got, `"61 * * * *"`) {
		t.Errorf("schedule[1] points at %q", got)
	}
	if line, col := Locate(text, "nothing.here"); line != 0 || col != 0 {
		t.Errorf("an unplaceable where = %d:%d, want 0:0", line, col)
	}
}

// A step id may hold dots, and CheckJob names a step whose id is invalid
// by its place: Locate matches a where against the ids the text declares
// (the longest that is the whole where or is followed by a step's key),
// not its first dot, and "pipelines[i]" against the i-th step.
func TestLocateSteps(t *testing.T) {
	text := `{
  "name": "nightly",
  "params": { "a.b": { "default": "", "doc": "nightly" } },
  "pipelines": [
    { "id": "start", "pipeline": "p" },
    { "id": "nightly", "pipeline": "p", "after": ["start"] },
    {
      "id": "nightly.copy",
      "pipeline": "p",
      "after": ["nightly"],
      "params": { "a.b": "1" }
    },
    { "id": "a b", "pipeline": "p", "after": ["start"] },
    { "id": "", "pipeline": "p", "after": ["start"] },
    { "id": "", "pipeline": "q", "after": ["start"] },
    { "id": "triggers", "pipeline": "p", "after": ["start"] },
    { "pipeline": "p" }
  ],
  "triggers": { "tz": "Mars/Base" }
}
`
	lines := strings.Split(text, "\n")
	lineOf := func(needle string) int {
		for i, l := range lines {
			if strings.Contains(l, needle) {
				return i + 1
			}
		}
		t.Fatalf("%q is not in the text", needle)
		return 0
	}
	for _, c := range []struct{ where, line string }{
		{"nightly", `"id": "nightly",`}, // not the job's "name", nor the param's doc
		{"nightly.after", `"id": "nightly",`},
		{"nightly.copy", `"id": "nightly.copy"`},
		{"nightly.copy.after", `"after": ["nightly"],`},
		{"nightly.copy.params.a.b", `"a.b": "1"`},
		{"pipelines[3]", `"id": "a b"`},
		{"a b.after", `"id": "a b"`},
		{"pipelines[4].after", `"id": "", "pipeline": "p"`},
		{"pipelines[5]", `"id": "", "pipeline": "q"`}, // the second "", as itself
		{"triggers", `"id": "triggers"`},
		{"triggers.tz", `"tz"`},         // tz is no step key: the job's own
		{"params.a.b", `"a.b": {`},      // the job's param, dots and all
		{"pipelines[7]", `"pipelines"`}, // no "id" to find: the list
		{"pipelines", `"pipelines"`},
	} {
		line, col := Locate(text, c.where)
		if want := lineOf(c.line); line != want || col == 0 {
			t.Errorf("Locate(%q) = %d:%d, want line %d (%s)", c.where, line, col, want, lines[want-1])
		}
	}
}
