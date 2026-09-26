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
//	dbc explain -a "SELECT …"      show a statement's plan and findings (see explain.go)
//	dbc web                        the workbench in a browser (see webcmd.go)
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
//	--tx               run a headless query's statements in one transaction:
//	                   all of them commit, or none do
//	-k, --keep-going   go on past a failed statement instead of stopping there
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
// (embedded bytdb, a file in the OS cache directory) and demo-sqlite (in-memory
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
	"strings"
	"syscall"
	"time"

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
	flagTx      bool
	flagKeep    bool
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
			&cli.BoolFlag{Name: "tx",
				Usage:       "run the statements in one transaction: all commit, or on the first failure none do",
				Destination: &flagTx},
			&cli.BoolFlag{Name: "keep-going", Aliases: []string{"k"},
				Usage:       "go on past a failed statement; exit 1 at the end if any failed",
				Destination: &flagKeep},
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
			explainCommand(),
			webCommand(),
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
	demos := demoForRun
	if !headless {
		demos = demoAll
	}
	cfg, mgr := setup(demos)
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
	refuseQueryFlags("script")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc script <file.go>")
	}
	// A script names its own connections, so nothing is opened for it up
	// front: each demo it uses seeds on first use.
	cfg, mgr := setup(demoLazy)
	defer mgr.Close()
	warnConfig(cfg)
	runScriptHeadless(mgr, cmd.Args().First(), outFormat())
	return nil
}

func migrateAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("migrate")
	cfg, mgr := setup(demoForRun)
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

// refuseQueryFlags rejects the headless-query flags on the subcommands, which
// would otherwise ignore them silently: --file, since they read their own
// input (a script file, migration files), and --tx and --keep-going, since
// they run their own statements their own way (goose-format migrations bring
// their own per-file transactions; a script calls s.Exec itself).
func refuseQueryFlags(sub string) {
	switch {
	case flagFile != "":
		usage(fmt.Sprintf("--file is SQL for a headless query; `dbc %s` does not read it", sub))
	case flagTx:
		usage(fmt.Sprintf("--tx wraps a headless query; `dbc %s` does not use it", sub))
	case flagKeep:
		usage(fmt.Sprintf("--keep-going is for a headless query; `dbc %s` does not use it", sub))
	}
}

// demoOpen is how much of the built-in demo a command opens up front, when
// there is no config and the app runs on the demos. Opening a demo seeds it
// (db.Manager seeds a demo on first open), so this is also how much seeding a
// launch pays for — and the bytdb demo is a file, so opening it is a write to
// the cache directory that a run which never uses it should not make.
type demoOpen int

const (
	// demoAll opens every demo, dropping any that cannot be opened. The TUI
	// lists every connection, so it has to know up front which ones work.
	demoAll demoOpen = iota
	// demoForRun opens only the active demo, and only when the run will use
	// it (no -c, no --dsn); if it cannot be opened the other demo takes over
	// as the default, with a warning, as it does in the TUI. A run that names
	// a connection opens just that one, on first use.
	demoForRun
	// demoLazy opens nothing: every demo seeds on first use, if at all.
	demoLazy
)

// setup loads the config, builds the connection manager and opens as much of
// the built-in demo as demos asks for (see demoOpen). It runs inside the
// actions, not before cli parsing, so `dbc --help` and a usage error cost
// nothing and touch no database. The caller owns mgr.Close.
func setup(demos demoOpen) (*config.Config, *db.Manager) {
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
	// The connections added in dbc web, for every command alike, so one
	// added in the browser is there in the TUI and for `dbc -c name`. Last,
	// so the config file's and the ad-hoc --dsn one keep their names: a
	// saved one that clashes is the one skipped, with a warning.
	cfg.LoadSaved(config.SavedFile(), func(driver string) bool {
		_, err := db.Driver(driver)
		return err == nil
	})
	mgr := db.NewManager(cfg)
	if !cfg.Demo {
		return cfg, mgr
	}

	// With no config the app runs on the built-in demos — one per embedded
	// engine — and each gets the same seed data, when it is first opened.
	// Both open paths below prune a demo that cannot be opened, so one bad
	// demo does not stop the launch.
	switch {
	case demos == demoAll:
		err = db.SeedDemos(mgr, cfg)
	case demos == demoForRun && flagConn == "" && flagDSN == "":
		err = db.OpenDefaultDemo(mgr, cfg)
	}
	if err != nil {
		mgr.Close()
		fail(err, "could not open the demo connection")
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
// first failure (or, with --keep-going, going on past it), and every result
// that made it is still rendered.
//
// Rendering takes one of two shapes (see streamsResults):
//
//	streamed   each result is written to stdout as its statement finishes,
//	           so a long run shows progress — block formats on stdout only
//	collected  every result is held until the run ends and rendered as one
//	           document — HTML, JSON, -o, and any single statement
//
// With --tx the run is bracketed by dbc's own BEGIN and COMMIT (see
// checkTx and endTx):
//
//	BEGIN ─► stmt 1 ─► stmt 2 ─► … ─► stmt n ─► COMMIT   all kept
//	            └──────── any failure, or Ctrl+C ─► ROLLBACK   none kept
//
// Either way the results that ran are still shown — in a rolled-back run
// they are what the statements saw, not what the database now holds, and a
// note on stderr says so.
func runQueryHeadless(cfg *config.Config, mgr *db.Manager, sql string, f export.Format) {
	conn := pickConn(cfg)
	stmts := sqlsplit.Split(sql)
	if len(stmts) == 0 {
		usage("nothing to run — the SQL holds no statement")
	}
	if err := checkRunFlags(stmts); err != nil {
		usage(err.Error())
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

	if flagTx {
		if _, err = sess.Run(ctx, "BEGIN"); err != nil {
			if errors.Is(err, db.ErrCanceled) {
				canceled("query")
			}
			fail(err, "could not begin the transaction")
		}
	}

	hooks := runHooks{keepGoing: flagKeep, onFail: reportFailed}
	var stream *blockStream
	if streamsResults(f, len(stmts)) {
		stream = &blockStream{out: os.Stdout, notes: os.Stderr, f: f, total: len(stmts)}
		hooks.onResult = stream.add
	}
	run, runErr := runStatements(ctx, sess, stmts, hooks)

	// Settle the transaction before anything can exit: fail and canceled
	// call os.Exit, which skips the deferred Close. (Were it skipped anyway,
	// the connection closing with the process makes the server roll back.)
	var txErr error
	if flagTx {
		txErr = endTx(ctx, sess, runErr == nil && (stream == nil || stream.err == nil))
	}

	if stream != nil && stream.err != nil {
		// the run stopped because output did, not because a statement failed
		fail(stream.err, "render failed")
	}
	// output first: a failing statement 3 does not invalidate results 1 and 2.
	// A streamed run already wrote them, one at a time.
	if stream == nil && len(run.results) > 0 {
		emitRun(run.results, run.at, len(stmts), f)
	}
	switch {
	case runErr != nil && errors.Is(runErr, db.ErrCanceled):
		canceled("query")
	case runErr != nil:
		fail(runErr, "query failed")
	case txErr != nil:
		fail(txErr, "commit failed")
	case run.failed > 0:
		// every failure was reported as it happened (reportFailed); this is
		// the count, and the exit status a script can test
		fail(serr.New(fmt.Sprintf("%d of %d statements failed", run.failed, len(stmts))),
			"query failed")
	}
}

// checkRunFlags refuses the --tx and --keep-going runs that could not do what
// they promise. It runs before a connection is opened, so a refusal costs
// nothing and changes nothing.
//
//   - --tx with --keep-going: the pair contradicts itself. --tx means all or
//     nothing, so the first failure has to end the run. And going on would not
//     even work everywhere: after an error Postgres refuses every statement
//     until the transaction ends ("current transaction is aborted").
//   - --tx with a transaction statement in the buffer: dbc's BEGIN is already
//     open, and a second one is an error on SQLite, only a warning on
//     Postgres, and on MySQL an implicit COMMIT of everything so far — so the
//     run would be neither the user's transaction nor dbc's. A buffer that
//     manages its own transaction does not need --tx.
func checkRunFlags(stmts []sqlsplit.Stmt) error {
	if !flagTx {
		return nil
	}
	if flagKeep {
		return errors.New("--tx and --keep-going do not mix: a transaction is all or nothing, so the first failure ends it")
	}
	for i, st := range stmts {
		if txControl(st.Text) {
			return fmt.Errorf("--tx opens its own transaction, but statement %d/%d (%s) manages one — drop --tx, or the statement",
				i+1, len(stmts), strings.ToUpper(sqlsplit.FirstKeyword(st.Text)))
		}
	}
	return nil
}

// txControl reports whether stmt begins or ends a transaction, by its leading
// keyword — the statements that would fight --tx for control of the one it
// opens:
//
//	BEGIN, START TRANSACTION           open one (MySQL: commit the open one first)
//	COMMIT, END                        commit (END is Postgres's COMMIT)
//	ROLLBACK, ABORT                    roll back (ABORT is Postgres's ROLLBACK)
//	PREPARE TRANSACTION                Postgres two-phase: ends the transaction
//
// SAVEPOINT, RELEASE and ROLLBACK TO work inside a transaction without ending
// it, so they are fine under --tx. MySQL also commits implicitly before DDL
// (CREATE, ALTER, DROP …); that cannot be read off the SQL reliably and is
// left to the --tx documentation rather than guessed at here.
func txControl(stmt string) bool {
	switch sqlsplit.FirstKeyword(stmt) {
	case "begin", "commit", "end", "abort":
		return true
	case "rollback":
		return !sqlsplit.HasKeyword(stmt, "to")
	case "start", "prepare":
		return sqlsplit.HasKeyword(stmt, "transaction")
	}
	return false
}

// endTx commits the --tx transaction when the run succeeded (ok), and rolls
// it back otherwise. It returns only a COMMIT failure: a commit that fails
// (a deferred constraint, a serialization failure) has kept nothing, and the
// run must fail with it.
//
// The rollback runs under a context of its own. The run's context is dead
// after Ctrl+C, which is exactly when a rollback matters most. If the
// rollback itself fails, the deferred Session.Close still discards the
// connection, and a server rolls back whatever a closed connection left
// open — so the note says "rolled back" either way.
func endTx(ctx context.Context, sess *db.Session, ok bool) error {
	if ok {
		if _, err := sess.Run(ctx, "COMMIT"); err != nil {
			return err
		}
		return nil
	}
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := sess.Run(rbCtx, "ROLLBACK"); err != nil {
		logger.LogErr(err, "rollback failed — the connection is being dropped, which rolls back too")
	}
	fmt.Fprintln(os.Stderr, "note: transaction rolled back — nothing this run changed was kept")
	return nil
}

// reportFailed logs a statement that failed in a --keep-going run, as it
// happens, in the same shape fail uses for the one that ends a run. The
// error already carries the statement's position.
func reportFailed(_ int, err error) {
	logger.LogErr(err, "statement failed")
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

// runHooks is how a caller shapes a run of statements. The zero value runs
// every statement in order and stops at the first failure.
type runHooks struct {
	// onResult, when not nil, is handed each result (with its 0-based index)
	// as soon as its statement finishes, which is what lets a headless run
	// stream. An error from it stops the run before the next statement and
	// comes back as is — it is the caller's failure, not the statement's, so
	// it is not tagged with a position.
	onResult func(i int, r *model.Result) error
	// keepGoing goes on past a failed statement instead of stopping there
	// (--keep-going). It still stops for a failure that says the rest cannot
	// run: a cancel (Ctrl+C means stop) or a dead connection (see
	// Session.Classify) — every later statement would fail too, or worse, run
	// on a fresh connection without the state the earlier ones set up.
	keepGoing bool
	// onFail, when not nil, is told of each failure keepGoing passes over,
	// with the error already tagged with the statement's position.
	onFail func(i int, err error)
}

// runOutcome is what a run of statements produced.
type runOutcome struct {
	results []*model.Result
	at      []int // at[i] is results[i]'s 1-based position among the statements
	failed  int   // statements that failed and were passed over (keepGoing)
}

// runStatements executes stmts in order on one session, as h says (see
// runHooks). It returns the results that completed, with their positions,
// and the error that stopped the run, so the caller can still report the
// work that got done. A failure in a multi-statement run is tagged with the
// statement's position.
func runStatements(ctx context.Context, sess *db.Session, stmts []sqlsplit.Stmt,
	h runHooks) (runOutcome, error) {

	out := runOutcome{
		results: make([]*model.Result, 0, len(stmts)),
		at:      make([]int, 0, len(stmts)),
	}
	for i, st := range stmts {
		res, err := sess.Run(ctx, st.Text)
		if err != nil {
			if len(stmts) > 1 {
				err = serr.Wrap(err, "statement", fmt.Sprintf("%d/%d", i+1, len(stmts)))
			}
			if !h.keepGoing || errors.Is(err, db.ErrCanceled) || sess.Classify(err) != db.FaultNone {
				return out, err
			}
			out.failed++
			if h.onFail != nil {
				h.onFail(i, err)
			}
			continue
		}
		out.results = append(out.results, res)
		out.at = append(out.at, i+1)
		if h.onResult != nil {
			if err = h.onResult(i, res); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// streamsResults decides whether a headless query writes each result as it
// arrives rather than all of them at the end. All three must hold:
//
//   - more than one statement: a single result has nothing to stream ahead
//     of, and it renders bare (no banner), which a stream could not know to
//     do until the run was over
//   - stdout, not -o: the file is written whole, and its "wrote N rows" line
//     is a total
//   - a block format (export.Streamable): HTML and JSON are one document
//     wrapped around every result, so they cannot close until the last one
//
// A streamed document is byte for byte what the collected one would be: both
// number each result by its statement's place in the whole run ("2/5"), so a
// run that stops early or passes over a failure reads the same either way.
func streamsResults(f export.Format, stmts int) bool {
	return stmts > 1 && flagOut == "" && export.Streamable(f)
}

// blockStream writes the results of a multi-statement run one block at a
// time, as export.RenderAll would lay them out: a banner (where the format
// has one) and the result, blocks joined by export.BlockSep. Each write goes
// straight to out — os.Stdout is unbuffered — so a block shows as soon as its
// statement finishes. The truncation note for a block goes to notes (stderr)
// right after it, so it lands next to the result it is about.
//
// A script's results stream too (show), with total 0: a script shows as many
// results as it likes, so its banners read "#2" rather than "2/5", and the
// document is export.RenderOpen's rather than RenderAll's.
type blockStream struct {
	out, notes io.Writer
	f          export.Format
	// total is the statements in the run, which the banners count against;
	// 0 means open-ended (a script), numbering blocks "#i"
	total int
	wrote int // blocks written so far, to know when a separator is due
	// err is the render or write failure that stopped the stream, kept so the
	// caller can tell it from the statement failure runStatements returns
	err error
}

// add writes result i (0-based). Its signature fits runStatements' onResult.
func (b *blockStream) add(i int, r *model.Result) error {
	noun, pos := b.where(i + 1)
	block, err := export.RenderBlock(r, b.f, i+1, b.total)
	if err != nil {
		b.err = serr.Wrap(err, noun, pos)
		return b.err
	}
	if b.wrote > 0 {
		block = export.BlockSep + block
	}
	b.wrote++
	if _, err = io.WriteString(b.out, block); err != nil {
		b.err = serr.Wrap(err, noun, pos)
		return b.err
	}
	noteTruncated(b.notes, r, noun+" "+pos)
	return nil
}

// show writes a script's next result — the one s.Show just pushed — as the
// block after the last. sdb's show callback has no error to return, so a
// failed write is kept on b.err, reported once through stop (the caller
// cancels the script with it), and every later result is dropped: a stream
// that lost a block must not go on as if the numbering still held.
func (b *blockStream) show(r *model.Result, stop func()) {
	if b.err != nil {
		return
	}
	if b.add(b.wrote, r) != nil && stop != nil {
		stop()
	}
}

// where names block i (1-based) for errors and notes: the statement it came
// from in a query run ("statement", "2/5"), or its place among a script's
// results ("result", "#2").
func (b *blockStream) where(i int) (noun, pos string) {
	if b.total <= 0 {
		return "result", export.Pos(i, 0)
	}
	return "statement", export.Pos(i, b.total)
}

// emit renders the results to stdout, or to the --out file when one was given.
func emit(results []*model.Result, f export.Format) {
	emitRun(results, export.Seq(len(results)), len(results), f)
}

// emitRun is emit for a run of total statements in which not every one
// produced a result: at[i] is results[i]'s 1-based position, so the banners
// and notes name the statement they are about (see export.RenderRun).
func emitRun(results []*model.Result, at []int, total int, f export.Format) {
	out, err := export.RenderRun(results, at, total, f)
	if err != nil {
		fail(err, "render failed")
	}
	warnTruncated(os.Stderr, results, at, total)
	writeOut(out, results)
}

// writeOut sends a rendered document to stdout, or to the --out file with a
// line saying how much went there. results are what out was rendered from,
// for that count.
func writeOut(out string, results []*model.Result) {
	if flagOut == "" {
		fmt.Print(out)
		return
	}
	if err := os.WriteFile(flagOut, []byte(out), 0644); err != nil {
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
//
// at and total place each result in its run, as for emitRun.
func warnTruncated(w io.Writer, results []*model.Result, at []int, total int) {
	for i, r := range results {
		what := ""
		if total > 1 {
			what = "statement " + export.Pos(at[i], total)
		}
		noteTruncated(w, r, what)
	}
}

// noteTruncated writes warnTruncated's note for one result, if it hit the
// cap. what names the result — "statement 2/5", a script's "result #2" — or
// is "" for a lone result, which needs no name.
func noteTruncated(w io.Writer, r *model.Result, what string) {
	if !r.Truncated {
		return
	}
	if what != "" {
		what += ": "
	}
	fmt.Fprintf(w, "note: %sresult truncated at %d rows — raise max_rows in config for more\n",
		what, len(r.Rows))
}

// runScriptHeadless runs a Go script. The results it pushes with s.Show go
// out one of two ways:
//
//   - streamed, in a block format on stdout (scriptStreams): each result is
//     written the moment it is shown, so it lands in order with the s.Print
//     lines around it instead of after all of them. A script's result count
//     is open-ended, so each block's banner reads "#2", not "2/5" — and a
//     lone result carries one too, since the stream could not know it would
//     stay alone.
//   - collected, for -o, HTML, and JSON: rendered together when the script
//     returns, so -t json yields one array rather than a run of separate
//     documents, and -o writes one file. A block format collected is
//     export.RenderOpen's document — the bytes the stream would have written,
//     so `> file` and `-o file` agree.
func runScriptHeadless(mgr *db.Manager, path string, f export.Format) {
	ctx, stop := interruptible()
	defer stop()

	var results []*model.Result
	show := func(r *model.Result) { results = append(results, r) }
	var stream *blockStream
	if scriptStreams(f) {
		// total 0: open-ended, "#i" banners. A failed write cancels the
		// script's context, so its next query fails and it unwinds rather than
		// computing results nobody will see.
		stream = &blockStream{out: os.Stdout, notes: os.Stderr, f: f}
		show = func(r *model.Result) { stream.show(r, stop) }
	}
	logOut := scriptLog(f)

	s := sdb.New(mgr, show,
		func(msg string) { fmt.Fprintln(logOut, msg) },
	).WithContext(ctx)

	err := script.Run(path, s)
	if stream != nil && stream.err != nil {
		// checked before err: the script was canceled because output failed,
		// and "script canceled" would hide why
		fail(stream.err, "render failed")
	}
	// output first: a script that failed on its third query still showed two.
	// A streamed script already wrote them, one at a time.
	if len(results) > 0 {
		emitScript(results, f)
	}
	if err != nil {
		if errors.Is(err, db.ErrCanceled) {
			canceled("script")
		}
		fail(err, "script failed")
	}
}

// scriptStreams decides whether a headless script writes each result as it
// is shown: on stdout (-o writes one file, with a total in its "wrote" line)
// and in a block format (HTML and JSON are one document around every result).
// Unlike streamsResults there is no count to check — a script's is not known
// until it returns, and the "#i" banners do not need it.
func scriptStreams(f export.Format) bool {
	return flagOut == "" && export.Streamable(f)
}

// emitScript renders a script's collected results. A block format gets
// export.RenderOpen's "#i" document, the one a stream to stdout would have
// written; HTML and JSON render as a run of that many results, as they always
// have — collected, their count is known.
func emitScript(results []*model.Result, f export.Format) {
	if !export.Streamable(f) {
		emit(results, f)
		return
	}
	out, err := export.RenderOpen(results, f)
	if err != nil {
		fail(err, "render failed")
	}
	for i, r := range results {
		noteTruncated(os.Stderr, r, "result "+export.Pos(i+1, 0))
	}
	writeOut(out, results)
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
