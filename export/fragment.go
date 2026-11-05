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
// style attributes, and a chat window may be light or dark.
//
// So the fragment differs from the document in three ways:
//
//   - EVERY STYLE IS INLINE, on the element it applies to. It is repetitive
//     by design; there is nowhere else a paste target will look.
//   - NO SURFACE COLORS: no background on any cell and no text color on the
//     values. The table takes the surface and the ink of wherever it lands.
//     This is a correction. The fragment used to carry its own light surface
//     (white cells, a #f6f8fa header band and zebra rows) with dark text
//     paired on every cell, on the theory that a pair cannot be half
//     overridden. Teams does exactly that: on paste it keeps the background
//     but rewrites the text color to the theme's, so in dark-theme Teams the
//     table arrived as white text on white cells — a blank grid, which reads
//     as "it did not paste as a table". Only lines and type survive every
//     theme, so they are all the table carries: a mid-gray border (#999,
//     visible on white and on Teams' near-black alike), padding, alignment
//     and a bold header. This is the styling the standalone copyhtml tool
//     uses, which pastes correctly into Teams in both themes.
//   - IT IS ONLY THE TABLE. No heading, no query echo: the person pasting it
//     is writing the message around it. No font either, for the same reason:
//     the table should read in the message's own typeface and size.
//
// Numeric columns are right-aligned, as a spreadsheet would, and a real SQL
// NULL is drawn as a muted italic NULL so it cannot pass for the string.
//
//	what a pasted table carries, and what it leaves to the target
//	┌──────────────────┬───────────────────────────────────────────┐
//	│ carried inline   │ 1px #999 borders, padding, text-align,    │
//	│                  │ vertical-align, bold <th>, italic NULL    │
//	├──────────────────┼───────────────────────────────────────────┤
//	│ left to target   │ background, text color, font family/size  │
//	└──────────────────┴───────────────────────────────────────────┘

// Colors of the pasted table — deliberately only two, neither a surface.
// fragBorder is the grid's lines; fragNullFg is the one text color, for a
// real NULL, and is a mid-gray for the same reason as the border: it stays
// legible whether the target's text is dark-on-light or light-on-dark, and a
// target that overrides text color (Teams) just draws NULL in its own ink,
// still italic.
const (
	fragBorder = "#999"
	fragNullFg = "#8c959f"
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
	numeric := NumericColumns(r)

	var sb strings.Builder
	sb.WriteString(`<meta charset="utf-8">`)
	sb.WriteString(`<table style="border-collapse:collapse">`)

	sb.WriteString("<thead><tr>")
	for ci, c := range r.Columns {
		sb.WriteString(`<th style="` + fragCellStyle(numeric, ci) + `;font-weight:600">`)
		sb.WriteString(esc(c))
		sb.WriteString("</th>")
	}
	sb.WriteString("</tr></thead><tbody>")

	// No zebra striping: it is a background, and backgrounds are what Teams
	// mangles (see the note at the top of this file). The cell borders keep
	// a wide row trackable on their own.
	for ri, row := range r.Rows {
		sb.WriteString("<tr>")
		for ci, v := range row {
			// A transposed result's column 0 holds the source's column
			// names: drawn as row headers, bold like the header row, so the
			// names read as labels down the left edge and not as data.
			if r.Transposed && ci == 0 {
				sb.WriteString(`<th scope="row" style="` + fragCellStyle(numeric, ci) +
					`;font-weight:600">` + esc(v) + "</th>")
				continue
			}
			if isNullAt(r, ri, ci) {
				sb.WriteString(`<td style="` + fragCellStyle(numeric, ci) +
					`;color:` + fragNullFg + `;font-style:italic">NULL</td>`)
				continue
			}
			sb.WriteString(`<td style="` + fragCellStyle(numeric, ci) + `">`)
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

// fragCellStyle is the inline style shared by header and body cells: lines,
// spacing and alignment only. It sets no background or color — see the note
// at the top of this file for why a pasted cell must not.
func fragCellStyle(numeric []bool, col int) string {
	align := "left"
	if col < len(numeric) && numeric[col] {
		align = "right"
	}
	return "border:1px solid " + fragBorder + ";padding:4px 8px;text-align:" + align + ";vertical-align:top"
}

// NumericColumns reports, per column, whether every non-NULL value in it is a
// Go number. The typed Raw values decide rather than the display strings,
// because "007" in a varchar column looks numeric and is not; a column of
// nothing but NULLs is not numeric, since there is no evidence either way.
//
// MySQL hands every value back as bytes, which rawVal turns into strings, so
// MySQL results stay left-aligned. That is the honest answer given what the
// driver reports, and it costs only alignment.
func NumericColumns(r *model.Result) []bool {
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
