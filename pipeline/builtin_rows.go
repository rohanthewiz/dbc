package pipeline

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// The row and column plugins, and the preview sink.

func init() {
	Register(Plugin{
		Name: "cols.select", Kind: KindTransform, Label: "Select columns",
		Doc: "Keeps, drops, renames and reorders columns. keep names the columns to keep, in the order wanted; " +
			"drop names those to remove; rename is one old=new per line. Keep runs first, then drop, then rename.",
		Fields: []Field{
			{Name: "keep", Type: FieldColumns, Doc: "Columns to keep, in this order; empty keeps all."},
			{Name: "drop", Type: FieldColumns, Doc: "Columns to remove."},
			{Name: "rename", Type: FieldText, Doc: "Renames, one old=new per line."},
		},
		New: func(cfg Config) (any, error) {
			t := &colsSelect{keep: cfg.List("keep"), drop: cfg.List("drop")}
			for _, line := range cfg.Lines("rename") {
				from, to, ok := strings.Cut(line, "=")
				from, to = strings.TrimSpace(from), strings.TrimSpace(to)
				if !ok || from == "" || to == "" {
					return nil, serr.New("a rename is old=new", "line", line)
				}
				t.renames = append(t.renames, [2]string{from, to})
			}
			return t, nil
		},
	})
	Register(Plugin{
		Name: "rows.filter", Kind: KindTransform, Label: "Filter rows",
		Doc: "Keeps the rows every rule holds for. A rule is a line: column, an operator, a value — " +
			"age >= 3, breed = Tabby, name like %kit%, email in a@x.org,b@y.org, adopted null, adopted notnull. " +
			"Operators: = != < <= > >= in like null notnull. Two numbers compare as numbers, anything else as text. " +
			"For anything richer, go.transform.",
		Fields: []Field{
			{Name: "rules", Type: FieldText, Required: true, Doc: "One rule per line; all must hold."},
		},
		New: func(cfg Config) (any, error) {
			t := &rowsFilter{}
			for _, line := range cfg.Lines("rules") {
				r, err := parseRule(line)
				if err != nil {
					return nil, err
				}
				t.rules = append(t.rules, r)
			}
			return t, nil
		},
		Check: func(cfg Config) []string {
			var out []string
			for _, line := range cfg.Lines("rules") {
				if _, err := parseRule(line); err != nil {
					out = append(out, "rules: "+err.Error())
				}
			}
			return out
		},
	})
	Register(Plugin{
		Name: "rows.limit", Kind: KindTransform, Label: "Limit rows",
		Doc: "Passes the first N rows and then stops the source: the fragment ends early and its sinks commit " +
			"what they have. A preview is this, after the source.",
		Fields: []Field{
			{Name: "rows", Type: FieldInt, Required: true, Doc: "How many rows to let through."},
		},
		New: func(cfg Config) (any, error) {
			n, _ := cfg.Int("rows", 0)
			if n <= 0 {
				return nil, serr.New("rows must be positive")
			}
			return &rowsLimit{n: n}, nil
		},
	})
	Register(Plugin{
		Name: "preview", Kind: KindSink, Label: "Preview",
		Doc: "Keeps the first N rows and shows them in the results view when the fragment ends; nothing is written. " +
			"The way to look at what a transform produces — a preview run puts one after every branch.",
		Fields: []Field{
			{Name: "rows", Type: FieldInt, Default: "50", Doc: "How many rows to show."},
			{Name: "title", Type: FieldString, Doc: "What the result is called; default the node's place."},
		},
		New: func(cfg Config) (any, error) {
			n, _ := cfg.Int("rows", 50)
			return &previewSink{n: n, title: cfg.Str("title", "")}, nil
		},
	})
}

type colsSelect struct {
	TransformBase
	keep, drop []string
	renames    [][2]string
}

func (t *colsSelect) Apply(_ *Env, b *Batch) (*Batch, error) {
	if len(t.keep) > 0 {
		b.Keep(t.keep...)
	}
	if len(t.drop) > 0 {
		b.Drop(t.drop...)
	}
	for _, r := range t.renames {
		b.Rename(r[0], r[1])
	}
	return b, nil
}

// rule is one rows.filter line, parsed.
type rule struct {
	col, op, val string
	list         []string
	re           *regexp.Regexp // for like
}

var ruleOps = []string{"notnull", "null", "like", "in", ">=", "<=", "!=", "=", ">", "<"}

// parseRule reads "col op value". The column is the first word; the
// operator the next; the rest, trimmed, is the value (so a value may hold
// spaces). null and notnull take no value.
func parseRule(line string) (rule, error) {
	col, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return rule{}, serr.New("a rule is: column operator value", "rule", line)
	}
	rest = strings.TrimSpace(rest)
	op, val, _ := strings.Cut(rest, " ")
	op = strings.ToLower(strings.TrimSpace(op))
	val = strings.TrimSpace(val)
	if !slices.Contains(ruleOps, op) {
		return rule{}, serr.New("unknown operator", "rule", line, "operator", op, "want", strings.Join(ruleOps, " "))
	}
	r := rule{col: col, op: op, val: val}
	switch op {
	case "null", "notnull":
		if val != "" {
			return rule{}, serr.New("takes no value", "rule", line, "operator", op)
		}
	case "in":
		for _, v := range strings.Split(val, ",") {
			r.list = append(r.list, strings.TrimSpace(v))
		}
	case "like":
		// SQL's % and _ as a regexp, case-insensitive, anchored
		pat := regexp.QuoteMeta(val)
		pat = strings.ReplaceAll(pat, "%", ".*")
		pat = strings.ReplaceAll(pat, "_", ".")
		r.re = regexp.MustCompile("(?is)^" + pat + "$")
	default:
		if val == "" {
			return rule{}, serr.New("needs a value", "rule", line, "operator", op)
		}
	}
	return r, nil
}

// holds reports whether v satisfies the rule.
func (r rule) holds(v any) bool {
	switch r.op {
	case "null":
		return v == nil
	case "notnull":
		return v != nil
	}
	if v == nil {
		return false // NULL compares with nothing, as in SQL
	}
	s := FormatValue(v)
	switch r.op {
	case "in":
		return slices.Contains(r.list, s)
	case "like":
		return r.re.MatchString(s)
	}
	cmp := strings.Compare(s, r.val)
	if a, errA := strconv.ParseFloat(s, 64); errA == nil {
		if b, errB := strconv.ParseFloat(r.val, 64); errB == nil {
			switch {
			case a < b:
				cmp = -1
			case a > b:
				cmp = 1
			default:
				cmp = 0
			}
		}
	}
	switch r.op {
	case "=":
		return cmp == 0
	case "!=":
		return cmp != 0
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return false
}

type rowsFilter struct {
	TransformBase
	rules []rule
}

func (t *rowsFilter) Apply(_ *Env, b *Batch) (*Batch, error) {
	idx := make([]int, len(t.rules))
	for i, r := range t.rules {
		if idx[i] = b.Col(r.col); idx[i] < 0 {
			return nil, serr.New("no such column", "column", r.col, "columns", strings.Join(b.Names(), ", "))
		}
	}
	b.Filter(func(row int) bool {
		for i, r := range t.rules {
			if !r.holds(b.Rows[row][idx[i]]) {
				return false
			}
		}
		return true
	})
	return b, nil
}

type rowsLimit struct {
	TransformBase
	n, seen int
}

func (t *rowsLimit) Apply(e *Env, b *Batch) (*Batch, error) {
	if t.seen >= t.n {
		e.Stop()
		return nil, nil
	}
	if room := t.n - t.seen; b.Len() > room {
		b.Rows = b.Rows[:room]
	}
	t.seen += b.Len()
	if t.seen >= t.n {
		e.Stop()
	}
	return b, nil
}

type previewSink struct {
	SinkBase
	n     int
	title string
	cols  []Col
	rows  [][]any
	seen  int64
}

func (s *previewSink) Open(_ *Env, cols []Col) error {
	s.cols = slices.Clone(cols)
	return nil
}

func (s *previewSink) Write(_ *Env, b *Batch) error {
	s.seen += int64(b.Len())
	if room := s.n - len(s.rows); room > 0 {
		s.rows = append(s.rows, b.Rows[:min(room, b.Len())]...)
	}
	return nil
}

func (s *previewSink) Commit(e *Env) (Stats, error) {
	title := s.title
	if title == "" {
		title = e.Where()
	}
	// the fragment stands where a result's connection would: the banner
	// and the result tab name it
	r := (&Batch{Cols: s.cols, Rows: s.rows}).Result(e.Fragment, "preview "+title)
	if s.seen > int64(len(s.rows)) {
		r.Truncated = true
	}
	e.S.Show(r)
	return Stats{Rows: s.seen, Shown: true, Note: fmt.Sprintf("showed %d of %d rows", len(s.rows), s.seen)}, nil
}
