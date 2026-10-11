package script

import (
	"strings"
	"testing"
)

// A copy's Name is rewritten only when taken, to the file's own name made
// free; a Name that is not a plain literal, or text that does not parse,
// is left as written.
func TestFitPluginName(t *testing.T) {
	src := func(name string) string {
		return "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\n" +
			"var Plugin = sdb.Plugin{\n\tName:  " + name + ",\n\tKind:  sdb.KindTransform,\n\tLabel: \"Mask\",\n}\n"
	}
	taken := map[string]bool{"mask.email": true, "mask.email-2": true, "gen.series": true}
	isTaken := func(n string) bool { return taken[n] }
	for _, c := range []struct {
		name, file, text, from, to string
	}{
		{"taken: the file's name", "my_mask.go", src(`"mask.email"`), "mask.email", "my.mask"},
		{"the file's name taken too: made free", "mask_email-2.go", src(`"mask.email"`), "mask.email", "mask.email-2-2"},
		{"the copy of an example: its stem says the same name, so -2", "mask_email.go", src(`"mask.email"`), "mask.email", "mask.email-3"},
		{"free: as written", "x.go", src(`"own.name"`), "", ""},
		{"computed: as written", "x.go", src(`prefix + ".x"`), "", ""},
		{"a raw string literal", "raw_one.go", src("`gen.series`"), "gen.series", "raw.one"},
		{"not Go: as written", "x.go", "not go", "", ""},
	} {
		out, from, to := FitPluginName(c.text, c.file, isTaken)
		if from != c.from || to != c.to {
			t.Errorf("%s: renamed %q → %q, want %q → %q", c.name, from, to, c.from, c.to)
			continue
		}
		want := c.text
		if to != "" {
			want = strings.Replace(c.text, c.text[strings.Index(c.text, "Name:  ")+7:strings.Index(c.text, ",\n\tKind")], `"`+to+`"`, 1)
		}
		if out != want {
			t.Errorf("%s: text\n%s\nwant\n%s", c.name, out, want)
		}
	}
	for file, want := range map[string]string{"mask_email.go": "mask.email", "gen_series-2.go": "gen.series-2", "odd_.go": "odd_", "plain.go": "plain"} {
		if got := PluginNameFor(file); got != want {
			t.Errorf("PluginNameFor(%q) = %q, want %q", file, got, want)
		}
	}
}
