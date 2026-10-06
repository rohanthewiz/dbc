package etl

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"
)

// CopyOptions shape a Copy. The zero value copies every column and row of
// the table into a table of the same name that must already exist.
type CopyOptions struct {
	// To names the destination table; empty means the source table's name.
	To string
	// Query, when set, is the source instead of a table: any SELECT, with
	// Args as its parameters. To is then required.
	Query string
	// Args are bind parameters for Query or Where, in the source's
	// placeholder style ($1 Postgres/bytdb, ? MySQL/SQLite).
	Args []any
	// Columns limits the copy to these source columns, in this order. The
	// destination columns have the same names.
	Columns []string
	// Where filters the source table's rows: "created_at >= $1".
	Where string
	// Create makes the destination table when it does not exist yet. A
	// Postgres-to-Postgres copy of a table reproduces the source's column
	// types, NOT NULLs and primary key from the catalog; any other pairing
	// maps each column to a broad type on the destination (integer, float,
	// numeric, boolean, date, timestamp, bytes, text). Defaults, indexes
	// other than the key, and constraints are not copied.
	Create bool
	// Truncate empties the destination first, in the load's transaction:
	// if the copy fails, the old rows are still there.
	Truncate bool
	// BatchSize is rows per INSERT on non-Postgres destinations (see
	// WriteOptions.BatchSize).
	BatchSize int
	// Transform is called on every row before it is written, and may
	// change values in place or return a new row of the same length.
	// Returning a nil row skips it; returning an error stops the copy and
	// rolls the destination back.
	Transform func(row []any) ([]any, error)
	// Progress, when set, is called every ProgressEvery rows (default
	// 100000) with the running total.
	Progress      func(rows int64)
	ProgressEvery int64
}

// CopyStats reports a finished copy.
type CopyStats struct {
	From     string // "conn:table" (or "conn:query")
	To       string // "conn:table"
	Rows     int64  // rows loaded
	Skipped  int64  // rows a Transform dropped
	Duration time.Duration
	// Direct is true when the rows streamed Postgres to Postgres as COPY
	// text, never decoded into Go values.
	Direct bool
}

func (s CopyStats) String() string {
	mode := "row by row"
	if s.Direct {
		mode = "direct COPY"
	}
	msg := fmt.Sprintf("copied %d rows %s → %s in %s (%s)", s.Rows, s.From, s.To,
		s.Duration.Round(time.Millisecond), mode)
	if s.Skipped > 0 {
		msg += fmt.Sprintf(", %d skipped", s.Skipped)
	}
	return msg
}

const defaultProgressEvery = 100_000

// Copy copies rows from table on src into a table on dst. The two may be
// the same connection or different engines entirely.
//
// Which path it takes:
//
//	src, dst both Postgres, no Transform, no Args?
//	   yes ─► direct: COPY (SELECT …) TO STDOUT piped into COPY … FROM STDIN.
//	          The rows are never decoded, so it is as fast as the two servers
//	          and the network between them allow, and lossless for every type
//	          both ends understand (arrays, json, ranges, enums …).
//	   no  ─► row by row: Reader ─► Transform ─► Writer (COPY FROM STDIN on a
//	          Postgres destination, batched INSERT elsewhere).
//
// COPY cannot take bind parameters, which is why Args forces the row path.
//
// The destination is loaded in one transaction together with the Create and
// Truncate: on failure, or when ctx is canceled, it is left as it was
// (MySQL excepted for the DDL, which it commits implicitly). The source is
// read by a single statement, so it sees one consistent snapshot.
func Copy(ctx context.Context, src Conn, table string, dst Conn, opt CopyOptions) (CopyStats, error) {
	start := time.Now()
	query, dest, err := copySource(src.Engine, table, opt)
	if err != nil {
		return CopyStats{}, err
	}
	st := CopyStats{From: src.Name + ":" + table, To: dst.Name + ":" + dest}
	if opt.Query != "" {
		st.From = src.Name + ":query"
	}
	if opt.Progress != nil && opt.ProgressEvery <= 0 {
		opt.ProgressEvery = defaultProgressEvery
	}

	if src.Engine == Postgres && dst.Engine == Postgres && opt.Transform == nil && len(opt.Args) == 0 {
		st.Direct = true
		err = copyDirect(ctx, src, table, query, dst, dest, opt, &st)
	} else {
		err = copyRows(ctx, src, table, query, dst, dest, opt, &st)
	}
	st.Duration = time.Since(start)
	if err != nil {
		return st, serr.Wrap(canceled(ctx, err), "from", st.From, "to", st.To)
	}
	return st, nil
}

// copySource builds the source SELECT and settles the destination name.
func copySource(e Engine, table string, opt CopyOptions) (query, dest string, err error) {
	switch {
	case opt.Query != "" && (opt.Where != "" || len(opt.Columns) > 0):
		return "", "", serr.New("etl: Query replaces Where and Columns — put them in the query")
	case opt.Query != "":
		if opt.To == "" {
			return "", "", serr.New("etl: To (the destination table) is required with Query")
		}
		return strings.TrimSuffix(strings.TrimSpace(opt.Query), ";"), opt.To, nil
	case strings.TrimSpace(table) == "":
		return "", "", serr.New("etl: no source table (or Query) given")
	}
	cols := "*"
	if len(opt.Columns) > 0 {
		cols = e.quoteCols(opt.Columns)
	}
	query = "SELECT " + cols + " FROM " + e.QuoteTable(table)
	if w := strings.TrimSpace(opt.Where); w != "" {
		query += " WHERE " + w
	}
	dest = opt.To
	if dest == "" {
		dest = table
	}
	return query, dest, nil
}

// copyRows is the general path: decode, transform, re-encode.
func copyRows(ctx context.Context, src Conn, table, query string, dst Conn, dest string,
	opt CopyOptions, st *CopyStats) error {
	// The read gets a context of its own, canceled before the Reader is
	// closed. Closing pgx (or MySQL) rows mid-result drains every remaining
	// row first, so when the destination fails part way through a big table
	// a plain Close would read the rest of it before the error came back;
	// canceling makes the source server stop the query instead. Errors are
	// still judged against ctx, so this cancel is never reported as the
	// user's.
	rctx, stopRead := context.WithCancel(ctx)
	rd, err := Read(rctx, src, query, opt.Args...)
	if err != nil {
		stopRead()
		return err
	}
	defer func() {
		stopRead()
		_ = rd.Close()
	}()

	setup, err := prepareDest(ctx, src, table, dst, dest, rd.Columns(), rd.DBTypes(), opt)
	if err != nil {
		return err
	}
	w, err := NewWriter(ctx, dst, dest, rd.Columns(),
		WriteOptions{Setup: setup, Truncate: opt.Truncate, BatchSize: opt.BatchSize})
	if err != nil {
		return err
	}
	defer w.Abort()

	for rd.Next() {
		row := rd.Row()
		if opt.Transform != nil {
			if row, err = opt.Transform(row); err != nil {
				return serr.Wrap(err, "op", "transform", "row", itoa(rd.Count()))
			}
			if row == nil {
				st.Skipped++
				continue
			}
		}
		if err = w.Write(row); err != nil {
			return err
		}
		if opt.Progress != nil && w.Count()%opt.ProgressEvery == 0 {
			opt.Progress(w.Count())
		}
	}
	if err = rd.Err(); err != nil {
		return err
	}
	st.Rows, err = w.Close()
	return err
}

// copyDirect streams Postgres COPY text from src to dst. The source runs on
// this goroutine; the destination's COPY FROM runs on the Writer's.
//
//	src conn.Raw ─► PgConn.CopyTo("COPY (query) TO STDOUT", rawSink)
//	                                                   │ Write(p)
//	                                                   ▼
//	                      Writer (BEGIN; CREATE?; TRUNCATE?) pgCopy.writeRaw ─► dst COPY FROM STDIN
//
// If the destination fails, rawSink's Write returns its error, which makes
// CopyTo stop reading the source. That error, not CopyTo's report of it, is
// the one returned.
func copyDirect(ctx context.Context, src Conn, table, query string, dst Conn, dest string,
	opt CopyOptions, st *CopyStats) error {
	// The column list (for the destination's COPY, and Create) comes from
	// describing the query: a LIMIT 0 run returns the columns and no rows.
	cols, types, err := describe(ctx, src, query)
	if err != nil {
		return err
	}
	setup, err := prepareDest(ctx, src, table, dst, dest, cols, types, opt)
	if err != nil {
		return err
	}
	w, err := NewWriter(ctx, dst, dest, cols, WriteOptions{Setup: setup, Truncate: opt.Truncate})
	if err != nil {
		return err
	}
	defer w.Abort()

	sink := &rawSink{w: w, pg: w.impl.(*pgCopy), opt: opt}
	conn, err := src.DB.Conn(ctx)
	if err != nil {
		return serr.Wrap(err, "conn", src.Name, "op", "checkout")
	}
	err = conn.Raw(func(dc any) error {
		pc, ok := dc.(*pgxstdlib.Conn)
		if !ok {
			return serr.New("etl: not a pgx connection")
		}
		_, err := pc.Conn().PgConn().CopyTo(ctx, sink, "COPY ("+query+") TO STDOUT")
		return err
	})
	if err != nil {
		// A COPY TO cut short leaves the protocol mid-stream; don't pool it.
		discard(conn)
		if sink.err != nil {
			return sink.err
		}
		return serr.Wrap(err, "conn", src.Name, "op", "copy out")
	}
	_ = conn.Close()
	st.Rows, err = w.Close()
	return err
}

// rawSink is the io.Writer the source's CopyTo writes into. COPY text is one
// row per line (a newline inside a value is escaped as \n), so counting
// newlines counts rows exactly — for Count and Progress.
type rawSink struct {
	w   *Writer
	pg  *pgCopy
	opt CopyOptions
	err error // the destination's failure, if that is what stopped the copy
}

func (s *rawSink) Write(p []byte) (int, error) {
	if err := s.pg.writeRaw(p); err != nil {
		s.err = s.w.fail(serr.Wrap(err, "conn", s.w.conn, "table", s.w.table))
		return 0, s.err
	}
	before := s.w.n
	s.w.n += int64(bytes.Count(p, []byte{'\n'}))
	if s.opt.Progress != nil && s.w.n/s.opt.ProgressEvery > before/s.opt.ProgressEvery {
		s.opt.Progress(s.w.n)
	}
	return len(p), nil
}

// describe returns a query's column names and driver type names without
// fetching its rows.
func describe(ctx context.Context, c Conn, query string) ([]string, []string, error) {
	rd, err := Read(ctx, c, "SELECT * FROM ("+query+") AS etl_src LIMIT 0")
	if err != nil {
		return nil, nil, err
	}
	defer rd.Close()
	return rd.Columns(), rd.DBTypes(), nil
}

// prepareDest returns the statements to run in the load's transaction
// before the first row: the CREATE TABLE when opt.Create asks for one and
// the engine can roll DDL back. On bytdb and MySQL, which cannot, the
// CREATE runs here, on its own, before the load starts — so there a failed
// copy can leave behind an empty new table, but never a partial one.
// (Truncate is added by the Writer.)
func prepareDest(ctx context.Context, src Conn, table string, dst Conn, dest string,
	cols, types []string, opt CopyOptions) ([]string, error) {
	if !opt.Create {
		return nil, nil
	}
	ddl, err := createDDL(ctx, src, table, opt.Query != "", dst, dest, cols, types)
	if err != nil {
		return nil, err
	}
	if dst.Engine.transactionalDDL() {
		return []string{ddl}, nil
	}
	if _, err = dst.DB.ExecContext(ctx, ddl); err != nil {
		return nil, serr.Wrap(err, "conn", dst.Name, "op", "create table", "stmt", clip(ddl))
	}
	return nil, nil
}

// createDDL is the CREATE TABLE IF NOT EXISTS for the destination.
//
//	src Postgres, dst Postgres, source is a table ─► from the source catalog:
//	    exact types (format_type keeps varchar(80), numeric(12,2), int[] …),
//	    NOT NULL, and the primary key
//	anything else ─► from the driver's column type names, mapped to the
//	    destination engine (pgTypeName when both are Postgres, else by
//	    family), plus the source table's primary key where it can be read
//
// The key is declared only when every one of its columns is being copied.
// bytdb requires one, so a copy into bytdb from a query, or from a table
// without a key, fails here with a hint rather than at the server.
func createDDL(ctx context.Context, src Conn, table string, fromQuery bool, dst Conn, dest string,
	cols, types []string) (string, error) {
	e := dst.Engine
	defs := make([]string, len(cols))
	var pk []string
	if src.Engine == Postgres && e == Postgres && !fromQuery {
		cat, err := pgCatalogColumns(ctx, src, table)
		if err != nil {
			return "", err
		}
		byName := map[string]pgColumn{}
		for _, c := range cat {
			byName[c.name] = c
		}
		for i, name := range cols {
			c, ok := byName[name]
			if !ok {
				return "", serr.New("etl: column not in source table", "table", table, "column", name)
			}
			defs[i] = e.QuoteIdent(name) + " " + c.typ
			if c.notNull {
				defs[i] += " NOT NULL"
			}
		}
		pk = pgPrimaryKey(cat, cols)
	} else {
		if !fromQuery {
			pk = sourceKey(ctx, src, table, cols)
		}
		for i, name := range cols {
			f := familyOf(types[i])
			typ := e.columnType(f)
			switch {
			case src.Engine == Postgres && e == Postgres:
				typ = pgTypeName(types[i])
			case e == MySQL && slices.Contains(pk, name) && (f == famText || f == famBytes):
				// MySQL cannot index a LONGTEXT/LONGBLOB without a prefix
				// length, so a text key gets a bounded type instead.
				typ = map[typeFamily]string{famText: "VARCHAR(255)", famBytes: "VARBINARY(255)"}[f]
			}
			defs[i] = e.QuoteIdent(name) + " " + typ
		}
	}
	if len(pk) > 0 {
		defs = append(defs, "PRIMARY KEY ("+e.quoteCols(pk)+")")
	} else if e == Bytdb {
		return "", serr.New("etl: bytdb tables need a primary key, and none could be carried over from the source",
			"table", dest, "hint", "create the destination table yourself (s.Exec), then copy without Create")
	}
	return "CREATE TABLE IF NOT EXISTS " + e.QuoteTable(dest) + " (\n\t" +
		strings.Join(defs, ",\n\t") + "\n)", nil
}

// sourceKey is the source table's primary key columns, in key order, if
// all of them are among those being copied — best effort: an engine or
// table whose key cannot be read yields nil, and the table is created
// without one.
func sourceKey(ctx context.Context, src Conn, table string, copied []string) []string {
	var (
		query string
		args  []any
	)
	schema, name := "", table
	if i := strings.LastIndexByte(table, '.'); i >= 0 && !strings.ContainsAny(table, "\"`") {
		schema, name = table[:i], table[i+1:]
	}
	switch src.Engine {
	case Postgres:
		cat, err := pgCatalogColumns(ctx, src, table)
		if err != nil {
			return nil
		}
		return pgPrimaryKey(cat, copied)
	case SQLite:
		query, args = "SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk", []any{name}
	case MySQL, Bytdb:
		// information_schema: MySQL's own, bytdb's virtual one
		query = `SELECT k.column_name FROM information_schema.table_constraints t
			JOIN information_schema.key_column_usage k
			  ON k.constraint_name = t.constraint_name AND k.table_schema = t.table_schema
			 AND k.table_name = t.table_name
			WHERE t.constraint_type = 'PRIMARY KEY' AND t.table_name = ` + src.Engine.placeholder(1)
		args = []any{name}
		switch {
		case schema != "":
			query += " AND t.table_schema = " + src.Engine.placeholder(2)
			args = append(args, schema)
		case src.Engine == MySQL:
			query += " AND t.table_schema = DATABASE()"
		}
		query += " ORDER BY k.ordinal_position"
	default:
		return nil
	}
	rows, err := src.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var key []string
	for rows.Next() {
		var col string
		if rows.Scan(&col) != nil {
			return nil
		}
		if !slices.Contains(copied, col) {
			return nil
		}
		key = append(key, col)
	}
	if rows.Err() != nil {
		return nil
	}
	return key
}

// pgTypeName turns a pgx-reported type name into one usable in DDL: "INT4"
// → "int4", array "_TEXT" → "text[]", and an unknown type (a user enum,
// reported as "") → text, which takes any value's text form.
func pgTypeName(t string) string {
	if t == "" {
		return "text"
	}
	t = strings.ToLower(t)
	if strings.HasPrefix(t, "_") {
		return t[1:] + "[]"
	}
	return t
}

type pgColumn struct {
	name    string
	typ     string // format_type: "character varying(80)", "integer[]"
	notNull bool
	pkPos   int // 1-based position in the primary key; 0 when not in it
}

// pgCatalogColumns reads a Postgres table's columns, in table order. A
// column's place in the primary key comes from unnest … WITH ORDINALITY
// rather than array_position(indkey::int2[], …): indkey is an int2vector,
// whose subscripts start at 0, so array_position reports the first key
// column as 0 — indistinguishable from "not in the key".
func pgCatalogColumns(ctx context.Context, c Conn, table string) ([]pgColumn, error) {
	rows, err := c.DB.QueryContext(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
		       coalesce((SELECT k.ord FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		                 WHERE k.attnum = a.attnum), 0)
		FROM pg_attribute a
		LEFT JOIN pg_index i ON i.indrelid = a.attrelid AND i.indisprimary
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, Postgres.QuoteTable(table))
	if err != nil {
		return nil, serr.Wrap(err, "conn", c.Name, "op", "read source columns", "table", table)
	}
	defer rows.Close()
	var out []pgColumn
	for rows.Next() {
		var col pgColumn
		if err = rows.Scan(&col.name, &col.typ, &col.notNull, &col.pkPos); err != nil {
			return nil, serr.Wrap(err, "conn", c.Name, "op", "read source columns")
		}
		out = append(out, col)
	}
	return out, rows.Err()
}

// pgPrimaryKey is the source's primary key in key order — or nil when one of
// its columns is not being copied, since a key over missing columns cannot
// be declared.
func pgPrimaryKey(cat []pgColumn, copied []string) []string {
	var key []pgColumn
	for _, c := range cat {
		if c.pkPos > 0 {
			if !slices.Contains(copied, c.name) {
				return nil
			}
			key = append(key, c)
		}
	}
	slices.SortFunc(key, func(a, b pgColumn) int { return a.pkPos - b.pkPos })
	names := make([]string, len(key))
	for i, c := range key {
		names[i] = c.name
	}
	return names
}
