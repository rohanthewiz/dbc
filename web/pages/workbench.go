// Package pages renders dbc web's HTML with element: the workbench shell,
// the result table the page swaps in after a run, and the sign-in notice.
//
// The server renders everything that is not live — the layout, the
// connection list, empty states, a result's table. The page's JavaScript
// owns only what has to change without a request: the editor, the log, the
// stream of events. So there is no client-side templating to keep in step
// with this file, and everything here is escaped by element on the way out.
package pages

import (
	"github.com/rohanthewiz/element"

	"github.com/rohanthewiz/dbc/config"
)

// Workbench is the page shell.
type Workbench struct {
	Conns  []config.Connection // names and drivers are shown; a DSN never leaves the server
	Active string              // the connection a new workspace starts on
	Ver    string              // asset version, for cache busting (?v=)
}

// Render returns the whole document.
//
//	┌ topbar: dbc · connection · [session state] · ▶ Run ▶▶ Run all ◈ Explain ■ Stop ⟲ History ┐
//	├ sidebar ──────┬ editor (textarea, upgraded to Monaco) ──────────────────────────────────────┤
//	│ Connections   ├ ═ splitter (drag; the height is saved) ══════════════════════════════════════┤
//	│ Tables        │ results bar: [Results][◈ Plan] · 8 rows · sorted by name asc  ⧉ Copy ▾ ⤓ Export ▾ │
//	│               │ the grid (virtualized; rows fetched a page at a time) — or the plan view      │
//	│               ├ log ──────────────────────────────────────────────────────────────────────────┤
//	└ status bar ───┴───────────────────────────────────────────────────────────────────────────────┘
//
// The body carries the asset version (data-ver) for the scripts that load
// more files themselves — Monaco's loader versions every module with it.
func (p Workbench) Render() string {
	b := element.AcquireBuilder()
	defer element.ReleaseBuilder(b)
	b.Html("lang", "en").R(
		head(b, "dbc web", p.Ver),
		b.Body("data-ver", p.Ver).R(
			b.DivClass("app").R(
				p.topbar(b),
				p.sidebar(b),
				b.MainClass("work").R(
					b.DivClass("editor-wrap", "id", "editor-wrap").R(
						b.TextArea("id", "editor", "spellcheck", "false", "autocomplete", "off",
							"autocapitalize", "off", "aria-label", "SQL editor",
							"placeholder", "SELECT * FROM cats;").R(),
						b.DivClass("monaco", "id", "monaco").R(),
					),
					b.DivClass("splitter", "id", "splitter", "role", "separator",
						"aria-orientation", "horizontal", "title", "Drag to resize").R(),
					b.SectionClass("results", "id", "results", "aria-label", "Results").R(
						b.DivClass("rbar").R(
							b.DivClass("rtabs", "id", "rtabs", "role", "tablist").R(
								b.ButtonClass("rtab on", "type", "button", "role", "tab", "data-rtab", "results",
									"title", "The result grid (p from the plan)").T("▦ Results"),
								b.ButtonClass("rtab", "id", "plan-tab", "type", "button", "role", "tab", "data-rtab", "plan", "hidden", "hidden",
									"title", "The last plan (p from the grid)").T("◈ Plan"),
							),
							b.SpanClass("grid-info", "id", "grid-info", "aria-live", "polite").R(),
							b.DivClass("ractions").R(
								b.Button("id", "copy-btn", "type", "button", "title", "Copy the result or the selection").T("⧉ Copy ▾"),
								b.Button("id", "export-btn", "type", "button", "title", "Download the result (Ctrl+E)").T("⤓ Export ▾"),
							),
						),
						b.DivClass("rpane", "id", "rpane-results").R(
							b.DivClass("grid-msg", "id", "grid-msg").R(),
							b.DivClass("grid", "id", "grid", "tabindex", "0", "hidden", "hidden",
								"aria-label", "Result grid — arrows move, Shift extends, y copies, Enter inspects").R(),
						),
						// the plan view (explain/assets/plan.js) is mounted here
						b.DivClass("rpane", "id", "rpane-plan", "hidden", "hidden").R(
							b.Div("id", "plan", "tabindex", "0", "aria-label",
								"Query plan — arrows walk the steps, e / a explain again, y copies, p back to the results").R(),
						),
					),
					b.SectionClass("log", "id", "log", "aria-label", "Log").R(),
				),
				b.FooterClass("statusbar").R(
					b.Span("id", "status").T("starting…"),
					b.SpanClass("keys").T("Ctrl+Enter run · Ctrl+Shift+Enter all · Ctrl+X explain · Ctrl+K stop · Ctrl+P history · Ctrl+E export"),
				),
			),
		),
	)
	return b.String()
}

func (p Workbench) topbar(b *element.Builder) any {
	b.HeaderClass("topbar").R(
		b.SpanClass("brand").T("dbc"),
		b.SpanClass("active-conn", "id", "active-conn").T(p.Active),
		b.SpanClass("badge", "id", "stateful", "hidden", "hidden",
			"title", "This tab's session may hold a transaction, SET values or temp tables. "+
				"Switching connections closes it, rolling back whatever is open.").
			T("session state"),
		b.SpanClass("busy", "id", "busy", "hidden", "hidden").T("●"),
		b.DivClass("actions").R(
			b.Button("id", "run", "type", "button", "title", "Run the statement under the caret (Ctrl+Enter)").T("▶ Run"),
			b.Button("id", "run-all", "type", "button", "title", "Run every statement (Ctrl+Shift+Enter)").T("▶▶ Run all"),
			b.Button("id", "explain-btn", "type", "button",
				"title", "Explain the statement under the caret (Ctrl+X with nothing selected; Ctrl+Shift+X analyzes)").T("◈ Explain"),
			b.Button("id", "stop", "type", "button", "disabled", "disabled", "title", "Stop the run or connect (Ctrl+K)").T("■ Stop"),
			b.Button("id", "history-btn", "type", "button", "title", "Past statements, here and in the TUI (Ctrl+P)").T("⟲ History"),
		),
	)
	return nil
}

func (p Workbench) sidebar(b *element.Builder) any {
	b.AsideClass("sidebar").R(
		b.H2().T("Connections"),
		b.Ul("id", "conns").R(
			element.ForEach(p.Conns, func(c config.Connection) {
				cls := "conn-item"
				if c.Name == p.Active {
					cls += " active"
				}
				b.Li().R(
					b.ButtonClass(cls, "type", "button", "data-conn", c.Name, "data-driver", c.Driver).R(
						b.SpanClass("name").T(c.Name),
						b.SpanClass("driver").T(c.Driver),
					),
				)
			}),
		),
		b.H2().R(b.T("Tables "), b.SpanClass("count", "id", "table-count").R()),
		b.Ul("id", "tables").R(),
	)
	return nil
}

// scripts are the workbench's modules, in load order: core first (the
// shared API, log and state), app last (boot, which uses all the others).
var scripts = []string{"core.js", "ui.js", "editor.js", "grid.js", "plan.js", "planview.js", "app.js"}

// head is the <head> every page shares: the theme as CSS variables, the
// stylesheet, and the script (deferred, so it runs once the DOM is parsed).
func head(b *element.Builder, title, ver string) any {
	b.Head().R(
		b.Meta("charset", "utf-8").R(),
		b.Meta("name", "viewport", "content", "width=device-width, initial-scale=1").R(),
		b.Title().T(title),
		b.Link("rel", "icon", "href", "/favicon.ico").R(),
		b.Link("rel", "stylesheet", "href", "/theme.css").R(),
		b.Link("rel", "stylesheet", "href", "/static/css/app.css?v="+ver).R(),
		b.Link("rel", "stylesheet", "href", "/static/css/plan.css?v="+ver).R(),
		b.Wrap(func() {
			// deferred, so they run in this order once the DOM is parsed;
			// see core.js for what each one hangs on window.dbc
			for _, js := range scripts {
				b.Script("src", "/static/js/"+js+"?v="+ver, "defer", "defer").R()
			}
		}),
	)
	return nil
}

// SignIn is what a browser without a session gets: where to find the link.
// There is no password form — the secret travels in the URL dbc web opened
// (and printed), and that is the only way in for a browser.
func SignIn(ver string) string {
	b := element.AcquireBuilder()
	defer element.ReleaseBuilder(b)
	b.Html("lang", "en").R(
		b.Head().R(
			b.Meta("charset", "utf-8").R(),
			b.Title().T("dbc web — sign in"),
			b.Link("rel", "stylesheet", "href", "/theme.css").R(),
			b.Link("rel", "stylesheet", "href", "/static/css/app.css?v="+ver).R(),
		),
		b.Body().R(
			b.DivClass("signin").R(
				b.H1().T("dbc web"),
				b.P().T("This browser is not signed in. Open the link dbc web printed in the terminal "+
					"when it started — it carries this launch's secret."),
				b.P().R(
					b.T("Restarted dbc web? Every restart signs browsers out; the new link is in the terminal."),
				),
			),
		),
	)
	return b.String()
}
