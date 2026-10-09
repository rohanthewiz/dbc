package pipeline

import (
	"crypto/rand"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rohanthewiz/serr"
)

// The column and row plugins added with the plugin SDK: typing text
// columns, adding columns, cleaning text, dropping duplicates — and the
// sink that writes nothing.
//
// The type names a transform puts on a column (Col.DBType) are the ones a
// sink creating a table maps to the destination's types. They are chosen
// from the set both readers of DBType understand: etl's type families
// (etl/engine.go familyOf: INT8 → integer, FLOAT8 → float, BOOL,
// TIMESTAMPTZ, DATE, TEXT) and, for a Postgres-to-Postgres create, which
// spells DBType as a Postgres type as it is (etl/copy.go pgTypeName), real
// Postgres names. "BIGINT" or "DOUBLE" would read right as a family but
// make CREATE TABLE fail on Postgres ("double" is not a type there).
const (
	typeInt  = "INT8"
	typeFlt  = "FLOAT8"
	typeBool = "BOOL"
	typeText = "TEXT"
	typeTime = "TIMESTAMPTZ"
	typeDate = "DATE"
	typeUUID = "UUID"
)

func init() {
	Register(Plugin{
		Name: "cols.cast", Kind: KindTransform, Label: "Cast columns",
		Doc: "Types columns: one cast per line, column type [layout], with type int, float, bool, text, time or date " +
			"(created_at time 2006-01-02 15:04 — a Go layout; without one, RFC 3339, \"2006-01-02 15:04:05\" and " +
			"\"2006-01-02\" are tried). An empty string becomes NULL for every type but text; NULL stays NULL. " +
			"A value that does not cast fails the fragment, or becomes NULL with on_error null. The cast column's " +
			"type goes with it, so a table created downstream gets an integer column, not text.",
		Fields: []Field{
			{Name: "casts", Type: FieldText, Required: true, Doc: "One per line: column type [layout]."},
			{Name: "on_error", Type: FieldEnum, Enum: []string{"fail", "null"}, Default: "fail",
				Doc: "A value that does not cast: fail the fragment, or make it NULL (counted in the log)."},
		},
		New: func(cfg Config) (any, error) {
			t := &colsCast{toNull: cfg.Str("on_error", "fail") == "null"}
			for _, line := range cfg.Lines("casts") {
				c, err := parseCast(line)
				if err != nil {
					return nil, err
				}
				t.casts = append(t.casts, c)
			}
			return t, nil
		},
		Check: func(cfg Config) []string {
			var out []string
			for _, line := range cfg.Lines("casts") {
				if _, err := parseCast(line); err != nil {
					out = append(out, "casts: "+err.Error())
				}
			}
			return out
		},
	})
	Register(Plugin{
		Name: "cols.add", Kind: KindTransform, Label: "Add columns",
		Doc: "Adds columns, one per line: name = value. The value is now() (when the node started, the same for " +
			"every row, as SQL's now()), uuid() (a random one per row), row() (1, 2, … across the fragment), null, " +
			"a number, true or false, 'quoted text' ('' for a quote), or any other text as written. ${param} and " +
			"${run.id} are substituted first, so batch = '${run.id}' stamps each row with the run. A column of that " +
			"name already there is replaced.",
		Fields: []Field{
			{Name: "columns", Type: FieldText, Required: true, Doc: "One per line: name = value."},
		},
		New: func(cfg Config) (any, error) {
			t := &colsAdd{}
			for _, line := range cfg.Lines("columns") {
				a, err := parseAdd(line)
				if err != nil {
					return nil, err
				}
				t.adds = append(t.adds, a)
			}
			return t, nil
		},
		Check: func(cfg Config) []string {
			var out []string
			for _, line := range cfg.Lines("columns") {
				if _, err := parseAdd(line); err != nil {
					out = append(out, "columns: "+err.Error())
				}
			}
			return out
		},
	})
	Register(Plugin{
		Name: "text.clean", Kind: KindTransform, Label: "Clean text",
		Doc: "Tidies text values: trims them, collapses runs of spaces, changes their case, and can make what is " +
			"left empty NULL. Only text values are touched; numbers, times and NULLs pass as they are.",
		Fields: []Field{
			{Name: "columns", Type: FieldColumns, Doc: "The columns to clean; empty means every column."},
			{Name: "trim", Type: FieldBool, Default: "true", Doc: "Remove leading and trailing white space."},
			{Name: "collapse", Type: FieldBool, Default: "false", Doc: "Make each run of white space one space."},
			{Name: "case", Type: FieldEnum, Enum: []string{"keep", "lower", "upper"}, Default: "keep", Doc: "Change the case."},
			{Name: "empty_null", Type: FieldBool, Default: "false", Doc: "A value left empty becomes NULL."},
		},
		New: func(cfg Config) (any, error) {
			trim, _ := cfg.Bool("trim")
			collapse, _ := cfg.Bool("collapse")
			emptyNull, _ := cfg.Bool("empty_null")
			return &textClean{cols: cfg.List("columns"), trim: trim, collapse: collapse,
				kase: cfg.Str("case", "keep"), emptyNull: emptyNull}, nil
		},
	})
	Register(Plugin{
		Name: "rows.dedupe", Kind: KindTransform, Label: "Drop duplicates",
		Doc: "Keeps the first row of each key and drops the rest, across every batch of the fragment's run. " +
			"The key is the columns named, or the whole row; values compare as text (1 and \"1\" are one key). " +
			"The keys seen are held in memory, so memory grows with the number of distinct keys.",
		Fields: []Field{
			{Name: "key", Type: FieldColumns, Doc: "The key columns; empty means the whole row."},
		},
		New: func(cfg Config) (any, error) {
			return &rowsDedupe{key: cfg.List("key")}, nil
		},
	})
	Register(Plugin{
		Name: "discard", Kind: KindSink, Label: "Discard",
		Doc: "Counts the rows and writes nothing: for measuring how fast a source or a transform runs, or for a " +
			"branch whose rows are not wanted. Its count is the fragment's row count only when no other sink wrote.",
		Fields: []Field{},
		New: func(Config) (any, error) {
			return &discardSink{}, nil
		},
	})
}

// ─── cols.cast ──────────────────────────────────────────────────────────────

// castTypes are the types cols.cast knows, in the order messages list them.
var castTypes = []string{"int", "float", "bool", "text", "time", "date"}

// defaultTimeLayouts are tried in order for a time (or date) cast without
// a layout: what databases and APIs usually write.
var defaultTimeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999", "2006-01-02"}

// cast is one casts line, parsed.
type cast struct {
	col, typ, layout string
}

// parseCast reads "column type [layout]": the column is the first word,
// the type the second, and the rest of the line — spaces and all — the
// layout, which only time and date take.
func parseCast(line string) (cast, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return cast{}, serr.F("a cast is: column type [layout] (got %q)", line)
	}
	c := cast{col: fields[0], typ: strings.ToLower(fields[1])}
	if !slices.Contains(castTypes, c.typ) {
		return cast{}, serr.F("unknown type %q in %q: the types are %s", fields[1], line, strings.Join(castTypes, ", "))
	}
	if len(fields) > 2 {
		if c.typ != "time" && c.typ != "date" {
			return cast{}, serr.F("only time and date take a layout (got %q)", line)
		}
		// the layout as written after the type, inner spaces kept
		_, rest, _ := strings.Cut(strings.TrimSpace(line), fields[1])
		c.layout = strings.TrimSpace(rest)
	}
	return c, nil
}

// dbType is the type the cast column carries on (see the const block).
func (c cast) dbType() string {
	return map[string]string{"int": typeInt, "float": typeFlt, "bool": typeBool, "text": typeText,
		"time": typeTime, "date": typeDate}[c.typ]
}

type colsCast struct {
	TransformBase
	casts  []cast
	toNull bool

	env    *Env  // kept from Open for the closing log line
	seen   int64 // rows before this batch, for a failing row's place
	nulled int64 // values on_error null turned into NULL
}

func (t *colsCast) Open(e *Env) error {
	t.env = e
	return nil
}

func (t *colsCast) Apply(_ *Env, b *Batch) (*Batch, error) {
	for _, c := range t.casts {
		j := b.Col(c.col)
		if j < 0 {
			return nil, noColumn(c.col, b)
		}
		for i, row := range b.Rows {
			v, err := c.apply(row[j])
			if err != nil {
				if !t.toNull {
					// the run's record keeps only the message, so it says where
					return nil, serr.F("column %s, row %d: %q is %w", c.col, t.seen+int64(i)+1, FormatValue(row[j]), err)
				}
				v = nil
				t.nulled++
			}
			row[j] = v
		}
		b.Cols[j].DBType = c.dbType()
	}
	t.seen += int64(b.Len())
	return b, nil
}

func (t *colsCast) Close() error {
	if t.nulled > 0 && t.env != nil && t.env.Logf != nil {
		t.env.Logf("%d values became NULL (they did not cast)", t.nulled)
	}
	return nil
}

// apply casts one value. NULL stays NULL; a blank string is NULL for
// every type but text (a CSV's empty field, after empty_null off, is
// "no value", not a parse error).
func (c cast) apply(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && c.typ != "text" && strings.TrimSpace(s) == "" {
		return nil, nil
	}
	switch c.typ {
	case "int":
		return castInt(v)
	case "float":
		return castFloat(v)
	case "bool":
		return castBool(v)
	case "text":
		if s, ok := v.(string); ok {
			return s, nil
		}
		return FormatValue(v), nil
	case "time", "date":
		return c.castTime(v)
	}
	return nil, serr.F("unknown type %q", c.typ)
}

// number reads any Go integer or float kind (a transform may put int or
// float32 in a batch; etl gives int64 and float64) as either an int64 or
// a float64, saying which.
func number(v any) (i int64, f float64, isInt, ok bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), 0, true, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return 0, float64(u), false, true
		}
		return int64(u), 0, true, true
	case reflect.Float32, reflect.Float64:
		return 0, rv.Float(), false, true
	}
	return 0, 0, false, false
}

func castInt(v any) (any, error) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return nil, serr.New("not an integer")
		}
		return n, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	}
	i, f, isInt, ok := number(v)
	switch {
	case !ok:
		return nil, serr.F("a %s, which cannot become an integer", reflect.TypeOf(v))
	case isInt:
		return i, nil
	case f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64:
		return int64(f), nil
	}
	return nil, serr.New("not a whole number")
}

func castFloat(v any) (any, error) {
	switch x := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return nil, serr.New("not a number")
		}
		return f, nil
	case bool:
		if x {
			return 1.0, nil
		}
		return 0.0, nil
	}
	i, f, isInt, ok := number(v)
	switch {
	case !ok:
		return nil, serr.F("a %s, which cannot become a number", reflect.TypeOf(v))
	case isInt:
		return float64(i), nil
	}
	return f, nil
}

func castBool(v any) (any, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "t", "yes", "y", "on", "1":
			return true, nil
		case "false", "f", "no", "n", "off", "0":
			return false, nil
		}
		return nil, serr.New("not a boolean (true/false, yes/no, on/off, 1/0)")
	}
	i, f, isInt, ok := number(v)
	switch {
	case ok && isInt && (i == 0 || i == 1):
		return i == 1, nil
	case ok && !isInt && (f == 0 || f == 1):
		return f == 1, nil
	}
	return nil, serr.New("not a boolean (only 0 and 1 are)")
}

// castTime parses a time by the cast's layout, or by the defaults. A date
// is the time's calendar day at midnight UTC, whatever zone it was read
// in. Beyond the spec's one default ("2006-01-02") a date without a
// layout also tries the time layouts, so a timestamp column cast to date
// keeps its days rather than failing.
func (c cast) castTime(v any) (any, error) {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case string:
		s := strings.TrimSpace(x)
		layouts := defaultTimeLayouts
		switch {
		case c.layout != "":
			layouts = []string{c.layout}
		case c.typ == "date":
			layouts = append([]string{"2006-01-02"}, defaultTimeLayouts...)
		}
		var err error
		for _, l := range layouts {
			if t, err = time.Parse(l, s); err == nil {
				break
			}
		}
		if err != nil {
			want := c.layout
			if want == "" {
				want = "RFC 3339, 2006-01-02 15:04:05 or 2006-01-02"
			}
			return nil, serr.F("not a time in %s", want)
		}
	default:
		return nil, serr.F("a %s, which cannot become a time", reflect.TypeOf(v))
	}
	if c.typ == "date" {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
	}
	return t, nil
}

// ─── cols.add ───────────────────────────────────────────────────────────────

// addSpec is one columns line of cols.add, parsed: a constant (val), or a
// function evaluated per row or per node (fn).
type addSpec struct {
	name   string
	fn     string // "now", "uuid", "row", or "" for the constant val
	val    any
	dbType string
}

var (
	// a decimal literal, without the forms ParseFloat also takes and no
	// one means as a number here (inf, nan, hex)
	decimalRe = regexp.MustCompile(`^[+-]?(\d+\.\d*|\.\d+|\d+)([eE][+-]?\d+)?$`)
	// a call: name()
	callRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\(\)$`)
)

// parseAdd reads "name = value" (see the plugin's Doc for the values).
func parseAdd(line string) (addSpec, error) {
	name, val, ok := strings.Cut(line, "=")
	name, val = strings.TrimSpace(name), strings.TrimSpace(val)
	if !ok {
		return addSpec{}, serr.F("a column is: name = value (got %q)", line)
	}
	if name == "" {
		return addSpec{}, serr.F("a column needs a name before = (got %q)", line)
	}
	a := addSpec{name: name}
	if m := callRe.FindStringSubmatch(val); m != nil {
		switch fn := strings.ToLower(m[1]); fn {
		case "now":
			a.fn, a.dbType = fn, typeTime
		case "uuid":
			a.fn, a.dbType = fn, typeUUID
		case "row":
			a.fn, a.dbType = fn, typeInt
		default:
			return addSpec{}, serr.F("unknown function %s() in %q: now(), uuid() and row() are known", m[1], line)
		}
		return a, nil
	}
	lower := strings.ToLower(val)
	switch {
	case lower == "null":
		a.val = nil // and DBType "": a created table makes it text
	case lower == "true" || lower == "false":
		a.val, a.dbType = lower == "true", typeBool
	case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
		a.val, a.dbType = strings.ReplaceAll(val[1:len(val)-1], "''", "'"), typeText
	case strings.HasPrefix(val, "'"):
		return addSpec{}, serr.F("a quoted value needs its closing ' (got %q)", line)
	default:
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			a.val, a.dbType = n, typeInt
		} else if f, err := strconv.ParseFloat(val, 64); err == nil && decimalRe.MatchString(val) {
			a.val, a.dbType = f, typeFlt
		} else {
			a.val, a.dbType = val, typeText
		}
	}
	return a, nil
}

type colsAdd struct {
	TransformBase
	adds []addSpec
	now  time.Time // when the node opened: now()'s one value
	rows int64     // rows before this batch: row()'s base
}

func (t *colsAdd) Open(*Env) error {
	t.now = time.Now()
	return nil
}

func (t *colsAdd) Apply(_ *Env, b *Batch) (*Batch, error) {
	if t.now.IsZero() {
		t.now = time.Now() // Apply without Open (a test, a builder func)
	}
	for _, a := range t.adds {
		var fill func(int) any
		switch a.fn {
		case "now":
			fill = func(int) any { return t.now }
		case "uuid":
			fill = func(int) any { return newUUID() }
		case "row":
			base := t.rows
			fill = func(i int) any { return base + int64(i) + 1 }
		default:
			v := a.val
			fill = func(int) any { return v }
		}
		b.AddCol(a.name, a.dbType, fill)
	}
	t.rows += int64(b.Len())
	return b, nil
}

// newUUID is a random (version 4) UUID in its usual text form. crypto/rand
// rather than a library: sixteen random bytes and two fixed nibbles are
// all a v4 UUID is.
func newUUID() string {
	var u [16]byte
	_, _ = rand.Read(u[:])  // never fails on the platforms Go supports (Go 1.24+ panics instead)
	u[6] = u[6]&0x0f | 0x40 // version 4
	u[8] = u[8]&0x3f | 0x80 // the RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// ─── text.clean ─────────────────────────────────────────────────────────────

type textClean struct {
	TransformBase
	cols                      []string
	trim, collapse, emptyNull bool
	kase                      string
}

func (t *textClean) Apply(_ *Env, b *Batch) (*Batch, error) {
	idx, err := colIndexes(b, t.cols)
	if err != nil {
		return nil, err
	}
	for _, row := range b.Rows {
		for _, j := range idx {
			s, ok := row[j].(string)
			if !ok {
				continue
			}
			if t.collapse {
				s = collapseSpace(s)
			}
			if t.trim {
				s = strings.TrimSpace(s)
			}
			switch t.kase {
			case "lower":
				s = strings.ToLower(s)
			case "upper":
				s = strings.ToUpper(s)
			}
			if s == "" && t.emptyNull {
				row[j] = nil
			} else {
				row[j] = s
			}
		}
	}
	return b, nil
}

// collapseSpace makes each run of white space one space (a run at either
// end too; trim then removes it).
func collapseSpace(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !inSpace {
				sb.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		sb.WriteRune(r)
	}
	return sb.String()
}

// noColumn is the error for a column a batch does not have, naming the
// ones it does — in the message itself, since a run's record keeps only
// the message.
func noColumn(name string, b *Batch) error {
	return serr.F("no column %s (the columns: %s)", name, strings.Join(b.Names(), ", "))
}

// colIndexes resolves column names in b: every column for none, else
// each named one, an unknown name an error listing the batch's columns.
func colIndexes(b *Batch, names []string) ([]int, error) {
	if len(names) == 0 {
		idx := make([]int, len(b.Cols))
		for j := range idx {
			idx[j] = j
		}
		return idx, nil
	}
	idx := make([]int, len(names))
	for i, n := range names {
		if idx[i] = b.Col(n); idx[i] < 0 {
			return nil, noColumn(n, b)
		}
	}
	return idx, nil
}

// ─── rows.dedupe ────────────────────────────────────────────────────────────

// keyOf is the text key of a row's values at idx, shared by rows.dedupe
// and lookup. Values compare as text (FormatValue), so an int64 1 from a
// database and "1" from a CSV are one key — the two sides of a lookup
// rarely agree on types. Each value is written length-prefixed ("5:Tabby")
// rather than joined by a separator, so no value can run into the next,
// and NULL is "-", which no prefixed value can be: NULL and the string
// "NULL" stay two keys.
func keyOf(row []any, idx []int) string {
	var sb strings.Builder
	for _, j := range idx {
		if row[j] == nil {
			sb.WriteByte('-')
			continue
		}
		s := FormatValue(row[j])
		sb.WriteString(strconv.Itoa(len(s)))
		sb.WriteByte(':')
		sb.WriteString(s)
	}
	return sb.String()
}

type rowsDedupe struct {
	TransformBase
	key []string

	env     *Env
	seen    map[string]struct{}
	dropped int64
}

func (t *rowsDedupe) Open(e *Env) error {
	t.env = e
	return nil
}

func (t *rowsDedupe) Apply(_ *Env, b *Batch) (*Batch, error) {
	idx, err := colIndexes(b, t.key)
	if err != nil {
		return nil, err
	}
	if t.seen == nil {
		t.seen = map[string]struct{}{}
	}
	b.Filter(func(i int) bool {
		k := keyOf(b.Rows[i], idx)
		if _, dup := t.seen[k]; dup {
			t.dropped++
			return false
		}
		t.seen[k] = struct{}{}
		return true
	})
	return b, nil
}

func (t *rowsDedupe) Close() error {
	if t.dropped > 0 && t.env != nil && t.env.Logf != nil {
		t.env.Logf("dropped %d duplicate rows", t.dropped)
	}
	t.seen = nil // the keys can be many: let them go with the run
	return nil
}

// ─── discard ────────────────────────────────────────────────────────────────

type discardSink struct {
	SinkBase
	n int64
}

func (s *discardSink) Open(*Env, []Col) error { return nil }

func (s *discardSink) Write(_ *Env, b *Batch) error {
	s.n += int64(b.Len())
	return nil
}

// Commit reports the count as Shown, the flag a preview uses: the runner
// adds a Shown sink's rows to the fragment's only when no sink wrote any.
// So a discard beside a sql.write does not double the fragment's count,
// and a discard alone — measuring a source — still says how many rows
// went by.
func (s *discardSink) Commit(*Env) (Stats, error) {
	return Stats{Rows: s.n, Shown: true, Note: fmt.Sprintf("discarded %d rows", s.n)}, nil
}
