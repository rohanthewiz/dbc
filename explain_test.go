package main

import (
	"context"
	"encoding/json"
	"image/png"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/theme"
)

// demoPlan explains a statement on the seeded demo, the way explainHeadless does.
func demoPlan(t *testing.T, stmt string) *explain.Plan {
	t.Helper()
	mgr := newTestManager(t)
	p, err := mgr.Explain(context.Background(), config.DemoSQLite, stmt, db.ExplainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Every headless format renders the same plan its own way.
func TestRenderPlanFormats(t *testing.T) {
	p := demoPlan(t, "SELECT breed, count(*) FROM cats GROUP BY breed")

	text, err := renderPlan(p, export.Text, false, 100, theme.Default())
	if err != nil || !strings.HasPrefix(text, "Plan · sqlite · estimated") || !strings.Contains(text, "Insights") {
		t.Errorf("text (%v):\n%s", err, text)
	}
	md, err := renderPlan(p, export.Markdown, true, 100, theme.Default())
	if err != nil || !strings.HasPrefix(md, "```sql\nSELECT breed") || strings.Contains(md, "\x1b[") {
		t.Errorf("markdown must fence the statement and plan, uncolored (%v):\n%s", err, md)
	}
	js, err := renderPlan(p, export.JSON, false, 100, theme.Default())
	var doc map[string]any
	if err != nil || json.Unmarshal([]byte(js), &doc) != nil || doc["engine"] != "sqlite" || doc["headline"] == nil {
		t.Errorf("json (%v):\n%s", err, js)
	}
	page, err := renderPlan(p, export.HTML, false, 100, theme.Default())
	if err != nil || !strings.Contains(page, `<script type="application/json" id="plan-data">`) {
		t.Errorf("html (%v)", err)
	}
}

// The plan-only formats: the PDF and the pictures as bytes, Mermaid as
// text, each reachable by its name and its common alias.
func TestExplainOnlyFormats(t *testing.T) {
	for in, want := range map[string]export.Format{"pdf": fmtPDF, "JPG": fmtJPEG, "jpeg": fmtJPEG, "png": fmtPNG,
		"mmd": fmtMermaid, "mermaid": fmtMermaid, "md": export.Markdown, "html": export.HTML} {
		if got, err := explainFormat(in); err != nil || got != want {
			t.Errorf("explainFormat(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := explainFormat("csv"); err != nil {
		t.Error("csv parses (and is then refused by explain with its list)")
	}
	if _, err := explainFormat("yaml"); err == nil || !strings.Contains(err.Error(), "mermaid") {
		t.Errorf("an unknown format should list explain's formats: %v", err)
	}
	if !binaryFormat(fmtPDF) || !binaryFormat(fmtJPEG) || binaryFormat(fmtMermaid) || binaryFormat(export.HTML) {
		t.Error("binaryFormat")
	}

	p := demoPlan(t, "SELECT breed, count(*) FROM cats GROUP BY breed")
	for f, magic := range map[export.Format]string{fmtPDF: "%PDF-1.4", fmtJPEG: "\xff\xd8\xff", fmtPNG: "\x89PNG",
		fmtMermaid: "%% Plan · sqlite"} {
		out, err := renderPlan(p, f, false, 100, theme.Default())
		if err != nil || !strings.HasPrefix(out, magic) {
			t.Errorf("%s (%v): starts %.16q", f, err, out)
		}
	}
}

func TestFailOn(t *testing.T) {
	for in, want := range map[string]explain.Severity{"": "", "warn": explain.SevWarn, "WARNING": explain.SevWarn, "crit": explain.SevCrit} {
		if got, err := parseFailOn(in); err != nil || got != want {
			t.Errorf("parseFailOn(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := parseFailOn("info"); err == nil {
		t.Error("info is not a gate")
	}
	p := &explain.Plan{Insights: []explain.Insight{
		{Severity: explain.SevInfo}, {Severity: explain.SevWarn}, {Severity: explain.SevCrit}, {Severity: explain.SevWarn},
	}}
	for sev, want := range map[explain.Severity]int{"": 0, explain.SevWarn: 3, explain.SevCrit: 1} {
		if got := findingsAtLeast(p, sev); got != want {
			t.Errorf("findingsAtLeast(%q) = %d, want %d", sev, got, want)
		}
	}
}

// The explain subcommand takes its own flags and the root's, before or after.
func TestExplainCLIParsing(t *testing.T) {
	resetFlags(t)
	setBoolFlag(t, &flagAnalyze, false)
	setBoolFlag(t, &flagOpen, false)
	setFlag(t, &flagFailOn, "")
	setFlag(t, &flagTheme, "")
	var args []string
	app := newCLI()
	for _, sub := range app.Commands {
		if sub.Name == "explain" {
			sub.Action = func(_ context.Context, cmd *cli.Command) error { args = cmd.Args().Slice(); return nil }
		}
	}
	err := app.Run(context.Background(), []string{"dbc", "-c", "pg", "explain", "-a", "SELECT 1", "--fail-on", "warn", "-t", "json", "--theme", "light"})
	if err != nil {
		t.Fatal(err)
	}
	if !flagAnalyze || flagFailOn != "warn" || flagConn != "pg" || flagFormat != "json" || flagTheme != "light" ||
		len(args) != 1 || args[0] != "SELECT 1" {
		t.Errorf("analyze=%v failOn=%q conn=%q format=%q theme=%q args=%v",
			flagAnalyze, flagFailOn, flagConn, flagFormat, flagTheme, args)
	}
}

// The pictures' palette: --theme wins over plan_theme, which wins over dark.
// The corner of a PNG is the page background, so it shows which one drew.
func TestExplainPictureTheme(t *testing.T) {
	setFlag(t, &flagTheme, "")
	for _, c := range []struct{ flag, key, want string }{
		{"", "", theme.Bg},
		{"", "dark", theme.Bg},
		{"", "light", theme.Light().Bg},
		{"light", "dark", theme.Light().Bg},
		{"dark", "light", theme.Bg},
	} {
		flagTheme = c.flag
		if got := planPalette(&config.Config{PlanTheme: c.key}).Bg; got != c.want {
			t.Errorf("--theme %q, plan_theme %q: Bg = %s, want %s", c.flag, c.key, got, c.want)
		}
	}

	p := demoPlan(t, "SELECT breed, count(*) FROM cats GROUP BY breed")
	for _, pal := range []theme.Palette{theme.Default(), theme.Light()} {
		out, err := renderPlan(p, fmtPNG, false, 100, pal)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(strings.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := img.At(0, 0).RGBA()
		wr, wg, wb, _ := theme.ParseHex(pal.Bg)
		if uint8(r>>8) != wr || uint8(g>>8) != wg || uint8(b>>8) != wb {
			t.Errorf("corner is #%02x%02x%02x, want %s", r>>8, g>>8, b>>8, pal.Bg)
		}
	}
}
