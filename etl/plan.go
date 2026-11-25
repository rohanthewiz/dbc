package etl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// What a copy would do, without doing it (N-297): `dbc copy --dry-run`.
//
// PlanCopy reads what Copy would read and decides what Copy would decide,
// writing nothing on either side:
//
//	the source SELECT ──► its columns and types (Postgres: Parse, as Copy
//	                      describes; else the query wrapped WHERE 1 = 0)
//	                 └──► its rows counted, within planCountWait
//	the destination  ──► is it there? (a probe)
//	                       no, Create ──► the CREATE TABLE Copy would run
//	                       no         ──► the copy would fail: said so
//	                 └──► Truncate ──► the statement that would empty it
//	the path         ──► direct COPY (Postgres to Postgres) or row by row
//
// The CREATE comes from createDDL and the truncate from loadSetup, the
// functions Copy itself uses, so the plan cannot drift from the copy.
//
// A Query that writes (DELETE … RETURNING: a move) is never run by the
// plan, since running it is the change. Its rows are not counted, as a
// count would run it. Its columns come from Postgres's Parse, which runs
// nothing. Off Postgres there is no such describe: wrapping the write in a
// subquery is a syntax error on every other engine. So its columns, and
// a CREATE built from them, are left unknown, and the plan says why.

// planCountWait bounds the dry run's row count: a count(*) of a big table
// can take minutes, which is the copy's cost, not the plan's. Past it the
// plan says the rows were not counted.
var planCountWait = 30 * time.Second

// CopyPlan is a copy's dry run.
type CopyPlan struct {
	From, To string // "conn:table", as CopyStats names them
	Query    string // the SELECT the source would run
	Direct   bool   // Postgres to Postgres: COPY to COPY
	Columns  []string
	Types    []string // the source's type names, one a column
	// ColumnsNote says why Columns is empty: a write off Postgres, which
	// only running it would describe
	ColumnsNote string
	// Writes is a Query that changes rows (… RETURNING), so the copy
	// moves them rather than copying them
	Writes    bool
	Rows      int64 // -1 when not counted (RowsNote says why)
	RowsNote  string
	DestThere bool
	Create    string // the CREATE TABLE that would run, "" for none
	Truncate  string // the statement that would empty the destination, "" for none
	Problem   string // why the real copy would fail before a row, "" when it would not
}

// PlanCopy is Copy's dry run: what Copy(ctx, src, table, dst, opt) would
// read, create, empty and load, with nothing written.
func PlanCopy(ctx context.Context, src Conn, table string, dst Conn, opt CopyOptions) (CopyPlan, error) {
	query, dest, err := copySource(src.Engine, table, opt)
	if err != nil {
		return CopyPlan{}, err
	}
	p := CopyPlan{From: src.Name + ":" + table, To: dst.Name + ":" + dest, Query: query, Rows: -1,
		Direct: src.Engine == Postgres && dst.Engine == Postgres && opt.Transform == nil && len(opt.Args) == 0}
	if opt.Query != "" {
		p.From = src.Name + ":query"
		p.Writes = writes(src.Engine, query)
	}

	switch {
	case src.Engine == Postgres:
		p.Columns, p.Types, err = describe(ctx, src, query)
	case p.Writes:
		p.ColumnsNote = "not known: the query writes, and off Postgres only running it shows its columns"
	default:
		p.Columns, p.Types, err = probeColumns(ctx, src, query, opt.Args)
	}
	if err != nil {
		return CopyPlan{}, serr.Wrap(err, "from", p.From)
	}

	if p.Writes {
		p.RowsNote = "not counted: the query writes, and counting its rows would run it"
	} else if p.Rows, err = countSource(ctx, src, query, opt.Args); err != nil {
		if ctx.Err() != nil {
			return CopyPlan{}, canceled(ctx, err)
		}
		p.RowsNote = "not counted: " + err.Error()
	}

	if _, err = dst.DB.ExecContext(ctx, "SELECT 1 FROM "+dst.Engine.QuoteTable(dest)+" WHERE 1 = 0"); err == nil {
		p.DestThere = true
	} else if ctx.Err() != nil {
		return CopyPlan{}, canceled(ctx, err)
	}
	switch {
	case !p.DestThere && opt.Create && len(p.Columns) == 0 && p.ColumnsNote != "":
		// the copy would make it from the columns the write returns, which
		// the plan cannot know without running it: no CREATE to show, and
		// no problem either. String says the table would be made.
	case !p.DestThere && opt.Create:
		if p.Create, err = createDDL(ctx, src, table, opt.Query != "", dst, dest, p.Columns, p.Types); err != nil {
			p.Problem = "the destination table could not be planned: " + err.Error()
		}
	case !p.DestThere:
		p.Problem = "the destination table " + dest + " does not exist (--create makes it)"
	}
	if opt.Truncate && p.DestThere {
		same := src.Engine == Postgres && dst.Engine == Postgres && samePGDatabase(ctx, src, dst)
		setup := loadSetup(dst, dest, WriteOptions{Truncate: true, sameDB: same, fromQuery: opt.Query != ""})
		if len(setup) > 0 {
			p.Truncate = setup[len(setup)-1]
		}
	}
	return p, nil
}

// writes says a copy's source query changes rows. copySource has refused
// a write without RETURNING, so a write verb here is one that returns its
// rows: a move. Running it, to describe or to count it, would be the move.
func writes(e Engine, query string) bool {
	return writeVerbs[e.Dialect().Verbs(query).Main]
}

// CountCopy is the number of rows Copy(ctx, src, table, dst, opt) would
// read, counted as the dry run counts them (countSource, within
// planCountWait). `dbc copy` runs it beside the copy for its progress
// line's percentage (N-336). A Query that writes is never counted:
// counting it would run it, which is the move itself.
func CountCopy(ctx context.Context, src Conn, table string, opt CopyOptions) (int64, error) {
	query, _, err := copySource(src.Engine, table, opt)
	if err != nil {
		return -1, err
	}
	if opt.Query != "" && writes(src.Engine, query) {
		return -1, errors.New("the query writes, and counting its rows would run it")
	}
	return countSource(ctx, src, query, opt.Args)
}

// countSource is the number of rows query returns, counted within
// planCountWait: past it, -1 and an error that says so. A count of a big
// table costs a scan, which is the copy's cost, not the plan's. When ctx
// itself ends, its error comes back as is, for the caller to call a
// cancel.
func countSource(ctx context.Context, src Conn, query string, args []any) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, planCountWait)
	defer cancel()
	var n int64
	err := src.DB.QueryRowContext(cctx, "SELECT count(*) FROM ("+query+") dbc_plan", args...).Scan(&n)
	switch {
	case err == nil:
		return n, nil
	case ctx.Err() == nil && cctx.Err() != nil:
		return -1, errors.New("the count ran past " + planCountWait.String())
	}
	return -1, err
}

// probeColumns is a query's columns and types off Postgres: the query run
// wrapped so it returns no rows. Types are the driver's names, as a
// Reader reports them (what createDDL maps).
func probeColumns(ctx context.Context, c Conn, query string, args []any) ([]string, []string, error) {
	rows, err := c.DB.QueryContext(ctx, "SELECT * FROM ("+query+") dbc_plan WHERE 1 = 0", args...)
	if err != nil {
		return nil, nil, serr.Wrap(canceled(ctx, err), "conn", c.Name, "op", "describe", "query", clip(query))
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, serr.Wrap(err, "conn", c.Name, "op", "describe")
	}
	cols, types := make([]string, len(cts)), make([]string, len(cts))
	for i, ct := range cts {
		cols[i], types[i] = ct.Name(), strings.ToUpper(ct.DatabaseTypeName())
	}
	return cols, types, rows.Err()
}

// String is the plan as `dbc copy --dry-run` prints it.
func (p CopyPlan) String() string {
	var b strings.Builder
	path := "row by row"
	if p.Direct {
		path = "direct COPY, Postgres to Postgres"
	}
	fmt.Fprintf(&b, "dry run: %s → %s (%s); nothing was written\n", p.From, p.To, path)
	fmt.Fprintf(&b, "  source:   %s\n", p.Query)
	cols := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		cols[i] = c
		if i < len(p.Types) && p.Types[i] != "" {
			cols[i] += " " + p.Types[i]
		}
	}
	if len(cols) == 0 && p.ColumnsNote != "" {
		fmt.Fprintf(&b, "  columns:  %s\n", p.ColumnsNote)
	} else {
		fmt.Fprintf(&b, "  columns:  %s\n", strings.Join(cols, ", "))
	}
	if p.Writes {
		fmt.Fprintf(&b, "  moves:    the query writes, so the copy moves its rows; the dry run did not run it\n")
	}
	if p.Rows >= 0 {
		fmt.Fprintf(&b, "  rows:     %d\n", p.Rows)
	} else {
		fmt.Fprintf(&b, "  rows:     %s\n", p.RowsNote)
	}
	switch {
	case p.DestThere:
		fmt.Fprintf(&b, "  dest:     exists\n")
	case p.Create != "":
		fmt.Fprintf(&b, "  dest:     missing; would be created:\n            %s\n", strings.ReplaceAll(p.Create, "\n", "\n            "))
	case p.Problem == "":
		// missing, to be created, from columns the plan could not learn
		// (a write off Postgres: see ColumnsNote)
		fmt.Fprintf(&b, "  dest:     missing; would be created from the columns the query returns\n")
	}
	if p.Truncate != "" {
		fmt.Fprintf(&b, "  first:    %s (in the load's transaction)\n", p.Truncate)
	}
	if p.Problem != "" {
		fmt.Fprintf(&b, "  problem:  %s\n", p.Problem)
	}
	return b.String()
}
