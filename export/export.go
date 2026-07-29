package export

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"os"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/rohanthewiz/element"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/model"
)

// Format is an output rendering for a query result.
type Format string

const (
	CSV      Format = "csv"
	TSV      Format = "tsv"
	Markdown Format = "markdown"
	HTML     Format = "html"
	JSON     Format = "json"
	Text     Format = "text" // aligned plain-text table
)

// Names lists the selectable format names for UI menus.
func Names() []string {
	return []string{"csv", "tsv", "markdown", "html", "json", "text"}
}

// ParseFormat resolves a format name, accepting common aliases.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "csv":
		return CSV, nil
	case "tsv", "tab":
		return TSV, nil
	case "markdown", "md":
		return Markdown, nil
	case "html", "htm":
		return HTML, nil
	case "json":
		return JSON, nil
	case "text", "txt", "table":
		return Text, nil
	}
	return "", serr.New("unknown format (use csv|tsv|markdown|html|json|text)", "format", s)
}

// Render produces the result in the requested format.
func Render(r *model.Result, f Format) (string, error) {
	if r == nil {
		return "", serr.New("no result to export")
	}
	switch f {
	case CSV:
		return delimited(r, ',')
	case TSV:
		return delimited(r, '\t')
	case Markdown:
		return markdown(r), nil
	case HTML:
		return htmlDoc(r), nil
	case JSON:
		return jsonArr(r)
	case Text:
		return TextTable(r), nil
	}
	return "", serr.New("unknown format", "format", string(f))
}

// ToClipboard renders the result and places it on the system clipboard.
func ToClipboard(r *model.Result, f Format) error {
	out, err := Render(r, f)
	if err != nil {
		return err
	}
	if err = clipboard.WriteAll(out); err != nil {
		return serr.Wrap(err, "op", "clipboard")
	}
	return nil
}

// ToFile renders the result and writes it to path.
func ToFile(r *model.Result, f Format, path string) error {
	out, err := Render(r, f)
	if err != nil {
		return err
	}
	if err = os.WriteFile(path, []byte(out), 0644); err != nil {
		return serr.Wrap(err, "path", path)
	}
	return nil
}

func delimited(r *model.Result, comma rune) (string, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Comma = comma
	if err := w.Write(r.Columns); err != nil {
		return "", serr.Wrap(err)
	}
	if err := w.WriteAll(r.Rows); err != nil {
		return "", serr.Wrap(err)
	}
	return sb.String(), nil
}

func markdown(r *model.Result) string {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "|", `\|`)
		s = strings.ReplaceAll(s, "\r\n", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		return s
	}
	var sb strings.Builder
	sb.WriteString("| ")
	for i, c := range r.Columns {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(esc(c))
	}
	sb.WriteString(" |\n|")
	for range r.Columns {
		sb.WriteString(" --- |")
	}
	sb.WriteString("\n")
	for _, row := range r.Rows {
		sb.WriteString("| ")
		for i, v := range row {
			if i > 0 {
				sb.WriteString(" | ")
			}
			sb.WriteString(esc(v))
		}
		sb.WriteString(" |\n")
	}
	return sb.String()
}

func jsonArr(r *model.Result) (string, error) {
	rows := r.Raw
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		m := make(map[string]any, len(r.Columns))
		for i, col := range r.Columns {
			if i < len(row) {
				m[col] = row[i]
			}
		}
		out = append(out, m)
	}
	bs, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", serr.Wrap(err)
	}
	return string(bs) + "\n", nil
}

const exportCSS = `
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 2rem; color: #222; }
.meta { color: #666; font-size: 0.9rem; }
pre { background: #f6f6f6; padding: 0.6rem 0.9rem; border-radius: 6px; overflow-x: auto; }
table { border-collapse: collapse; margin-top: 1rem; }
th, td { border: 1px solid #ccc; padding: 0.35rem 0.7rem; text-align: left; }
th { background: #eef2f7; }
tbody tr:nth-child(even) { background: #fafafa; }
`

func htmlDoc(r *model.Result) string {
	esc := stdhtml.EscapeString
	b := element.B()
	b.Html().R(
		b.Head().R(
			b.Meta("charset", "utf-8").R(),
			b.Title().T("dbc export"),
			b.Style().T(exportCSS),
		),
		b.Body().R(
			b.H2().T("Query Result"),
			b.PClass("meta").T(esc(fmt.Sprintf("connection: %s · %d rows · %s",
				r.Conn, len(r.Rows), r.Duration.Round(time.Millisecond)))),
			b.Pre().T(esc(r.Query)),
			b.Table().R(
				b.THead().R(
					b.Tr().R(
						element.ForEach(r.Columns, func(c string) {
							b.Th().T(esc(c))
						}),
					),
				),
				b.TBody().R(
					element.ForEach(r.Rows, func(row []string) {
						b.Tr().R(
							element.ForEach(row, func(v string) {
								b.Td().T(esc(v))
							}),
						)
					}),
				),
			),
		),
	)
	return b.String()
}

const maxTextColWidth = 60

// TextTable renders an aligned plain-text table (used for terminal output).
func TextTable(r *model.Result) string {
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = len([]rune(c))
	}
	clip := func(s string) string {
		rs := []rune(strings.ReplaceAll(s, "\n", " "))
		if len(rs) > maxTextColWidth {
			return string(rs[:maxTextColWidth-1]) + "…"
		}
		return string(rs)
	}
	rows := make([][]string, len(r.Rows))
	for ri, row := range r.Rows {
		rows[ri] = make([]string, len(row))
		for ci, v := range row {
			v = clip(v)
			rows[ri][ci] = v
			if ci < len(widths) && len([]rune(v)) > widths[ci] {
				widths[ci] = len([]rune(v))
			}
		}
	}
	var sb strings.Builder
	writeRow := func(cells []string) {
		for i, c := range cells {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(c)
			if pad := widths[i] - len([]rune(c)); pad > 0 && i < len(cells)-1 {
				sb.WriteString(strings.Repeat(" ", pad))
			}
		}
		sb.WriteString("\n")
	}
	writeRow(r.Columns)
	seps := make([]string, len(r.Columns))
	for i, w := range widths {
		seps[i] = strings.Repeat("-", w)
	}
	writeRow(seps)
	for _, row := range rows {
		writeRow(row)
	}
	return sb.String()
}
