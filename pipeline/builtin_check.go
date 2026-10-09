package pipeline

import (
	"fmt"
	"strings"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/serr"
)

// The plugins that ask a second query: lookup, a join against a side
// table read once per fragment, and pipeline.check, a gate between
// fragments.

func init() {
	Register(Plugin{
		Name: "lookup", Kind: KindTransform, Label: "Look up",
		Doc: "Joins each row to a side query on another (or the same) connection: the row's key columns are matched " +
			"against the query's match columns, and the query's add columns are appended. The side query runs once, " +
			"when the fragment starts, and is held in memory (max_rows guards against a side table too big for that). " +
			"Keys compare as text, so an integer 1 from a database matches \"1\" from a CSV. A key the side query has " +
			"twice keeps its first row. A row with no match gets NULLs, is dropped, or fails the fragment (missing).",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection the side query runs on."},
			{Name: "query", Type: FieldSQL, Required: true, Doc: "The side query: SELECT id, name, region FROM customers."},
			{Name: "args", Type: FieldText, Doc: "Bind values for the query's placeholders, one per line."},
			{Name: "key", Type: FieldColumns, Required: true, Doc: "The row's key columns."},
			{Name: "match", Type: FieldColumns, Doc: "The query's columns matched against key, in the same order; default the same names."},
			{Name: "add", Type: FieldColumns, Doc: "The query's columns to append; default every one not matched."},
			{Name: "prefix", Type: FieldString, Doc: "Put before each added column's name (for a name the rows already have)."},
			{Name: "missing", Type: FieldEnum, Enum: []string{"null", "drop", "fail"}, Default: "null",
				Doc: "A row whose key the query lacks: NULLs in the added columns, dropped, or a failed fragment."},
			{Name: "max_rows", Type: FieldInt, Default: "1000000", Doc: "The most side rows to hold in memory; more is an error."},
		},
		New: func(cfg Config) (any, error) {
			maxRows, _ := cfg.Int("max_rows", 1000000)
			t := &lookup{conn: cfg.Str("conn", ""), query: strings.TrimSpace(cfg["query"]),
				args: anyStrings(cfg.Lines("args")), key: cfg.List("key"), match: cfg.List("match"),
				add: cfg.List("add"), prefix: strings.TrimSpace(cfg["prefix"]), missing: cfg.Str("missing", "null"),
				maxRows: maxRows}
			if len(t.match) == 0 {
				t.match = t.key
			}
			if len(t.match) != len(t.key) {
				return nil, serr.F("match names %d columns for %d key columns (key %s, match %s)",
					len(t.match), len(t.key), strings.Join(t.key, ", "), strings.Join(t.match, ", "))
			}
			return t, nil
		},
		Check: func(cfg Config) []string {
			key, match := cfg.List("key"), cfg.List("match")
			if len(match) > 0 && len(match) != len(key) {
				return []string{fmt.Sprintf("match: %d columns for %d key columns", len(match), len(key))}
			}
			return nil
		},
	})
	Register(Plugin{
		Name: "pipeline.check", Kind: KindAction, Label: "Check",
		Doc: "A gate between fragments: runs a query and checks its first row's first value against a rule — " +
			"an operator and a value as rows.filter has them: >= 1000, = 0, != 0, notnull, in a,b, like %x%. " +
			"A failed rule (or no row at all) fails the fragment, and so stops the pipeline before the fragments " +
			"after it. Publishes the value as ${frag.<fragment>.value}. Row count: SELECT count(*) FROM t with >= N; " +
			"no NULLs: SELECT count(*) FROM t WHERE c IS NULL with = 0.",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection the query runs on."},
			{Name: "query", Type: FieldSQL, Required: true, Doc: "A query whose first row's first column is the value checked."},
			{Name: "args", Type: FieldText, Doc: "Bind values for the query's placeholders, one per line."},
			{Name: "rule", Type: FieldString, Required: true, Doc: "An operator and a value: >= 1000, = 0, notnull, in a,b, like %x%."},
			{Name: "message", Type: FieldString, Doc: "What a failure says; default names the query, the value and the rule."},
		},
		New: func(cfg Config) (any, error) {
			r, err := parseCheckRule(cfg["rule"])
			if err != nil {
				return nil, err
			}
			return &checkAction{conn: cfg.Str("conn", ""), query: strings.TrimSpace(cfg["query"]),
				args: anyStrings(cfg.Lines("args")), rule: r, ruleText: strings.TrimSpace(cfg["rule"]),
				message: strings.TrimSpace(cfg["message"])}, nil
		},
		Check: func(cfg Config) []string {
			if len(Refs(cfg["rule"])) > 0 {
				return nil // its value comes from a param; the run checks it
			}
			if _, err := parseCheckRule(cfg["rule"]); err != nil {
				return []string{"rule: " + err.Error()}
			}
			return nil
		},
	})
}

// ─── lookup ─────────────────────────────────────────────────────────────────

type lookup struct {
	conn, query string
	args        []any
	key, match  []string
	add         []string
	prefix      string
	missing     string
	maxRows     int

	env     *Env
	table   map[string][]any // key → the added columns' values
	addCols []Col            // the appended columns, prefixed, with the side query's types
	unmatch int64            // rows that found no match (missing null or drop)
}

// Open reads the side query whole. It runs before the first batch — not
// lazily on it — so a broken query fails the fragment at once, before the
// source has done any work.
func (t *lookup) Open(e *Env) error {
	t.env = e
	c, err := e.S.ETLConn(t.conn)
	if err != nil {
		return err
	}
	rd, err := etl.Read(e.Ctx, c, t.query, t.args...)
	if err != nil {
		return serr.F("the side query: %w", err)
	}
	defer rd.Abort() // no-op after the read ran out (it closes itself); an early return rolls back
	side := ColsOf(rd.Columns(), rd.DBTypes())
	sb := &Batch{Cols: side} // for Col's name lookup, exact then case-insensitive
	matchIdx := make([]int, len(t.match))
	for i, m := range t.match {
		if matchIdx[i] = sb.Col(m); matchIdx[i] < 0 {
			return serr.F("the side query has no column %s to match (it has %s)", m, strings.Join(sb.Names(), ", "))
		}
	}
	add := t.add
	if len(add) == 0 {
		for j, col := range side {
			if !containsInt(matchIdx, j) {
				add = append(add, col.Name)
			}
		}
	}
	addIdx := make([]int, len(add))
	t.addCols = make([]Col, len(add))
	for i, a := range add {
		if addIdx[i] = sb.Col(a); addIdx[i] < 0 {
			return serr.F("the side query has no column %s to add (it has %s)", a, strings.Join(sb.Names(), ", "))
		}
		t.addCols[i] = Col{Name: t.prefix + side[addIdx[i]].Name, DBType: side[addIdx[i]].DBType}
	}
	t.table = map[string][]any{}
	var n, dups int64
	for rd.Next() {
		if n++; t.maxRows > 0 && n > int64(t.maxRows) {
			return serr.F("the side query returned more than max_rows (%d) rows: narrow it, or raise max_rows if the memory is there",
				t.maxRows)
		}
		row := rd.Row()
		k := keyOf(row, matchIdx)
		if _, dup := t.table[k]; dup {
			dups++
			continue
		}
		vals := make([]any, len(addIdx))
		for i, j := range addIdx {
			vals[i] = row[j]
		}
		t.table[k] = vals
	}
	if err = rd.Err(); err != nil {
		return serr.F("the side query: %w", err)
	}
	e.Logf("side query: %d rows, %d keys", n, len(t.table))
	if dups > 0 {
		e.Logf("%d side rows repeated a key; the first of each was kept", dups)
	}
	return nil
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (t *lookup) Apply(_ *Env, b *Batch) (*Batch, error) {
	idx := make([]int, len(t.key))
	for i, k := range t.key {
		if idx[i] = b.Col(k); idx[i] < 0 {
			return nil, noColumn(k, b)
		}
	}
	for _, c := range t.addCols {
		if b.Col(c.Name) >= 0 {
			return nil, serr.F("the rows already have a column %s, which the lookup adds: set prefix, or name other add columns",
				c.Name)
		}
	}
	// match first, then append: AddCol fills a column at a time, and the
	// row's match is found once rather than once per added column
	found := make([][]any, b.Len())
	for i, row := range b.Rows {
		vals, ok := t.table[keyOf(row, idx)]
		if !ok {
			if t.missing == "fail" {
				var key []string
				for _, j := range idx {
					key = append(key, FormatValue(row[j]))
				}
				return nil, serr.F("no match in the side query for %s (missing: fail)", strings.Join(key, ", "))
			}
			t.unmatch++
			continue
		}
		found[i] = vals
	}
	for k, c := range t.addCols {
		b.AddCol(c.Name, c.DBType, func(i int) any {
			if found[i] == nil {
				return nil
			}
			return found[i][k]
		})
	}
	if t.missing == "drop" {
		b.Filter(func(i int) bool { return found[i] != nil }) // i is the row's place before the filter
	}
	return b, nil
}

func (t *lookup) Flush(*Env) (*Batch, error) { return nil, nil }

func (t *lookup) Close() error {
	if t.unmatch > 0 && t.env != nil && t.env.Logf != nil {
		what := "got NULLs"
		if t.missing == "drop" {
			what = "were dropped"
		}
		t.env.Logf("%d rows had no match and %s", t.unmatch, what)
	}
	t.table = nil
	return nil
}

// ─── pipeline.check ─────────────────────────────────────────────────────────

// parseCheckRule reads a check's rule with rows.filter's parser, the
// value standing where a column name would.
func parseCheckRule(text string) (rule, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return rule{}, serr.New("a rule is an operator and a value: >= 1000, = 0, notnull")
	}
	r, err := parseRule("value " + text)
	if err != nil {
		return rule{}, serr.F("%q: %w", text, err) // parseRule's own message does not quote the rule
	}
	return r, nil
}

type checkAction struct {
	conn, query string
	args        []any
	rule        rule
	ruleText    string
	message     string
}

func (a *checkAction) Run(e *Env) (Stats, error) {
	r, err := e.S.Query(a.conn, a.query, a.args...)
	if err != nil {
		return Stats{}, err
	}
	if r == nil || len(r.Rows) == 0 || len(r.Columns) == 0 {
		return Stats{}, serr.New(a.failure("the query returned no rows"), "query", clipQuery(a.query))
	}
	// the typed value when the host gives one (db.Manager and etl do), so
	// a number compares as a number; the display text otherwise
	var v any = r.Rows[0][0]
	if len(r.Raw) > 0 && len(r.Raw[0]) > 0 {
		v = r.Raw[0][0]
	}
	shown := FormatValue(v)
	if !a.rule.holds(v) {
		return Stats{}, serr.New(a.failure(fmt.Sprintf("%s is not %s", shown, a.ruleText)),
			"value", shown, "rule", a.ruleText, "query", clipQuery(a.query))
	}
	e.Logf("check passed: %s %s", shown, a.ruleText)
	return Stats{Vars: map[string]string{"value": shown}, Note: "check passed: " + shown + " " + a.ruleText}, nil
}

// failure is the message a failed check gives: the spec's own, or what
// went wrong.
func (a *checkAction) failure(what string) string {
	if a.message != "" {
		return a.message + " (" + what + ")"
	}
	return "check failed: " + what
}

// clipQuery keeps a query short enough for a message.
func clipQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 120 {
		return q[:117] + "..."
	}
	return q
}
