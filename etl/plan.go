package etl

import (
	"context"

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

// planCountWait bounds the dry run's row count: a count(*) of a big table
// can take minutes, which is the copy's cost, not the plan's. Past it the
// plan says the rows were not counted.
var planCountWait = 30 * time.Second

// CopyPlan is a copy's dry run.
type CopyPlan struct {
	From, To  string // "conn:table", as CopyStats names them
	Query     string // the SELECT the source would run
	Direct    bool   // Postgres to Postgres: COPY to COPY
	Columns   []string
	Types     []string // the source's type names, one a column
	Rows      int64    // -1 when not counted (RowsNote says why)
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
	}

	if src.Engine == Postgres {
		p.Columns, p.Types, err = describe(ctx, src, query)
	} else {
		p.Columns, p.Types, err = probeColumns(ctx, src, query, opt.Args)
	}
	if err != nil {
		return CopyPlan{}, serr.Wrap(err, "from", p.From)
	}

	cctx, cancel := context.WithTimeout(ctx, planCountWait)
	err = src.DB.QueryRowContext(cctx, "SELECT count(*) FROM ("+query+") dbc_plan", opt.Args...).Scan(&p.Rows)
	cancel()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return CopyPlan{}, canceled(ctx, err)
	case cctx.Err() != nil:
		p.Rows, p.RowsNote = -1, "not counted: the count ran past "+planCountWait.String()
	default:
		p.Rows, p.RowsNote = -1, "not counted: "+err.Error()
	}

	if _, err = dst.DB.ExecContext(ctx, "SELECT 1 FROM "+dst.Engine.QuoteTable(dest)+" WHERE 1 = 0"); err == nil {
		p.DestThere = true
	} else if ctx.Err() != nil {
		return CopyPlan{}, canceled(ctx, err)
	}
	switch {
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
	fmt.Fprintf(&b, "  columns:  %s\n", strings.Join(cols, ", "))
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
	}
	if p.Truncate != "" {
		fmt.Fprintf(&b, "  first:    %s (in the load's transaction)\n", p.Truncate)
	}
	if p.Problem != "" {
		fmt.Fprintf(&b, "  problem:  %s\n", p.Problem)
	}
	return b.String()
}
