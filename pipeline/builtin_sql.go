package pipeline

import (
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/serr"
)

// The database plugins: a query or a table as a source, a table as a
// sink, a statement as an action. "sql" rather than "pg": each works on
// every engine dbc connects to, taking its dialect from the connection;
// on Postgres the read and write are COPY, and a fragment that is exactly
// sql.read/sql.table → sql.write between two Postgres connections runs as
// one direct COPY (run.go, direct).

func init() {
	Register(Plugin{
		Name: "sql.read", Kind: KindSource, Label: "SQL query",
		Doc: "Runs a query on a connection and streams its rows, typed and uncapped (unlike s.Query). " +
			"Placeholders in the connection's style ($1 on Postgres and bytdb, ? on MySQL and SQLite) " +
			"take the args, one per line. ${param} in the query is spliced in as text. " +
			"A write with RETURNING is a source too: on Postgres its change commits only after the sinks have.",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection to read from."},
			{Name: "query", Type: FieldSQL, Required: true, Doc: "One statement that returns rows."},
			{Name: "args", Type: FieldText, Doc: "Bind values for the query's placeholders, one per line."},
		},
		New: func(cfg Config) (any, error) {
			return &sqlRead{conn: cfg.Str("conn", ""), query: strings.TrimSpace(cfg["query"]),
				args: anyStrings(cfg.Lines("args"))}, nil
		},
	})
	Register(Plugin{
		Name: "sql.table", Kind: KindSource, Label: "Table",
		Doc: "Streams a table's rows: every column, or the ones named, those a where clause keeps, " +
			"in an order if given. The query is built with the connection's quoting, so a mixed-case " +
			"or reserved-word name needs no quotes here.",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection to read from."},
			{Name: "table", Type: FieldTable, Required: true, Doc: "The table, schema-qualified if need be (sales.orders)."},
			{Name: "columns", Type: FieldColumns, Doc: "The columns to read, in this order; empty means all."},
			{Name: "where", Type: FieldSQL, Doc: "A condition on the rows, without the WHERE."},
			{Name: "order", Type: FieldString, Doc: "An ORDER BY list, without the ORDER BY (an order rules out the direct COPY path)."},
		},
		New: func(cfg Config) (any, error) {
			return &sqlRead{conn: cfg.Str("conn", ""), isTable: true, table: strings.TrimSpace(cfg["table"]),
				columns: cfg.List("columns"), where: strings.TrimSpace(cfg["where"]), order: strings.TrimSpace(cfg["order"])}, nil
		},
	})
	Register(Plugin{
		Name: "sql.write", Kind: KindSink, Label: "Table (load)",
		Doc: "Loads the rows into a table, in one transaction: on Postgres by COPY, elsewhere by batched INSERT. " +
			"Nothing is visible until the fragment ends, and a failed fragment leaves the table as it was. " +
			"Create makes the table from the rows' columns when it is missing; truncate empties it first, " +
			"in the same transaction. The rows' columns must match the table's by name. " +
			"Mode upsert (Postgres and SQLite) matches rows on key: a row whose key is in the table updates it, " +
			"any other is inserted, and when a key comes twice the last row wins. The key needs a unique " +
			"constraint on the table (a created table gets it as its primary key).",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection to load into."},
			{Name: "table", Type: FieldTable, Required: true, Doc: "The destination table."},
			{Name: "create", Type: FieldBool, Default: "false", Doc: "Create the table if it does not exist, from the incoming columns."},
			{Name: "truncate", Type: FieldBool, Default: "false", Doc: "Empty the table before the first row, in the load's transaction."},
			{Name: "mode", Type: FieldEnum, Default: "insert", Enum: []string{"insert", "upsert"},
				Doc: "insert adds every row; upsert updates the rows whose key is already there and inserts the rest (Postgres and SQLite)."},
			{Name: "key", Type: FieldColumns, Doc: "Primary key columns for a created table (bytdb needs one); with mode upsert, the columns a row is matched on."},
			{Name: "setup", Type: FieldSQL, Doc: "Statements to run before the first row, in the load's transaction (an index, a DELETE of the slice being reloaded)."},
			{Name: "batch", Type: FieldInt, Default: "500", Doc: "Rows per INSERT on engines loaded by INSERT (not Postgres)."},
		},
		New: func(cfg Config) (any, error) {
			create, _ := cfg.Bool("create")
			truncate, _ := cfg.Bool("truncate")
			batch, _ := cfg.Int("batch", 500)
			w := &sqlWrite{conn: cfg.Str("conn", ""), table: strings.TrimSpace(cfg["table"]), create: create,
				truncate: truncate, key: cfg.List("key"), setup: strings.TrimSpace(cfg["setup"]), batch: batch,
				upsert: strings.TrimSpace(cfg["mode"]) == "upsert"}
			if w.upsert && len(w.key) == 0 {
				return nil, serr.New("mode upsert needs key: the columns a row is matched on", "table", w.table)
			}
			return w, nil
		},
		Check: func(cfg Config) []string {
			if strings.TrimSpace(cfg["mode"]) == "upsert" && len(cfg.List("key")) == 0 {
				return []string{"key: mode upsert needs key, the columns a row is matched on"}
			}
			return nil
		},
	})
	Register(Plugin{
		Name: "sql.exec", Kind: KindAction, Label: "SQL statement",
		Doc: "Runs one or more statements on a connection, each committed as it runs, and publishes the rows " +
			"affected as ${frag.<fragment>.affected}. DDL is logged as a script's is.",
		Fields: []Field{
			{Name: "conn", Type: FieldConn, Required: true, Doc: "The connection to run on."},
			{Name: "sql", Type: FieldSQL, Required: true, Doc: "The statements, separated by semicolons."},
		},
		New: func(cfg Config) (any, error) {
			return &sqlExec{conn: cfg.Str("conn", ""), sql: strings.TrimSpace(cfg["sql"])}, nil
		},
	})
}

func anyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// engineNamed is the etl.Engine called name (Env.SourceEngine); 0 for
// none.
func engineNamed(name string) etl.Engine {
	for _, e := range []etl.Engine{etl.Postgres, etl.MySQL, etl.SQLite, etl.Bytdb} {
		if e.String() == name {
			return e
		}
	}
	return 0
}

// sqlRead is sql.read and sql.table: an etl.Reader paged into batches.
type sqlRead struct {
	conn  string
	query string
	args  []any
	// sql.table's pieces; the query is built at Open, once the engine's
	// quoting is known
	isTable bool
	table   string
	columns []string
	where   string
	order   string

	rd   *etl.Reader
	cols []Col
}

func (s *sqlRead) Open(e *Env) error {
	c, err := e.S.ETLConn(s.conn)
	if err != nil {
		return err
	}
	if s.isTable {
		s.query = tableQuery(c.Engine, s.table, s.columns, s.where, s.order)
	}
	rd, err := etl.Read(e.Ctx, c, s.query, s.args...)
	if err != nil {
		return err
	}
	s.rd = rd
	s.cols = ColsOf(rd.Columns(), rd.DBTypes())
	e.SourceEngine = c.Engine.String()
	return nil
}

// tableQuery is SELECT cols FROM table [WHERE …] [ORDER BY …], quoted for e.
func tableQuery(e etl.Engine, table string, cols []string, where, order string) string {
	sel := "*"
	if len(cols) > 0 {
		q := make([]string, len(cols))
		for i, c := range cols {
			q[i] = e.QuoteIdent(c)
		}
		sel = strings.Join(q, ", ")
	}
	q := "SELECT " + sel + " FROM " + e.QuoteTable(table)
	if where != "" {
		q += " WHERE " + where
	}
	if order != "" {
		q += " ORDER BY " + order
	}
	return q
}

// Cols is the query's columns, known after Open (for a sink no rows reached).
func (s *sqlRead) Cols() []Col { return s.cols }

func (s *sqlRead) Next(e *Env) (*Batch, error) {
	n := e.Batch
	if n <= 0 {
		n = DefaultBatch
	}
	rows := make([][]any, 0, n)
	for len(rows) < n && s.rd.Next() {
		rows = append(rows, s.rd.Row())
	}
	if err := s.rd.Err(); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// each batch gets its own column list: a transform may rename a
	// column in place, and the next batch must still carry the original
	return &Batch{Cols: slices.Clone(s.cols), Rows: rows}, nil
}

func (s *sqlRead) Close(ok bool) error {
	if s.rd == nil {
		return nil
	}
	if !ok {
		s.rd.Abort()
		return nil
	}
	return s.rd.Close()
}

func (s *sqlRead) directSource() (string, string, etl.CopyOptions, bool) {
	if s.isTable {
		return s.conn, s.table, etl.CopyOptions{Columns: s.columns, Where: s.where}, s.order == ""
	}
	return s.conn, "", etl.CopyOptions{Query: s.query}, len(s.args) == 0
}

// sqlWrite is sql.write: an etl.Writer, opened with the first batch's columns.
type sqlWrite struct {
	conn, table      string
	create, truncate bool
	key              []string
	setup            string
	batch            int
	// upsert is mode upsert: rows matched on key update the table's row
	// (etl.WriteOptions.Upsert), the rest are inserted
	upsert bool

	w     *etl.Writer
	ncols int
}

func (s *sqlWrite) Open(e *Env, cols []Col) error {
	c, err := e.S.ETLConn(s.conn)
	if err != nil {
		return err
	}
	names := make([]string, len(cols))
	types := make([]string, len(cols))
	for i, col := range cols {
		names[i], types[i] = col.Name, col.DBType
	}
	var setup []string
	for _, st := range sqlsplit.Split(s.setup) {
		setup = append(setup, st.Text)
	}
	if s.create {
		ddl, err := etl.CreateStmt(engineNamed(e.SourceEngine), c.Engine, s.table, names, types, s.key)
		if err != nil {
			return err
		}
		if c.Engine.TransactionalDDL() {
			setup = append([]string{ddl}, setup...)
		} else {
			// bytdb and MySQL cannot roll DDL back: the CREATE runs on its
			// own first, so a failed load can leave an empty table but
			// never a partial one (as etl.Copy does)
			if c.Trace != nil {
				c.Trace(ddl)
			}
			if _, err = c.DB.ExecContext(e.Ctx, ddl); err != nil {
				return serr.Wrap(err, "op", "create table", "table", s.table)
			}
		}
	}
	opt := etl.WriteOptions{Setup: setup, Truncate: s.truncate, BatchSize: s.batch}
	if s.upsert {
		opt.Upsert = s.key
	}
	w, err := etl.NewWriter(e.Ctx, c, s.table, names, opt)
	if err != nil {
		return err
	}
	s.w, s.ncols = w, len(cols)
	return nil
}

func (s *sqlWrite) Write(e *Env, b *Batch) error {
	if len(b.Cols) != s.ncols {
		return serr.New("the columns changed between batches", "table", s.table,
			"opened_with", strconv.Itoa(s.ncols), "now", strconv.Itoa(len(b.Cols)))
	}
	for _, row := range b.Rows {
		if err := s.w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlWrite) Commit(*Env) (Stats, error) {
	n, err := s.w.Close()
	if err != nil {
		return Stats{}, err
	}
	return Stats{Rows: n, Vars: map[string]string{"rows": strconv.FormatInt(n, 10)}}, nil
}

func (s *sqlWrite) Abort() error {
	if s.w == nil {
		return nil
	}
	return s.w.Abort()
}

// directSink is what the direct COPY path needs of the sink, and whether
// the sink allows it at all: a COPY straight into the table cannot run
// setup statements, declare a key, or upsert (its rows must land in the
// staging table and be merged — etl.Writer's work, on the row path).
func (s *sqlWrite) directSink() (string, string, bool, bool, int, bool) {
	return s.conn, s.table, s.create, s.truncate, s.batch, s.setup == "" && len(s.key) == 0 && !s.upsert
}

// sqlExec is sql.exec.
type sqlExec struct {
	conn, sql string
}

func (a *sqlExec) Run(e *Env) (Stats, error) {
	var total int64
	for _, st := range sqlsplit.Split(a.sql) {
		n, err := e.S.Exec(a.conn, st.Text)
		if err != nil {
			return Stats{}, err
		}
		// a DDL statement's count is noise (SQLite reports the last
		// change count after a CREATE), so only data statements add up
		if !sqlsplit.IsDDL(st.Text) {
			total += n
		}
	}
	return Stats{Rows: total, Vars: map[string]string{"affected": strconv.FormatInt(total, 10)}}, nil
}
