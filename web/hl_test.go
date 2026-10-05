package web

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/codehl"
)

// The browser's highlighter carries its own copy of the keyword lists (it
// runs in the page, away from Go); this keeps each copy equal to codehl's,
// so a word added on one side cannot silently go uncolored on the other.
func TestHLKeywordsMatchCodehl(t *testing.T) {
	src, err := staticFiles.ReadFile("static/js/hl.js")
	if err != nil {
		t.Fatal(err)
	}
	// each language is `name: ` + a backquoted, whitespace-separated list
	lists := regexp.MustCompile("(?m)^\\s*(\\w+): `([^`]*)`").FindAllStringSubmatch(string(src), -1)
	seen := map[string]bool{}
	for _, m := range lists {
		lang, js := m[1], strings.Fields(m[2])
		slices.Sort(js)
		if want := codehl.Keywords(lang); !slices.Equal(js, want) {
			t.Errorf("hl.js %s keywords differ from codehl:\n js: %v\n go: %v", lang, js, want)
		}
		seen[lang] = true
	}
	for _, lang := range []string{"sql", "go", "python", "js", "json", "shell"} {
		if !seen[lang] {
			t.Errorf("hl.js has no %s keyword list", lang)
		}
	}
}
