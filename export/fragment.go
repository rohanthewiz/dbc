package export

import (
	stdhtml "html"
	"reflect"
	"strings"

	"github.com/rohanthewiz/dbc/model"
)

// An HTML table built to be PASTED, not opened.
//
// The page htmlDoc builds is a document: a <style> block, a dark theme, a
// heading and the query. None of that survives a paste. Teams, Outlook, Slack
// and Google Docs strip <style> and <head> on the way in and keep only inline
// style attributes, and a chat window may be light or dark, so dbc's own dark
// palette pasted into a light Teams would be a black slab.
//
// So the fragment differs from the document in three ways:
//
//   - EVERY STYLE IS INLINE, on the element it applies to. It is repetitive
//     by design; there is nowhere else a paste target will look.
//   - COLORS ARE NEUTRAL AND ALWAYS PAIRED. Each cell sets its background and
//     its text color together. A cell that set only one would inherit the
//     other from the chat theme — dark text on a dark-mode Teams background,
//     for instance — so the table carries its own light surface wherever it
//     lands and reads the same in both themes.
//   - IT IS ONLY THE TABLE. No heading, no query echo: the person pasting it
//     is writing the message around it.
//
// Numeric columns are right-aligned, as a spreadsheet would, and a real SQL
// NULL is drawn as a muted italic NULL so it cannot pass for the string.

// Colors of the pasted table. Named rather than inline so the choice is in
// one place: a GitHub-like light grid, which is what people expect a table
// in a chat message to look like.
const (
	fragBorder  = "#d0d7de"
	fragHeadBg  = "#f6f8fa"
	fragHeadFg  = "#1f2328"
	fragCellBg  = "#ffffff"
	fragZebraBg = "#f6f8fa"
	fragCellFg  = "#1f2328"
	fragNullFg  = "#8c959f"
	fragFont    = "-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif"
)

// HTMLFragment renders a result as one self-styled <table>, for the HTML
// flavor of a clipboard copy. It is not a full document; see the note above
// for why that is the point.
//
// A leading <meta charset> is included even though a fragment has no <head>:
// macOS hands public.html to the pasting app as bytes with no encoding
// attached, and some readers fall back to Latin-1 without it, which turns
// "café" into "cafÃ©". The browsers and chat apps that honor it inside a
// fragment far outnumber any that choke on it.
func HTMLFragment(r *model.Result) string {
	esc := stdhtml.EscapeString
	numeric := numericColumns(r)

	var sb strings.Builder
	sb.WriteString(`<meta charset="utf-8">`)
	sb.WriteString(`<table style="border-collapse:collapse;font-family:` + fragFont +
		`;font-size:13px;line-height:1.4">`)

	sb.WriteString("<thead><tr>")
	for ci, c := range r.Columns {
		sb.WriteString(`<th style="` + fragCellStyle(fragHeadBg, fragHeadFg, numeric, ci) +
			`;font-weight:600">`)
		sb.WriteString(esc(c))
		sb.WriteString("</th>")
	}
	sb.WriteString("</tr></thead><tbody>")

	for ri, row := range r.Rows {
		// Zebra striping helps an eye track a wide row across a chat window,
		// where there is no hover highlight to lean on.
		bg := fragCellBg
		if ri%2 == 1 {
			bg = fragZebraBg
		}
		sb.WriteString("<tr>")
		for ci, v := range row {
			if isNullAt(r, ri, ci) {
				sb.WriteString(`<td style="` + fragCellStyle(bg, fragNullFg, numeric, ci) +
					`;font-style:italic">NULL</td>`)
				continue
			}
			sb.WriteString(`<td style="` + fragCellStyle(bg, fragCellFg, numeric, ci) + `">`)
			// A newline inside a value would collapse to a space in HTML;
			// <br> keeps a multi-line text column readable after the paste.
			sb.WriteString(strings.ReplaceAll(esc(v), "\n", "<br>"))
			sb.WriteString("</td>")
		}
		sb.WriteString("</tr>")
	}
	sb.WriteString("</tbody></table>")
	return sb.String()
}

// fragCellStyle is the inline style shared by header and body cells. The
// background and foreground always travel together — see the note at the
// top of this file.
func fragCellStyle(bg, fg string, numeric []bool, col int) string {
	align := "left"
	if col < len(numeric) && numeric[col] {
		align = "right"
	}
	return "border:1px solid " + fragBorder + ";padding:4px 10px;background:" + bg +
		";color:" + fg + ";text-align:" + align + ";vertical-align:top"
}

// numericColumns reports, per column, whether every non-NULL value in it is a
// Go number. The typed Raw values decide rather than the display strings,
// because "007" in a varchar column looks numeric and is not; a column of
// nothing but NULLs is not numeric, since there is no evidence either way.
//
// MySQL hands every value back as bytes, which rawVal turns into strings, so
// MySQL results stay left-aligned. That is the honest answer given what the
// driver reports, and it costs only alignment.
func numericColumns(r *model.Result) []bool {
	out := make([]bool, len(r.Columns))
	for ci := range r.Columns {
		seen := false
		ok := true
		for ri := range r.Raw {
			if ci >= len(r.Raw[ri]) || r.Raw[ri][ci] == nil {
				continue
			}
			seen = true
			if !isNumber(r.Raw[ri][ci]) {
				ok = false
				break
			}
		}
		out[ci] = seen && ok
	}
	return out
}

// isNumber reports whether v is one of Go's integer or float kinds.
func isNumber(v any) bool {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// isNullAt reports whether the value at (row, col) was a real SQL NULL. The
// display string alone cannot say: a text column may hold "NULL".
func isNullAt(r *model.Result, row, col int) bool {
	return row < len(r.Raw) && col < len(r.Raw[row]) && r.Raw[row][col] == nil
}
