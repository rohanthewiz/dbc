package export

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	var sb strings.Builder
	if err := Write(&sb, r, f); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// Write renders r in format f to w: Render, without the document in
// memory (N-282). A 1M-row export used to be held as the result, then as
// the rendered string, then as a []byte copy of it for os.WriteFile — and
// for JSON, as a map per row and two encoder buffers before that. Written
// here, the CSV, TSV, Markdown, text and JSON renderings go out a row at a
// time through one buffer; HTML is still built whole (the element builder
// holds its document) and written once.
//
// The bytes are Render's, exactly: Render is Write into a strings.Builder.
//
// A failure part way — w's own, or JSON's refusal of a value (NaN) — is
// returned, and what was written before it stays written: a caller that
// must not leave a partial file writes through WriteFile.
func Write(w io.Writer, r *model.Result, f Format) error {
	if r == nil {
		return serr.New("no result to export")
	}
	bw := bufio.NewWriter(w)
	var err error
	switch f {
	case CSV:
		err = delimited(bw, r, ',')
	case TSV:
		err = delimited(bw, r, '\t')
	case Markdown:
		markdown(bw, r)
	case HTML:
		_, _ = bw.WriteString(htmlDoc(r))
	case JSON:
		err = jsonArr(bw, r)
	case Text:
		writeText(bw, r)
	default:
		return serr.New("unknown format", "format", string(f))
	}
	// bufio.Writer keeps its first write error and turns every later write
	// into a no-op, so the renderers need not check each one: Flush reports
	// it. A renderer's own error comes first, as the cause.
	if ferr := bw.Flush(); err == nil && ferr != nil {
		err = serr.Wrap(ferr)
	}
	return err
}

// RenderAll produces one document holding the results of a multi-statement
// run, in the requested format. Each result carries a banner naming its
// position, connection, and row count, so the blocks stay tellable apart —
// except in CSV and TSV, where a banner would not be data; there the blocks
// are separated by a blank line and each keeps its own header row. A single
// result renders exactly as Render does.
func RenderAll(rs []*model.Result, f Format) (string, error) {
	return RenderRun(rs, Seq(len(rs)), len(rs), f)
}

// RenderRun is RenderAll for a run in which not every statement produced a
// result — one that stopped at a failure, or went on past some. at[i] is the
// 1-based position of rs[i] among the run's total statements, and the
// banners (and HTML headings) count against total, so result 4 of a run of 5
// in which statement 2 failed still reads "4/5". The shape follows total,
// not len(rs): a multi-statement run is always a multi-result document, even
// when only one of its statements made it — a JSON consumer gets the array of
// envelopes it asked for, not a bare result array. A run of one statement
// renders exactly as Render does — unless that statement returned several
// result sets (model.Result.More, which the caller passes as results of
// their own at the statement's position): those are a multi-result
// document too, each block bannered "1/1".
func RenderRun(rs []*model.Result, at []int, total int, f Format) (string, error) {
	var sb strings.Builder
	if err := WriteRun(&sb, rs, at, total, f); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// WriteRun is RenderRun to w, as Write is Render: the same bytes, written
// a block (and within a block, a row) at a time. The arguments are checked
// before anything is written.
func WriteRun(w io.Writer, rs []*model.Result, at []int, total int, f Format) error {
	switch {
	case len(rs) == 0:
		return serr.New("no result to export")
	case len(at) != len(rs):
		return serr.New("a position is needed for every result",
			"results", fmt.Sprint(len(rs)), "positions", fmt.Sprint(len(at)))
	case total == 1 && len(rs) == 1:
		return Write(w, rs[0], f)
	}
	switch f {
	case CSV, TSV, Markdown, Text:
		// Block formats go through WriteBlock, the same call a streaming
		// caller makes one result at a time, so the two can never drift.
		for i, r := range rs {
			if i > 0 {
				if _, err := io.WriteString(w, BlockSep); err != nil {
					return serr.Wrap(err)
				}
			}
			if err := WriteBlock(w, r, f, at[i], total); err != nil {
				return err
			}
		}
		return nil
	case HTML:
		if _, err := io.WriteString(w, htmlDocAll(rs, at, total)); err != nil {
			return serr.Wrap(err)
		}
		return nil
	case JSON:
		return jsonAll(w, rs)
	}
	return serr.New("unknown format", "format", string(f))
}

// RenderOpen produces the document a stream of rs writes when the total was
// open-ended while it ran — a script's s.Show results, whose count is known
// only once the script returns. Every block carries an "#i" banner, a lone
// one included: the stream had to write the first block before it could know
// whether a second would follow, and rendering it bare only when collected
// would make `> file` and `-o file` differ. Only Streamable formats have such
// a document; HTML and JSON are collected whole, so RenderAll serves them.
func RenderOpen(rs []*model.Result, f Format) (string, error) {
	if len(rs) == 0 {
		return "", serr.New("no result to export")
	}
	if !Streamable(f) {
		return "", serr.New("format is not a block format", "format", string(f))
	}
	blocks := make([]string, 0, len(rs))
	for i, r := range rs {
		b, err := RenderBlock(r, f, i+1, 0)
		if err != nil {
			return "", err
		}
		blocks = append(blocks, b)
	}
	return strings.Join(blocks, BlockSep), nil
}

// Pos formats block i's place for a banner or a note: "i/n" against a known
// total, "#i" when the total is open-ended (n <= 0).
func Pos(i, n int) string {
	if n <= 0 {
		return fmt.Sprintf("#%d", i)
	}
	return fmt.Sprintf("%d/%d", i, n)
}

// Seq returns the positions 1…n, for a run in which every statement produced
// a result.
func Seq(n int) []int {
	at := make([]int, n)
	for i := range at {
		at[i] = i + 1
	}
	return at
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
//
// n <= 0 means the total is open-ended — a script's results, which arrive
// until the script returns — and the banner reads "#i" instead of "i/n".
func RenderBlock(r *model.Result, f Format, i, n int) (string, error) {
	var sb strings.Builder
	if err := WriteBlock(&sb, r, f, i, n); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// WriteBlock is RenderBlock to w.
func WriteBlock(w io.Writer, r *model.Result, f Format, i, n int) error {
	if !Streamable(f) {
		return serr.New("format is not a block format", "format", string(f))
	}
	if f == Markdown || f == Text {
		banner := textBanner(r, Pos(i, n))
		if f == Markdown {
			banner = mdBanner(r, Pos(i, n))
		}
		if _, err := io.WriteString(w, banner); err != nil {
			return serr.Wrap(err)
		}
	}
	return Write(w, r, f)
}

// durText is a result's duration as the banners and the JSON envelope say
// it, rounded as the status bar rounds it — a fast statement reads as 380µs
// rather than 0s. One rounding for both (N-208): the envelope rounded to the
// millisecond, so `-t json` wrote "0s" for the statement a text banner said
// took 80µs.
func durText(d time.Duration) string { return d.Round(10 * time.Microsecond).String() }

// summary describes what a statement did, for the multi-result banners.
func summary(r *model.Result) string {
	d := durText(r.Duration)
	if r.IsExec {
		return fmt.Sprintf("%d rows affected in %s", r.Affected, d)
	}
	// a transposed result's rows are its source's columns: the rows the
	// statement returned are its columns, less the one of names
	if r.Transposed {
		return fmt.Sprintf("%d rows in %s, transposed", max(len(r.Columns)-1, 0), d) + truncNote(r)
	}
	return fmt.Sprintf("%d rows in %s", len(r.Rows), d) + truncNote(r)
}

func truncNote(r *model.Result) string {
	if r.Truncated {
		return " (truncated)"
	}
	return ""
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
// The plain-text flavor of an HTML copy is the aligned text table (TextTable),
// NOT the fragment's source. The plain flavor is what the user sees whenever
// the rich one does not make it — a paste target that takes text, the OSC 52
// fallback over SSH, a system with no rich writer, dbc web on a plain-http
// page — and in every one of those the user asked for a TABLE: a screenful of
// <td style="…"> tags is the one outcome that is never useful. An aligned
// table still reads as a table in a monospace target and stays legible in a
// proportional one. (The fragment used to be the plain flavor too, "for
// editors"; that turned every lost rich flavor into a wall of markup. The
// HTML source is still one step away: Export → HTML page.)
//
//	clipboard entry for one "Table for Teams" copy
//	┌────────────────────────┬──────────────────────────────────┐
//	│ text/html  (rich)      │ HTMLFragment: inline-styled table │ ← Teams, Outlook, Docs
//	│ text/plain (fallback)  │ TextTable: aligned columns        │ ← terminals, editors, OSC 52
//	└────────────────────────┴──────────────────────────────────┘
func ClipContent(r *model.Result, f Format) (clip.Content, error) {
	if r == nil {
		return clip.Content{}, serr.New("no result to copy")
	}
	if f == HTML {
		return clip.Content{Text: TextTable(r), HTML: HTMLFragment(r)}, nil
	}
	out, err := Render(r, f)
	if err != nil {
		return clip.Content{}, err
	}
	return clip.Content{Text: out}, nil
}

// PlainCells is the plain, keyboard-shortcut copy of r: one value alone, as
// it is, or rows of tab-separated values without a header — what pastes
// cleanly into a spreadsheet or back into a query. The TUI's y / Y and dbc
// web's copy both use it.
func PlainCells(r *model.Result) string {
	if len(r.Rows) == 1 && len(r.Rows[0]) == 1 {
		return r.Rows[0][0]
	}
	lines := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		lines[i] = strings.Join(row, "\t")
	}
	return strings.Join(lines, "\n")
}

// ToFile renders the result and writes it to path, through WriteFile: a
// failure part way leaves whatever was at path as it was.
func ToFile(r *model.Result, f Format, path string) error {
	return WriteFile(path, func(w io.Writer) error { return Write(w, r, f) })
}

// WriteFile writes a document to path as write produces it, without
// holding it: through a File, which is renamed over path once write has
// returned and the bytes are on disk. On any error path is left as it was.
func WriteFile(path string, write func(io.Writer) error) error {
	f, err := Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	if err = write(bw); err == nil {
		err = bw.Flush()
	}
	if err != nil {
		f.Abort()
		return err
	}
	return f.Commit()
}

// File is an export on its way to path: written into a temporary file
// beside it, and put in its place only by Commit.
//
//	Create ─► .<name>.<random>.partial ─► Write … ─► Commit ─► Sync, Close, Chmod, Rename ─► path
//	                                           └───► Abort ──► the .partial removed; path untouched
//
// Writing straight into path would cost nothing more, but an export that
// fails part way — a full disk, a NaN JSON refuses, a statement that errors
// after its first thousand rows, a Ctrl+C — would leave a truncated file
// where the last good one was. os.WriteFile of a rendered string had that
// only for a write failure, since the render came first; a document
// written as it is made has it for every failure, so the rename. It is the
// one the pipeline's csv.write sink and the user-files writer do.
//
// The file gets the mode of the one it replaces, or 0644 (CreateTemp makes
// 0600, which would be a change from os.WriteFile's). A path that is a
// symlink stays one: the file it points at is replaced. A process killed
// between Create and Commit leaves its dot-named .partial behind.
//
// File does not buffer; its writer does (WriteFile, the headless -o).
type File struct {
	path   string // as given, for errors
	target string // path, its symlinks resolved: what the rename replaces
	mode   os.FileMode
	tmp    *os.File
	done   bool // committed or aborted
}

// Create starts an export to path. Its directory must exist, as
// os.WriteFile's had to.
func Create(path string) (*File, error) {
	f := &File{path: path, target: path, mode: 0o644}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		f.target = real
	}
	if fi, err := os.Stat(f.target); err == nil {
		f.mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.target), "."+filepath.Base(f.target)+".*.partial")
	if err != nil {
		return nil, serr.Wrap(err, "path", path)
	}
	f.tmp = tmp
	return f, nil
}

// Write writes to the temporary file.
func (f *File) Write(p []byte) (int, error) { return f.tmp.Write(p) }

// Commit puts the file in place of path. On an error it aborts, and path
// is left as it was.
func (f *File) Commit() (err error) {
	if f.done {
		return serr.New("export file already finished", "path", f.path)
	}
	defer func() {
		if err != nil {
			f.Abort()
		}
	}()
	// fsync before the rename: otherwise a crash just after it can leave
	// the name pointing at a file whose blocks never reached the disk
	if err = f.tmp.Sync(); err != nil {
		return serr.Wrap(err, "path", f.path)
	}
	if err = f.tmp.Close(); err != nil {
		return serr.Wrap(err, "path", f.path)
	}
	if err = os.Chmod(f.tmp.Name(), f.mode); err != nil {
		return serr.Wrap(err, "path", f.path)
	}
	if err = os.Rename(f.tmp.Name(), f.target); err != nil {
		return serr.Wrap(err, "path", f.path)
	}
	f.done = true
	return nil
}

// Abort drops the temporary file; path is untouched. A no-op once the
// file is committed or aborted.
func (f *File) Abort() {
	if f.done {
		return
	}
	f.done = true
	_ = f.tmp.Close() // may be closed already (a Commit that failed after it)
	_ = os.Remove(f.tmp.Name())
}

// delimited writes r as CSV, or TSV with comma '\t'. csv.Writer buffers
// on its own and keeps the first write error; its Error reports it. A SQL
// NULL is an empty field (DelimitedRecord).
func delimited(w io.Writer, r *model.Result, comma rune) error {
	cw := csv.NewWriter(w)
	cw.Comma = comma
	if err := cw.Write(r.Columns); err != nil {
		return serr.Wrap(err)
	}
	var rec []string
	for i, row := range r.Rows {
		var raw []any
		if i < len(r.Raw) {
			raw = r.Raw[i]
		}
		rec = DelimitedRecord(rec, row, raw)
		if err := cw.Write(rec); err != nil {
			return serr.Wrap(err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

// DelimitedRecord is a row as a CSV or TSV record: its display text, with a
// SQL NULL (a nil raw value) as an empty field. The text "NULL" a NULL
// displays as is the same as the string 'NULL' in a text column, which a
// reader of the file could not tell apart; an empty field is how CSV says
// "no value" to spreadsheets, COPY … CSV and LOAD DATA alike. (An empty
// string is an empty field too: CSV has no way to tell those two apart
// without quoting rules no reader agrees on.)
//
// dst is reused when it has room, so a caller writing row after row
// allocates one record; disp is never written to. A row with no raw values
// (a result built by hand) is its text as it is.
func DelimitedRecord(dst, disp []string, raw []any) []string {
	dst = append(dst[:0], disp...)
	for i := range dst {
		if i < len(raw) && raw[i] == nil {
			dst[i] = ""
		}
	}
	return dst
}

// jsonValue is a raw value as a JSON export writes it. encoding/json
// refuses a NaN or an infinite float, which used to fail the whole -t json
// render after the query had run; Postgres keeps those in float columns as
// values, not NULLs, so they are written as the strings it spells them
// with ("NaN", "Infinity", "-Infinity"). Every other value is itself.
func jsonValue(v any) any {
	switch f := v.(type) {
	case float64:
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nonFinite(f)
		}
	case float32:
		if g := float64(f); math.IsNaN(g) || math.IsInf(g, 0) {
			return nonFinite(g)
		}
	}
	return v
}

func nonFinite(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case f > 0:
		return "Infinity"
	}
	return "-Infinity"
}

// markdown writes r as a Markdown table. A cell's pipes are escaped and
// its line breaks become spaces, so it stays one table row.
func markdown(w *bufio.Writer, r *model.Result) {
	esc := func(s string) string {
		// most cells need none of it: three ReplaceAlls that find nothing
		// still scan, but allocate nothing
		s = strings.ReplaceAll(s, "|", `\|`)
		s = strings.ReplaceAll(s, "\r\n", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		return s
	}
	w.WriteString("| ")
	for i, c := range r.Columns {
		if i > 0 {
			w.WriteString(" | ")
		}
		w.WriteString(esc(c))
	}
	w.WriteString(" |\n|")
	for range r.Columns {
		w.WriteString(" --- |")
	}
	w.WriteString("\n")
	for _, row := range r.Rows {
		w.WriteString("| ")
		for i, v := range row {
			if i > 0 {
				w.WriteString(" | ")
			}
			w.WriteString(esc(v))
		}
		w.WriteString(" |\n")
	}
}

// jsonArr writes r as a JSON array of row objects, one row at a time —
// the bytes json.MarshalIndent(rowMaps(r), "", "  ") made of the whole
// array (TestWriteMatchesRender) — through JSONRows, the writer a
// streamed export uses too, so the two cannot differ.
func jsonArr(w *bufio.Writer, r *model.Result) error {
	j := NewJSONRows(w, r.Columns)
	for _, row := range r.Raw {
		if err := j.Row(row); err != nil {
			return err
		}
	}
	return j.End()
}

// JSONRows writes one result set's rows as a JSON array of row objects, a
// row at a time, as they are handed to it — what a lone -t json statement
// streams from the driver (N-332), and what jsonArr writes of a held
// result:
//
//	[\n
//	  {…row 1, indented one level deeper…},\n
//	  {…row 2…}\n
//	]\n
//
// and "[]\n" for no rows. Each row is the one map, cleared and refilled,
// encoded with the array's indent as its prefix: Indent puts the prefix
// after every newline it writes and none before the first byte, which is
// where the array's own "  " goes. Keys come out sorted, as a map's always
// did; duplicate column names are suffixed (jsonKeys); a non-finite float
// is its string (jsonValue).
type JSONRows struct {
	w    io.Writer
	keys []string
	m    map[string]any
	buf  bytes.Buffer
	enc  *json.Encoder
	n    int
}

// NewJSONRows starts a JSON array of rows with columns cols on w.
func NewJSONRows(w io.Writer, cols []string) *JSONRows {
	j := &JSONRows{w: w, keys: jsonKeys(cols)}
	j.m = make(map[string]any, len(j.keys))
	j.enc = json.NewEncoder(&j.buf) // reuses its indent buffer row to row
	j.enc.SetIndent("  ", "  ")
	return j
}

// Row writes one row of raw values, in the columns' order.
func (j *JSONRows) Row(row []any) error {
	clear(j.m)
	for ci, key := range j.keys {
		if ci < len(row) {
			j.m[key] = jsonValue(row[ci])
		}
	}
	j.buf.Reset()
	if err := j.enc.Encode(j.m); err != nil {
		return serr.Wrap(err, "row", strconv.Itoa(j.n+1))
	}
	sep := ",\n  "
	if j.n == 0 {
		sep = "[\n  "
	}
	j.n++
	if _, err := io.WriteString(j.w, sep); err != nil {
		return err
	}
	// Encode ends every value with a newline; the array's separator takes
	// its place
	_, err := j.w.Write(bytes.TrimSuffix(j.buf.Bytes(), []byte{'\n'}))
	return err
}

// End closes the array: "[]\n" when no row was written.
func (j *JSONRows) End() error {
	end := "\n]\n"
	if j.n == 0 {
		end = "[]\n"
	}
	_, err := io.WriteString(j.w, end)
	return err
}

// rowMaps turns the typed rows into column-keyed objects.
func rowMaps(r *model.Result) []map[string]any {
	keys := jsonKeys(r.Columns)
	out := make([]map[string]any, 0, len(r.Raw))
	for _, row := range r.Raw {
		m := make(map[string]any, len(keys))
		for i, key := range keys {
			if i < len(row) {
				m[key] = jsonValue(row[i])
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
//
// The document is built whole, as it was (its envelopes' keys sort with
// "rows" between "duration" and "statement", so a row cannot go out before
// its envelope's head is known), and encoded straight to w: an Encoder
// with the indent writes MarshalIndent's bytes and the newline marshal
// adds, without the string and []byte copies of them.
func jsonAll(w io.Writer, rs []*model.Result) error {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{
			"statement": r.Query,
			"conn":      r.Conn,
			"duration":  durText(r.Duration),
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
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return serr.Wrap(err)
	}
	return nil
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

func htmlDoc(r *model.Result) string { return htmlDocAll([]*model.Result{r}, []int{1}, 1) }

// htmlDocAll builds one page with a section per result. With a single result
// the section is the whole page, as a one-statement export always was.
func htmlDocAll(rs []*model.Result, at []int, total int) string {
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
				if total > 1 {
					heading = fmt.Sprintf("Statement %d of %d", at[i], total)
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
								element.ForEach2(row, func(v string, ci int) {
									// a transposed result's names column is
									// row headers (see Transpose); the page's
									// th style already marks them as labels
									if r.Transposed && ci == 0 {
										b.Th("scope", "row").T(esc(v))
										return
									}
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
	var sb strings.Builder
	bw := bufio.NewWriter(&sb)
	writeText(bw, r)
	_ = bw.Flush() // a strings.Builder does not fail
	return sb.String()
}

// textClip is a value as a text table cell: line breaks as spaces, cut to
// maxTextColWidth runes with an ellipsis. Its rune count comes along, since
// the caller pads by it.
//
// The table used to hold every cell clipped — a second copy of the
// result, made through a []rune of each value (four bytes a character) —
// to measure the widths before writing. It now clips each value twice,
// once to measure and once to write, and a value with no line break, no
// invalid UTF-8 and at most maxTextColWidth runes (almost all of them) is
// its own clip, with no allocation either time. The rest go the old way,
// []rune and back, which is also what turns an invalid byte into U+FFFD —
// so the text is the same, byte for byte.
func textClip(s string) (string, int) {
	if strings.IndexByte(s, '\n') < 0 && utf8.ValidString(s) {
		if n := utf8.RuneCountInString(s); n <= maxTextColWidth {
			return s, n
		}
	}
	rs := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(rs) > maxTextColWidth {
		return string(rs[:maxTextColWidth-1]) + "…", maxTextColWidth
	}
	return string(rs), len(rs)
}

// textPad is blanks to pad cells from, a slice at a time, in place of a
// strings.Repeat per cell.
const textPad = "                                                                "

// writeText writes r as TextTable lays it out: the widest clipped value of
// each column (or its header) sets the column's width, every column but
// the last is padded to it, and columns are two spaces apart.
func writeText(w *bufio.Writer, r *model.Result) {
	widths := make([]int, len(r.Columns))
	for i, c := range r.Columns {
		widths[i] = utf8.RuneCountInString(c)
	}
	for _, row := range r.Rows {
		for ci, v := range row {
			if _, n := textClip(v); ci < len(widths) && n > widths[ci] {
				widths[ci] = n
			}
		}
	}
	pad := func(n int) {
		for n > 0 {
			k := min(n, len(textPad))
			w.WriteString(textPad[:k])
			n -= k
		}
	}
	// a header is written as it is, unclipped, as it always was; values
	// are clipped
	writeRow := func(cells []string, clip bool) {
		for i, c := range cells {
			if i > 0 {
				w.WriteString("  ")
			}
			n := 0
			if clip {
				c, n = textClip(c)
			} else {
				n = utf8.RuneCountInString(c)
			}
			w.WriteString(c)
			if i < len(cells)-1 {
				pad(widths[i] - n)
			}
		}
		w.WriteString("\n")
	}
	writeRow(r.Columns, false)
	for i, wd := range widths {
		if i > 0 {
			w.WriteString("  ")
		}
		// a rule is exactly its column's width, so it never needs padding
		for range wd {
			w.WriteByte('-')
		}
	}
	w.WriteString("\n")
	for _, row := range r.Rows {
		writeRow(row, true)
	}
}
