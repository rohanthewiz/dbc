package export

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// N-334: GuardFormula quotes text a spreadsheet would run, and leaves
// numbers alone — those a driver hands over as Go numbers and those it
// sends as text (MySQL's text protocol: an INT's -5 arrives as "-5").
func TestGuardFormula(t *testing.T) {
	for _, c := range []struct {
		cell string
		raw  any
		want string
	}{
		{"=SUM(A1:A9)", "=SUM(A1:A9)", "'=SUM(A1:A9)"},
		{"=HYPERLINK(\"x\")", []byte("=HYPERLINK(\"x\")"), "'=HYPERLINK(\"x\")"},
		{"+1 555 0100", "+1 555 0100", "'+1 555 0100"},
		{"-5+3", "-5+3", "'-5+3"},
		{"@home", "@home", "'@home"},
		{"\tx", "\tx", "'\tx"},
		{"\rx", "\rx", "'\rx"},
		{"-5", "-5", "-5"},             // a number sent as text
		{"+2.5e3", "+2.5e3", "+2.5e3"}, // still a number
		{"-5", int64(-5), "-5"},
		{"-0.25", -0.25, "-0.25"},
		{"plain", "plain", "plain"},
		{"a=b", "a=b", "a=b"},  // only the first byte counts
		{"", nil, ""},          // a NULL's empty field
		{"=1+1", nil, "'=1+1"}, // a row built by hand: its text decides
		{"-7", nil, "-7"},
	} {
		if got := GuardFormula(c.cell, c.raw); got != c.want {
			t.Errorf("GuardFormula(%q, %T) = %q, want %q", c.cell, c.raw, got, c.want)
		}
	}
}

// The options reach every CSV and TSV writer, and nothing else: a BOM once
// at the document's start (a multi-result run too), formulas guarded in
// the rows but never in the header, the zero options the bytes as before.
func TestCSVOptions(t *testing.T) {
	r := &model.Result{
		Columns: []string{"=name", "n"},
		Rows:    [][]string{{"=cmd|' /C calc'!A0", "-5"}, {"Ann", "3"}, {"NULL", "-1"}},
		Raw:     [][]any{{"=cmd|' /C calc'!A0", int64(-5)}, {"Ann", int64(3)}, {nil, "-1"}},
	}
	all := CSVOptions{BOM: true, FormulaGuard: true}
	for _, f := range []Format{CSV, TSV} {
		plain, err := Render(r, f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderWith(r, f, all)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(plain, BOM) || strings.Contains(plain, "'=cmd") {
			t.Fatalf("%s without options changed:\n%q", f, plain)
		}
		if !strings.HasPrefix(got, BOM) || strings.Count(got, BOM) != 1 {
			t.Fatalf("%s with BOM: %q", f, got)
		}
		body := strings.TrimPrefix(got, BOM)
		if !strings.HasPrefix(body, "=name") {
			t.Errorf("%s: the header was guarded: %q", f, body)
		}
		if !strings.Contains(body, "'=cmd") || strings.Contains(body, "'-5") || strings.Contains(body, "'-1") {
			t.Errorf("%s: the guard is wrong:\n%q", f, body)
		}
		// the guard alone, then the BOM alone
		if g, _ := RenderWith(r, f, CSVOptions{FormulaGuard: true}); strings.HasPrefix(g, BOM) || !strings.Contains(g, "'=cmd") {
			t.Errorf("%s guard alone: %q", f, g)
		}
		if b, _ := RenderWith(r, f, CSVOptions{BOM: true}); b != BOM+plain {
			t.Errorf("%s BOM alone: %q, want the plain export after a BOM", f, b)
		}
	}
	// a run of two results: one BOM, before the first block
	var sb strings.Builder
	if err := WriteRunWith(&sb, []*model.Result{r, r}, []int{1, 2}, 2, CSV, all); err != nil {
		t.Fatal(err)
	}
	if s := sb.String(); !strings.HasPrefix(s, BOM) || strings.Count(s, BOM) != 1 || strings.Count(s, "'=cmd") != 2 {
		t.Errorf("a run's document: %q", s)
	}
	// the other formats ignore them
	for _, f := range []Format{JSON, Markdown, Text, HTML} {
		a, _ := Render(r, f)
		b, _ := RenderWith(r, f, all)
		if a != b {
			t.Errorf("%s changed with CSV options", f)
		}
	}
}
