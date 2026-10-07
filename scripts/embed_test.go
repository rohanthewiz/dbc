package scripts_test

import (
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
)

// Every sample on disk (a package main file beside embed.go) is named in
// the embed directive, and nothing else is: not embed.go, not a test.
func TestSamplesAllEmbedded(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk, embedded []string
	for _, e := range ents {
		if src, err := os.ReadFile(e.Name()); err == nil && strings.HasSuffix(e.Name(), ".go") &&
			strings.Contains(string(src), "\npackage main\n") {
			onDisk = append(onDisk, e.Name())
		}
	}
	for _, ex := range scripts.Examples() {
		embedded = append(embedded, ex.Name)
	}
	if strings.Join(onDisk, " ") != strings.Join(embedded, " ") {
		t.Errorf("samples on disk %v, embedded %v: update the //go:embed line in embed.go", onDisk, embedded)
	}
}

// Every sample ships in the binary, described, and passes Check: an
// example that no longer compiles against this binary's sdb fails here, not
// for a user who duplicated it.
func TestExamples(t *testing.T) {
	exs := scripts.Examples()
	if len(exs) == 0 {
		t.Fatal("no examples embedded")
	}
	for _, ex := range exs {
		if ex.Desc == "" || strings.HasPrefix(ex.Desc, scripts.SamplePrefix) || ex.Desc[:1] != strings.ToUpper(ex.Desc[:1]) {
			t.Errorf("%s: desc %q", ex.Name, ex.Desc)
		}
		if d := script.Check(ex.Name, ex.Text); script.HasError(d) {
			t.Errorf("%s does not pass Check: %v", ex.Name, d)
		}
		if got, ok := scripts.ExampleByName(ex.Name); !ok || got.Text != ex.Text {
			t.Errorf("scripts.ExampleByName(%s) = %v", ex.Name, ok)
		}
	}
	if _, ok := scripts.ExampleByName("embed.go"); ok {
		t.Error("scripts.ExampleByName found embed.go")
	}
}

func TestIsExample(t *testing.T) {
	ex := scripts.Examples()[0]
	if !scripts.IsExample([]byte(ex.Text)) {
		t.Error("an example is not scripts.IsExample")
	}
	if !scripts.IsExample([]byte(strings.ReplaceAll(ex.Text, "\n", "\r\n"))) {
		t.Error("CRLF line endings (a Windows checkout) are not scripts.IsExample")
	}
	if scripts.IsExample([]byte(ex.Text + "// edited\n")) {
		t.Error("an edited example is scripts.IsExample")
	}
}

// The template table and templates/ agree, each filled template passes
// Check without even a warning, and each has a description.
func TestTemplates(t *testing.T) {
	disk, _ := fs.Glob(scripts.Files, "templates/*.go")
	var names []string
	for _, tp := range scripts.Templates() {
		names = append(names, tp.Name)
		if tp.Title == "" || tp.Desc == "" {
			t.Errorf("%s: title %q, desc %q", tp.Name, tp.Title, tp.Desc)
		}
		src, ok := scripts.Fill(tp.Name, []string{"a", "b"})
		if !ok {
			t.Fatalf("scripts.Fill(%s) failed", tp.Name)
		}
		if strings.Contains(src, "{{") {
			t.Errorf("%s: a placeholder is left after scripts.Fill", tp.Name)
		}
		if d := script.Check(tp.Name+".go", src); d != nil {
			t.Errorf("%s does not pass Check: %v", tp.Name, d)
		}
	}
	for _, p := range disk {
		n := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".go")
		if !slices.Contains(names, n) {
			t.Errorf("%s has no row in the templates table", p)
		}
	}
	if len(names) != len(disk) {
		t.Errorf("templates table has %d rows, templates/ %d files", len(names), len(disk))
	}
	if _, ok := scripts.Fill("nope", nil); ok {
		t.Error("scripts.Fill of an unknown template succeeded")
	}
}

// scripts.Fill writes connection names as Go string literals, and falls back to
// placeholders when the config has too few.
func TestFill(t *testing.T) {
	src, _ := scripts.Fill("copy", []string{`we"ird\name`})
	if !strings.Contains(src, `from  = "we\"ird\\name"`) || !strings.Contains(src, `to    = "`+scripts.NoConn2+`"`) {
		t.Errorf("copy filled with one odd name:\n%s", src)
	}
	if d := script.Check("copy.go", src); d != nil {
		t.Errorf("does not pass Check: %v", d)
	}
	src, _ = scripts.Fill("query", nil)
	if !strings.Contains(src, `"`+scripts.NoConn+`"`) {
		t.Errorf("query with no connections:\n%s", src)
	}
}
