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
//	┌ topbar: dbc · active connection · [session state] ·     ▶ Run  ▶▶ Run all  ■ Stop ┐
//	├ sidebar ──────┬ editor (textarea) ─────────────────────────────────────────────────┤
//	│ Connections   ├ ═ splitter (drag; the height is saved) ════════════════════════════┤
//	│ Tables        │ results (a server-rendered table)                                   │
//	│               ├ log ────────────────────────────────────────────────────────────────┤
//	└ status bar ───┴─────────────────────────────────────────────────────────────────────┘
func (p Workbench) Render() string {
	b := element.AcquireBuilder()
	defer element.ReleaseBuilder(b)
	b.Html("lang", "en").R(
		head(b, "dbc web", p.Ver),
		b.Body().R(
			b.DivClass("app").R(
				p.topbar(b),
				p.sidebar(b),
				b.MainClass("work").R(
					b.TextArea("id", "editor", "spellcheck", "false", "autocomplete", "off",
						"autocapitalize", "off", "aria-label", "SQL editor",
						"placeholder", "SELECT * FROM cats;").R(),
					b.DivClass("splitter", "id", "splitter", "role", "separator",
						"aria-orientation", "horizontal", "title", "Drag to resize").R(),
					b.SectionClass("results", "id", "results", "aria-live", "polite").R(
						b.DivClass("empty").T("Ctrl+Enter runs the statement under the caret; "+
							"Ctrl+Shift+Enter runs them all."),
					),
					b.SectionClass("log", "id", "log", "aria-label", "Log").R(),
				),
				b.FooterClass("statusbar").R(
					b.Span("id", "status").T("starting…"),
					b.SpanClass("keys").T("Ctrl+Enter run · Ctrl+Shift+Enter run all · Ctrl+K stop"),
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
			b.Button("id", "stop", "type", "button", "disabled", "disabled", "title", "Stop the run or connect (Ctrl+K)").T("■ Stop"),
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
					b.ButtonClass(cls, "type", "button", "data-conn", c.Name).R(
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
		b.Script("src", "/static/js/app.js?v="+ver, "defer", "defer").R(),
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
