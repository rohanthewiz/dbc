package explain

import (
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/theme"
)

// The interactive plan view is three assets:
//
//	plan.js    the view itself: DbcPlan.mount(root, doc, opts)
//	plan.css   its look, scoped under .dbc-plan
//	plan.html  the standalone page: a template that inlines the other two
//
// The standalone page (HTML below — `dbc explain --open`, the TUI's `b`)
// and dbc web's Plan tab (which serves PlanJS and PlanCSS as files) run the
// SAME script, so the two cannot drift: a feature added to the view is in
// both. The page stays one self-contained file — no CDN, no web font, no
// second request — so it opens the same from a temp file, an email
// attachment, or an air-gapped machine.
//
// WHY A TEMPLATE FILE AND NOT element/html/template. The page is mostly
// JavaScript — a tree layout, pan and zoom, a flame graph — and html/template
// would contextually escape inside the <script> blocks, rewriting code it has
// no business touching. So the assets are plain text, edited as what they
// are, and the Go side does a handful of substitutions, each escaped for the
// one context it lands in (see HTML).
var (
	//go:embed assets/plan.html
	planPage string

	// PlanJS is the plan view's script, for a page that loads it as a file
	// (dbc web). It defines window.DbcPlan.
	//
	//go:embed assets/plan.js
	PlanJS string

	// PlanCSS is the plan view's stylesheet, scoped under .dbc-plan.
	//
	//go:embed assets/plan.css
	PlanCSS string
)

// planBoot mounts the view on the standalone page, from the plan-data block.
const planBoot = `DbcPlan.mount(document.getElementById("plan"), ` +
	`JSON.parse(document.getElementById("plan-data").textContent), { hash: location.hash });`

// planScript is the standalone page's one inline, executable script: the
// view and its boot. It is the same for every plan — the plan itself rides
// in a separate, non-executable application/json block — so a page served
// with a Content-Security-Policy can allow exactly this script by its hash
// (see ScriptHash) instead of allowing inline script at all.
var planScript = "\n" + PlanJS + "\n" + planBoot + "\n"

// ScriptHash is the CSP source for the standalone page's inline script,
// "'sha256-…'", for a server that sends the page (dbc web's "open as
// page"): script-src with this and nothing else runs the view and would
// run nothing injected beside it.
func ScriptHash() string {
	sum := sha256.Sum256([]byte(planScript))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

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
	//
	// PLAN_CSS and PLAN_SCRIPT are this package's own assets, not user data;
	// plan.js promises never to contain "</script" (a test holds it to that).
	r := strings.NewReplacer(
		"/*{{PALETTE}}*/", paletteCSS(),
		"/*{{PLAN_CSS}}*/", PlanCSS,
		"{{TITLE}}", title,
		"{{PLAN_JSON}}", js,
		"{{PLAN_SCRIPT}}", planScript,
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
