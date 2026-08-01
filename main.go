// dbc — a TUI database client with Go scripting.
//
//	dbc                            launch the TUI
//	dbc "SELECT * FROM cats"       run one query headless (uses -c / default conn)
//	dbc "INSERT …; SELECT …"       run several statements in order, on one conn
//	dbc script scripts/loop.go     run a Go script headless
//
// Headless runs are cancelable with Ctrl+C, which aborts the statement on the
// server and exits 130.
//
// Flags (before positional args):
//
//	-config path   config file (default: ./dbc.toml, ~/.config/dbc/config.toml)
//	-c name        connection name for a one-off query
//	-f format      headless output format: text|csv|tsv|markdown|html|json
//	-o file        write headless output to a file instead of stdout
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/ui"
)

var (
	flagConfig = flag.String("config", "", "path to config file")
	flagConn   = flag.String("c", "", "connection name for a one-off query")
	flagFormat = flag.String("f", "text", "headless output format: text|csv|tsv|markdown|html|json")
	flagOut    = flag.String("o", "", "write headless output to file instead of stdout")
)

func main() {
	flag.Parse()
	// warn level keeps informational logger chatter out of headless pipelines
	logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"})
	defer logger.CloseLog()

	cfg, err := config.Load(*flagConfig)
	if err != nil {
		fail(err, "could not load config")
	}
	mgr := db.NewManager(cfg)
	defer mgr.Close()

	if cfg.Demo {
		if err = db.SeedDemo(mgr, "demo"); err != nil {
			fail(err, "could not seed demo data")
		}
	}

	args := flag.Args()
	switch {
	case len(args) > 0 && args[0] == "script":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: dbc script <file.go>")
			os.Exit(2)
		}
		runScriptHeadless(mgr, args[1])
	case len(args) > 0:
		runQueryHeadless(cfg, mgr, args[0])
	default:
		if err = ui.Run(cfg, mgr); err != nil {
			fail(err, "UI error")
		}
	}
}

// runQueryHeadless runs the SQL buffer given on the command line. It may hold
// several statements: they run in order on one connection, stopping at the
// first failure, and every result that made it is still rendered.
func runQueryHeadless(cfg *config.Config, mgr *db.Manager, sql string) {
	conn := *flagConn
	if conn == "" {
		conn = cfg.DefaultConnection
	}
	if conn == "" && len(cfg.Connections) == 1 {
		conn = cfg.Connections[0].Name
	}
	if conn == "" {
		fmt.Fprintln(os.Stderr, "multiple connections configured — pick one with -c <name>")
		os.Exit(2)
	}
	f, err := export.ParseFormat(*flagFormat)
	if err != nil {
		fail(err, "bad -f format")
	}
	stmts := sqlsplit.Split(sql)
	if len(stmts) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to run — the SQL holds no statement")
		os.Exit(2)
	}
	ctx, stop := interruptible()
	defer stop()

	// one pinned session for the whole buffer, so BEGIN/COMMIT, SET, and temp
	// tables mean what they say across statements
	sess, err := mgr.Session(ctx, conn)
	if err != nil {
		fail(err, "could not open connection")
	}
	defer sess.Close()

	results, runErr := runStatements(ctx, sess, stmts)
	// output first: a failing statement 3 does not invalidate results 1 and 2
	if len(results) > 0 {
		emit(results, f)
	}
	if runErr != nil {
		if errors.Is(runErr, db.ErrCanceled) {
			canceled("query")
		}
		fail(runErr, "query failed")
	}
}

// runStatements executes stmts in order on one session, stopping at the first
// failure. It returns the results that completed and the error that stopped
// it, so the caller can still report the work that got done. A failure in a
// multi-statement run is tagged with the statement's position.
func runStatements(ctx context.Context, sess *db.Session,
	stmts []sqlsplit.Stmt) ([]*model.Result, error) {

	results := make([]*model.Result, 0, len(stmts))
	for i, st := range stmts {
		res, err := sess.Run(ctx, st.Text)
		if err != nil {
			if len(stmts) > 1 {
				err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
			}
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// emit renders the results to stdout, or to the -o file when one was given.
func emit(results []*model.Result, f export.Format) {
	out, err := export.RenderAll(results, f)
	if err != nil {
		fail(err, "render failed")
	}
	warnTruncated(os.Stderr, results)
	if *flagOut == "" {
		fmt.Print(out)
		return
	}
	if err = os.WriteFile(*flagOut, []byte(out), 0644); err != nil {
		fail(serr.Wrap(err, "path", *flagOut), "write failed")
	}
	rows := 0
	for _, r := range results {
		rows += len(r.Rows)
	}
	if len(results) > 1 {
		fmt.Printf("wrote %d results (%d rows) to %s\n", len(results), rows, *flagOut)
		return
	}
	fmt.Printf("wrote %d rows to %s\n", rows, *flagOut)
}

// warnTruncated notes on w (stderr) every result that hit the max_rows cap.
// Multi-statement banners and JSON envelopes carry the flag themselves, but a
// single-statement CSV/TSV/text export would otherwise look complete while
// quietly missing rows — and stderr keeps the note out of the data stream.
func warnTruncated(w io.Writer, results []*model.Result) {
	for i, r := range results {
		if !r.Truncated {
			continue
		}
		pos := ""
		if len(results) > 1 {
			pos = fmt.Sprintf("statement %d/%d: ", i+1, len(results))
		}
		fmt.Fprintf(w, "note: %sresult truncated at %d rows — raise max_rows in config for more\n",
			pos, len(r.Rows))
	}
}

func runScriptHeadless(mgr *db.Manager, path string) {
	ctx, stop := interruptible()
	defer stop()

	s := sdb.New(mgr,
		func(r *model.Result) {
			fmt.Printf("-- %s │ %d rows in %s\n", r.Conn, len(r.Rows), r.Duration)
			fmt.Println(export.TextTable(r))
		},
		func(msg string) {
			fmt.Println(msg)
		}).WithContext(ctx)

	if err := script.Run(path, s); err != nil {
		if errors.Is(err, db.ErrCanceled) {
			canceled("script")
		}
		fail(err, "script failed")
	}
}

// interruptible returns a context canceled by Ctrl+C or SIGTERM, so a long
// headless query is aborted on the server rather than orphaned.
func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func canceled(what string) {
	fmt.Fprintf(os.Stderr, "%s canceled\n", what)
	logger.CloseLog()
	os.Exit(130) // conventional exit status for an interrupt
}

func fail(err error, msg string) {
	logger.LogErr(err, msg)
	logger.CloseLog()
	os.Exit(1)
}
