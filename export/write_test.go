package export

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/model"
)

// The renderers as they were before they wrote to an io.Writer (N-282),
// kept as the reference the writers must match byte for byte.

func oldDelimited(r *model.Result, comma rune) (string, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	w.Comma = comma
	if err := w.Write(r.Columns); err != nil {
		return "", err
	}
	if err := w.WriteAll(r.Rows); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func oldMarkdown(r *model.Result) string {
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

func oldMarshal(v any) (string, error) {
	bs, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(bs) + "\n", nil
}

func oldJSONAll(rs []*model.Result) (string, error) {
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
			m["columns"] = jsonKeys(r.Columns)
			m["rows"] = rowMaps(r)
			if r.Truncated {
				m["truncated"] = true
			}
		}
		out = append(out, m)
	}
	return oldMarshal(out)
}

func oldTextTable(r *model.Result) string {
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

// oldRender is Render as it was, format by format.
func oldRender(r *model.Result, f Format) (string, error) {
	switch f {
	case CSV:
		return oldDelimited(r, ',')
	case TSV:
		return oldDelimited(r, '\t')
	case Markdown:
		return oldMarkdown(r), nil
	case HTML:
		return htmlDoc(r), nil
	case JSON:
		return oldMarshal(rowMaps(r))
	case Text:
		return oldTextTable(r), nil
	}
	return "", errors.New("unknown format")
}

// writeCases are results with the awkward parts each renderer has: NULLs,
// line breaks, pipes, quotes and commas, long and wide values, invalid
// UTF-8, HTML's special characters, nested JSON values, duplicate column
// names, no rows, no columns, and a value JSON refuses.
func writeCases() map[string]*model.Result {
	long := strings.Repeat("abcdefghij", 8)
	wide := strings.Repeat("日本語", 25)
	cases := map[string]*model.Result{
		"plain": {
			Conn: "c", Query: "SELECT 1", Duration: 380 * time.Microsecond,
			Columns: []string{"id", "name", "note"},
			Rows:    [][]string{{"1", "Ann", "NULL"}, {"2", "Bo, \"B\"", "a|b\r\nc\nd"}},
			Raw:     [][]any{{int64(1), "Ann", nil}, {int64(2), "Bo, \"B\"", "a|b\r\nc\nd"}},
		},
		"wide": {
			Columns: []string{"k", "v"},
			Rows:    [][]string{{long, wide}, {"\xff\xfebad", "short"}, {"x\ny", long + "\n" + wide}},
			Raw:     [][]any{{long, wide}, {"\xff\xfebad", "short"}, {"x\ny", long + "\n" + wide}},
		},
		"json": {
			Columns: []string{"a", "a", "doc", "html"},
			Rows:    [][]string{{"1", "2", "{}", "<b>&</b>"}},
			Raw: [][]any{{1.5, true, map[string]any{"z": []any{1.0, "x", nil}, "a": map[string]any{}},
				"<b>&</b>"}},
		},
		"empty":     {Columns: []string{"id"}},
		"nocols":    {Rows: [][]string{{}, {}}, Raw: [][]any{{}, {}}},
		"shortraw":  {Columns: []string{"a", "b"}, Rows: [][]string{{"1", "2"}}, Raw: [][]any{{int64(1)}}},
		"rowsonly":  {Columns: []string{"a"}, Rows: [][]string{{"1"}}},
		"truncated": {Columns: []string{"a"}, Rows: [][]string{{"1"}}, Raw: [][]any{{int64(1)}}, Truncated: true},
		"exec":      {IsExec: true, Affected: 3, Columns: []string{"rows_affected"}, Rows: [][]string{{"3"}}, Raw: [][]any{{int64(3)}}},
	}
	// many rows, so the writers' buffers fill and flush part way
	big := &model.Result{Columns: []string{"n", "s"}}
	for i := range 3000 {
		s := "row " + strconv.Itoa(i) + strings.Repeat("·", i%70)
		big.Rows = append(big.Rows, []string{strconv.Itoa(i), s})
		big.Raw = append(big.Raw, []any{int64(i), s})
	}
	cases["big"] = big
	return cases
}

// Write, WriteRun and the renderers built on them make exactly what the
// renderers made before they wrote to a writer.
func TestWriteMatchesRender(t *testing.T) {
	cases := writeCases()
	for name, r := range cases {
		for _, f := range []Format{CSV, TSV, Markdown, HTML, JSON, Text} {
			want, werr := oldRender(r, f)
			got, gerr := Render(r, f)
			if (werr != nil) != (gerr != nil) || got != want {
				t.Errorf("%s as %s: got (%d bytes, %v), want (%d bytes, %v)\n got: %.300q\nwant: %.300q",
					name, f, len(got), gerr, len(want), werr, got, want)
			}
		}
	}
	// a multi-result run: blocks, banners, envelopes
	rs := []*model.Result{cases["plain"], cases["exec"], cases["json"], cases["truncated"]}
	for _, f := range []Format{CSV, TSV, Markdown, Text, JSON} {
		var want string
		if f == JSON {
			want, _ = oldJSONAll(rs)
		} else {
			var blocks []string
			for i, r := range rs {
				b := ""
				switch f {
				case Markdown:
					b = mdBanner(r, Pos(i+1, 6)) + oldMarkdown(r)
				case Text:
					b = textBanner(r, Pos(i+1, 6)) + oldTextTable(r)
				default:
					comma := ','
					if f == TSV {
						comma = '\t'
					}
					b, _ = oldDelimited(r, comma)
				}
				blocks = append(blocks, b)
			}
			want = strings.Join(blocks, BlockSep)
		}
		got, err := RenderRun(rs, []int{1, 2, 3, 4}, 6, f)
		if err != nil || got != want {
			t.Errorf("run as %s: %v\n got: %.400q\nwant: %.400q", f, err, got, want)
		}
	}
}

// A value JSON cannot hold fails the export, as it did, and says which row.
func TestWriteJSONRefusesNaN(t *testing.T) {
	r := &model.Result{Columns: []string{"x"}, Rows: [][]string{{"1"}, {"NaN"}},
		Raw: [][]any{{1.0}, {math.NaN()}}}
	_, err := Render(r, JSON)
	if err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("err = %v, want the encoder's NaN refusal", err)
	}
	if !strings.Contains(serr.StringFromErr(err), "row") {
		t.Errorf("err %q does not name the row", serr.StringFromErr(err))
	}
}

// failWriter takes n bytes and then fails, as a full disk does.
type failWriter struct{ n int }

var errFull = errors.New("no space left on device")

func (w *failWriter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		k := w.n
		w.n = 0
		return k, errFull
	}
	w.n -= len(p)
	return len(p), nil
}

// A writer's failure part way is the export's failure, in every format.
func TestWriteReportsWriterFailure(t *testing.T) {
	big := writeCases()["big"]
	for _, f := range []Format{CSV, TSV, Markdown, HTML, JSON, Text} {
		if err := Write(&failWriter{n: 10_000}, big, f); !errors.Is(err, errFull) {
			t.Errorf("%s: err = %v, want the writer's", f, err)
		}
	}
}

// WriteFile replaces path only once the document is whole: a failure part
// way leaves the old file as it was and no temporary file behind. The new
// file keeps the old one's mode, a new one gets 0644, and a symlink stays
// a symlink.
func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("render failed")
	err := WriteFile(path, func(w io.Writer) error {
		_, _ = io.WriteString(w, strings.Repeat("partial\n", 20_000))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old\n" {
		t.Errorf("a failed write changed the file: %.40q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	if err = ToFile(writeCases()["plain"], CSV, path); err != nil {
		t.Fatal(err)
	}
	want, _ := Render(writeCases()["plain"], CSV)
	if b, _ := os.ReadFile(path); string(b) != want {
		t.Errorf("wrote %q, want %q", b, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want the old file's 0600", fi.Mode().Perm())
	}

	fresh := filepath.Join(dir, "fresh.json")
	if err = ToFile(writeCases()["json"], JSON, fresh); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o644 {
		t.Errorf("a new file's mode %v, want 0644", fi.Mode().Perm())
	}

	link := filepath.Join(dir, "link.csv")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err = ToFile(writeCases()["empty"], CSV, link); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced by a file")
	}
	if b, _ := os.ReadFile(path); string(b) != "id\n" {
		t.Errorf("the link's target holds %q, want the new export", b)
	}

	// a directory that is not there fails before anything is written
	if err = ToFile(writeCases()["plain"], CSV, filepath.Join(dir, "no", "such.csv")); err == nil {
		t.Errorf("no error for a missing directory")
	}
}

// BenchmarkExport is a 100,000-row result rendered and written to a file,
// the TUI's Export and `-o`'s path, in each format that writes row by row.
func BenchmarkExport(b *testing.B) {
	r := &model.Result{Columns: []string{"id", "name", "email", "created", "score"}}
	for i := range 100_000 {
		name := "Customer " + strconv.Itoa(i)
		email := "c" + strconv.Itoa(i) + "@example.com"
		r.Rows = append(r.Rows, []string{strconv.Itoa(i), name, email, "2026-11-16T09:30:00Z", "12.5"})
		r.Raw = append(r.Raw, []any{int64(i), name, email, "2026-11-16T09:30:00Z", 12.5})
	}
	path := filepath.Join(b.TempDir(), "out")
	for _, f := range []Format{CSV, JSON, Text, Markdown} {
		b.Run(string(f), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := ToFile(r, f, path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
