package explain

import (
	_ "embed"
	"fmt"
	"html"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/theme"
)

// planPage is the interactive plan viewer: one HTML file holding its own CSS
// and JavaScript, with three placeholders the Go side fills in.
//
// WHY A TEMPLATE FILE AND NOT element/html/template. The page is mostly
// JavaScript — a tree layout, pan and zoom, a flame graph — and html/template
// would contextually escape inside the <script> blocks, rewriting code it has
// no business touching. So the asset is plain text, edited as HTML, and the
// Go side does exactly three substitutions, each escaped for the one context
// it lands in (see HTML).
//
//go:embed assets/plan.html
var planPage string

// HTML renders the plan as a self-contained interactive page: a tidy-tree
// graph of the steps with heat-colored cards and data-flow edges, a flame
// (icicle) view, the insights with copyable fixes, and a detail panel for
// the selected step. It references nothing on the network — no CDN, no web
// font — so it opens the same from a temp file, an email attachment, or an
// air-gapped machine.
//
// The plan travels as the Document JSON (see JSON) inside a
// <script type="application/json"> block, parsed by the page at load. That
// keeps one source of truth for what a plan contains: the page reads the
// same document `dbc explain -t json` prints, weights included, so its bars
// and blocks are sized exactly as the terminal's.
func (p *Plan) HTML() (string, error) {
	data, err := p.JSON()
	if err != nil {
		return "", serr.Wrap(err, "op", "encode plan for html")
	}
	// Inside a <script> element the HTML parser ends the block at the first
	// "</script", whatever the JSON means. encoding/json already escapes <
	// as < by default; this makes the guarantee independent of that
	// setting — "<\/" is the same string to JSON.parse and is not an end tag
	// to the HTML parser.
	js := strings.ReplaceAll(string(data), "</", `<\/`)

	title := "dbc plan"
	if p.Root != nil {
		title = html.EscapeString(p.Headline())
	}

	// A strings.Replacer substitutes in one left-to-right pass and never
	// rescans what it inserted, so a statement that happens to contain a
	// placeholder's text cannot trigger a second substitution.
	r := strings.NewReplacer(
		"/*{{PALETTE}}*/", paletteCSS(),
		"{{TITLE}}", title,
		"{{PLAN_JSON}}", js,
	)
	return r.Replace(planPage), nil
}

// paletteCSS is the dbc palette as CSS custom properties. Like the export
// page, it reads the theme CONSTANTS rather than whatever palette the TUI is
// wearing: a saved plan is a document that travels, and should look like dbc
// wherever it is opened.
func paletteCSS() string {
	vars := []struct{ name, val string }{
		{"bg", theme.Bg}, {"panel", theme.Panel}, {"panel2", theme.Panel2}, {"sel", theme.Sel},
		{"line", theme.Line}, {"fg", theme.Fg}, {"muted", theme.Muted}, {"accent", theme.Accent},
		{"warn", theme.Warn}, {"err", theme.Err},
	}
	var b strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&b, "--%s:%s;", v.name, v.val)
	}
	return b.String()
}
