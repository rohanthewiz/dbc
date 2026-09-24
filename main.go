// dbc — a TUI database client with Go scripting.
//
//	dbc                            launch the TUI
//	dbc "SELECT * FROM cats"       run one query headless (uses -c / default conn)
//	dbc "INSERT …; SELECT …"       run several statements in order, on one conn
//	dbc -f report.sql              run the SQL in a file headless
//	dbc -f - < report.sql          … or from stdin, spelled out
//	dbc < report.sql               … or from stdin, when it is a pipe or a file
//	dbc script scripts/loop.go     run a Go script headless
//	dbc migrate up                 apply pending migrations (see migrate.go)
//
// Headless runs are cancelable with Ctrl+C, which aborts the statement on the
// server and exits 130.
//
// Flags (github.com/urfave/cli/v3, so a long name takes one dash or two, and
// flags may come before or after the SQL):
//
//	--config path      config file (default: ./dbc.toml, ~/.config/dbc/config.toml)
//	-c, --conn name    connection name for a one-off query
//	-f, --file path    read the SQL to run headless from a file ("-" = stdin)
//	-t, --format fmt   headless output format: text|csv|tsv|markdown|html|json
//	-o, --out file     write headless output to a file instead of stdout
//	--demo engine      which built-in demo starts active: bytdb (default) | sqlite
//	                   (also $DBC_DEMO; only applies when there is no config file)
//	--driver name      with --dsn: run against an ad-hoc connection instead of a
//	--dsn string       configured one (no config file needed)
//	--dir path         migrations directory for `dbc migrate`
//	--allow-missing    let `migrate up` apply out-of-order migrations
//
// --format and --out apply to scripts and migrate too: the results they show
// are rendered in the chosen format, to the chosen destination. --file is SQL
// input for a headless query only; script and migrate refuse it.
//
// With no config file, dbc starts on two seeded demo connections — demo-bytdb
// (embedded bytdb, a file in the OS cache directory) and demo (in-memory
// SQLite) — so the same query can be run against both engines. --demo picks
// which one starts active; the other is still there in the connection list.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/tui"
)

// The flag values. Each cli flag writes into one of these through its
// Destination, so the rest of the file reads plain variables rather than
// threading *cli.Command through every helper — the headless paths and their
// tests predate the cli package and only ever needed the values.
var (
	flagConfig  string
	flagConn    string
	flagFile    string
	flagFormat  = "text"
	flagOut     string
	flagDemo    string
	flagDriver  string
	flagDSN     string
	flagDir     string
	flagMissing bool
)

func main() {
	// warn level keeps informational logger chatter out of headless pipelines
	logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"})
	defer logger.CloseLog()

	// The actions exit on their own (fail, canceled, usage), so what reaches
	// here is a parse error the cli package has already printed along with
	// the help text. 2 is the conventional status for bad usage, and what
	// Go's flag package exited with before.
	if err := newCLI().Run(context.Background(), os.Args); err != nil {
		logger.CloseLog()
		os.Exit(2)
	}
}

// newCLI builds the command tree. The root command is both the TUI and a
// headless query: which one depends on whether any SQL arrived (argument,
// --file, or piped stdin). script and migrate are subcommands.
//
// Every flag lives on the root and is persistent (the cli default), so it may
// come before or after a subcommand: `dbc -t json migrate version` and
// `dbc migrate version -t json` mean the same. --dir and --allow-missing are
// migrate-only but stay on the root, so goose's word order
// (`dbc --dir db/migrate migrate up`) keeps working.
func newCLI() *cli.Command {
	return &cli.Command{
		Name:      "dbc",
		Usage:     "a TUI database client with Go scripting",
		ArgsUsage: `["SQL"]`,
		Description: "With no SQL, dbc opens the TUI. SQL given as the argument, with --file, " +
			"or piped to stdin runs headless: statements run in order on one connection " +
			"and the results go to stdout (or --out) in --format.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Usage: "config `FILE` (default: ./dbc.toml, then ~/.config/dbc/config.toml)",
				Destination: &flagConfig},
			&cli.StringFlag{Name: "conn", Aliases: []string{"c"}, Usage: "connection `NAME` for a headless run",
				Destination: &flagConn},
			&cli.StringFlag{Name: "file", Aliases: []string{"f"}, Usage: "read the SQL to run from `FILE` (- for stdin)",
				Destination: &flagFile},
			&cli.StringFlag{Name: "format", Aliases: []string{"t"}, Value: "text",
				Usage: "headless output `FORMAT`: text|csv|tsv|markdown|html|json", Destination: &flagFormat},
			&cli.StringFlag{Name: "out", Aliases: []string{"o"}, Usage: "write headless output to `FILE` instead of stdout",
				Destination: &flagOut},
			&cli.StringFlag{Name: "demo", Sources: cli.EnvVars("DBC_DEMO"),
				Usage: "built-in demo `ENGINE` to start on when no config exists: bytdb|sqlite", Destination: &flagDemo},
			&cli.StringFlag{Name: "driver", Category: "ad-hoc connection",
				Usage: "with --dsn: `DRIVER` of an ad-hoc connection (postgres|mysql|sqlite|bytdb)", Destination: &flagDriver},
			&cli.StringFlag{Name: "dsn", Category: "ad-hoc connection",
				Usage: "ad-hoc connection `DSN`, used instead of any configured connection", Destination: &flagDSN},
			&cli.StringFlag{Name: "dir", Category: "migrate",
				Usage: "migrations `DIR` (default: the connection's migrations, else .)", Destination: &flagDir},
			&cli.BoolFlag{Name: "allow-missing", Category: "migrate",
				// no backticks here: the cli package reads a `quoted` word in
				// Usage as the flag's value placeholder, even on a bool flag
				Usage: "let migrate up apply migrations older than the current version", Destination: &flagMissing},
		},
		Action: rootAction,
		Commands: []*cli.Command{
			{
				Name:      "script",
				Usage:     "run a Go script headless",
				ArgsUsage: "<file.go>",
				Action:    scriptAction,
			},
			{
				Name:      "migrate",
				Usage:     "apply or inspect goose-format migrations (dbc migrate help)",
				ArgsUsage: "<command> [VERSION|NAME]",
				// migrate parses its own verbs (see migrate.go); the cli's
				// help command would otherwise claim `migrate help`
				HideHelpCommand: true,
				Action:          migrateAction,
			},
		},
	}
}

// rootAction is either the TUI or a headless query, decided by sqlInput.
func rootAction(ctx context.Context, cmd *cli.Command) error {
	sql, headless, err := sqlInput(cmd.Args().Slice(), flagFile, os.Stdin, stdinHasInput())
	if err != nil {
		usage(err.Error())
	}
	cfg, mgr := setup()
	defer mgr.Close()

	if !headless {
		if err = tui.Run(cfg, mgr); err != nil {
			fail(err, "UI error")
		}
		return nil
	}
	warnConfig(cfg)
	runQueryHeadless(cfg, mgr, sql, outFormat())
	return nil
}

func scriptAction(ctx context.Context, cmd *cli.Command) error {
	refuseFile("script")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc script <file.go>")
	}
	cfg, mgr := setup()
	defer mgr.Close()
	warnConfig(cfg)
	runScriptHeadless(mgr, cmd.Args().First(), outFormat())
	return nil
}

func migrateAction(ctx context.Context, cmd *cli.Command) error {
	refuseFile("migrate")
	cfg, mgr := setup()
	defer mgr.Close()
	warnConfig(cfg)
	runMigrate(cfg, mgr, cmd.Args().Slice(), outFormat())
	return nil
}

// sqlInput decides where the SQL for a run comes from, and whether there is
// any (headless) or not (the TUI):
//
//	SQL argument     --file        stdin piped    →  source
//	───────────────  ────────────  ─────────────     ──────────────────
//	one              —             any               the argument
//	—                path | -      any               the file | stdin
//	—                —             yes               stdin
//	—                —             no                none: open the TUI
//	one              path | -      any               error: two sources
//	two or more      any           any               error: quote the SQL
//
// Piped stdin can safely mean "run this": the TUI needs a terminal on stdin,
// so a pipe or file there was never a TUI launch. More than one argument is
// almost always unquoted SQL (`dbc SELECT * FROM t`, with * globbed by the
// shell); it used to run just the first word and drop the rest silently.
func sqlInput(args []string, file string, stdin io.Reader, stdinPiped bool) (string, bool, error) {
	// -f used to be the output format. `dbc -f csv …` from old habits or old
	// scripts would otherwise fail with a puzzling "no such file: csv", so a
	// format name that is not also a real file gets pointed at -t instead.
	if _, err := export.ParseFormat(file); err == nil && file != "" {
		if _, statErr := os.Stat(file); statErr != nil {
			return "", false, fmt.Errorf("-f now reads SQL from a file — for the output format use -t %s", file)
		}
	}
	switch {
	case len(args) > 1:
		return "", false, fmt.Errorf("got %d arguments — pass the SQL as one quoted argument", len(args))
	case len(args) == 1 && file != "":
		return "", false, errors.New("give the SQL as an argument or with --file, not both")
	case len(args) == 1:
		return args[0], true, nil
	case file == "-", file == "" && stdinPiped:
		bs, err := io.ReadAll(stdin)
		if err != nil {
			return "", false, fmt.Errorf("reading SQL from stdin: %w", err)
		}
		return string(bs), true, nil
	case file != "":
		bs, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("--file: %w", err)
		}
		return string(bs), true, nil
	}
	return "", false, nil
}

// stdinHasInput reports whether stdin is a pipe or a redirected file — the
// two shapes that mean "here is SQL". A terminal, /dev/null (a character
// device, as when launched by a GUI or cron) or a closed stdin all read as no
// input, so they still open the TUI or fall through to its own error.
func stdinHasInput() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	m := fi.Mode()
	return m&os.ModeNamedPipe != 0 || m.IsRegular()
}

// refuseFile rejects --file on the subcommands, which read their own input
// (a script file, migration files) and would otherwise ignore it silently.
func refuseFile(sub string) {
	if flagFile != "" {
		usage(fmt.Sprintf("--file is SQL for a headless query; `dbc %s` does not read it", sub))
	}
}

// setup loads the config, builds the connection manager and seeds the demos.
// It runs inside the actions, not before cli parsing, so `dbc --help` and a
// usage error cost nothing and touch no database. The caller owns mgr.Close.
func setup() (*config.Config, *db.Manager) {
	demo, err := config.ParseDemoEngine(flagDemo)
	if err != nil {
		fail(err, "bad --demo engine")
	}
	cfg, err := config.LoadDemo(flagConfig, demo)
	if err != nil {
		fail(err, "could not load config")
	}
	if err = addAdHocConn(cfg); err != nil {
		fail(err, "bad --dsn/--driver")
	}
	mgr := db.NewManager(cfg)

	// With no config the app runs on the built-in demos — one per embedded
	// engine — and each gets the same seed data. SeedDemos prunes any that
	// cannot be opened, so one bad demo does not stop the launch.
	if cfg.Demo {
		if err = db.SeedDemos(mgr, cfg); err != nil {
			mgr.Close()
			fail(err, "could not seed demo data")
		}
	}
	return cfg, mgr
}

// usage reports bad command-line usage and exits 2, the status Go's flag
// package used and that scripts can tell apart from a failed query (1).
func usage(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	logger.CloseLog()
	os.Exit(2)
}

// outFormat resolves the --format flag, failing before any work starts when the name
// is not one we render. Both headless paths render through it.
func outFormat() export.Format {
	f, err := export.ParseFormat(flagFormat)
	if err != nil {
		fail(err, "bad --format")
	}
	return f
}

// warnConfig prints load-time config warnings for the headless paths; the TUI
// shows them in its log pane instead.
func warnConfig(cfg *config.Config) {
	for _, w := range cfg.Warnings {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
}

// runQueryHeadless runs the SQL buffer given on the command line. It may hold
// several statements: they run in order on one connection, stopping at the
// first failure, and every result that made it is still rendered.
func runQueryHeadless(cfg *config.Config, mgr *db.Manager, sql string, f export.Format) {
	conn := pickConn(cfg)
	stmts := sqlsplit.Split(sql)
	if len(stmts) == 0 {
		usage("nothing to run — the SQL holds no statement")
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

// pickConn decides which connection a headless run uses: -c, else the
// config default, else the only one there is. An ad-hoc --dsn connection is
// the default whenever one was given, since typing a DSN is as explicit as a
// name gets.
func pickConn(cfg *config.Config) string {
	conn := flagConn
	if conn == "" && flagDSN != "" {
		conn = adHocConnName
	}
	if conn == "" {
		conn = cfg.DefaultConnection
	}
	if conn == "" && len(cfg.Connections) == 1 {
		conn = cfg.Connections[0].Name
	}
	if conn == "" {
		usage("multiple connections configured — pick one with -c <name>")
	}
	return conn
}

// adHocConnName is the connection --dsn registers under.
const adHocConnName = "dsn"

// addAdHocConn appends the -dsn/-driver connection to the config so it runs
// through the same manager as configured ones. It exists so a CI job or a
// fresh server can `dbc --driver postgres --dsn "$DATABASE_URL" migrate up`
// with no config file — goose's whole command line, one flag longer.
func addAdHocConn(cfg *config.Config) error {
	if flagDSN == "" && flagDriver == "" {
		return nil
	}
	if flagDSN == "" || flagDriver == "" {
		return serr.New("--dsn and --driver go together")
	}
	if _, err := db.Driver(flagDriver); err != nil {
		return err
	}
	if _, taken := cfg.ConnByName(adHocConnName); taken {
		return serr.New("a configured connection already uses the reserved name", "name", adHocConnName)
	}
	cfg.Connections = append(cfg.Connections, config.Connection{
		Name: adHocConnName, Driver: flagDriver, DSN: flagDSN,
	})
	return nil
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

// emit renders the results to stdout, or to the --out file when one was given.
func emit(results []*model.Result, f export.Format) {
	out, err := export.RenderAll(results, f)
	if err != nil {
		fail(err, "render failed")
	}
	warnTruncated(os.Stderr, results)
	if flagOut == "" {
		fmt.Print(out)
		return
	}
	if err = os.WriteFile(flagOut, []byte(out), 0644); err != nil {
		fail(serr.Wrap(err, "path", flagOut), "write failed")
	}
	rows := 0
	for _, r := range results {
		rows += len(r.Rows)
	}
	if len(results) > 1 {
		fmt.Printf("wrote %d results (%d rows) to %s\n", len(results), rows, flagOut)
		return
	}
	fmt.Printf("wrote %d rows to %s\n", rows, flagOut)
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

// runScriptHeadless runs a Go script. The results it pushes with s.Show are
// collected and rendered together when it finishes, exactly as the statements
// of a multi-statement query are — so -t json yields one array rather than a
// run of separate documents, and -o writes one file.
func runScriptHeadless(mgr *db.Manager, path string, f export.Format) {
	ctx, stop := interruptible()
	defer stop()

	var results []*model.Result
	logOut := scriptLog(f)

	s := sdb.New(mgr,
		func(r *model.Result) { results = append(results, r) },
		func(msg string) { fmt.Fprintln(logOut, msg) },
	).WithContext(ctx)

	err := script.Run(path, s)
	// output first: a script that failed on its third query still showed two
	if len(results) > 0 {
		emit(results, f)
	}
	if err != nil {
		if errors.Is(err, db.ErrCanceled) {
			canceled("script")
		}
		fail(err, "script failed")
	}
}

// scriptLog picks where a script's s.Print output goes. It is progress, not
// data, so it streams as it happens — but it can only share stdout with the
// results when they are a human-readable table. Interleaved in csv or json it
// would corrupt the stream, so there it goes to stderr instead.
func scriptLog(f export.Format) io.Writer {
	if flagOut == "" && f != export.Text {
		return os.Stderr
	}
	return os.Stdout
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
