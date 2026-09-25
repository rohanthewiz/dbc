package explain

import (
	"encoding/json"
	"strings"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Strip removes a leading EXPLAIN clause, so "explain this" works on a
// statement the user already wrote as an EXPLAIN — dbc then sends its own,
// in the format it can read. analyze reports whether the clause asked for
// ANALYZE, which the caller honors. ok is false when stmt is not an EXPLAIN.
//
// Every engine's spelling is recognized:
//
//	EXPLAIN [ANALYZE|ANALYSE] [VERBOSE] stmt           Postgres, MySQL
//	EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) stmt       Postgres
//	EXPLAIN FORMAT=TREE stmt  /  EXPLAIN FORMAT = JSON  MySQL
//	EXPLAIN QUERY PLAN stmt                            SQLite
//	EXPLAIN EXTENDED | PARTITIONS stmt                 older MySQL
//
// It walks words, not a grammar: after EXPLAIN it skips option words and one
// parenthesized option list, and the statement starts at the first word that
// is none of those. Comments before EXPLAIN are skipped the way the splitter
// skips them.
func Strip(stmt string) (inner string, analyze, ok bool) {
	s := skipLeadingComments(stmt)
	if !strings.EqualFold(firstWord(s), "explain") {
		return stmt, false, false
	}
	s = strings.TrimSpace(s[len("explain"):])
	for {
		w := strings.ToLower(firstWord(s))
		switch {
		case strings.HasPrefix(s, "("):
			end := strings.IndexByte(s, ')')
			if end < 0 {
				return stmt, false, false
			}
			for _, opt := range strings.Split(s[1:end], ",") {
				f := strings.Fields(strings.ToLower(opt))
				if len(f) > 0 && (f[0] == "analyze" || f[0] == "analyse") {
					analyze = len(f) == 1 || (f[1] != "false" && f[1] != "off" && f[1] != "0")
				}
			}
			s = strings.TrimSpace(s[end+1:])
		case w == "analyze" || w == "analyse":
			analyze = true
			s = strings.TrimSpace(s[len(w):])
		case w == "verbose" || w == "extended" || w == "partitions":
			s = strings.TrimSpace(s[len(w):])
		case w == "query" && strings.EqualFold(firstWord(strings.TrimSpace(s[len(w):])), "plan"):
			s = strings.TrimSpace(strings.TrimSpace(s[len(w):])[len("plan"):])
		case strings.HasPrefix(w, "format"):
			// FORMAT=TREE, FORMAT = JSON, FORMAT JSON
			s = strings.TrimSpace(s[len("format"):])
			s = strings.TrimSpace(strings.TrimPrefix(s, "="))
			s = strings.TrimSpace(s[len(firstWord(s)):])
		default:
			if s == "" {
				return stmt, false, false
			}
			return s, analyze, true
		}
	}
}

// firstWord is the leading run of letters, digits and underscores.
func firstWord(s string) string {
	i := 0
	for i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z' || s[i] >= '0' && s[i] <= '9') {
		i++
	}
	return s[:i]
}

// skipLeadingComments drops the whitespace and comments before the first
// word, using the lexer the editor highlights with so the two agree.
func skipLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		toks := sqlsplit.Lex(s)
		if len(toks) == 0 || toks[0].Start != 0 || toks[0].Kind != sqlsplit.TokComment {
			return s
		}
		s = s[toks[0].End:]
	}
}

// Aliases maps each alias a statement gives a table to the table's name:
// "FROM orders o JOIN users AS u" → {o: orders, u: users}. MySQL and SQLite
// name steps by alias ("Table scan on o"), and an index suggestion needs the
// table — CREATE INDEX … ON o would fail. A schema-qualified table is also
// reachable by its bare name ("users" → "public.users") unless two schemas'
// tables share it, in which case the bare name maps to neither.
//
// It is a word walk, not a parse. A table reference starts after FROM, JOIN,
// UPDATE or INTO, or after a comma while a FROM list is open at the same
// parenthesis depth; the list closes at the clause words that end it (WHERE,
// GROUP, ORDER, …). A subquery's derived-table alias maps to nothing and is
// left alone.
func Aliases(stmt string) map[string]string {
	out := map[string]string{}
	ambiguous := map[string]bool{}
	words := sqlWords(stmt)
	lower := func(i int) string { return strings.ToLower(words[i]) }
	isKw := func(w string) bool {
		switch strings.ToLower(w) {
		case "where", "join", "inner", "left", "right", "full", "cross", "on", "using", "group", "order",
			"limit", "set", "values", "select", "natural", "straight_join", "union", "having", "window",
			"as", "lateral", "outer", "for", "returning", "offset", "fetch", "(", ")", ",", ";", "except",
			"intersect", "default", "tablesample", "force", "ignore", "use":
			return true
		}
		return false
	}
	endsFrom := func(w string) bool {
		switch w {
		case "where", "group", "order", "limit", "having", "window", "union", "returning", "set",
			"values", "offset", "fetch", "for", "except", "intersect", ";":
			return true
		}
		return false
	}
	// ref reads "table [AS] alias" at i and records it; it returns the index
	// after the reference.
	ref := func(i int) int {
		if i >= len(words) || words[i] == "(" || isKw(words[i]) {
			return i
		}
		table := strings.Trim(words[i], "`\"")
		i++
		if i < len(words) && strings.EqualFold(words[i], "as") {
			i++
		}
		alias := ""
		if i < len(words) && !isKw(words[i]) {
			alias = strings.Trim(words[i], "`\"")
			i++
		}
		out[table] = table
		if short := table[strings.LastIndex(table, ".")+1:]; short != table && !ambiguous[short] {
			if prev, ok := out[short]; ok && prev != table {
				delete(out, short)
				ambiguous[short] = true
			} else {
				out[short] = table
			}
		}
		if alias != "" {
			out[alias] = table
		}
		return i
	}

	depth := 0
	fromAt := map[int]bool{} // paren depths with an open FROM list
	for i := 0; i < len(words); i++ {
		switch w := lower(i); {
		case w == "(":
			depth++
		case w == ")":
			delete(fromAt, depth)
			depth--
		case w == "from":
			fromAt[depth] = true
			i = ref(i+1) - 1
		case w == "join" || w == "straight_join" || w == "update" || w == "into":
			i = ref(i+1) - 1
		case w == "," && fromAt[depth]:
			i = ref(i+1) - 1
		case endsFrom(w):
			delete(fromAt, depth)
		}
	}
	return out
}

// sqlWords splits a statement into words and single punctuation, skipping
// strings and comments, which is all Aliases needs.
func sqlWords(stmt string) []string {
	var out []string
	toks := sqlsplit.Lex(stmt)
	skip := func(i int) (int, bool) {
		for _, t := range toks {
			if i >= t.Start && i < t.End && (t.Kind == sqlsplit.TokString || t.Kind == sqlsplit.TokComment) {
				return t.End, true
			}
		}
		return i, false
	}
	i := 0
	for i < len(stmt) {
		if j, ok := skip(i); ok {
			i = j
			continue
		}
		c := stmt[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(' || c == ')' || c == ',' || c == ';':
			out = append(out, string(c))
			i++
		default:
			j := i
			for j < len(stmt) && !strings.ContainsRune(" \t\n\r(),;", rune(stmt[j])) {
				j++
			}
			out = append(out, stmt[i:j])
			i = j
		}
	}
	return out
}

// ResolveAliases fills in the table behind each step that names only an
// alias, from the statement's FROM and JOIN clauses, then re-runs the rules
// so their suggestions can name real tables.
func (p *Plan) ResolveAliases(stmt string) {
	al := Aliases(stmt)
	changed := false
	for _, n := range p.nodes {
		// Postgres names the table without its schema unless asked to be
		// VERBOSE; the statement's own spelling puts it back, so a CREATE
		// INDEX suggestion lands on dbc_explain.orders, not whatever
		// "orders" the search_path finds first
		if q, ok := al[n.Relation]; ok && n.Relation != "" && q != n.Relation && strings.Contains(q, ".") {
			n.Relation, changed = q, true
		}
		if n.Relation == "" && n.Alias != "" {
			if t, ok := al[n.Alias]; ok {
				n.Relation = t
				if n.Alias == t {
					n.Alias = ""
				}
				changed = true
			}
		}
	}
	if changed {
		p.Finalize()
	}
}

// Detect recognizes a result grid that holds a plan — the output of an
// EXPLAIN the user ran themselves — and parses it, so the grid can offer
// "show as plan". driver names the engine the result came from. ok is false
// for any other result.
func Detect(r *model.Result, driver string) (*Plan, bool) {
	if r == nil || r.IsExec || len(r.Columns) == 0 || len(r.Rows) == 0 {
		return nil, false
	}
	cols := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = strings.ToLower(c)
	}
	var p *Plan
	var err error
	switch {
	case len(cols) == 1 && cols[0] == "query plan":
		first := strings.TrimSpace(r.Rows[0][0])
		if len(r.Rows) == 1 && (strings.HasPrefix(first, "[") || strings.HasPrefix(first, "{") || strings.HasPrefix(first, "map[")) {
			// pgx hands a json column back decoded; the raw value marshals
			// back to the JSON it was
			data := []byte(first)
			if len(r.Raw) > 0 && len(r.Raw[0]) > 0 {
				if _, isStr := r.Raw[0][0].(string); !isStr {
					if b, merr := json.Marshal(r.Raw[0][0]); merr == nil {
						data = b
					}
				}
			}
			p, err = ParsePostgresJSON(data)
		} else {
			lines := make([]string, len(r.Rows))
			for i, row := range r.Rows {
				lines[i] = row[0]
			}
			eng := Postgres
			if Engine(driver) == Bytdb {
				eng = Bytdb
			}
			p, err = ParsePostgresText(lines, eng)
		}
	case len(cols) == 1 && cols[0] == "explain" && strings.HasPrefix(strings.TrimSpace(r.Rows[0][0]), "->"):
		p, err = ParseMySQLTree(r.Rows[0][0])
	case hasCols(cols, "select_type", "table", "type"):
		p, err = ParseMySQLTable(r.Columns, r.Rows)
	case hasCols(cols, "id", "parent", "detail"):
		p, err = ParseSQLite(r.Columns, r.Rows)
	default:
		return nil, false
	}
	if err != nil || p == nil {
		return nil, false
	}
	p.Conn = r.Conn
	if inner, _, ok := Strip(r.Query); ok {
		p.Statement = inner
		p.Command = r.Query
		p.ResolveAliases(inner)
	}
	return p, true
}

func hasCols(cols []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, c := range cols {
			if c == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Engine maps a config driver name (postgres, pg, sqlite3, mariadb, …) onto
// an engine constant.
func Engine(d string) string {
	switch strings.ToLower(d) {
	case "postgres", "postgresql", "pg", "pgx":
		return Postgres
	case "mysql", "mariadb":
		return MySQL
	case "sqlite", "sqlite3":
		return SQLite
	case "bytdb":
		return Bytdb
	}
	return strings.ToLower(d)
}
