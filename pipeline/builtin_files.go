package pipeline

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rohanthewiz/serr"
)

// The file plugins: CSV and JSON Lines as sources and as sinks.
//
// The sources read text and say so: every CSV value is a string (a
// cols.cast after the source types them), and a JSON Lines value is what
// JSON makes it (string, int64 or float64, bool, NULL; a nested object or
// array as its JSON text). No type sniffing: a column of zip codes stays
// "02134", and what a value becomes is written in the spec, not guessed
// from the first thousand rows.
//
// The sinks keep the fragment's promise that nothing is visible before
// Commit. They write into a temporary file beside the destination and
// rename it over the destination at Commit — a rename within a directory
// is atomic on every OS dbc runs on — so a fragment that fails (or is
// stopped) leaves an existing file exactly as it was, and a reader never
// sees half a file:
//
//	Open    ─► .out.csv.123456.partial  (os.CreateTemp, same directory)
//	Write…  ─► buffered rows into it
//	Commit  ─► flush, fsync, close, chmod 0644, rename over out.csv
//	Abort   ─► close, remove the .partial
//
// Paths (Env.Path): a leading ~/ is the home directory; a relative path is
// in files_dir — the home directory unless the config sets it — whichever
// process runs the pipeline: `dbc pipeline run` in a shell, dbc web, its
// scheduler, dbc.app. Each of those has a working directory of its own,
// so resolving against it made one spec read and write different files
// depending on who ran it.

func init() {
	Register(Plugin{
		Name: "csv.read", Kind: KindSource, Label: "CSV file",
		Doc: "Streams the rows of a CSV file. Every value is text (empty fields NULL unless empty_null is off): " +
			"put a cols.cast after it for numbers, booleans and times. The first record names the columns unless " +
			"header is off; columns renames them, or names them for a file without a header (c1, c2, … otherwise). " +
			"A record with a different number of fields than the first is an error naming its line.",
		Fields: []Field{
			{Name: "path", Type: FieldString, Required: true, Doc: "The file: ~/ for the home directory; a relative path is in files_dir (the home directory unless the config sets it)."},
			{Name: "delimiter", Type: FieldString, Default: ",", Doc: `The field separator, one character; \t or tab for a tab.`},
			{Name: "header", Type: FieldBool, Default: "true", Doc: "The first record names the columns."},
			{Name: "columns", Type: FieldColumns, Doc: "Column names, in file order: replace the header's, or name a file without one."},
			{Name: "empty_null", Type: FieldBool, Default: "true", Doc: "An empty field is NULL; off keeps it as an empty string."},
		},
		New: func(cfg Config) (any, error) {
			delim, err := parseDelim(cfg.Str("delimiter", ","))
			if err != nil {
				return nil, err
			}
			header, _ := cfg.Bool("header")
			emptyNull, _ := cfg.Bool("empty_null")
			return &csvRead{path: strings.TrimSpace(cfg["path"]), delim: delim, header: header,
				names: cfg.List("columns"), emptyNull: emptyNull}, nil
		},
		Check: checkDelim,
	})
	Register(Plugin{
		Name: "jsonl.read", Kind: KindSource, Label: "JSON Lines file",
		Doc: "Streams a JSON Lines file: one JSON object per line, blank lines skipped. The columns are the first " +
			"object's keys in the order written, or those named in columns. A string stays a string, a whole number " +
			"is an integer and any other number a float, true/false a boolean, null NULL; a nested object or array " +
			"is kept as its JSON text. A key a row lacks is NULL; keys outside the columns are dropped (and counted in the log).",
		Fields: []Field{
			{Name: "path", Type: FieldString, Required: true, Doc: "The file: ~/ for the home directory; a relative path is in files_dir (the home directory unless the config sets it)."},
			{Name: "columns", Type: FieldColumns, Doc: "The keys to read, in this order; empty means the first object's keys."},
		},
		New: func(cfg Config) (any, error) {
			return &jsonlRead{path: strings.TrimSpace(cfg["path"]), names: cfg.List("columns")}, nil
		},
	})
	Register(Plugin{
		Name: "csv.write", Kind: KindSink, Label: "CSV file (write)",
		Doc: "Writes the rows to a CSV file, replacing it whole when the fragment ends: the rows go to a temporary " +
			"file beside it, renamed over it at the end, so a failed fragment leaves the old file untouched. " +
			"Numbers are written in full (no exponent for ordinary sizes), times in RFC 3339, bytes in hex, " +
			"arrays and maps as JSON. Missing directories are created. Publishes rows and path.",
		Fields: []Field{
			{Name: "path", Type: FieldString, Required: true, Doc: "The file to write: ~/ for the home directory; a relative path is in files_dir (the home directory unless the config sets it)."},
			{Name: "delimiter", Type: FieldString, Default: ",", Doc: `The field separator, one character; \t or tab for a tab.`},
			{Name: "header", Type: FieldBool, Default: "true", Doc: "Write the column names first."},
			{Name: "null", Type: FieldString, Doc: `What a NULL is written as (default an empty field; \N for Postgres COPY).`},
		},
		New: func(cfg Config) (any, error) {
			delim, err := parseDelim(cfg.Str("delimiter", ","))
			if err != nil {
				return nil, err
			}
			header, _ := cfg.Bool("header")
			return &fileWrite{path: strings.TrimSpace(cfg["path"]), csv: true, delim: delim, header: header,
				null: cfg["null"]}, nil
		},
		Check: checkDelim,
	})
	Register(Plugin{
		Name: "jsonl.write", Kind: KindSink, Label: "JSON Lines file (write)",
		Doc: "Writes the rows to a JSON Lines file, one object per row with the keys in column order, replacing the " +
			"file whole when the fragment ends (a failed fragment leaves the old one untouched). NULL is null, " +
			"times are RFC 3339 strings, bytes base64, NaN and infinities null. Missing directories are created. " +
			"Publishes rows and path.",
		Fields: []Field{
			{Name: "path", Type: FieldString, Required: true, Doc: "The file to write: ~/ for the home directory; a relative path is in files_dir (the home directory unless the config sets it)."},
		},
		New: func(cfg Config) (any, error) {
			return &fileWrite{path: strings.TrimSpace(cfg["path"])}, nil
		},
	})
}

// checkDelim is the Check of a plugin with a delimiter field. A value
// still holding a ${…} reference is left to the run, where it has been
// substituted.
func checkDelim(cfg Config) []string {
	if len(Refs(cfg["delimiter"])) > 0 {
		return nil
	}
	if _, err := parseDelim(cfg.Str("delimiter", ",")); err != nil {
		return []string{"delimiter: " + err.Error()}
	}
	return nil
}

// Path is p as a node opens it:
//
//	"~" or "~/…"    the home directory
//	absolute        as written
//	relative        joined to files_dir (Options.FilesDir), so "exports/orders.csv"
//	                is one file whether a shell, dbc web, the scheduler or
//	                dbc.app runs the pipeline; with no FilesDir, left
//	                relative to the working directory
//
// The built-in file plugins open every path through it; a plugin of one's
// own that reads or writes a file does the same (e.Path(e.Cfg.Str("path",
// ""))), so its paths follow files_dir as theirs do. A nil Env has no
// files_dir.
func (e *Env) Path(p string) (string, error) {
	if e.inFilesDir(p) {
		return filepath.Join(e.filesDir, p), nil
	}
	return expandPath(p)
}

// inFilesDir reports whether Path joins p to files_dir: there is one, and
// p is neither absolute nor under ~.
func (e *Env) inFilesDir(p string) bool {
	return e != nil && e.filesDir != "" && p != "" && !filepath.IsAbs(p) && !isHomePath(p)
}

// pathErr wraps err from op ("open") on the file at p, which Path made
// from raw. A raw path that was relative also names files_dir: "no such
// file" for orders.csv then says where it was looked for, and why there,
// to a user who expected the directory they ran dbc in. In the message,
// not a serr field: a node's error is shown by its text (NodeStats.Error,
// the run monitor), which a field does not reach.
func (e *Env) pathErr(err error, op, raw, p string) error {
	if e.inFilesDir(raw) {
		err = serr.F("%w (a relative path is in files_dir, %s)", err, e.filesDir)
	}
	return serr.Wrap(err, "op", op, "path", p)
}

// isHomePath reports whether p starts at the home directory: "~", "~/…",
// or "~\…" as written on Windows.
func isHomePath(p string) bool {
	return p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`)
}

// expandPath resolves a leading ~ to the home directory. Anything else is
// left as written; Path, which nodes call, joins a relative one to
// files_dir first.
func expandPath(p string) (string, error) {
	if !isHomePath(p) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", serr.Wrap(err, "op", "expand ~", "path", p)
	}
	return filepath.Join(home, p[1:]), nil
}

// parseDelim reads a delimiter field: one character, or \t / tab for a
// tab (a literal tab is hard to type into a form or a JSON string).
func parseDelim(s string) (rune, error) {
	switch strings.ToLower(s) {
	case `\t`, "tab":
		return '\t', nil
	}
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 || size != len(s) || r == utf8.RuneError {
		return 0, serr.F(`a delimiter is one character (\t or tab for a tab), not %q`, s)
	}
	// encoding/csv's own rules, said here rather than as its later
	// "invalid field or comment delimiter"
	if r == '"' || r == '\r' || r == '\n' {
		return 0, serr.F("a delimiter cannot be a quote or a line break (%q)", s)
	}
	return r, nil
}

// uniqueNames makes a column list usable: a blank name becomes cN (its
// 1-based position) and a repeated one gets _2, _3 … — a sink creating a
// table, and Batch.Col, both need every name distinct.
func uniqueNames(names []string) []string {
	out := make([]string, len(names))
	taken := map[string]bool{}
	for i, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			n = "c" + strconv.Itoa(i+1)
		}
		base, k := n, 2
		for taken[n] {
			n = base + "_" + strconv.Itoa(k)
			k++
		}
		taken[n] = true
		out[i] = n
	}
	return out
}

// csvRead is csv.read.
type csvRead struct {
	path      string
	delim     rune
	header    bool
	names     []string // the columns field
	emptyNull bool

	f       *os.File
	r       *csv.Reader
	cols    []Col
	pending []string // the first data record, read at Open to count the fields
	done    bool
}

// Open reads the first record: the header, or the first data record held
// back for Next. Either way the column list is known before any batch,
// so a sink no rows reach (a header-only file) still opens with it — the
// runner asks through Cols.
func (s *csvRead) Open(e *Env) error {
	p, err := e.Path(s.path)
	if err != nil {
		return err
	}
	if s.f, err = os.Open(p); err != nil {
		return e.pathErr(err, "open", s.path, p)
	}
	s.path = p
	s.r = csv.NewReader(bufio.NewReaderSize(s.f, 64<<10))
	s.r.Comma = s.delim
	s.r.FieldsPerRecord = 0 // set by the first record: a ragged one after it is an error
	s.r.ReuseRecord = false // the strings go into rows that outlive the call
	first, err := s.r.Read()
	if errors.Is(err, io.EOF) {
		// an empty file: no rows, and no columns unless columns names them
		s.done = true
		if len(s.names) > 0 {
			s.cols = ColsOf(uniqueNames(s.names), nil)
		}
		return nil
	}
	if err != nil {
		return s.readErr(err)
	}
	if len(first) > 0 {
		first[0] = strings.TrimPrefix(first[0], "\uFEFF") // a UTF-8 BOM, as Excel writes one
	}
	names := s.names
	switch {
	case len(names) > 0 && len(names) != len(first):
		return serr.F("%s: columns names %d columns, the file has %d", s.path, len(names), len(first))
	case len(names) == 0 && s.header:
		names = first
	case len(names) == 0:
		names = make([]string, len(first)) // c1, c2, … from uniqueNames
	}
	s.cols = ColsOf(uniqueNames(names), nil)
	if !s.header {
		s.pending = first
	}
	return nil
}

// readErr names the file and, for a parse error, the line.
func (s *csvRead) readErr(err error) error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		// the message already says "record on line N: …"
		return serr.F("%s: %w", s.path, err)
	}
	return serr.F("%s: %w", s.path, err)
}

// Cols is the columns, known after Open.
func (s *csvRead) Cols() []Col { return s.cols }

func (s *csvRead) Next(e *Env) (*Batch, error) {
	if s.done && s.pending == nil {
		return nil, nil
	}
	n := e.Batch
	if n <= 0 {
		n = DefaultBatch
	}
	rows := make([][]any, 0, min(n, 4096))
	add := func(rec []string) {
		row := make([]any, len(rec))
		for i, v := range rec {
			if v == "" && s.emptyNull {
				row[i] = nil
			} else {
				row[i] = v
			}
		}
		rows = append(rows, row)
	}
	if s.pending != nil {
		add(s.pending)
		s.pending = nil
	}
	for !s.done && len(rows) < n {
		rec, err := s.r.Read()
		if errors.Is(err, io.EOF) {
			s.done = true
			break
		}
		if err != nil {
			return nil, s.readErr(err)
		}
		add(rec)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &Batch{Cols: slices.Clone(s.cols), Rows: rows}, nil
}

func (s *csvRead) Close(bool) error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// jsonlRead is jsonl.read.
type jsonlRead struct {
	path  string
	names []string // the columns field

	env     *Env // kept from Open for the closing log line
	f       *os.File
	r       *bufio.Reader
	line    int // the last line read, 1-based, for messages
	cols    []Col
	index   map[string]int // column name → position
	pending []any          // the first object, read at Open to learn the keys
	done    bool
	// rows with keys outside the columns, and which keys: said once at
	// the end rather than per row, so a wide file does not flood the log
	extraRows int64
	extraKeys map[string]bool
}

func (s *jsonlRead) Open(e *Env) error {
	s.env = e
	p, err := e.Path(s.path)
	if err != nil {
		return err
	}
	if s.f, err = os.Open(p); err != nil {
		return e.pathErr(err, "open", s.path, p)
	}
	s.path = p
	// a bufio.Reader rather than a Scanner: ReadBytes grows to any line
	// length, where a Scanner stops at 64 KB unless told a maximum
	s.r = bufio.NewReaderSize(s.f, 64<<10)
	s.extraKeys = map[string]bool{}
	if len(s.names) > 0 {
		s.setCols(s.names)
		return nil
	}
	// no columns named: the first object's keys, in the order written
	keys, vals, err := s.nextObject()
	if err != nil {
		return err
	}
	if keys == nil {
		s.done = true // no objects at all
		return nil
	}
	s.setCols(keys)
	s.pending = s.row(keys, vals)
	return nil
}

func (s *jsonlRead) setCols(names []string) {
	names = uniqueNames(names)
	s.cols = ColsOf(names, nil)
	s.index = make(map[string]int, len(names))
	for i, n := range names {
		s.index[n] = i
	}
}

// Cols is the columns, known after Open.
func (s *jsonlRead) Cols() []Col { return s.cols }

// nextObject reads up to the next non-blank line and parses it as an
// object. nil keys at the end of the file.
func (s *jsonlRead) nextObject() ([]string, []any, error) {
	for {
		raw, err := s.r.ReadBytes('\n')
		if len(raw) == 0 && errors.Is(err, io.EOF) {
			return nil, nil, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, serr.F("%s:%d: %w", s.path, s.line+1, err)
		}
		s.line++
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		keys, vals, perr := parseObject(line)
		if perr != nil {
			return nil, nil, serr.F("%s:%d: %w", s.path, s.line, perr)
		}
		return keys, vals, nil
	}
}

// parseObject is one JSON Lines line as keys and values, keeping the
// keys' order (encoding/json's map would lose it): the decoder is walked
// token by token — '{', then key, value, key, value … — each value taken
// whole as a RawMessage and converted by jsonValue. A repeated key keeps
// its first place and its last value, as encoding/json would.
func parseObject(line []byte) ([]string, []any, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, serr.New("not a JSON object")
	}
	keys := []string{} // not nil: an empty object is an object, not the end of the file
	var vals []any
	at := map[string]int{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, serr.F("a key: %w", err)
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err = dec.Decode(&v); err != nil {
			return nil, nil, serr.F("key %q: %w", key, err)
		}
		val, err := jsonValue(v)
		if err != nil {
			return nil, nil, serr.F("key %q: %w", key, err)
		}
		if i, dup := at[key]; dup {
			vals[i] = val
			continue
		}
		at[key] = len(keys)
		keys = append(keys, key)
		vals = append(vals, val)
	}
	if _, err = dec.Token(); err != nil { // the closing '}'
		return nil, nil, serr.F("the object's end: %w", err)
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, serr.New("more than one JSON value on the line")
	}
	return keys, vals, nil
}

// jsonValue turns one raw JSON value into a row value, as the plugin's
// Doc says: by its first byte, which JSON makes unambiguous.
func jsonValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	switch raw[0] {
	case 'n':
		return nil, nil
	case 't', 'f':
		return raw[0] == 't', nil
	case '"':
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	case '{', '[':
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			return nil, err
		}
		return b.String(), nil
	}
	s := string(raw)
	n, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, strconv.ErrRange) {
		// a whole number past int64: kept as its exact text rather than
		// rounded through a float — an id that big must not change
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, serr.F("not a number: %s", s)
	}
	return f, nil
}

// row places an object's values in the columns: a missing key is NULL,
// a key outside them is counted and dropped.
func (s *jsonlRead) row(keys []string, vals []any) []any {
	out := make([]any, len(s.cols))
	extra := false
	for i, k := range keys {
		j, ok := s.index[k]
		if !ok {
			extra = true
			s.extraKeys[k] = true
			continue
		}
		out[j] = vals[i]
	}
	if extra {
		s.extraRows++
	}
	return out
}

func (s *jsonlRead) Next(e *Env) (*Batch, error) {
	if s.done && s.pending == nil {
		return nil, nil
	}
	n := e.Batch
	if n <= 0 {
		n = DefaultBatch
	}
	rows := make([][]any, 0, min(n, 4096))
	if s.pending != nil {
		rows = append(rows, s.pending)
		s.pending = nil
	}
	for !s.done && len(rows) < n {
		keys, vals, err := s.nextObject()
		if err != nil {
			return nil, err
		}
		if keys == nil {
			s.done = true
			break
		}
		rows = append(rows, s.row(keys, vals))
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &Batch{Cols: slices.Clone(s.cols), Rows: rows}, nil
}

func (s *jsonlRead) Close(ok bool) error {
	if s.f == nil {
		return nil
	}
	if ok && s.extraRows > 0 && s.env != nil && s.env.Logf != nil {
		keys := make([]string, 0, len(s.extraKeys))
		for k := range s.extraKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 10 {
			keys = append(keys[:10], "…")
		}
		s.env.Logf("%d rows had keys outside the columns (%s): name them in columns to keep them",
			s.extraRows, strings.Join(keys, ", "))
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// fileWrite is csv.write (csv true) and jsonl.write: a temporary file
// beside the destination, renamed over it at Commit.
type fileWrite struct {
	path   string
	csv    bool
	delim  rune
	header bool
	null   string

	abs       string // the destination, absolute (published as ${frag.<f>.path})
	tmp       *os.File
	bw        *bufio.Writer
	cw        *csv.Writer
	ncols     int
	keys      [][]byte // jsonl: each column's name as a JSON string, encoded once
	rec       []string // csv: a record's fields, reused row to row
	n         int64
	committed bool
}

func (w *fileWrite) Open(e *Env, cols []Col) error {
	p, err := e.Path(w.path)
	if err != nil {
		return err
	}
	// absolute already when files_dir applied (the config's is); Abs
	// covers a host with none, so what is published never depends on a
	// later reader's working directory
	if w.abs, err = filepath.Abs(p); err != nil {
		return serr.Wrap(err, "path", p)
	}
	dir := filepath.Dir(w.abs)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return e.pathErr(serr.F("make the directory %s: %w", dir, err), "mkdir", w.path, w.abs)
	}
	// beside the destination, so the rename at Commit stays within one
	// file system (and so is atomic); a dot file, so a directory listing
	// mid-load does not show it as a result
	if w.tmp, err = os.CreateTemp(dir, "."+filepath.Base(w.abs)+".*.partial"); err != nil {
		return serr.F("create a temporary file in %s: %w", dir, err)
	}
	w.bw = bufio.NewWriterSize(w.tmp, 64<<10)
	w.ncols = len(cols)
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	if w.csv {
		w.cw = csv.NewWriter(w.bw)
		w.cw.Comma = w.delim
		w.rec = make([]string, len(cols))
		if w.header {
			if err = w.cw.Write(names); err != nil {
				return serr.F("%s: %w", w.abs, err)
			}
		}
		return nil
	}
	w.keys = make([][]byte, len(names))
	for i, n := range names {
		w.keys[i], _ = json.Marshal(n) // a string always marshals
	}
	return nil
}

func (w *fileWrite) Write(_ *Env, b *Batch) error {
	if len(b.Cols) != w.ncols {
		return serr.F("%s: the columns changed between batches (opened with %d, now %d)", w.abs, w.ncols, len(b.Cols))
	}
	for _, row := range b.Rows {
		var err error
		if w.csv {
			for i, v := range row {
				w.rec[i] = csvText(v, w.null)
			}
			err = w.cw.Write(w.rec)
		} else {
			err = w.jsonLine(row)
		}
		if err != nil {
			return serr.F("%s, row %d: %w", w.abs, w.n+1, err)
		}
		w.n++
	}
	return nil
}

// jsonLine writes one row as an object. It is built by hand rather than
// through a map so the keys come out in column order, as the source had
// them — encoding/json sorts a map's keys. Each value is encoded before
// anything of the row is written, so a value that cannot be encoded fails
// the node without leaving half a line behind.
func (w *fileWrite) jsonLine(row []any) error {
	vals := make([][]byte, len(row))
	for i, v := range row {
		js, err := jsonText(v)
		if err != nil {
			return serr.F("column %s: %w", w.keys[i], err)
		}
		vals[i] = js
	}
	w.bw.WriteByte('{')
	for i, js := range vals {
		if i > 0 {
			w.bw.WriteByte(',')
		}
		w.bw.Write(w.keys[i])
		w.bw.WriteByte(':')
		w.bw.Write(js)
	}
	_, err := w.bw.WriteString("}\n") // a bufio.Writer keeps its first error: one check covers the row
	return err
}

// jsonText is a value as JSON: NULL null, a time in RFC 3339 with its
// fraction (made explicit so it reads the same as csv.write's), NaN and
// infinities null (JSON has no spelling for them), the rest as
// encoding/json writes it.
func jsonText(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("null"), nil
	case time.Time:
		return json.Marshal(x.Format(time.RFC3339Nano))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return []byte("null"), nil
		}
	case float32:
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return []byte("null"), nil
		}
	}
	return json.Marshal(v)
}

// csvText is a value as a CSV field. Floats are written in full
// ('f' format) across the ordinary range rather than %g's exponent —
// 1000000 reads back as 1000000 in a spreadsheet, not 1e+06 — and in the
// shortest exact form outside it.
func csvText(v any, null string) string {
	switch x := v.(type) {
	case nil:
		return null
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if a := math.Abs(x); x == 0 || (a >= 1e-6 && a < 1e21) {
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case []byte:
		return FormatValue(x)
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer:
		if js, err := json.Marshal(v); err == nil {
			return string(js)
		}
	}
	return fmt.Sprint(v)
}

func (w *fileWrite) Commit(*Env) (Stats, error) {
	if w.cw != nil {
		w.cw.Flush()
		if err := w.cw.Error(); err != nil {
			return Stats{}, serr.F("%s: %w", w.abs, err)
		}
	}
	if err := w.bw.Flush(); err != nil {
		return Stats{}, serr.F("write %s: %w", w.abs, err)
	}
	// fsync before the rename: otherwise a crash just after it can leave
	// the new name pointing at a file whose blocks never reached the disk
	if err := w.tmp.Sync(); err != nil {
		return Stats{}, serr.F("sync %s: %w", w.abs, err)
	}
	if err := w.tmp.Close(); err != nil {
		return Stats{}, serr.F("close %s: %w", w.abs, err)
	}
	// CreateTemp makes 0600; an output file gets the mode dbc's exports do
	if err := os.Chmod(w.tmp.Name(), 0o644); err != nil {
		return Stats{}, serr.F("chmod %s: %w", w.abs, err)
	}
	if err := os.Rename(w.tmp.Name(), w.abs); err != nil {
		return Stats{}, serr.F("rename into place %s: %w", w.abs, err)
	}
	w.committed = true
	n := strconv.FormatInt(w.n, 10)
	return Stats{Rows: w.n, Vars: map[string]string{"rows": n, "path": w.abs},
		Note: "wrote " + n + " rows to " + w.abs}, nil
}

// Abort drops the temporary file; the destination was never touched.
// Idempotent, and a no-op after Commit, as the Sink contract wants.
func (w *fileWrite) Abort() error {
	if w.committed || w.tmp == nil {
		return nil
	}
	_ = w.tmp.Close() // may already be closed by a Commit that failed after it
	err := os.Remove(w.tmp.Name())
	w.tmp = nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
