package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
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

var (
	flagFrom     string
	flagTo       string
	flagCreate   bool
	flagTruncate bool
	flagWhere    string
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
		ArgsUsage: "<table> [dest-table]",
		Description: "Copies every row of <table> on --from into the table of the same name (or [dest-table]) " +
			"on --to, which may be the same connection or a different engine. The destination loads in one " +
			"transaction: a failed or canceled copy leaves it as it was. Postgres to Postgres streams COPY to " +
			"COPY; other pairings go row by row. --create makes the destination when it is missing, " +
			"--truncate empties it first, --where filters the source rows.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from", Usage: "source connection `NAME`", Destination: &flagFrom},
			&cli.StringFlag{Name: "to", Usage: "destination connection `NAME`", Destination: &flagTo},
			&cli.BoolFlag{Name: "create",
				Usage: "create the destination table when it does not exist", Destination: &flagCreate},
			&cli.BoolFlag{Name: "truncate",
				Usage: "empty the destination table first (rolled back if the copy fails)", Destination: &flagTruncate},
			&cli.StringFlag{Name: "where",
				Usage: "copy only the source rows matching this SQL `CONDITION`", Destination: &flagWhere},
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
	req, err := copyRequest(cmd.Args().Slice())
	if err != nil {
		usage(err.Error())
	}
	// Like a script, a copy names its own connections: nothing is opened up
	// front, and a demo it uses seeds on first use.
	cfg, mgr := setup(demoLazy)
	defer mgr.Close()
	warnConfig(cfg)
	if err = checkCopyConns(cfg, req); err != nil {
		usage(err.Error())
	}

	var progress io.Writer
	if term.IsTerminal(os.Stderr.Fd()) {
		progress = os.Stderr
	}
	runCtx, stop := interruptible()
	defer stop()
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
//   - --file, --tx, --keep-going: a copy runs no SQL buffer.
//   - --format, --out: the output is one summary line, not a result set.
func checkCopyFlags() error {
	switch {
	case flagConn != "":
		return errors.New("copy takes --from and --to, not -c")
	case flagFile != "":
		return errors.New("--file is SQL for a headless query; `dbc copy` does not read it")
	case flagTx:
		return errors.New("--tx wraps a headless query; a copy always loads in one transaction of its own")
	case flagKeep:
		return errors.New("--keep-going is for a headless query; a copy stops at its first failure")
	case flagFormat != "" && flagFormat != "text":
		return errors.New("copy prints a one-line summary; --format does not apply")
	case flagOut != "":
		return errors.New("copy prints a one-line summary; --out does not apply (redirect stdout)")
	}
	return nil
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
func copyRequest(args []string) (copyReq, error) {
	const use = "usage: dbc copy --from <conn> --to <conn> [--create] [--truncate] [--where SQL] <table> [dest-table]"
	switch {
	case flagFrom == "" || flagTo == "":
		return copyReq{}, errors.New("copy needs both --from and --to\n" + use)
	case len(args) == 0:
		return copyReq{}, errors.New("no table given\n" + use)
	case len(args) > 2:
		return copyReq{}, fmt.Errorf("got %d arguments — copy takes a table and, optionally, the destination table's name\n%s",
			len(args), use)
	}
	req := copyReq{from: flagFrom, to: flagTo, table: strings.TrimSpace(args[0]),
		opts: sdb.CopyOpts{Create: flagCreate, Truncate: flagTruncate, Where: flagWhere}}
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

// checkCopyConns makes an unknown connection name a usage error (exit 2),
// which it is, rather than the "copy failed" (exit 1) the manager's own
// "unknown connection" would become — a cron script can then tell a broken
// command line from a broken copy.
func checkCopyConns(cfg *config.Config, req copyReq) error {
	for _, name := range []string{req.from, req.to} {
		if _, ok := cfg.ConnByName(name); !ok {
			return fmt.Errorf("no connection named %q (see the config file, or use --driver/--dsn for one named %q)",
				name, adHocConnName)
		}
	}
	return nil
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
