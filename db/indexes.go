package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"
)

// Reading a table's indexes, for the query designer's performance rules
// (package designer): "a filter on orders.status with no index that starts
// with status", "orders.customer_id has no index, so each customer's
// orders are found by reading all of orders".
//
// Every engine's query returns the same ten columns, one row per KEY
// column of each index, in key order:
//
//	schema, table, index, kind (p primary | u unique | i other),
//	partial ('partial' or ''), method (btree, hash, gin…), position, column,
//	order (Postgres and SQLite; '' on MySQL — see OrderPattern, OrderNocase),
//	recoll ('recoll' when the key's collation is not its column's own, else
//	        ''; '' on MySQL; on SQLite the key's collation name, which
//	        Indexes turns into one or the other — see Recollated)
//
// An expression column (an index on lower(email)) comes back as "" — it
// has a position in the key, but no column a plain filter could use, so a
// rule reading the key stops there. Only key columns are listed: a
// Postgres INCLUDE column cannot narrow a search, so it serves none of the
// rules that read this.
//
// Like ColumnsQuery, the tables are named in the statement as literals
// (the catalog's own names, escaped), not bound, since the placeholder
// syntax differs per driver.

// Index is one index of a table: its key columns in order, and what kind
// of key it is.
type Index struct {
	Schema, Table, Name string
	Primary             bool // the primary key's index
	Unique              bool // a unique index (true for the primary key's too)
	// Partial: a Postgres or SQLite index with a WHERE clause. It serves
	// only queries whose filter implies that clause, so a rule saying "this
	// column is indexed" must not count it as covering every query.
	Partial bool
	Method  string   // btree, hash, gin, gist, brin, FULLTEXT…; lower-case
	Cols    []string // key columns in order; "" for an expression
	// Order is, per key column (parallel to Cols), how the b-tree keeps
	// it: on Postgres OrderPattern, OrderC or "" (see OrderPattern), on
	// SQLite OrderNocase or "" (see OrderNocase). nil when every one is ""
	// — always so on MySQL.
	Order []string
	// Recollated is, per key column (parallel to Cols), whether the index
	// keeps it in a collation other than the column's own: an index on
	// (ref COLLATE NOCASE) over a plain ref TEXT on SQLite, on
	// (ref COLLATE "C") over an en_US column on Postgres. Such a key serves
	// none of the column's =, ranges or sorts — those compare in the
	// column's collation, and both planners use an index only when its
	// collation is the comparison's (Postgres compares the collation OIDs,
	// so even a COLLATE "C" key in a C database is foreign to a
	// default-collated column). It can still serve a prefix, when its
	// order is the one the engine's LIKE needs (Order). nil when no key
	// column is — always so on MySQL, whose keys take the column's
	// collation.
	Recollated []bool
}

// recollMark is the listing's tenth column for a recollated key column.
const recollMark = "recoll"

// The order a Postgres b-tree keeps a text key column in, which decides
// whether it can serve a prefix (LIKE 'abc%'). The planner turns a prefix
// into a range (>= 'abc' AND < 'abd') only on an index whose order is
// byte order, since only there do the values starting with 'abc' sit side
// by side:
//
//	OrderPattern  a *_pattern_ops class (text_pattern_ops…): byte order
//	              whatever the collation. Serves = and a prefix, but not
//	              <, BETWEEN or ORDER BY, which want the collation's order.
//	OrderC        the default class under a C-ordered collation (C or
//	              POSIX, or the builtin provider's): byte order, and the
//	              collation's too, so it serves all of them.
//	""            anything else: the default class under a linguistic
//	              collation (en_US.UTF-8, an ICU one), where "ab-c" may
//	              sort between "abc" and "abd". Serves = and ranges, never
//	              a prefix. Also a column with no collation (an integer).
const (
	OrderPattern = "pattern"
	OrderC       = "c"
)

// OrderNocase is a SQLite key column kept in the NOCASE collation, the one
// order in which SQLite can serve a prefix. Its LIKE ignores the case of
// A–Z (PRAGMA case_sensitive_like is off by default, deprecated, and never
// turned on by dbc), so it rewrites LIKE 'ab%' into a range only on an
// index whose order ignores that case too, and finds "AB…", "Ab…" and
// "ab…" side by side:
//
//	CREATE INDEX o_ref    ON orders (ref)                 BINARY: SCAN orders
//	CREATE INDEX o_ref_nc ON orders (ref COLLATE NOCASE)  SEARCH … (ref>? AND ref<?)
//
// A key column takes its column's declared collation unless the index
// names one, so an index on a column declared COLLATE NOCASE is one too.
// Other collations (BINARY, RTRIM) are listed as "": = and ranges, no
// prefix. A NOCASE key on a BINARY column serves the LIKE but not
// ref = 'x' or ORDER BY ref, which compare in the column's own collation;
// that is Recollated's to say, as for Postgres's OrderC.
const OrderNocase = "nocase"

// IndexesQuery returns the statement listing the indexes of tables, in the
// shape described at the top of this file. bytdb gets none: its catalog
// serves pg_index's rows without the set-returning functions the Postgres
// statement needs, and its planner picks indexes its own way.
func IndexesQuery(driver string, tables []TableRef) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	if len(tables) == 0 {
		return "", serr.New("no tables to read indexes of")
	}
	switch drv {
	case "pgx":
		// indkey is an int2vector of attribute numbers (0 for an
		// expression); WITH ORDINALITY keeps the key order, and
		// indnkeyatts (Postgres 11+) leaves INCLUDE columns out. An index
		// left invalid by a failed CREATE INDEX CONCURRENTLY is never used
		// by the planner, so it is not listed as if it served anything.
		pairs := make([]string, len(tables))
		for i, t := range tables {
			pairs[i] = fmt.Sprintf("(n.nspname = %s AND t.relname = %s)", sqlLit(t.Schema, false), sqlLit(t.Name, false))
		}
		//
		// The order column (OrderPattern) reads each key column's operator
		// class and collation: indclass and indcollation are oidvectors
		// with one entry per KEY column, unnested beside indkey (which also
		// holds the INCLUDE columns; the shorter arrays pad with NULL, and
		// those rows are cut by indnkeyatts anyway). Whether a collation
		// orders like C is Postgres's own test (lc_collate_is_c): a libc
		// collation named C or POSIX, or a builtin one (Postgres 17+, all
		// code point order). The database default ('d') is whatever the
		// database was created with: datlocprovider (Postgres 15+) says
		// ICU (never C) or builtin (always), and otherwise datcollate is
		// the libc locale. datlocprovider is read through to_jsonb so the
		// statement still runs on Postgres 11–14, where it is no column.
		//
		// The recoll column (Recollated) is the planner's own test
		// (IndexCollMatchesExprColl): the key's collation OID against the
		// column's attcollation, as OIDs — "C" (950) is not "default"
		// (100) even in a database whose default is C, and the planner
		// skips the index for = on it there too. 0 on either side is a
		// type without collation (an integer); an expression key has no
		// attribute (a is NULL), so the test is NULL and reads as ''.
		return `SELECT n.nspname, t.relname, i.relname,
       CASE WHEN x.indisprimary THEN 'p' WHEN x.indisunique THEN 'u' ELSE 'i' END,
       CASE WHEN x.indpred IS NULL THEN '' ELSE 'partial' END,
       am.amname, k.ord, COALESCE(a.attname, ''),
       CASE WHEN oc.opcname IN ('text_pattern_ops', 'varchar_pattern_ops', 'bpchar_pattern_ops') THEN 'pattern'
            WHEN co.collprovider = 'b' OR co.collcollate IN ('C', 'POSIX') THEN 'c'
            WHEN co.collprovider = 'd' AND db.c THEN 'c'
            ELSE '' END,
       CASE WHEN k.coll <> 0 AND a.attcollation <> 0 AND k.coll <> a.attcollation THEN 'recoll' ELSE '' END
FROM pg_catalog.pg_index x
JOIN pg_catalog.pg_class t ON t.oid = x.indrelid
JOIN pg_catalog.pg_class i ON i.oid = x.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = i.relam
CROSS JOIN LATERAL unnest(x.indkey::int2[], x.indclass::oid[], x.indcollation::oid[])
  WITH ORDINALITY AS k(attnum, opc, coll, ord)
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum AND k.attnum > 0
LEFT JOIN pg_catalog.pg_opclass oc ON oc.oid = k.opc
LEFT JOIN pg_catalog.pg_collation co ON co.oid = k.coll
CROSS JOIN (SELECT CASE to_jsonb(d) ->> 'datlocprovider' WHEN 'b' THEN true WHEN 'i' THEN false
                   ELSE d.datcollate IN ('C', 'POSIX') END AS c
            FROM pg_catalog.pg_database d WHERE d.datname = current_database()) db
WHERE x.indisvalid AND k.ord <= x.indnkeyatts AND (` + strings.Join(pairs, " OR ") + `)
ORDER BY 1, 2, 3, 7`, nil
	case "mysql":
		// statistics has a row per index column; column_name is NULL for
		// a functional key part (8.0.13+). MySQL has no partial indexes.
		return `SELECT table_schema, table_name, index_name,
       CASE WHEN index_name = 'PRIMARY' THEN 'p' WHEN non_unique = 0 THEN 'u' ELSE 'i' END,
       '', LOWER(index_type), seq_in_index, COALESCE(column_name, ''), '', ''
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name IN (` + nameList(tables, true) + `)
ORDER BY 1, 2, 3, 7`, nil
	case "sqlite":
		// index_list's origin is 'pk' for the primary key's automatic
		// index ('c' CREATE INDEX, 'u' a UNIQUE constraint's). A rowid
		// table's INTEGER PRIMARY KEY has no index of its own — the rowid
		// is the key — so it is not listed here; the designer takes the
		// primary key from the schema's keys as well. index_xinfo's name is
		// NULL for an expression.
		//
		// index_xinfo rather than index_info for its coll, the collation
		// each column is kept in (OrderNocase). It also lists the columns
		// past the key — the rowid, or a WITHOUT ROWID table's primary
		// key — with key = 0; those find nothing, as INCLUDE columns on
		// Postgres, so they are cut. The key columns' seqno run 0..n-1, as
		// in index_info. A collation name is matched without case (SQLite
		// reads COLLATE nocase as NOCASE).
		//
		// The tenth column is the key's collation, upper-cased, not yet a
		// verdict: SQLite keeps no column's collation where a pragma could
		// read it (table_xinfo has none), so Indexes probes the columns in
		// a second statement (sqliteRecoll) and puts 'recoll' or '' here.
		return `SELECT 'main', m.name, il.name,
       CASE WHEN il.origin = 'pk' THEN 'p' WHEN il."unique" THEN 'u' ELSE 'i' END,
       CASE WHEN il.partial THEN 'partial' ELSE '' END,
       'btree', ii.seqno + 1, COALESCE(ii.name, ''),
       CASE WHEN upper(ii.coll) = 'NOCASE' THEN 'nocase' ELSE '' END,
       COALESCE(upper(ii.coll), '')
FROM sqlite_master m JOIN pragma_index_list(m.name) il JOIN pragma_index_xinfo(il.name) ii
WHERE m.type = 'table' AND ii."key" AND m.name IN (` + nameList(tables, false) + `)
ORDER BY 1, 2, 3, 7`, nil
	case bytdbdrv.DriverName:
		return "", nil
	}
	return "", serr.New("no index listing for this driver", "driver", driver)
}

// BuildIndexes turns IndexesQuery's rows into indexes, in the order the
// rows came (by schema, table, index). Rows of another shape are skipped;
// a row without the order column (the eight-column shape) reads as "".
func BuildIndexes(rows [][]string) []Index {
	var out []Index
	for _, r := range rows {
		if len(r) < 8 {
			continue
		}
		pos, err := strconv.Atoi(r[6])
		if err != nil || pos < 1 {
			continue
		}
		last := len(out) - 1
		if last < 0 || out[last].Schema != r[0] || out[last].Table != r[1] || out[last].Name != r[2] {
			out = append(out, Index{
				Schema: r[0], Table: r[1], Name: r[2],
				Primary: r[3] == "p", Unique: r[3] == "p" || r[3] == "u",
				Partial: r[4] != "", Method: strings.ToLower(r[5]),
			})
			last++
		}
		// positions are 1-based and arrive in order; a gap (a row the
		// driver could not read) is padded as an expression, so a later
		// column is never taken for a leading one. Order is kept in step
		// with Cols, so Order[i] is always Cols[i]'s.
		x := &out[last]
		for len(x.Cols) < pos-1 {
			x.Cols = append(x.Cols, "")
			x.Order = append(x.Order, "")
			x.Recollated = append(x.Recollated, false)
		}
		x.Cols = append(x.Cols, r[7])
		ord := ""
		if len(r) > 8 {
			ord = r[8]
		}
		x.Order = append(x.Order, ord)
		// only the mark counts: an unresolved SQLite collation name (a
		// listing read without Indexes' probe) says nothing about the
		// column's own, so it reads as the column's, as before the probe
		x.Recollated = append(x.Recollated, len(r) > 9 && r[9] == recollMark)
	}
	// an all-"" Order (all-false Recollated) says nothing Cols does not:
	// drop it, so the other engines' indexes (and most Postgres ones)
	// carry none
	for i := range out {
		if !slices.ContainsFunc(out[i].Order, func(o string) bool { return o != "" }) {
			out[i].Order = nil
		}
		if !slices.Contains(out[i].Recollated, true) {
			out[i].Recollated = nil
		}
	}
	return out
}

// sqliteColls are the built-in SQLite collations the probe can tell apart
// (sqliteRecoll). A key in any other (one an application registered) is
// left as its column's: its order cannot be probed for.
var sqliteColls = []string{"BINARY", "NOCASE", "RTRIM"}

// sqliteRecoll resolves the SQLite listing's tenth column — the key's
// collation name — into Recollated's mark, by asking SQLite for each key
// column's own collation. No pragma reads it, and parsing the CREATE TABLE
// would have to follow SQLite's grammar (a COLLATE in a constraint, a
// quoted name, a generated column…), so the engine is asked instead, by a
// compound SELECT: a compound compares its rows in the collation of its
// leftmost term's column, so
//
//	SELECT count(*) FROM (SELECT ref FROM orders WHERE 0 UNION SELECT 'a' UNION SELECT 'A')
//
// is 1 only when ref's collation is NOCASE ('a' and 'A' are one row), and
// the same with 'a' and 'a ' is 1 only under RTRIM; BINARY keeps both
// pairs apart. WHERE 0 reads no row of the table. Every column asked about
// goes in one statement (a pair of scalar subqueries each), so the probe
// is one round trip whatever the number of indexes.
//
// probe runs that statement and returns its one row. On any failure the
// rows are left unresolved, which BuildIndexes reads as "the column's
// own" — the reading before the probe existed, so a failed probe costs
// only this check.
func sqliteRecoll(rows [][]string, probe func(q string) ([]string, error)) {
	type tc struct{ table, col string }
	var asked []tc
	at := map[tc]int{}
	for _, r := range rows {
		if len(r) < 10 || r[7] == "" || !slices.Contains(sqliteColls, r[9]) {
			continue
		}
		k := tc{r[1], r[7]}
		if _, ok := at[k]; !ok {
			at[k] = len(asked)
			asked = append(asked, k)
		}
	}
	if len(asked) == 0 {
		return
	}
	terms := make([]string, 0, 2*len(asked))
	for _, k := range asked {
		// each name quoted whole (not quoteIdent, which splits on dots for
		// a schema-qualified name: a column may hold a dot)
		from := `"` + strings.ReplaceAll(k.col, `"`, `""`) + `" FROM "` + strings.ReplaceAll(k.table, `"`, `""`) + `"`
		for _, other := range []string{"'A'", "'a '"} {
			terms = append(terms, "(SELECT count(*) FROM (SELECT "+from+" WHERE 0 UNION SELECT 'a' UNION SELECT "+other+"))")
		}
	}
	got, err := probe("SELECT " + strings.Join(terms, ", "))
	if err != nil || len(got) != len(terms) {
		for _, r := range rows {
			if len(r) >= 10 {
				r[9] = ""
			}
		}
		return
	}
	own := make([]string, len(asked))
	for i := range asked {
		switch {
		case got[2*i] == "1":
			own[i] = "NOCASE"
		case got[2*i+1] == "1":
			own[i] = "RTRIM"
		default:
			own[i] = "BINARY"
		}
	}
	for _, r := range rows {
		if len(r) < 10 {
			continue
		}
		i, ok := at[tc{r[1], r[7]}]
		if ok && slices.Contains(sqliteColls, r[9]) && r[9] != own[i] {
			r[9] = recollMark
		} else {
			r[9] = ""
		}
	}
}

// Indexes reads the indexes of tables on the named connection. It runs on
// the pool, as Columns does. A driver with no index listing (bytdb) answers
// ErrNoIndexes, so a caller can tell "none" from "not known": a rule that
// would say "this column has no index" must stay silent on the second.
func (m *Manager) Indexes(ctx context.Context, name string, tables []TableRef) ([]Index, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	q, err := IndexesQuery(cc.Driver, tables)
	if err != nil {
		return nil, err
	}
	if q == "" {
		return nil, ErrNoIndexes
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := stringRows(ctx, dbh, q)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "read indexes")
	}
	if drv, _ := driverFor(cc.Driver); drv == "sqlite" {
		sqliteRecoll(rows, func(q string) ([]string, error) {
			got, err := stringRows(ctx, dbh, q)
			if err != nil {
				return nil, err
			}
			if len(got) != 1 {
				return nil, errors.New("collation probe gave no row")
			}
			return got[0], nil
		})
	}
	return BuildIndexes(rows), nil
}

// ErrNoIndexes is Indexes' answer on an engine whose indexes dbc cannot
// list.
var ErrNoIndexes = errors.New("this engine's indexes are not listed")

// RowEstimates reads the connection's per-table row estimates
// (RowEstimatesQuery) as a map, or nil for an engine that keeps none. An
// unknown estimate (never analyzed) is left out. One catalog query: no
// table is scanned.
func (m *Manager) RowEstimates(ctx context.Context, name string) (map[TableRef]int64, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	q, err := RowEstimatesQuery(cc.Driver)
	if err != nil || q == "" {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := stringRows(ctx, dbh, q)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "read row estimates")
	}
	out := make(map[TableRef]int64, len(rows))
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		if n, err := strconv.ParseInt(r[2], 10, 64); err == nil && n >= 0 {
			out[TableRef{Schema: r[0], Name: r[1]}] = n
		}
	}
	return out, nil
}
