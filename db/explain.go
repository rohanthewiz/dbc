package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Explaining a statement: which EXPLAIN each engine gets, and how "analyze"
// — actually running the statement to measure it — is kept from doing harm.
//
//	engine    estimate                         analyze (a read)                   analyze (a write)
//	───────   ──────────────────────────────   ────────────────────────────────   ───────────────────────────
//	postgres  EXPLAIN (FORMAT JSON)            + ANALYZE, BUFFERS                 same, inside BEGIN…ROLLBACK
//	                                                                              (or a SAVEPOINT in an open tx)
//	mysql     EXPLAIN FORMAT=TREE              EXPLAIN ANALYZE                    refused: estimate instead
//	          ↳ classic EXPLAIN table          ↳ MariaDB: ANALYZE (table form)
//	sqlite    EXPLAIN QUERY PLAN               the plan, then the statement run   refused: estimate instead
//	bytdb     EXPLAIN (Postgres-style text)    and timed as a whole               refused: estimate instead
//
// WRITES ARE NEVER CHANGED BY EXPLAINING THEM. EXPLAIN ANALYZE executes the
// statement: on an UPDATE it updates. Postgres has transactional everything,
// so an analyzed write there runs inside a transaction dbc rolls back — the
// user gets the real timings and the table is as it was. Elsewhere there is
// no such guarantee (MySQL commits DDL implicitly; SQLite and bytdb cannot
// time steps anyway), so the write gets the estimated plan and a note saying
// why, rather than a measurement bought with the user's data.
//
// EXPLAIN RUNS ON THE SESSION. search_path, temp tables, SET enable_seqscan
// and an open transaction's uncommitted rows all change the plan, so the plan
// worth seeing is the one the user's next Ctrl+R would get — on their pinned
// session, not a fresh pooled connection.

// ExplainOptions choose what Explain does.
type ExplainOptions struct {
	// Analyze runs the statement to measure it (see the table above).
	Analyze bool
}

// Explain describes how the session's database runs stmt. A stmt that is
// itself an EXPLAIN is unwrapped first (and its ANALYZE honored), so a user's
// own "EXPLAIN ANALYZE SELECT …" works too.
func (s *Session) Explain(ctx context.Context, stmt string, opt ExplainOptions) (*explain.Plan, error) {
	stmt = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
	if inner, analyze, ok := explain.Strip(stmt); ok {
		stmt, opt.Analyze = inner, opt.Analyze || analyze
	}
	if stmt == "" {
		return nil, serr.New("nothing to explain")
	}
	cc, _ := s.m.cfg.ConnByName(s.name)
	engine := explain.Engine(cc.Driver)
	read := isRead(stmt)

	var notes []string
	if opt.Analyze && !read && engine != explain.Postgres {
		opt.Analyze = false
		notes = append(notes, "not analyzed: that would run the write for real on "+engine+
			" — this is the planner's estimate instead")
	}

	var p *explain.Plan
	var err error
	switch engine {
	case explain.Postgres:
		p, err = s.explainPostgres(ctx, stmt, opt, read)
	case explain.MySQL:
		p, err = s.explainMySQL(ctx, stmt, opt)
	case explain.SQLite:
		p, err = s.explainSQLite(ctx, stmt)
	case explain.Bytdb:
		p, err = s.explainBytdb(ctx, stmt)
	default:
		return nil, serr.New("EXPLAIN is not supported for this driver", "driver", cc.Driver)
	}
	if err != nil {
		return nil, err
	}
	if opt.Analyze && (engine == explain.SQLite || engine == explain.Bytdb) {
		if err = s.measure(ctx, stmt, p); err != nil {
			return nil, err
		}
	}
	if engine == explain.SQLite || engine == explain.Bytdb {
		// no row estimates at all: the table sizes are what tell a harmless
		// full scan of a lookup table from a painful one
		s.tableSizes(ctx, stmt, p)
	}
	p.Statement, p.Conn = stmt, s.name
	p.Notes = append(notes, p.Notes...)
	p.ResolveAliases(stmt)
	p.Finalize()
	return p, nil
}

// Explain opens a session on the named connection, explains stmt on it and
// closes it — the headless path, which has no session to reuse.
func (m *Manager) Explain(ctx context.Context, name, stmt string, opt ExplainOptions) (*explain.Plan, error) {
	sess, err := m.Session(ctx, name)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return sess.Explain(ctx, stmt, opt)
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// explainPostgres asks for the JSON format, the richest one: exact numbers,
// every property named, nothing to parse out of prose.
func (s *Session) explainPostgres(ctx context.Context, stmt string, opt ExplainOptions, read bool) (*explain.Plan, error) {
	opts := []string{"FORMAT JSON"}
	var notes []string
	if hasParams(stmt) {
		// $1 has no value to plan with. Postgres 16 can plan it generically,
		// as a prepared statement would be; analyzing needs real values.
		if opt.Analyze {
			return nil, serr.New("cannot analyze a statement with $1-style parameters — put values in first")
		}
		opts = append(opts, "GENERIC_PLAN")
		notes = append(notes, "generic plan: parameters were left unbound, as a prepared statement plans them (Postgres 16+)")
	}
	if opt.Analyze {
		opts = append(opts, "ANALYZE", "BUFFERS")
	}
	cmd := "EXPLAIN (" + strings.Join(opts, ", ") + ") "

	run := func() (*explain.Plan, error) {
		out, err := s.pgSimple(ctx, cmd+stmt)
		if err != nil {
			return nil, err
		}
		return explain.ParsePostgresJSON([]byte(out))
	}
	var p *explain.Plan
	var err error
	if opt.Analyze && !read {
		var note string
		p, note, err = s.rolledBack(ctx, run)
		notes = append(notes, note)
	} else {
		p, err = run()
	}
	if err != nil {
		return nil, err
	}
	p.Command = strings.TrimSpace(cmd) + " …"
	p.Notes = append(notes, p.Notes...)
	return p, nil
}

// savepoint is the name dbc's rollback wrapper uses inside a transaction the
// user already has open.
const savepoint = "dbc_explain"

// rolledBack runs f — an EXPLAIN ANALYZE of a write — so that nothing it
// changes survives, whatever state the session is in:
//
//	no transaction open   BEGIN ─ f ─ ROLLBACK
//	a transaction open    SAVEPOINT ─ f ─ ROLLBACK TO SAVEPOINT ─ RELEASE
//	a failed transaction  refuse: nothing can run until the user rolls it back
//
// The savepoint case is why this asks the connection rather than assuming:
// a plain BEGIN inside the user's transaction is only a warning on Postgres,
// and the ROLLBACK after it would then throw away the user's own uncommitted
// work along with dbc's.
//
// The undo runs under its own short-lived context: after Ctrl+K the run's
// context is dead, which is exactly when the undo matters most. If even that
// fails, the session is left marked stateful, so a later lost connection is
// reported rather than papered over.
func (s *Session) rolledBack(ctx context.Context, f func() (*explain.Plan, error)) (*explain.Plan, string, error) {
	status := s.pgTxStatus()
	var begin, undo []string
	switch status {
	case 'I':
		begin, undo = []string{"BEGIN"}, []string{"ROLLBACK"}
	case 'T':
		begin = []string{"SAVEPOINT " + savepoint}
		undo = []string{"ROLLBACK TO SAVEPOINT " + savepoint, "RELEASE SAVEPOINT " + savepoint}
	case 'E':
		return nil, "", serr.New("the session's transaction has failed — ROLLBACK before explaining a write")
	default:
		return nil, "", serr.New("could not tell whether the session is in a transaction, so the write was not run")
	}
	for _, q := range begin {
		if _, err := s.conn.ExecContext(ctx, q); err != nil {
			return nil, "", wrapRunErr(ctx, err, s.name, "op", "explain begin")
		}
	}
	p, err := f()
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, q := range undo {
		if _, uerr := s.conn.ExecContext(uctx, q); uerr != nil {
			s.stateful = true
			return nil, "", serr.Wrap(uerr, "conn", s.name, "op", "explain rollback",
				"detail", "the analyzed write may not have been undone — check before COMMIT")
		}
	}
	note := "analyzed inside a transaction that was rolled back: no rows were changed " +
		"(sequences still advanced, and row locks were held while it ran)"
	if status == 'T' {
		note = "analyzed inside a savepoint that was rolled back: your open transaction is as it was " +
			"(sequences still advanced)"
	}
	return p, note, err
}

// pgSimple sends q over Postgres's simple query protocol, straight through
// pgconn, and returns the first column of its rows joined by newlines — for
// EXPLAIN, the plan document as the server wrote it.
//
// Not database/sql, for two reasons. A statement with $1 in it (explained as
// a generic plan) would be refused there before it was sent: the extended
// protocol insists on a value per parameter. And database/sql's pgx adapter
// decodes a json column into Go maps; the simple protocol hands back the
// text itself, which is exactly what the parser wants.
func (s *Session) pgSimple(ctx context.Context, q string) (string, error) {
	var out []string
	var qerr error
	err := s.conn.Raw(func(dc any) error {
		pc, ok := dc.(*pgxstdlib.Conn)
		if !ok {
			qerr = serr.New("not a Postgres connection")
			return nil
		}
		results, err := pc.Conn().PgConn().Exec(ctx, q).ReadAll()
		if err != nil {
			qerr = err
			return nil
		}
		for _, r := range results {
			for _, row := range r.Rows {
				if len(row) > 0 {
					out = append(out, string(row[0]))
				}
			}
		}
		return nil
	})
	if err == nil {
		err = qerr
	}
	if err != nil {
		return "", wrapRunErr(ctx, err, s.name, "op", "explain")
	}
	if len(out) == 0 {
		return "", serr.New("EXPLAIN returned nothing", "conn", s.name)
	}
	return strings.Join(out, "\n"), nil
}

// pgTxStatus asks pgx for the connection's transaction status byte: 'I'
// idle, 'T' in a transaction, 'E' in a failed one; 0 when the connection is
// not a pgx one.
func (s *Session) pgTxStatus() byte {
	var st byte
	_ = s.conn.Raw(func(dc any) error {
		if pc, ok := dc.(*pgxstdlib.Conn); ok {
			st = pc.Conn().PgConn().TxStatus()
		}
		return nil
	})
	return st
}

// hasParams reports whether stmt holds a bind parameter ($1, ?, :name).
func hasParams(stmt string) bool {
	for _, t := range sqlsplit.Lex(stmt) {
		if t.Kind == sqlsplit.TokParam {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

// explainMySQL tries the tree format first — it nests like every other
// engine's plan and carries costs — and falls back to the classic table,
// which every MySQL and MariaDB speaks. A statement MySQL rejects outright
// (a syntax error, a missing table) fails the same way in both, so the
// fallback's error is the one reported.
func (s *Session) explainMySQL(ctx context.Context, stmt string, opt ExplainOptions) (*explain.Plan, error) {
	tree := func(cmd string) (*explain.Plan, error) {
		_, rows, err := s.queryText(ctx, cmd+stmt)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 || len(rows[0]) == 0 {
			return nil, serr.New("EXPLAIN returned nothing")
		}
		p, err := explain.ParseMySQLTree(rows[0][0])
		if p != nil {
			p.Command = strings.TrimSpace(cmd) + " …"
		}
		return p, err
	}
	table := func(cmd string) (*explain.Plan, error) {
		cols, rows, err := s.queryText(ctx, cmd+stmt)
		if err != nil {
			return nil, err
		}
		p, err := explain.ParseMySQLTable(cols, rows)
		if p != nil {
			p.Command = strings.TrimSpace(cmd) + " …"
		}
		return p, err
	}

	if opt.Analyze {
		if p, err := tree("EXPLAIN ANALYZE "); err == nil {
			return p, nil
		} else if errors.Is(err, ErrCanceled) {
			return nil, err
		}
		if p, err := table("ANALYZE "); err == nil { // MariaDB
			return p, nil
		} else if errors.Is(err, ErrCanceled) {
			return nil, err
		}
		p, err := s.explainMySQL(ctx, stmt, ExplainOptions{})
		if p != nil {
			p.Notes = append(p.Notes, "this server cannot EXPLAIN ANALYZE (MySQL 8.0.18+ can) — showing the estimate")
		}
		return p, err
	}
	if p, err := tree("EXPLAIN FORMAT=TREE "); err == nil {
		return p, nil
	} else if errors.Is(err, ErrCanceled) {
		return nil, err
	}
	return table("EXPLAIN ")
}

// ---------------------------------------------------------------------------
// SQLite and bytdb
// ---------------------------------------------------------------------------

func (s *Session) explainSQLite(ctx context.Context, stmt string) (*explain.Plan, error) {
	cols, rows, err := s.queryText(ctx, "EXPLAIN QUERY PLAN "+stmt)
	if err != nil {
		return nil, err
	}
	p, err := explain.ParseSQLite(cols, rows)
	if err != nil {
		return nil, err
	}
	p.Command = "EXPLAIN QUERY PLAN …"
	return p, nil
}

func (s *Session) explainBytdb(ctx context.Context, stmt string) (*explain.Plan, error) {
	_, rows, err := s.queryText(ctx, "EXPLAIN "+stmt)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			lines = append(lines, r[0])
		}
	}
	p, err := explain.ParsePostgresText(lines, explain.Bytdb)
	if err != nil {
		return nil, err
	}
	p.Command = "EXPLAIN …"
	return p, nil
}

// measure runs a read to completion and times it, for the engines that
// cannot time their steps. Every row is fetched (so the time is the whole
// statement's, not the first row's) and counted, but none is kept: this is a
// stopwatch, not a query, and max_rows does not apply.
func (s *Session) measure(ctx context.Context, stmt string, p *explain.Plan) error {
	if !isRead(stmt) {
		return nil
	}
	start := time.Now()
	rows, err := s.conn.QueryContext(ctx, stmt)
	if err != nil {
		return wrapRunErr(ctx, err, s.name, "op", "explain measure")
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err = rows.Err(); err != nil {
		return wrapRunErr(ctx, err, s.name, "op", "explain measure")
	}
	p.Measured = true
	p.ExecutionMs = float64(time.Since(start).Microseconds()) / 1000
	p.ResultRows = float64(n)
	p.Notes = append(p.Notes, fmt.Sprintf("the statement was run and timed as a whole — %s returned %s; "+
		"%s cannot time its steps one by one", explain.FmtMs(p.ExecutionMs), plural(n, "row"), p.Engine))
	return nil
}

// Table-size lookups are bounded: each count gets tableSizeTimeout, and at
// most maxTableSizes tables are counted, so a plan over a huge table waits
// a fraction of a second for its size rather than for a full count.
const (
	tableSizeTimeout = 300 * time.Millisecond
	maxTableSizes    = 8
)

// tableSizes counts the rows of each table the plan reads, for engines whose
// plans carry no row numbers at all. A count that fails or times out is
// simply left out; the rules then say less, never something wrong.
func (s *Session) tableSizes(ctx context.Context, stmt string, p *explain.Plan) {
	aliases := explain.Aliases(stmt)
	sizes := map[string]float64{}
	for _, n := range p.Nodes() {
		if n.Kind != explain.KindScan && n.Kind != explain.KindIndex {
			continue
		}
		table := n.Relation
		if table == "" {
			table = aliases[n.Alias]
		}
		if table == "" || strings.HasPrefix(table, "<") || strings.HasPrefix(table, `"*`) {
			continue
		}
		size, seen := sizes[table]
		if !seen {
			if len(sizes) >= maxTableSizes {
				continue
			}
			size = -1
			tctx, cancel := context.WithTimeout(ctx, tableSizeTimeout)
			_, rows, err := s.queryText(tctx, "SELECT count(*) FROM "+quoteIdent(table))
			cancel()
			if err == nil && len(rows) == 1 && len(rows[0]) == 1 {
				fmt.Sscan(rows[0][0], &size)
			}
			sizes[table] = size
		}
		if size >= 0 {
			n.TableRows = size
			n.Relation = table
		}
	}
}

// quoteIdent quotes a possibly schema-qualified name for SQLite and bytdb,
// both of which take standard double quotes.
func quoteIdent(name string) string {
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(strings.Trim(p, `"`), `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// queryText runs q on the session and returns every value as text. EXPLAIN
// output is small (a plan, not a table), so there is no row cap; and it
// bypasses Run's result building because pgx decodes a json column into Go
// maps, which the renderer would print as "map[…]" — here it is turned back
// into the JSON it was.
func (s *Session) queryText(ctx context.Context, q string) ([]string, [][]string, error) {
	rows, err := s.conn.QueryContext(ctx, q)
	if err != nil {
		return nil, nil, wrapRunErr(ctx, err, s.name, "op", "explain")
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, nil, wrapRunErr(ctx, err, s.name, "op", "explain columns")
	}
	var out [][]string
	holders := make([]any, len(cols))
	for i := range holders {
		holders[i] = new(any)
	}
	for rows.Next() {
		if err = rows.Scan(holders...); err != nil {
			return nil, nil, wrapRunErr(ctx, err, s.name, "op", "explain scan")
		}
		row := make([]string, len(cols))
		for i, h := range holders {
			switch v := (*(h.(*any))).(type) {
			case nil:
				row[i] = "NULL"
			case string:
				row[i] = v
			case []byte:
				row[i] = string(v)
			case map[string]any, []any:
				b, _ := json.Marshal(v)
				row[i] = string(b)
			default:
				row[i] = renderVal(v)
			}
		}
		out = append(out, row)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, wrapRunErr(ctx, err, s.name, "op", "explain")
	}
	return cols, out, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
