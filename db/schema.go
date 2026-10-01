package db

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/erd"
)

// Reading a connection's schema for an entity-relationship diagram: every
// table's columns and its keys (primary, unique, foreign), from the
// engine's catalog, into an erd.Schema.
//
//	TablesQuery ───────► the tables and views (the sidebar's list)
//	SchemaColumnsQuery ► schema · table · column · type · nullable · position
//	SchemaKeysQuery ───► one row per key column (see below)
//	PartitionsQuery ───► schema · table of each partition (Postgres only)
//	                            │
//	                BuildSchema (pure, tested without a database)
//	                            ▼
//	                       *erd.Schema
//
// THE KEY ROWS. Every engine's key query returns the same eight columns:
//
//	schema, table, constraint, kind (p|u|f), position, column,
//	ref_schema, ref_table, ref_column          (the ref_* only on f rows)
//
// one row per column of each key, except on Postgres and bytdb, whose
// catalog (pg_constraint) keeps a key's columns as arrays of attribute
// numbers — {2,3} — not names. There the query returns one row per key
// with the arrays in the position and column slots, and expandPgKeys turns
// it into the per-column shape using the column query's positions. Reading
// the arrays in Go rather than unnesting them in SQL keeps one query that
// both servers run: bytdb serves pg_constraint but not every set-returning
// function Postgres would use to unnest it.
//
// WHY NOT information_schema FOR THE FOREIGN KEYS. The standard answer is
// referential_constraints joined to key_column_usage twice. bytdb does not
// serve referential_constraints, and on Postgres the standard views are
// markedly slower on big catalogs; pg_constraint is one scan on both.

// SchemaColumnsQuery returns the statement listing every column of every
// table and view the connection shows, as schema · table · column · type ·
// nullable (YES/NO) · position. The type is the declared one, as
// ColumnsQuery gives the assistant ("character varying(80)"), which is what
// a diagram's reader wants to see. Position is pg_attribute's attnum on
// Postgres and bytdb — what pg_constraint's key arrays refer to — and the
// ordinal position elsewhere.
func SchemaColumnsQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx", bytdbdrv.DriverName:
		return `SELECT n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod),
       CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END, a.attnum
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'v', 'm', 'p', 'f')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1, 2, a.attnum`, nil
	case "mysql":
		// column_type is the declared type (int unsigned, enum(...))
		return `SELECT table_schema, table_name, column_name, column_type, is_nullable, ordinal_position
FROM information_schema.columns
WHERE table_schema = DATABASE()
ORDER BY table_name, ordinal_position`, nil
	case "sqlite":
		return `SELECT 'main', m.name, p.name, p.type, CASE WHEN p."notnull" THEN 'NO' ELSE 'YES' END, p.cid + 1
FROM sqlite_master m JOIN pragma_table_info(m.name) p
WHERE m.type IN ('table', 'view') AND m.name NOT LIKE 'sqlite_%'
ORDER BY m.name, p.cid`, nil
	}
	return "", serr.New("no schema listing for this driver", "driver", driver)
}

// SchemaKeysQuery returns the statement listing every primary, unique and
// foreign key, in the shape described at the top of this file.
func SchemaKeysQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx", bytdbdrv.DriverName:
		// ::text renders the int2[] arrays as {1,2} on Postgres; bytdb
		// already stores them as that text. The LEFT JOINs keep p and u
		// rows, which have no referenced table; COALESCE keeps their
		// ref_* cells empty rather than NULL.
		return `SELECT n.nspname, c.relname, con.conname, con.contype, con.conkey::text, '',
       COALESCE(fn.nspname, ''), COALESCE(fc.relname, ''), COALESCE(con.confkey::text, '')
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = con.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE con.contype IN ('p', 'u', 'f') AND n.nspname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1, 2, 3`, nil
	case "mysql":
		// key_column_usage lists the columns of every key, and on a
		// foreign key's rows the referenced column too; table_constraints
		// says which kind of key each is. A foreign key may reference
		// another database — it is dropped later, having no box to go to.
		return `SELECT k.table_schema, k.table_name, k.constraint_name,
       CASE t.constraint_type WHEN 'PRIMARY KEY' THEN 'p' WHEN 'UNIQUE' THEN 'u' ELSE 'f' END,
       k.ordinal_position, k.column_name,
       COALESCE(k.referenced_table_schema, ''), COALESCE(k.referenced_table_name, ''), COALESCE(k.referenced_column_name, '')
FROM information_schema.key_column_usage k
JOIN information_schema.table_constraints t
  ON t.constraint_schema = k.constraint_schema AND t.table_name = k.table_name
 AND t.constraint_name = k.constraint_name
WHERE k.table_schema = DATABASE() AND t.constraint_type IN ('PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY')
ORDER BY 1, 2, 3, 5`, nil
	case "sqlite":
		// SQLite has no constraint catalog; its pragmas carry the same
		// facts. Foreign keys are unnamed, so they are named by table and
		// the pragma's id. A foreign key written REFERENCES parent, with
		// no column, references the parent's primary key and reports its
		// "to" as NULL; BuildSchema fills it in. The primary key comes
		// from table_info's pk (the column's position in the key), and
		// unique keys from the unique indexes, bar the primary key's own.
		return `SELECT 'main', m.name, 'fk_' || m.name || '_' || f.id, 'f', f.seq + 1, f."from", 'main', f."table", COALESCE(f."to", '')
FROM sqlite_master m JOIN pragma_foreign_key_list(m.name) f
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%'
UNION ALL
SELECT 'main', m.name, m.name || '_pkey', 'p', p.pk, p.name, '', '', ''
FROM sqlite_master m JOIN pragma_table_info(m.name) p
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND p.pk > 0
UNION ALL
SELECT 'main', m.name, il.name, 'u', ii.seqno + 1, COALESCE(ii.name, ''), '', '', ''
FROM sqlite_master m JOIN pragma_index_list(m.name) il JOIN pragma_index_info(il.name) ii
WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND il."unique" AND il.origin <> 'pk'
ORDER BY 1, 2, 3, 5`, nil
	}
	return "", serr.New("no key listing for this driver", "driver", driver)
}

// PartitionsQuery returns the statement listing the tables that are
// partitions of another (schema · table), or "" when the engine has none
// that the diagram needs to hide.
//
// WHY HIDE THEM. A Postgres partitioned table's partitions are tables in
// the catalog, and the sidebar lists them (a person may well query one
// directly). But in a diagram each would be a box of its own, with a clone
// of every one of the parent's foreign keys: Postgres copies a key
// declared on a partitioned table onto each partition (pg_constraint rows
// with conparentid <> 0), and a key referencing a partitioned table onto
// each of the referenced partitions too. A table split by month over three
// years would draw 36 copies of itself, each with the parent's lines. The
// partitioned table itself carries the model: its columns and the keys
// declared on it.
//
// relispartition is set on every partition, a sub-partition included, and
// has been since partitioning arrived (Postgres 10). bytdb has no
// partitioning, and its pg_class need not carry the column, so it gets no
// query.
func PartitionsQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	if drv != "pgx" {
		return "", nil
	}
	return `SELECT n.nspname, c.relname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relispartition`, nil
}

// BuildSchema assembles the diagram's model from the catalog queries' rows.
// tables is the sidebar's catalog (TableRefs of TablesQuery), which decides
// which tables exist and which are views; columns and keys of anything not
// in it (an index, a sequence, a system table) are ignored. hidden lists
// (schema, table) rows of catalog tables the diagram leaves out — the
// partitions of PartitionsQuery — and so the columns and keys of those
// too: a key cloned onto a partition, or cloned to reference one, has no
// box at one end and is dropped with it. Rows of the wrong shape are
// skipped rather than guessed at, as TableRefs does.
//
// A table's Label follows TableIndex.Display over the whole catalog,
// hidden tables included, so the name the sidebar shows is the name
// erd.Schema.Find resolves: hiding a schema's only tables (partitions kept
// in a schema of their own) does not turn every other label bare.
func BuildSchema(driver, conn string, tables []TableRef, colRows, keyRows, hidden [][]string) *erd.Schema {
	s := &erd.Schema{Conn: conn, Driver: driver}
	idx := NewTableIndex(tables)
	type key struct{ schema, name string }
	skip := map[key]bool{}
	for _, r := range hidden {
		if len(r) >= 2 {
			skip[key{r[0], r[1]}] = true
		}
	}
	byKey := map[key]*erd.Table{}
	// MySQL reports table_schema in whatever case the server stores it,
	// which need not match DATABASE(); a name alone is unambiguous there
	// (one database per connection), so it is the fallback.
	byName := map[string][]*erd.Table{}
	for _, t := range tables {
		if skip[key{t.Schema, t.Name}] {
			continue
		}
		et := &erd.Table{Schema: t.Schema, Name: t.Name, Label: idx.Display(t), View: t.View}
		s.Tables = append(s.Tables, et)
		byKey[key{t.Schema, t.Name}] = et
		byName[t.Name] = append(byName[t.Name], et)
	}
	find := func(schema, name string) *erd.Table {
		if t := byKey[key{schema, name}]; t != nil {
			return t
		}
		if ts := byName[name]; len(ts) == 1 {
			return ts[0]
		}
		return nil
	}

	// columns, and each table's position → name map for the pg key arrays
	pos := map[*erd.Table]map[string]string{}
	for _, r := range colRows {
		if len(r) < 6 {
			continue
		}
		t := find(r[0], r[1])
		if t == nil {
			continue
		}
		t.Cols = append(t.Cols, &erd.Column{Name: r[2], Type: r[3], Nullable: !strings.EqualFold(r[4], "NO")})
		if pos[t] == nil {
			pos[t] = map[string]string{}
		}
		pos[t][r[5]] = r[2]
	}

	drv, _ := driverFor(driver)
	if drv == "pgx" || drv == bytdbdrv.DriverName {
		keyRows = expandPgKeys(keyRows, func(schema, name string) map[string]string {
			return pos[find(schema, name)]
		})
	}

	// Group the per-column rows into keys. A key is identified by its
	// table and name; its columns are sorted by position, since not every
	// engine's query can promise the order.
	type keyCol struct {
		pos           int
		col, refCol   string
		refSch, refTb string
	}
	type rawKey struct {
		table *erd.Table
		name  string
		kind  string
		cols  []keyCol
	}
	var order []*rawKey
	keys := map[string]*rawKey{}
	for _, r := range keyRows {
		if len(r) < 9 || r[5] == "" {
			continue
		}
		t := find(r[0], r[1])
		if t == nil {
			continue
		}
		id := r[0] + "\x00" + r[1] + "\x00" + r[2] + "\x00" + r[3]
		k := keys[id]
		if k == nil {
			k = &rawKey{table: t, name: r[2], kind: r[3]}
			keys[id] = k
			order = append(order, k)
		}
		p, _ := strconv.Atoi(r[4])
		k.cols = append(k.cols, keyCol{pos: p, col: r[5], refSch: r[6], refTb: r[7], refCol: r[8]})
	}

	for _, k := range order {
		sort.SliceStable(k.cols, func(i, j int) bool { return k.cols[i].pos < k.cols[j].pos })
		names := make([]string, len(k.cols))
		for i, c := range k.cols {
			names[i] = c.col
		}
		switch k.kind {
		case "p":
			k.table.PK = names
		case "u":
			k.table.Uniques = append(k.table.Uniques, names)
		}
	}
	// Foreign keys last: an implicit reference (SQLite's REFERENCES
	// parent) needs the parent's primary key, which may be listed after
	// the child.
	for _, k := range order {
		if k.kind != "f" {
			continue
		}
		parent := find(k.cols[0].refSch, k.cols[0].refTb)
		if parent == nil {
			continue // references a table outside the catalog
		}
		rel := &erd.Rel{Name: k.name, Child: k.table, Parent: parent}
		for i, c := range k.cols {
			ref := c.refCol
			if ref == "" && i < len(parent.PK) {
				ref = parent.PK[i]
			}
			rel.ChildCols = append(rel.ChildCols, c.col)
			rel.ParentCols = append(rel.ParentCols, ref)
		}
		s.Rels = append(s.Rels, rel)
	}
	s.MarkKeys()
	s.Sort()
	return s
}

// expandPgKeys turns pg_constraint rows — one per key, its columns as
// attnum arrays ({2,3}) in the position slot and the referenced columns'
// in the ref_column slot — into the per-column rows the other engines
// return. posOf gives a table's attnum → column name map. A key naming an
// attnum the column query did not return (dropped since, or a table the
// user cannot read) is skipped whole: a key with a column missing would
// draw a wrong line.
func expandPgKeys(rows [][]string, posOf func(schema, name string) map[string]string) [][]string {
	var out [][]string
	for _, r := range rows {
		if len(r) < 9 {
			continue
		}
		cols := pgArray(r[4])
		var refs []string
		var refPos map[string]string
		if r[3] == "f" {
			refs = pgArray(r[8])
			refPos = posOf(r[6], r[7])
			if len(refs) != len(cols) {
				continue
			}
		}
		own := posOf(r[0], r[1])
		var expanded [][]string
		for i, a := range cols {
			name, ok := own[a]
			if !ok {
				expanded = nil
				break
			}
			ref := ""
			if refs != nil {
				if ref, ok = refPos[refs[i]]; !ok {
					expanded = nil
					break
				}
			}
			expanded = append(expanded, []string{r[0], r[1], r[2], r[3], strconv.Itoa(i + 1), name, r[6], r[7], ref})
		}
		out = append(out, expanded...)
	}
	return out
}

// pgArray reads an int2[] literal, {1,2}, into its elements. An empty or
// NULL array is nil.
func pgArray(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// maxSchemaRows bounds each catalog query of Schema. It is not max_rows —
// that caps what a person sees of a result, and a diagram cut at 1,000
// columns would be silently wrong — but a catalog of a quarter of a million
// columns is past anything a diagram can show, and stopping keeps a
// runaway catalog from eating memory.
const maxSchemaRows = 250_000

// Schema reads the connection's schema for a diagram: its tables and views
// (as the sidebar lists them), their columns and their keys. It runs on
// the pool, like the sidebar's catalog query, so it never lands inside a
// transaction the user has open on their session, and it reads rows
// itself rather than through Run so the max_rows cap does not apply.
func (m *Manager) Schema(ctx context.Context, name string) (*erd.Schema, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	tq, err := TablesQuery(cc.Driver)
	if err != nil {
		return nil, err
	}
	cq, err := SchemaColumnsQuery(cc.Driver)
	if err != nil {
		return nil, err
	}
	kq, err := SchemaKeysQuery(cc.Driver)
	if err != nil {
		return nil, err
	}
	pq, err := PartitionsQuery(cc.Driver)
	if err != nil {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	var rows [4][][]string
	for i, q := range []string{tq, cq, kq, pq} {
		if q == "" {
			continue // an engine with nothing to hide
		}
		if rows[i], err = stringRows(ctx, dbh, q); err != nil {
			return nil, wrapRunErr(ctx, err, name, "op", "read the schema")
		}
	}
	return BuildSchema(cc.Driver, name, TableRefs(rows[0]), rows[1], rows[2], rows[3]), nil
}

// stringRows runs a catalog query and returns its cells as strings, NULL as
// "". Scanning into sql.NullString has database/sql convert every driver's
// integers and byte slices to text.
func stringRows(ctx context.Context, dbh *sql.DB, q string) ([][]string, error) {
	_, rows, truncated, err := limitedRows(ctx, dbh, q, maxSchemaRows)
	if err != nil {
		return nil, err
	}
	// a diagram of part of a catalog would be silently wrong, so for these
	// callers running past the bound is a failure, not a shorter answer
	if truncated {
		return nil, serr.New("the catalog is too big to diagram", "rows", strconv.Itoa(maxSchemaRows))
	}
	return rows, nil
}

// limitedRows runs a catalog query and returns its column names and at most
// limit rows of cells as strings, NULL as "". truncated reports that the
// query had more rows than that; the rest are not read. It is the shared
// reader behind stringRows (where more is an error) and Manager.Catalog
// (where more is a note), and unlike Run it ignores max_rows.
func limitedRows(ctx context.Context, dbh *sql.DB, q string, limit int) (cols []string, out [][]string, truncated bool, err error) {
	rows, err := dbh.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	if cols, err = rows.Columns(); err != nil {
		return nil, nil, false, err
	}
	cells := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	for rows.Next() {
		if len(out) >= limit {
			return cols, out, true, nil
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, nil, false, err
		}
		row := make([]string, len(cols))
		for i, c := range cells {
			row[i] = c.String
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return cols, out, false, nil
}
