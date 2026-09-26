package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/tui"
)

// The explain subcommand: how the database will run a statement, headless.
//
//	dbc explain "SELECT …"                  the plan as a tree, with findings
//	dbc explain -a "SELECT …"               run it and measure (ANALYZE)
//	dbc explain -t json -f q.sql            the plan as JSON, for tooling
//	dbc explain -t html -o plan.html "…"    the interactive page, to a file
//	dbc explain --open "SELECT …"           … or straight into the browser
//	dbc explain -t pdf -o plan.pdf "…"      the graph and findings, to send
//	dbc explain -t jpeg -o plan.jpg "…"     … as a picture (png too)
//	dbc explain -t pdf --theme light …      … on paper rather than slate, to print
//	dbc explain -t mermaid "…"              … as a Mermaid chart, for a PR or wiki
//	dbc explain --fail-on warn -f q.sql     exit 3 when a finding is that bad
//
// The SQL comes the way a headless query's does — the argument, -f, or piped
// stdin — and must be ONE statement: a plan is of a statement, and a buffer of
// several has no single plan to show. A statement already written as an
// EXPLAIN is unwrapped (its ANALYZE honored), so pasting psql habit works.
//
// --fail-on makes the findings a gate. A CI job can keep a folder of the
// queries that matter and fail the build when one of them starts scanning a
// big table — a plan regression caught before it reaches production:
//
//	for q in queries/*.sql; do dbc -c staging explain --fail-on warn -f "$q" || exit 1; done
//
// The pictures are dark (dbc's own palette) unless --theme light, or
// plan_theme = "light" in the config, asks for paper — the flag wins, so a
// config that prints light can still send one dark picture to a chat.
//
// Exit status: 0 fine, 1 the explain failed, 2 bad usage, 3 a finding at or
// above --fail-on, 130 Ctrl+C.

var (
	flagAnalyze bool
	flagOpen    bool
	flagFailOn  string
	flagTheme   string
)

// exitFindings is the status for "the plan has a finding at --fail-on or
// worse" — distinct from 1 (the explain itself failed) so a CI script can tell
// "this query got slower" from "this query is broken".
const exitFindings = 3

func explainCommand() *cli.Command {
	return &cli.Command{
		Name:      "explain",
		Usage:     "show how the database runs a statement: its plan, where the time goes, what to fix",
		ArgsUsage: `["SQL"]`,
		Description: "Explains one statement (the argument, --file, or piped stdin) on the connection -c names. " +
			"--format text (default) draws the plan as a tree with findings; json is the plan for tooling; " +
			"html is an interactive page; pdf, jpeg and png are the graph and its findings as a document or " +
			"a picture (written with -o, or piped), dark unless --theme light or the config's plan_theme asks for paper; mermaid is a flowchart for a pull request or a wiki. --analyze runs the statement to measure it — on Postgres a write is " +
			"run inside a transaction that is rolled back; on the other engines a write is not run at all.",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "analyze", Aliases: []string{"a"},
				Usage: "run the statement and measure it (EXPLAIN ANALYZE)", Destination: &flagAnalyze},
			&cli.BoolFlag{Name: "open",
				Usage: "write the interactive HTML plan to a temp file and open it in the browser", Destination: &flagOpen},
			&cli.StringFlag{Name: "fail-on",
				Usage: "exit 3 when a finding is at least this `SEVERITY`: warn|crit", Destination: &flagFailOn},
			&cli.StringFlag{Name: "theme",
				Usage:       "palette of a pdf, jpeg or png: light|dark (default: the config's plan_theme, else dark)",
				Destination: &flagTheme},
		},
		Action: explainAction,
	}
}

func explainAction(ctx context.Context, cmd *cli.Command) error {
	if flagTx || flagKeep {
		usage("--tx and --keep-going are for running statements; explain does not run a buffer")
	}
	failOn, err := parseFailOn(flagFailOn)
	if err != nil {
		usage(err.Error())
	}
	f, err := explainFormat(flagFormat)
	if err != nil {
		usage(err.Error())
	}
	if f != export.Text && f != export.JSON && f != export.HTML && f != export.Markdown &&
		f != fmtPDF && f != fmtJPEG && f != fmtPNG && f != fmtMermaid {
		usage(fmt.Sprintf("explain renders text, markdown, json, html, pdf, jpeg, png or mermaid — not %s", f))
	}
	// A PDF or a picture is bytes, not text: on a terminal it would be a
	// screenful of garbage that can also leave the terminal in a strange
	// state. Refused before the explain runs, so an --analyze is not spent
	// on output nobody can read. A pipe or a redirect is fine — that is
	// the caller asking for the bytes.
	// Checked here, before the explain runs, for the same reason as the
	// terminal check below. --theme with a text format is refused rather
	// than ignored: only the pictures have a palette to change, and a flag
	// that silently does nothing reads as a bug.
	if flagTheme != "" {
		if _, err = theme.ByName(flagTheme); err != nil {
			usage("--theme: " + err.Error())
		}
		if !binaryFormat(f) {
			usage(fmt.Sprintf("--theme colors a pdf, jpeg or png — %s has no palette to change", f))
		}
	}
	if binaryFormat(f) && flagOut == "" && !flagOpen && term.IsTerminal(os.Stdout.Fd()) {
		usage(fmt.Sprintf("%s is binary — write it with -o plan.%s, or pipe it", f, f))
	}
	sql, ok, err := sqlInput(cmd.Args().Slice(), flagFile, os.Stdin, stdinHasInput())
	if err != nil {
		usage(err.Error())
	}
	if !ok {
		usage(`usage: dbc explain [-a] [-c conn] ["SQL" | -f file]`)
	}
	stmts := sqlsplit.Split(sql)
	switch len(stmts) {
	case 0:
		usage("nothing to explain — the SQL holds no statement")
	case 1:
	default:
		usage(fmt.Sprintf("explain takes one statement; this SQL holds %d", len(stmts)))
	}

	cfg, mgr := setup(demoForRun)
	defer mgr.Close()
	warnConfig(cfg)
	p := explainHeadless(cfg, mgr, stmts[0].Text)

	// the plan's notes are part of every rendering (the text's header, the
	// JSON's "notes", the page's callouts), so they are not repeated here
	out, err := renderPlan(p, f, planColor(), termWidth(), planPalette(cfg))
	if err != nil {
		fail(err, "render failed")
	}
	if flagOpen {
		openPlan(p)
	}
	if flagOut != "" {
		if err = os.WriteFile(flagOut, []byte(out), 0o644); err != nil {
			fail(err, "write failed")
		}
		fmt.Printf("wrote the plan (%s) to %s\n", f, flagOut)
	} else if !flagOpen || (f != export.HTML && !binaryFormat(f)) {
		fmt.Print(out)
	}
	if n := findingsAtLeast(p, failOn); n > 0 {
		fmt.Fprintf(os.Stderr, "%d finding(s) at or above --fail-on %s\n", n, failOn)
		os.Exit(exitFindings)
	}
	return nil
}

// explainHeadless runs the explain, exiting the way every headless path does
// on failure or Ctrl+C.
func explainHeadless(cfg *config.Config, mgr *db.Manager, stmt string) *explain.Plan {
	conn := pickConn(cfg)
	ctx, stop := interruptible()
	defer stop()
	p, err := mgr.Explain(ctx, conn, stmt, db.ExplainOptions{Analyze: flagAnalyze})
	if err != nil {
		if errors.Is(err, db.ErrCanceled) {
			canceled("explain")
		}
		fail(err, "explain failed")
	}
	return p
}

// The renderings explain has beyond the result formats export knows. They
// live here rather than in export because they exist only for a plan: a
// query result has no graph to draw.
const (
	fmtPDF     export.Format = "pdf"
	fmtJPEG    export.Format = "jpeg"
	fmtPNG     export.Format = "png"
	fmtMermaid export.Format = "mermaid"
)

// explainFormat reads --format for explain: the plan-only renderings first,
// then everything export.ParseFormat accepts.
func explainFormat(s string) (export.Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "pdf":
		return fmtPDF, nil
	case "jpeg", "jpg":
		return fmtJPEG, nil
	case "png":
		return fmtPNG, nil
	case "mermaid", "mmd":
		return fmtMermaid, nil
	}
	f, err := export.ParseFormat(s)
	if err != nil {
		// export's own message lists only the result formats
		return "", fmt.Errorf("unknown format %q (use text|markdown|json|html|pdf|jpeg|png|mermaid)", s)
	}
	return f, nil
}

// binaryFormat reports whether f renders bytes rather than text.
func binaryFormat(f export.Format) bool { return f == fmtPDF || f == fmtJPEG || f == fmtPNG }

// planPalette is the palette explain's pictures are drawn in: --theme when
// given, else the config's plan_theme. Both were validated already (the flag
// by explainAction, the key by config.Load), so the error cannot happen; a
// zero Palette would be drawn dark by explain.Picture anyway.
func planPalette(cfg *config.Config) theme.Palette {
	name := cfg.PlanTheme
	if flagTheme != "" {
		name = flagTheme
	}
	pal, _ := theme.ByName(name)
	return pal
}

// renderPlan renders a plan in a headless format. Markdown is the text tree in
// a code fence, with the statement above it, which is how a plan is pasted
// into a pull request or a wiki. The pictures (pdf, jpeg, png) are drawn in
// pal — dbc's dark palette by default, as the page `--open` writes is, so a
// plan that travels looks like dbc wherever it is opened; light when asked
// for, for a page that will be printed. Only the pictures read pal.
func renderPlan(p *explain.Plan, f export.Format, color bool, width int, pal theme.Palette) (string, error) {
	opt := explain.TextOptions{Width: width, Color: color, Insights: true}
	pic := explain.PictureOptions{Palette: pal}
	switch f {
	case fmtPDF:
		b, err := p.PDF(pic)
		return string(b), err
	case fmtJPEG:
		b, err := p.JPEG(pic)
		return string(b), err
	case fmtPNG:
		b, err := p.PNG(pic)
		return string(b), err
	case fmtMermaid:
		return p.Mermaid(), nil
	case export.JSON:
		b, err := p.JSON()
		return string(b) + "\n", err
	case export.HTML:
		return p.HTML()
	case export.Markdown:
		opt.Color = false
		var b strings.Builder
		if p.Statement != "" {
			b.WriteString("```sql\n" + p.Statement + "\n```\n\n")
		}
		b.WriteString("```\n" + p.Text(opt) + "```\n")
		return b.String(), nil
	}
	return p.Text(opt), nil
}

// parseFailOn reads --fail-on.
func parseFailOn(s string) (explain.Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "warn", "warning":
		return explain.SevWarn, nil
	case "crit", "critical":
		return explain.SevCrit, nil
	}
	return "", fmt.Errorf("--fail-on takes warn or crit, not %q", s)
}

// findingsAtLeast counts the plan's findings at severity least or worse.
func findingsAtLeast(p *explain.Plan, least explain.Severity) int {
	if least == "" {
		return 0
	}
	n := 0
	for _, in := range p.Insights {
		if in.Severity == explain.SevCrit || (least == explain.SevWarn && in.Severity == explain.SevWarn) {
			n++
		}
	}
	return n
}

// planColor decides whether the text tree gets ANSI colors: only for a
// terminal on stdout, never for a file or a pipe, and never under NO_COLOR
// (https://no-color.org).
func planColor() bool {
	if flagOut != "" || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return term.IsTerminal(os.Stdout.Fd())
}

// termWidth is the terminal's width, or 100 when stdout is not one.
func termWidth() int {
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w >= 60 {
		return w
	}
	return 100
}

// openPlan writes the interactive page to a temp file and opens it.
func openPlan(p *explain.Plan) {
	path, err := p.WriteHTML(explain.PlanDir())
	if err != nil {
		fail(err, "could not save the plan")
	}
	tui.OpenURL("file://" + path)
	fmt.Fprintln(os.Stderr, "opened the plan in your browser — saved as "+path)
}
