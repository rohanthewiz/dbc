// Package theme holds the muted green palette, ported from cdx (GoNotes):
// warm dark gray-green surfaces with a single green accent.
//
// These hex strings are the one source of truth for every surface dbc paints.
// The TUI turns them into tcell colors and tview color tags; the HTML export
// drops them straight into its stylesheet. The package deliberately has no
// dependencies so both sides can import it.
package theme

const (
	Bg     = "#1f2420" // deepest surface — editor, results table, export page
	Panel  = "#242a25" // sidebar, log pane, code blocks, zebra rows
	Panel2 = "#2b322c" // status bar, modal fields, table header band
	Sel    = "#3a4a3f" // selected row, hovered row
	Line   = "#38403a" // borders and rules at rest
	Fg     = "#d6ddd6"
	Muted  = "#9db0a2"
	Accent = "#4db380"
	Warn   = "#d9a45b" // not in cdx; dbc needs a busy/stopped tone
	Err    = "#e57373"
)
