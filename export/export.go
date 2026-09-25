package export

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"os"
	"strings"
	"time"

	"github.com/rohanthewiz/element"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/clip"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/theme"
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

// RenderAll produces one document holding the results of a multi-statement
// run, in the requested format. Each result carries a banner naming its
// position, connection, and row count, so the blocks stay tellable apart —
// except in CSV and TSV, where a banner would not be data; there the blocks
// are separated by a blank line and each keeps its own header row. A single
// result renders exactly as Render does.
func RenderAll(rs []*model.Result, f Format) (string, error) {
	switch {
	case len(rs) == 0:
		return "", serr.New("no result to export")
	case len(rs) == 1:
		return Render(rs[0], f)
	}
	switch f {
	case CSV, TSV, Markdown, Text:
		// Block formats go through RenderBlock, the same call a streaming
		// caller makes one result at a time, so the two can never drift.
		blocks := make([]string, 0, len(rs))
		for i, r := range rs {
			b, err := RenderBlock(r, f, i+1, len(rs))
			if err != nil {
				return "", err
			}
			blocks = append(blocks, b)
		}
		return strings.Join(blocks, BlockSep), nil
	case HTML:
		return htmlDocAll(rs), nil
	case JSON:
		return jsonAll(rs)
	}
	return "", serr.New("unknown format", "format", string(f))
}

// BlockSep goes between two blocks of a multi-result document in a block
// format. Each block already ends with a newline, so one more makes the
// blank line that keeps CSV blocks apart and gives the banners air.
const BlockSep = "\n"

// Streamable reports whether a multi-result document in f is nothing but its
// blocks joined by BlockSep. Such a document can be written a block at a
// time, as each result arrives, and still come out exactly as RenderAll
// would make it. HTML and JSON wrap the blocks in one document (a <html>
// shell, a JSON array), so they have to wait for the last result.
func Streamable(f Format) bool {
	switch f {
	case CSV, TSV, Markdown, Text:
		return true
	}
	return false
}

// RenderBlock renders r as block i (1-based) of n in a multi-result document
// in a Streamable format: its banner, where the format has one, then the
// result itself. CSV and TSV carry no banner — it would not be data. n is
// what the banner counts against; a streaming caller passes the number of
// statements it means to run, since it cannot know how many will succeed.
func RenderBlock(r *model.Result, f Format, i, n int) (string, error) {
	pos := fmt.Sprintf("%d/%d", i, n)
	switch f {
	case CSV:
		return delimited(r, ',')
	case TSV:
		return delimited(r, '\t')
	case Markdown:
		return mdBanner(r, pos) + markdown(r), nil
	case Text:
		return textBanner(r, pos) + TextTable(r), nil
	}
	return "", serr.New("format is not a block format", "format", string(f))
}

// summary describes what a statement did, for the multi-result banners. The
// duration is rounded as the status bar rounds it — a fast statement reads as
// 380µs rather than 0s.
func summary(r *model.Result) string {
	d := r.Duration.Round(10 * time.Microsecond)
	if r.IsExec {
		return fmt.Sprintf("%d rows affected in %s", r.Affected, d)
	}
	s := fmt.Sprintf("%d rows in %s", len(r.Rows), d)
	if r.Truncated {
		s += " (truncated)"
	}
	return s
}

func textBanner(r *model.Result, pos string) string {
	return fmt.Sprintf("-- %s │ %s │ %s\n-- %s\n", pos, r.Conn, summary(r), preview(r.Query))
}

func mdBanner(r *model.Result, pos string) string {
	return fmt.Sprintf("**%s** · `%s` · %s\n\n```sql\n%s\n```\n\n", pos, r.Conn, summary(r), r.Query)
}

const maxPreviewLen = 72

// preview flattens a statement to one clipped line, for banners.
func preview(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	if rs := []rune(s); len(rs) > maxPreviewLen {
		s = string(rs[:maxPreviewLen-1]) + "…"
	}
	return s
}

// ToClipboard renders the result and places it on the system clipboard. The
// HTML format goes on as a real table (see ClipContent), so a script's
// s.Export(r, "html", "") pastes into a chat app the way the TUI's copy does.
func ToClipboard(r *model.Result, f Format) error {
	c, err := ClipContent(r, f)
	if err != nil {
		return err
	}
	_, err = clip.Write(c)
	return err
}

// ClipContent is what a copy of r in format f should put on the clipboard.
//
// Every format but HTML is its rendered text and nothing else. HTML is the one
// that means something different on a clipboard than in a file: whoever copies
// "as HTML" is about to paste into Teams or a document and wants a TABLE, so
// the HTML flavor is the inline-styled fragment (HTMLFragment), not the
// full-page export with its <style> block that paste targets strip.
//
// The plain-text flavor of an HTML copy is the same fragment's source. A
// developer pasting into an editor asked for HTML and gets HTML; everything
// that understands the rich flavor never looks at it.
func ClipContent(r *model.Result, f Format) (clip.Content, error) {
	if r == nil {
		return clip.Content{}, serr.New("no result to copy")
	}
	if f == HTML {
		frag := HTMLFragment(r)
		return clip.Content{Text: frag, HTML: frag}, nil
	}
	out, err := Render(r, f)
	if err != nil {
		return clip.Content{}, err
	}
	return clip.Content{Text: out}, nil
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
	return marshal(rowMaps(r))
}

// rowMaps turns the typed rows into column-keyed objects.
func rowMaps(r *model.Result) []map[string]any {
	keys := jsonKeys(r.Columns)
	out := make([]map[string]any, 0, len(r.Raw))
	for _, row := range r.Raw {
		m := make(map[string]any, len(keys))
		for i, key := range keys {
			if i < len(row) {
				m[key] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

// jsonKeys returns the column names with duplicates suffixed (a, a_2, a_3),
// so SELECT a, b AS a keeps both columns when a row becomes a JSON object.
func jsonKeys(cols []string) []string {
	used := make(map[string]bool, len(cols))
	keys := make([]string, len(cols))
	for i, c := range cols {
		key := c
		for n := 2; used[key]; n++ {
			key = fmt.Sprintf("%s_%d", c, n)
		}
		used[key] = true
		keys[i] = key
	}
	return keys
}

// jsonAll wraps each result of a multi-statement run in an envelope naming
// the statement it came from — a bare concatenation of row arrays would lose
// which rows belong to which statement.
func jsonAll(rs []*model.Result) (string, error) {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{
			"statement": r.Query,
			"conn":      r.Conn,
			"duration":  r.Duration.Round(time.Millisecond).String(),
		}
		if r.IsExec {
			m["rows_affected"] = r.Affected
		} else {
			// the envelope's column list matches the row-object keys, so a
			// duplicate SELECT column shows up suffixed here too
			m["columns"] = jsonKeys(r.Columns)
			m["rows"] = rowMaps(r)
			if r.Truncated {
				m["truncated"] = true
			}
		}
		out = append(out, m)
	}
	return marshal(out)
}

func marshal(v any) (string, error) {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", serr.Wrap(err)
	}
	return string(bs) + "\n", nil
}

// exportCSS dresses the page in the same muted green as the TUI, surface for
// surface: the page is the editor's background, the header band and zebra
// rows are the panels, and a row lights up on hover the way a selected row
// does. color-scheme keeps the scrollbars and form chrome from coming back
// light against it.
const exportCSS = `
:root { color-scheme: dark; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 2rem;
       background: ` + theme.Bg + `; color: ` + theme.Fg + `; }
h2 { color: ` + theme.Accent + `; font-weight: 600; }
.meta { color: ` + theme.Muted + `; font-size: 0.9rem; }
pre { background: ` + theme.Panel + `; border: 1px solid ` + theme.Line + `; color: ` + theme.Fg + `;
      padding: 0.6rem 0.9rem; border-radius: 6px; overflow-x: auto; }
table { border-collapse: collapse; margin-top: 1rem; }
th, td { border: 1px solid ` + theme.Line + `; padding: 0.35rem 0.7rem; text-align: left; }
th { background: ` + theme.Panel2 + `; color: ` + theme.Accent + `; }
tbody tr:nth-child(even) { background: ` + theme.Panel + `; }
tbody tr:hover { background: ` + theme.Sel + `; }
`

func htmlDoc(r *model.Result) string { return htmlDocAll([]*model.Result{r}) }

// htmlDocAll builds one page with a section per result. With a single result
// the section is the whole page, as a one-statement export always was.
func htmlDocAll(rs []*model.Result) string {
	esc := stdhtml.EscapeString
	b := element.B()
	b.Html().R(
		b.Head().R(
			b.Meta("charset", "utf-8").R(),
			b.Title().T("dbc export"),
			b.Style().T(exportCSS),
		),
		b.Body().R(
			element.ForEach2(rs, func(r *model.Result, i int) {
				heading := "Query Result"
				if len(rs) > 1 {
					heading = fmt.Sprintf("Statement %d of %d", i+1, len(rs))
				}
				b.H2().T(heading)
				b.PClass("meta").T(esc(fmt.Sprintf("connection: %s · %s",
					r.Conn, summary(r))))
				b.Pre().T(esc(r.Query))
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
				)
			}),
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
