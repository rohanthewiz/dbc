package db

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// TablesQuery returns the statement that lists a driver's tables and views —
// the `\dt` of whichever database this is. The shape is deliberately the same
// across drivers (schema/owner, name, type), so the results table reads the
// same wherever you press the key.
//
// It is ordinary SQL run through the ordinary path, which means it is
// cancelable, capped by max_rows, and exportable like any other result.
func TablesQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx":
		// information_schema.tables is the SQL standard's view, and the
		// standard has no materialized views, so Postgres leaves them out
		// of it; pg_matviews supplies them. Their type contains VIEW, so
		// TableRefs and the sidebar both read them as views.
		//
		// information_schema.tables shows only relations the user holds
		// some privilege on, while pg_matviews shows all of them; the
		// has_table_privilege filter keeps the two halves to the same
		// rule. A matview's only useful privilege is SELECT. The name is
		// quoted, as that function parses its text argument as SQL.
		//
		// ORDER BY after a UNION applies to the whole result, by the
		// first branch's column names.
		return `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
UNION ALL
SELECT schemaname, matviewname, 'MATERIALIZED VIEW'
FROM pg_catalog.pg_matviews
WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
  AND has_table_privilege(quote_ident(schemaname) || '.' || quote_ident(matviewname), 'SELECT')
ORDER BY table_schema, table_name`, nil
	case bytdbdrv.DriverName:
		// The two catalog schemas are always there and never what was
		// meant. bytdb serves the same information_schema.tables, views
		// included (listed as 'VIEW', as Postgres does). It has no
		// materialized views, and no pg_matviews, so it keeps the plain
		// query rather than sharing Postgres's.
		return `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY table_schema, table_name`, nil
	case "mysql":
		// DATABASE() scopes this to the schema the DSN connected to, rather
		// than every schema on the server
		return `SELECT table_schema, table_name, table_type
FROM information_schema.tables
WHERE table_schema = DATABASE()
ORDER BY table_name`, nil
	case "sqlite":
		// sqlite has no schemas; the literal keeps the column count matching
		// the other drivers. The sqlite_ prefix is reserved for internals.
		return `SELECT 'main' AS table_schema, name AS table_name, upper(type) AS table_type
FROM sqlite_master
WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'
ORDER BY type, name`, nil
	}
	return "", serr.New("no table listing for this driver", "driver", driver)
}

// TableRef names one table or view in a connection's catalog, as a row of
// TablesQuery reports it.
type TableRef struct {
	Schema string
	Name   string
	View   bool
}

// TableRefs reads TablesQuery's rows (table_schema, table_name, table_type).
// Rows of any other shape are skipped rather than guessed at.
func TableRefs(rows [][]string) []TableRef {
	out := make([]TableRef, 0, len(rows))
	for _, row := range rows {
		if len(row) < 3 {
			continue
		}
		out = append(out, TableRef{Schema: row[0], Name: row[1],
			View: strings.Contains(strings.ToUpper(row[2]), "VIEW")})
	}
	return out
}

// TableIndex finds the catalog's tables that a piece of text mentions. It is
// built once per connect and asked on every frame the assistant's context
// chip draws, so the lookups are two maps rather than a scan of the catalog.
//
// Matching is by WORD, not by parse. A statement can name a table in more
// places than any small grammar covers (FROM, JOIN, INTO, UPDATE, a CTE body,
// a subquery, a DDL statement), and a question in prose names it with no
// grammar at all ("which cats are oldest?"). A word that happens to equal a
// table name costs one table's column list in the prompt; a table the model
// never hears about costs a guessed column name in the SQL it writes back.
// The first error is much cheaper, so the matcher errs toward it.
type TableIndex struct {
	tables  []TableRef
	byName  map[string][]int // lower(name) → every schema's table of that name
	byQual  map[string]int   // lower(schema.name) → the one table
	schemas int              // distinct schemas; 1 means names need no qualifier
}

// NewTableIndex indexes a catalog.
func NewTableIndex(tables []TableRef) *TableIndex {
	x := &TableIndex{tables: tables, byName: map[string][]int{}, byQual: map[string]int{}}
	seen := map[string]bool{}
	for i, t := range tables {
		n := strings.ToLower(t.Name)
		x.byName[n] = append(x.byName[n], i)
		x.byQual[strings.ToLower(t.Schema)+"."+n] = i
		if !seen[t.Schema] {
			seen[t.Schema] = true
			x.schemas++
		}
	}
	return x
}

// Len is the number of tables in the catalog.
func (x *TableIndex) Len() int { return len(x.tables) }

// Display is how a table should be named to a reader: bare when the
// connection has one schema (every sqlite and mysql connection, most
// Postgres ones), qualified when the name alone could be ambiguous. The
// sidebar follows the same rule.
func (x *TableIndex) Display(t TableRef) string {
	if x.schemas > 1 && t.Schema != "" {
		return t.Schema + "." + t.Name
	}
	return t.Name
}

// Mentioned returns the tables named in a SQL statement or in prose, in
// order of first mention, each once. The statement's words come first, since
// that is what the question is most often about.
//
// In SQL the lexer's view of the text is used, so a table name inside a
// string literal or a comment does not count, and a quoted identifier
// ("Cats", `cats`) counts without its quotes. Prose has no such structure —
// an apostrophe in "what's" is not a string — so it is split on word
// boundaries only.
//
// A dotted word (public.cats, mydb.cats, c.name) matches only as
// schema.table. It never falls back to its last part: in `c.name`, `c` is a
// table alias and `name` a column, and a table called `name` is not meant.
func (x *TableIndex) Mentioned(sql, prose string) []TableRef {
	if x == nil || len(x.tables) == 0 {
		return nil
	}
	var out []TableRef
	seen := map[int]bool{}
	add := func(i int) {
		if !seen[i] {
			seen[i] = true
			out = append(out, x.tables[i])
		}
	}
	match := func(word string) {
		word = strings.ToLower(strings.Trim(word, "."))
		if word == "" {
			return
		}
		if dot := strings.LastIndexByte(word, '.'); dot >= 0 {
			// db.schema.table (3 parts) keeps its last two
			qual := word
			if prev := strings.LastIndexByte(word[:dot], '.'); prev >= 0 {
				qual = word[prev+1:]
			}
			if i, ok := x.byQual[qual]; ok {
				add(i)
			}
			return
		}
		for _, i := range x.byName[word] {
			add(i)
		}
	}
	for _, w := range sqlWords(sql) {
		match(w)
	}
	for _, w := range strings.FieldsFunc(prose, notWordRune) {
		match(w)
	}
	return out
}

// sqlWords is the words of a statement outside strings, comments, numbers
// and parameters, with quoted identifiers unquoted in place so that
// "public"."Cats" reads as public.Cats.
func sqlWords(sql string) []string {
	b := []byte(sql)
	for _, tk := range sqlsplit.Lex(sql) {
		switch tk.Kind {
		case sqlsplit.TokString, sqlsplit.TokComment, sqlsplit.TokNumber, sqlsplit.TokParam:
			for i := tk.Start; i < tk.End; i++ {
				b[i] = ' '
			}
		case sqlsplit.TokIdent:
			// only the delimiters go; the name keeps its bytes (and
			// its adjacency to a neighbouring dot)
			b[tk.Start], b[tk.End-1] = '\x00', '\x00'
		}
	}
	s := strings.ReplaceAll(string(b), "\x00", "")
	return strings.FieldsFunc(s, notWordRune)
}

// notWordRune separates words: anything but a letter, digit, _, $ or the dot
// that joins a qualified name.
func notWordRune(r rune) bool {
	return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' || r == '.')
}

// ColumnsQuery returns the statement that lists the columns of the given
// tables, in declared order, as table_schema · table_name · column_name ·
// data_type. It feeds the assistant's schema context, so the type is the
// one a person would write in DDL ("character varying(80)", "enum('a','b')"),
// which is what makes the model's SQL right, rather than a normalized family
// name ("USER-DEFINED", "ARRAY").
//
// The table names are inlined as literals rather than bound as parameters:
// the placeholder syntax differs per driver ($1 vs ?), the names come from
// the connection's own catalog rather than from a user, and they are
// escaped all the same.
func ColumnsQuery(driver string, tables []TableRef) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	if len(tables) == 0 {
		return "", serr.New("no tables to describe")
	}
	switch drv {
	case "pgx", bytdbdrv.DriverName:
		// pg_attribute rather than information_schema.columns: format_type
		// gives the declared type with its modifiers and array/enum names,
		// and bytdb serves both tables and the function. relkind keeps
		// index relations (which have attributes too) out. The pairs are an
		// OR chain, not a row-value IN, as the more widely parsed spelling.
		pairs := make([]string, len(tables))
		for i, t := range tables {
			pairs[i] = fmt.Sprintf("(n.nspname = %s AND c.relname = %s)", sqlLit(t.Schema, false), sqlLit(t.Name, false))
		}
		return `SELECT n.nspname AS table_schema, c.relname AS table_name, a.attname AS column_name,
       format_type(a.atttypid, a.atttypmod) AS data_type
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'v', 'm', 'p', 'f')
  AND (` + strings.Join(pairs, "\n    OR ") + `)
ORDER BY n.nspname, c.relname, a.attnum`, nil
	case "mysql":
		// column_type is the declared type (varchar(80), enum(...),
		// int unsigned); data_type would drop exactly those details
		return `SELECT table_schema, table_name, column_name, column_type AS data_type
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name IN (` + nameList(tables, true) + `)
ORDER BY table_name, ordinal_position`, nil
	case "sqlite":
		// the pragma_table_info table-valued function works on views as
		// well; a view's computed column may report an empty type
		return `SELECT 'main' AS table_schema, m.name AS table_name, p.name AS column_name, p.type AS data_type
FROM sqlite_master m JOIN pragma_table_info(m.name) p
WHERE m.type IN ('table', 'view') AND m.name IN (` + nameList(tables, false) + `)
ORDER BY m.name, p.cid`, nil
	}
	return "", serr.New("no column listing for this driver", "driver", driver)
}

// nameList is the tables' names as a comma-separated list of literals.
func nameList(tables []TableRef, mysql bool) string {
	lits := make([]string, len(tables))
	for i, t := range tables {
		lits[i] = sqlLit(t.Name, mysql)
	}
	return strings.Join(lits, ", ")
}

// sqlLit quotes s as a string literal. MySQL (by default) also treats a
// backslash in a literal as an escape, so it is doubled there.
func sqlLit(s string, mysql bool) string {
	if mysql {
		s = strings.ReplaceAll(s, `\`, `\\`)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Column is one column of a table: its name and declared type.
type Column struct {
	Name string
	Type string
}

// Columns describes tables on the named connection: result i holds the
// columns of tables[i], in declared order, and is nil for a table the
// catalog did not return (dropped since the connect, or a view the driver
// cannot describe). It runs on the pool, like the sidebar's catalog query,
// so it never lands inside a transaction the user has open on their session.
func (m *Manager) Columns(ctx context.Context, name string, tables []TableRef) ([][]Column, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	q, err := ColumnsQuery(cc.Driver, tables)
	if err != nil {
		return nil, err
	}
	res, err := m.RunContext(ctx, name, q)
	if err != nil {
		return nil, err
	}
	// Group by (schema, table). A schema that does not match exactly falls
	// back to the name alone: MySQL reports the database under table_schema
	// in both queries, but in whatever letter case the server stores it.
	type key struct{ schema, name string }
	bySchema := map[key][]Column{}
	byName := map[string][]Column{}
	for _, row := range res.Rows {
		if len(row) < 4 {
			continue
		}
		col := Column{Name: row[2], Type: row[3]}
		k := key{row[0], row[1]}
		bySchema[k] = append(bySchema[k], col)
		byName[row[1]] = append(byName[row[1]], col)
	}
	out := make([][]Column, len(tables))
	for i, t := range tables {
		if cols, ok := bySchema[key{t.Schema, t.Name}]; ok {
			out[i] = cols
		} else {
			out[i] = byName[t.Name]
		}
	}
	return out, nil
}
