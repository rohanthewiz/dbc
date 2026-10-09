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
