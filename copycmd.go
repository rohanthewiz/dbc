package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/rohanthewiz/logger"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/dbc/sdb"
)

// The copy subcommand: etl.Copy from the shell, so a cron job or a pipeline
// can move a table between connections without writing a script.
//
//	dbc copy --from prod --to local public.orders                 into an existing table
//	dbc copy --from prod --to local --create public.orders        … made when missing
//	dbc copy --from prod --to local --truncate public.orders      … emptied first
//	dbc copy --from prod --to local public.orders archive.orders  … under another name
//	dbc copy --from prod --to local --where "created_at >= now() - interval '1 day'" orders
//	dbc copy --from prod --to local --columns id,email,created_at users      a column subset
//	dbc copy --from prod --to local --create --query "SELECT …" order_totals  a query's rows
//	dbc copy --from prod --to mysql --batch 2000 orders                       bigger INSERTs
//	dbc copy --from prod --to local --create --tables customers,orders,items  several tables
//
// It is exactly the one-call script it replaces —
//
//	func Run(s *sdb.S) error { _, err := s.Copy("prod", "local", "orders", sdb.CopyOpts{…}); return err }
//
// — and runs through the same sdb.S.Copy, so the two cannot drift apart: the
// direct Postgres COPY path, the one-transaction load (Create and Truncate
// inside it where the engine allows), and the rollback on failure or Ctrl+C
// are etl's, unchanged. See package etl for those guarantees.
//
// The destination table is a second argument rather than a flag: --to
// already names the destination connection, and `--to local --to-table x`
// reads worse than the two names side by side, source then destination.
//
// Output: the one-line summary (CopyStats.String) on stdout. A running row
// count goes to stderr, and only when stderr is a terminal — someone is
// watching — so a cron job's mail holds just the summary.
//
// Exit status: 0 copied, 1 the copy failed (the destination is as it was),
// 2 bad usage, 130 Ctrl+C (rolled back too).
//
// Several tables (--tables) are as many copies, one after another, each
// in its own transaction, each with its own summary line as it ends; a
// total line follows them. A table that fails is rolled back on its own:
// the ones before it stay copied, and by default the ones after it are not
// tried (-k tries them). The total line names what failed and what was not
// tried, so a re-run can name just those. See copyMany.

var (
	flagFrom     string
	flagTo       string
	flagCreate   bool
	flagTruncate bool
	flagWhere    string
	flagDryRun   bool
	// --columns, --query and --batch are CopyOptions' Columns, Query and
	// BatchSize, which a script could set and the command could not (N-336)
	flagColumns []string
	flagQuery   string
	// flagQuerySet says --query was given at all, so `--query "$Q"` with
	// an empty $Q is refused rather than read as no --query — which would
	// take the destination table's name for the source table's
	flagQuerySet bool
	flagBatch    int
	// --tables: several tables, each copied under its own name (N-336)
	flagTables []string
)

// copyProgressEvery is how often the terminal progress line is printed. It
// matches etl's own default: on a direct Postgres copy that is roughly one
// line a quarter second, often enough to show it moving, rare enough not to
// scroll the summary away.
const copyProgressEvery = 100_000

func copyCommand() *cli.Command {
	return &cli.Command{
		Name:      "copy",
		Usage:     "copy a table from one connection to another (any engines)",
		ArgsUsage: "<table> [dest-table]  |  --query SQL <dest-table>  |  --tables T1,T2,…",
		Description: "Copies every row of <table> on --from into the table of the same name (or [dest-table]) " +
			"on --to, which may be the same connection or a different engine. The destination loads in one " +
			"transaction: a failed or canceled copy leaves it as it was. Postgres to Postgres streams COPY to " +
			"COPY; other pairings go row by row. --create makes the destination when it is missing, " +
			"--truncate empties it first, --where filters the source rows, --columns picks some of them. " +
			"--query copies a query's rows instead of a table, into <dest-table>. --tables copies several " +
			"tables, each under its own name and in its own transaction, stopping at the first that fails " +
			"unless -k is given.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "source connection `NAME`", Destination: &flagFrom},
			&cli.StringFlag{Name: "to", Usage: "destination connection `NAME`", Destination: &flagTo},
			&cli.BoolFlag{Name: "create",
				Usage: "create the destination table when it does not exist", Destination: &flagCreate},
			&cli.BoolFlag{Name: "truncate",
				Usage: "empty the destination table first (rolled back if the copy fails)", Destination: &flagTruncate},
			&cli.StringFlag{Name: "where",
				Usage: "copy only the source rows matching this SQL `CONDITION`", Destination: &flagWhere},
			&cli.StringSliceFlag{Name: "columns",
				Usage:       "copy only these source `COLUMNS`, in this order (comma-separated, or repeated); the destination's have the same names",
				Destination: &flagColumns},
			&cli.StringFlag{Name: "query",
				Usage:       "copy the rows this `SQL` returns instead of a table; the one argument is then the destination table",
				Destination: &flagQuery,
				Action: func(context.Context, *cli.Command, string) error {
					flagQuerySet = true
					return nil
				}},
			&cli.StringSliceFlag{Name: "tables",
				Usage:       "copy each of these `TABLES` (comma-separated, or repeated) under its own name, in this order, in place of a table argument; -k goes on past one that fails",
				Destination: &flagTables},
			&cli.IntFlag{Name: "batch",
				Usage: "`N` rows per INSERT into a destination that is not Postgres (default 500, lowered under the engine's bind-parameter limit)",
				// 0 is "not given", which the help would print as "(default:
				// 0)" beside the real default the usage names
				HideDefault: true,
				Destination: &flagBatch},
			&cli.BoolFlag{Name: "dry-run",
				Usage:       "say what the copy would do — columns, rows, the CREATE, the truncate — and write nothing",
				Destination: &flagDryRun},
		},
		Action: copyAction,
	}
}

// copyReq is one copy, as the command line describes it. Kept apart from the
// flag globals so the checks and the run can be tested without parsing.
type copyReq struct {
	from, to    string // connection names
	table, dest string // source table, destination table ("" = same name)
	opts        sdb.CopyOpts
}

func copyAction(ctx context.Context, cmd *cli.Command) error {
	if err := checkCopyFlags(); err != nil {
		usage(err.Error())
	}
	reqs, err := copyRequests(cmd.Args().Slice())
	if err != nil {
		usage(err.Error())
	}
	// Like a script, a copy names its own connections: nothing is opened up
	// front, and a demo it uses seeds on first use.
	cfg, mgr := setup(demoLazy)
	defer mgr.Close()
	warnConfig(cfg)
	// every request has the same connections and options: one check does
	if err = checkCopyConns(cfg, reqs[0]); err != nil {
		usage(err.Error())
	}

	var progress io.Writer
	if term.IsTerminal(os.Stderr.Fd()) {
		progress = os.Stderr
	}
	runCtx, stop := interruptible()
	defer stop()
	if len(reqs) > 1 {
		copyManyAction(runCtx, mgr, reqs, progress)
		return nil
	}
	req := reqs[0]
	if flagDryRun {
		// the plan, on stdout as the summary would be; a copy that would
		// fail before its first row (a missing table without --create)
		// exits 1, so a script can check a copy before it runs it (N-297)
		plan, err := planCopy(runCtx, mgr, req)
		if err != nil {
			if sdb.IsCanceled(err) {
				canceled("copy")
			}
			fail(err, "copy plan failed")
		}
		fmt.Print(plan.String())
		if plan.Problem != "" {
			logger.CloseLog()
			os.Exit(1)
		}
		return nil
	}
	st, err := runCopy(runCtx, mgr, req, progress)
	if err != nil {
		if sdb.IsCanceled(err) {
			canceled("copy")
		}
		fail(err, "copy failed")
	}
	fmt.Println(st.String())
	return nil
}

// checkCopyFlags refuses the root flags that copy would otherwise ignore
// silently. The rule is the one refuseQueryFlags applies to script and
// migrate: a flag that does nothing reads as a bug, or worse, as a promise
// (-c reads as "copy from here"; -o as "the summary goes there").
//
//   - -c, --conn: the two ends are --from and --to; one name cannot say which.
//   - --file, --tx: a copy runs no SQL buffer.
//   - --keep-going, unless --tables: a copy of one table has nothing to go
//     on to; with several, -k tries the rest past one that fails.
//   - --format, --out: the output is one summary line, not a result set.
func checkCopyFlags() error {
	switch {
	case flagConn != "":
		return errors.New("copy takes --from and --to, not -c")
	case flagFile != "":
		return errors.New("--file is SQL for a headless query; `dbc copy` does not read it")
	case flagTx:
		return errors.New("--tx wraps a headless query; a copy always loads in one transaction of its own")
	case flagKeep && len(flagTables) == 0:
		return errors.New("--keep-going is for a headless query, or a copy of several --tables; a copy of one table stops at its failure")
	case flagFormat != "" && flagFormat != "text":
		return errors.New("copy prints a one-line summary; --format does not apply")
	case flagOut != "":
		return errors.New("copy prints a one-line summary; --out does not apply (redirect stdout)")
	}
	return nil
}

// copyRequests is the command line's copies: one, from copyRequest, or one
// a table for --tables. Each of those is built by copyRequest too, as if
// the table had been the argument, so it is checked by the same rules and
// carries the same options (Where, Columns, Create, Truncate, BatchSize).
//
// --tables is a flag, not more arguments, because a second argument
// already means something: `copy orders archive.orders` copies under
// another name. Taking a third as a third table would make two arguments
// mean a rename and three mean three tables.
//
// Refused with --tables: a table argument as well (which list wins?),
// --query (one source, not several), a table named twice (its second copy
// would duplicate or replace the first), and --from equal to --to, which
// with every table under its own name would copy each onto itself.
func copyRequests(args []string) ([]copyReq, error) {
	if len(flagTables) == 0 {
		req, err := copyRequest(args)
		if err != nil {
			return nil, err
		}
		return []copyReq{req}, nil
	}
	switch {
	case len(args) > 0:
		return nil, fmt.Errorf("--tables names the tables to copy; got %s as well — put it in --tables", strings.Join(args, " "))
	case flagQuerySet || flagQuery != "":
		return nil, errors.New("--query copies one query's rows, --tables several tables: give one of them")
	case flagFrom != "" && flagFrom == flagTo:
		return nil, fmt.Errorf("--tables copies each table under its own name, so on one connection (%s) each would "+
			"be copied onto itself — copy them one at a time, each with a destination name", flagFrom)
	}
	seen := map[string]bool{}
	reqs := make([]copyReq, 0, len(flagTables))
	for _, t := range flagTables {
		t = strings.TrimSpace(t)
		switch {
		case t == "":
			return nil, errors.New("--tables has an empty name in it")
		case seen[t]:
			return nil, fmt.Errorf("--tables names %s twice", t)
		}
		seen[t] = true
		req, err := copyRequest([]string{t})
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}
	return reqs, nil
}

// copyRequest turns the flags and arguments into a copyReq, refusing the
// shapes that cannot mean anything. It runs before any config is read or
// connection opened, so a typo costs nothing.
//
// Copying a table onto itself (same connection, same name) is refused: with
// no Transform to change the rows it can only duplicate them, fail on the
// key, or — with --truncate — put back exactly the rows it emptied, at the
// cost of rewriting the whole table. (A script's s.Copy allows it, because
// there a Transform makes it an in-place rewrite; etl empties the table
// with DELETE then, as TRUNCATE's lock would stall the copy's own read.)
// The names are compared as typed, so `orders` against `public.orders` is
// not caught; that is a guard against the slip, not a proof.
//
// With --query the source is the query, and the one argument is the
// destination table (etl requires To with Query). A copy of a query onto
// its own connection is no self-copy — the query names its own tables — so
// that check does not apply.
func copyRequest(args []string) (copyReq, error) {
	const use = "usage: dbc copy --from <conn> --to <conn> [--create] [--truncate] [--where SQL] [--columns COLS] [--batch N] [--dry-run] <table> [dest-table]\n" +
		"       dbc copy --from <conn> --to <conn> --query SQL [--create] [--truncate] [--batch N] [--dry-run] <dest-table>\n" +
		"       dbc copy --from <conn> --to <conn> --tables T1,T2,… [-k] [--create] [--truncate] [--where SQL] [--columns COLS] [--batch N] [--dry-run]"
	switch {
	case flagFrom == "" || flagTo == "":
		return copyReq{}, errors.New("copy needs both --from and --to\n" + use)
	case flagBatch < 0:
		// 0 is "not given" (etl's default, 500); below it means nothing
		return copyReq{}, fmt.Errorf("--batch takes a number of rows above 0, not %d", flagBatch)
	}
	cols, err := copyColumns(flagColumns)
	if err != nil {
		return copyReq{}, err
	}
	opts := sdb.CopyOpts{Create: flagCreate, Truncate: flagTruncate, Where: flagWhere, Columns: cols,
		BatchSize: flagBatch}
	if flagQuerySet || flagQuery != "" {
		return queryCopyRequest(args, opts, use)
	}
	switch {
	case len(args) == 0:
		return copyReq{}, errors.New("no table given\n" + use)
	case len(args) > 2:
		return copyReq{}, fmt.Errorf("got %d arguments — copy takes a table and, optionally, the destination table's name\n%s",
			len(args), use)
	}
	req := copyReq{from: flagFrom, to: flagTo, table: strings.TrimSpace(args[0]), opts: opts}
	if len(args) == 2 {
		req.dest = strings.TrimSpace(args[1])
		req.opts.To = req.dest
	}
	switch {
	case req.table == "":
		return copyReq{}, errors.New("the table name is empty\n" + use)
	case len(args) == 2 && req.dest == "":
		return copyReq{}, errors.New("the destination table name is empty\n" + use)
	case req.from == req.to && (req.dest == "" || req.dest == req.table):
		return copyReq{}, fmt.Errorf("that copies %s onto itself on %s — name a different destination table, or connection",
			req.table, req.from)
	}
	return req, nil
}

// queryCopyRequest is copyRequest's --query form: the query as the source,
// the one argument as the destination table. --where and --columns are
// refused here rather than by etl (which refuses them too), so the mistake
// is a usage error, exit 2, before any connection is opened.
func queryCopyRequest(args []string, opts sdb.CopyOpts, use string) (copyReq, error) {
	q := strings.TrimSpace(flagQuery)
	switch {
	case q == "":
		return copyReq{}, errors.New("--query is empty\n" + use)
	case opts.Where != "" || len(opts.Columns) > 0:
		return copyReq{}, errors.New("--query replaces --where and --columns — put them in the query")
	case len(args) == 0:
		return copyReq{}, errors.New("--query needs the destination table's name as its argument\n" + use)
	case len(args) > 1:
		return copyReq{}, fmt.Errorf("got %d arguments — with --query, copy takes one: the destination table\n%s",
			len(args), use)
	}
	dest := strings.TrimSpace(args[0])
	if dest == "" {
		return copyReq{}, errors.New("the destination table name is empty\n" + use)
	}
	opts.Query, opts.To = q, dest
	return copyReq{from: flagFrom, to: flagTo, dest: dest, opts: opts}, nil
}

// copyColumns is --columns as CopyOptions.Columns: each name trimmed (so
// "id, name" works), none empty, none twice. The names are passed as typed
// and quoted by etl for the source's engine, so they are column names, not
// expressions; an expression belongs in --query.
//
// A duplicate is refused here because nothing after would say it plainly:
// the SELECT would return the column twice, and the destination then
// fails on a CREATE with two columns of one name, or loads one column
// twice.
func copyColumns(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		switch {
		case c == "":
			return nil, errors.New("--columns has an empty name in it")
		case seen[c]:
			return nil, fmt.Errorf("--columns names %s twice", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// checkCopyConns makes an unknown connection name a usage error (exit 2),
// which it is, rather than the "copy failed" (exit 1) the manager's own
// "unknown connection" would become — a cron script can then tell a broken
// command line from a broken copy.
//
// It is also where --batch into Postgres is refused, as the first point
// that knows the destination's engine: a Postgres load streams COPY FROM
// STDIN and has no INSERTs to size, so the flag would do nothing — the
// rule checkCopyFlags applies to the root flags.
func checkCopyConns(cfg *config.Config, req copyReq) error {
	for _, name := range []string{req.from, req.to} {
		if _, ok := cfg.ConnByName(name); !ok {
			// the same words as a query's -c (pickConn): the names there are
			return errors.New(unknownConn(cfg, name))
		}
	}
	if req.opts.BatchSize > 0 {
		cn, _ := cfg.ConnByName(req.to)
		// an unknown driver is left for the copy to report, as it was
		if drv, err := db.Driver(cn.Driver); err == nil && drv == "pgx" {
			return fmt.Errorf("--batch sizes the INSERTs a load is made of; %s is Postgres, which a copy loads "+
				"with COPY, in no batches", req.to)
		}
	}
	return nil
}

// planCopy is the copy's dry run (etl.PlanCopy) on the connections a
// script's s.Copy would use. It is the CLI's, not a script method: the
// script API's summary, sent to the assistant, has a size budget, and a
// script can check a copy by reading the tables itself.
func planCopy(ctx context.Context, mgr *db.Manager, req copyReq) (etl.CopyPlan, error) {
	s := sdb.New(mgr, nil, nil).WithContext(ctx)
	from, err := s.ETLConn(req.from)
	if err != nil {
		return etl.CopyPlan{}, err
	}
	to, err := s.ETLConn(req.to)
	if err != nil {
		return etl.CopyPlan{}, err
	}
	return etl.PlanCopy(ctx, from, req.table, to, req.opts)
}

// runCopy does the copy through sdb.S.Copy, the call a script would make.
// progress, when not nil, receives a running row count every
// copyProgressEvery rows (sdb's ProgressEvery formatting: "  src → dst: N
// rows").
func runCopy(ctx context.Context, mgr *db.Manager, req copyReq, progress io.Writer) (sdb.CopyStats, error) {
	var print func(string)
	if progress != nil {
		print = func(msg string) { fmt.Fprintln(progress, msg) }
		req.opts.ProgressEvery = copyProgressEvery
	}
	s := sdb.New(mgr, nil, print).WithContext(ctx)
	return s.Copy(req.from, req.to, req.table, req.opts)
}

// manyRun is the outcome of a copy of several tables (--tables), or of its
// dry run: what the total line says and the exit status follows from.
type manyRun struct {
	dry      bool // a dry run: done counts plans with no problem
	tables   int
	done     int      // tables copied (or planned with no problem)
	rows     int64    // rows copied, summed over the tables done
	failed   []string // tables whose copy failed, or whose plan has a problem
	canceled string   // the table Ctrl+C stopped, "" for none
	notTried []string // tables after a stop, in order
	firstErr error    // the first failure's error, for its exit status
	took     time.Duration
}

// String is the total line: what was done, then — named, so a re-run can
// list just those — what failed, was canceled, or was not tried.
//
//	copied 2 of 3 tables, 1290 rows in 41ms; failed: items
//	copied 1 of 3 tables, 990 rows in 18ms; failed: orders; not tried: items
//	dry run of 3 tables, nothing was written; problem: items
func (r manyRun) String() string {
	var msg string
	if r.dry {
		msg = fmt.Sprintf("dry run of %d tables, nothing was written", r.tables)
	} else {
		msg = fmt.Sprintf("copied %d of %d tables, %d rows in %s", r.done, r.tables, r.rows, r.took.Round(time.Millisecond))
	}
	if len(r.failed) > 0 {
		word := "failed"
		if r.dry {
			word = "problem"
		}
		msg += "; " + word + ": " + strings.Join(r.failed, ", ")
	}
	if r.canceled != "" {
		msg += "; canceled: " + r.canceled
	}
	if len(r.notTried) > 0 {
		msg += "; not tried: " + strings.Join(r.notTried, ", ")
	}
	return msg
}

// tableNames is the source tables of reqs, for a manyRun's lists.
func tableNames(reqs []copyReq) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.table
	}
	return out
}

// copyMany copies each of reqs in turn, each in its own transaction (it is
// runCopy, one table at a time), printing a table's summary line to out as
// it ends and handing a failure to onFail (copyManyAction prints it on
// stderr). The tables run one after another, not side by side: the order
// is the user's (a parent before the child whose foreign key needs it),
// and one at a time is what each table's copy already assumes of the two
// servers.
//
//	for each table:
//	    copied    ─► its summary line; next
//	    canceled  ─► stop: it rolled back, the rest are not tried
//	    failed    ─► onFail; with keepGoing next, else stop
//	                 (a connection that could not be opened stops either
//	                 way: every table after would fail on it too)
func copyMany(ctx context.Context, mgr *db.Manager, reqs []copyReq, keepGoing bool,
	out, progress io.Writer, onFail func(copyReq, error)) manyRun {
	start := time.Now()
	r := manyRun{tables: len(reqs)}
	for i, req := range reqs {
		st, err := runCopy(ctx, mgr, req, progress)
		if err == nil {
			r.done++
			r.rows += st.Rows
			fmt.Fprintln(out, st.String())
			continue
		}
		if sdb.IsCanceled(err) {
			r.canceled, r.notTried = req.table, tableNames(reqs[i+1:])
			break
		}
		r.failed = append(r.failed, req.table)
		if r.firstErr == nil {
			r.firstErr = err
		}
		onFail(req, err)
		if !keepGoing || db.IsConnectErr(err) {
			r.notTried = tableNames(reqs[i+1:])
			break
		}
	}
	r.took = time.Since(start)
	return r
}

// planMany is copyMany's dry run: each table's plan to out, a blank line
// between them. It goes through every table, whatever -k says, since a dry
// run is for finding the problems, all of them; only a cancel, or a
// connection that could not be opened, stops it.
func planMany(ctx context.Context, mgr *db.Manager, reqs []copyReq, out io.Writer,
	onErr func(copyReq, error)) manyRun {
	r := manyRun{dry: true, tables: len(reqs)}
	for i, req := range reqs {
		if i > 0 {
			fmt.Fprintln(out)
		}
		plan, err := planCopy(ctx, mgr, req)
		switch {
		case err != nil && sdb.IsCanceled(err):
			r.canceled, r.notTried = req.table, tableNames(reqs[i+1:])
			return r
		case err != nil:
			r.failed = append(r.failed, req.table)
			if r.firstErr == nil {
				r.firstErr = err
			}
			onErr(req, err)
			if db.IsConnectErr(err) {
				r.notTried = tableNames(reqs[i+1:])
				return r
			}
			continue
		}
		fmt.Fprint(out, plan.String())
		if plan.Problem != "" {
			r.failed = append(r.failed, req.table)
		} else {
			r.done++
		}
	}
	return r
}

// copyManyAction is copyAction for several tables: the copies, or with
// --dry-run their plans, then the total line on stdout and the exit status.
//
//	0    every table copied (planned without a problem)
//	1    one or more failed (had a problem), the rest as the total line says
//	4    … where the first failure was a connection that could not be opened
//	130  Ctrl+C: the table being copied rolled back, those before it stay
func copyManyAction(ctx context.Context, mgr *db.Manager, reqs []copyReq, progress io.Writer) {
	var r manyRun
	if flagDryRun {
		r = planMany(ctx, mgr, reqs, os.Stdout, func(req copyReq, err error) {
			reportErr(err, "copy plan of "+req.table+" failed")
		})
	} else {
		r = copyMany(ctx, mgr, reqs, flagKeep, os.Stdout, progress, func(req copyReq, err error) {
			reportErr(err, "copy of "+req.table+" failed")
		})
	}
	if flagDryRun {
		fmt.Println() // the total apart from the last plan, as the plans are from each other
	}
	fmt.Println(r.String())
	switch {
	case r.canceled != "":
		canceled("copy")
	case r.firstErr != nil && db.IsConnectErr(r.firstErr):
		logger.CloseLog()
		os.Exit(exitConnect)
	case len(r.failed) > 0:
		logger.CloseLog()
		os.Exit(1)
	}
}
