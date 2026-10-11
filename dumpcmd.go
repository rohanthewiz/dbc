package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/pgdump"
)

// The dump subcommand: a Postgres connection dumped with pg_dump, headless.
//
//	dbc dump -c prod -o prod.sql                     plain SQL, for psql
//	dbc dump -c prod > prod.sql                      … to stdout
//	dbc dump -c prod -t custom -o prod.dump          one archive, for pg_restore
//	dbc dump -c prod -t directory -j 4 -o prod.d     a file per table, 4 tables at once
//	dbc dump -c prod -t split -j 4 -o prod-sql       plain SQL, a file per table (see pgdump/split.go)
//	dbc dump -c prod/analytics --schema-only -o s.sql   another database on the server
//	dbc dump -c prod --table 'sales.*' --exclude-table-data audit_log -o sales.sql
//	dbc dump -c prod -o x.sql -- --no-comments       any other pg_dump option, after --
//	dbc dump -c prod -t split -o out --dry-run       the commands, not run
//
// The connection is the configured one — DSN, tls keys, derived database —
// restated for libpq and handed to pg_dump in a private service file and
// PGPASSWORD (db.LibpqFor), so no password shows in `ps`. The format is the
// global -t, as for erd: "text", its default, is plain SQL.
//
// pg_dump must be at least the server's major version. dbc asks the server
// its version and picks the first pg_dump new enough: --pg-bin (or
// $DBC_PG_BIN, or the config's pg_bin) if given, else PATH, else Homebrew's,
// Postgres.app's, Debian's and RHEL's usual install directories
// (pgdump.Locate) — and when none of those is new enough (and no directory
// was named), the server's own image in Docker, postgres:<major>, when
// Docker answers (pgdump.LocateOrDocker).
//
// Messages: pg_dump's own on stderr as they come; dbc's progress on stderr
// when it is a terminal; with -o, a summary line on stdout.
//
// Exit status: 0 dumped, 1 the dump failed (or no usable pg_dump), 2 bad
// usage (a non-Postgres connection included), 130 Ctrl+C.

var (
	flagDumpJobs         int
	flagDumpSchemaOnly   bool
	flagDumpDataOnly     bool
	flagDumpSchemas      []string
	flagDumpExclSchemas  []string
	flagDumpTables       []string
	flagDumpExclTables   []string
	flagDumpExclData     []string
	flagDumpClean        bool
	flagDumpIfExists     bool
	flagDumpCreate       bool
	flagDumpNoOwner      bool
	flagDumpNoPrivileges bool
	flagDumpInserts      bool
	flagDumpColInserts   bool
	flagDumpCompress     string
	flagDumpEncoding     string
	flagDumpKeepArchive  bool
	flagDumpPGBin        string
	flagDumpDryRun       bool
)

func dumpCommand() *cli.Command {
	return &cli.Command{
		Name:      "dump",
		Usage:     "dump a Postgres database with pg_dump: one file, an archive, or a file per table",
		ArgsUsage: "[-- pg_dump options…]",
		Description: "Runs pg_dump against the Postgres connection -c names (TLS settings and NAME/otherdb included). " +
			"-t picks the format: plain (the default) is one SQL file for psql, or stdout without -o; custom and tar " +
			"are one archive for pg_restore; directory is a file per table for pg_restore, and the only pg_dump " +
			"format that dumps several tables at once (--jobs); split is plain SQL with a file per table plus a " +
			"restore.sql that psql runs, made from one consistent directory dump. pg_dump options dbc has no flag " +
			"for go after --. pg_dump must be at least the server's version: dbc looks on PATH and in the usual " +
			"install directories for one, or uses --pg-bin.",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "jobs", Aliases: []string{"j"},
				Usage: "dump `N` tables at once (-t directory or split), on one shared snapshot", Destination: &flagDumpJobs},
			&cli.BoolFlag{Name: "schema-only", Usage: "only the definitions, no rows", Destination: &flagDumpSchemaOnly},
			&cli.BoolFlag{Name: "data-only", Usage: "only the rows, no definitions", Destination: &flagDumpDataOnly},
			&cli.StringSliceFlag{Name: "schema", Usage: "only schemas matching `PATTERN` (repeatable)",
				Destination: &flagDumpSchemas},
			&cli.StringSliceFlag{Name: "exclude-schema", Usage: "leave out schemas matching `PATTERN` (repeatable)",
				Destination: &flagDumpExclSchemas},
			&cli.StringSliceFlag{Name: "table", Usage: "only tables matching `PATTERN`, e.g. 'sales.*' (repeatable)",
				Destination: &flagDumpTables},
			&cli.StringSliceFlag{Name: "exclude-table", Usage: "leave out tables matching `PATTERN` (repeatable)",
				Destination: &flagDumpExclTables},
			&cli.StringSliceFlag{Name: "exclude-table-data",
				Usage:       "keep the definition but not the rows of tables matching `PATTERN` (repeatable)",
				Destination: &flagDumpExclData},
			&cli.BoolFlag{Name: "clean", Usage: "drop objects before creating them (plain; split with --create)",
				Destination: &flagDumpClean},
			&cli.BoolFlag{Name: "if-exists", Usage: "with --clean: DROP … IF EXISTS", Destination: &flagDumpIfExists},
			&cli.BoolFlag{Name: "create", Usage: "begin with CREATE DATABASE and connect to it (plain, split)",
				Destination: &flagDumpCreate},
			&cli.BoolFlag{Name: "no-owner", Usage: "no ALTER … OWNER: objects belong to whoever restores (plain, split)",
				Destination: &flagDumpNoOwner},
			&cli.BoolFlag{Name: "no-privileges", Usage: "no GRANT / REVOKE", Destination: &flagDumpNoPrivileges},
			&cli.BoolFlag{Name: "inserts", Usage: "rows as INSERT statements rather than COPY (slower to restore)",
				Destination: &flagDumpInserts},
			&cli.BoolFlag{Name: "column-inserts", Usage: "rows as INSERTs that name their columns",
				Destination: &flagDumpColInserts},
			&cli.StringFlag{Name: "compress",
				Usage:       "compression `LEVEL` or METHOD[:DETAIL] of an archive (zstd:3, lz4 and gzip need pg_dump 16+)",
				Destination: &flagDumpCompress},
			&cli.StringFlag{Name: "encoding", Usage: "write the dump in `ENCODING` (default: the database's)",
				Destination: &flagDumpEncoding},
			&cli.BoolFlag{Name: "keep-archive",
				Usage: "-t split: keep the directory archive the SQL was made from, as OUT/archive", Destination: &flagDumpKeepArchive},
			&cli.StringFlag{Name: "pg-bin", Sources: cli.EnvVars("DBC_PG_BIN"),
				Usage:       "`DIR` holding pg_dump (and pg_restore, for split), e.g. /opt/homebrew/opt/libpq/bin",
				Destination: &flagDumpPGBin},
			&cli.BoolFlag{Name: "dry-run", Usage: "print the pg_dump command line(s) instead of running them",
				Destination: &flagDumpDryRun},
		},
		Action: dumpAction,
	}
}

func dumpAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("dump")
	format, err := pgdump.ParseFormat(flagFormat)
	if err != nil {
		usage(err.Error())
	}
	// Everything after the flags is for pg_dump. A first word that is not
	// an option is far more likely a database name meant for dbc ("dbc dump
	// mydb") than something pg_dump should get.
	extra := cmd.Args().Slice()
	if len(extra) > 0 && !strings.HasPrefix(extra[0], "-") {
		usage(fmt.Sprintf("unexpected %q — pick the connection with -c NAME (NAME/otherdb for another database "+
			"on its server); pg_dump options go after --", extra[0]))
	}
	opts := pgdump.Options{
		Format: format, Out: flagOut, Jobs: flagDumpJobs,
		SchemaOnly: flagDumpSchemaOnly, DataOnly: flagDumpDataOnly,
		Schemas: flagDumpSchemas, ExcludeSchemas: flagDumpExclSchemas,
		Tables: flagDumpTables, ExcludeTables: flagDumpExclTables, ExcludeData: flagDumpExclData,
		Clean: flagDumpClean, IfExists: flagDumpIfExists, Create: flagDumpCreate,
		NoOwner: flagDumpNoOwner, NoPrivileges: flagDumpNoPrivileges,
		Inserts: flagDumpInserts, ColumnInserts: flagDumpColInserts,
		Compress: flagDumpCompress, Encoding: flagDumpEncoding,
		KeepArchive: flagDumpKeepArchive, Extra: extra,
	}
	// Everything checkable is checked before anything connects, as erd and
	// explain do: a mistake should cost nothing.
	if err = opts.Check(); err != nil {
		usage(err.Error())
	}
	if format.ToDir() && !flagDumpDryRun {
		if err = pgdump.CheckOutDir(flagOut); err != nil {
			usage(err.Error())
		}
	}
	if format.Binary() && flagOut == "" && !flagDumpDryRun && term.IsTerminal(os.Stdout.Fd()) {
		usage(fmt.Sprintf("a %s dump is binary — write it with -o, or pipe it", format))
	}

	// Postgres only, which the config says without opening anything: no
	// demo is opened (demoLazy), as none is ever Postgres.
	cfg, mgr := setup(demoLazy)
	defer mgr.Close()
	warnConfig(cfg)
	conn := pickConn(cfg)
	lc, err := pgdump.Target(cfg, conn)
	switch {
	case errors.Is(err, pgdump.ErrNoConn), errors.Is(err, pgdump.ErrNotPostgres):
		mgr.Close() // usage exits; the deferred Close would not run
		usage(err.Error())
	case err != nil:
		fail(err, "cannot hand this connection to pg_dump")
	}
	if len(lc.Dropped) > 0 {
		fmt.Fprintf(os.Stderr, "note: pg_dump connects without the DSN's %s (session settings libpq does not take)\n",
			strings.Join(lc.Dropped, ", "))
	}

	c, stop := interruptible()
	defer stop()
	// The server's version picks the pg_dump (see pgdump.Locate), and
	// asking it is also the connection check: a refused password is better
	// said by dbc, once, than by every pg_dump worker. A dry run connects
	// to nothing and takes the first pg_dump found.
	major := 0
	if !flagDumpDryRun {
		if major, err = pgdump.ServerMajor(c, mgr, conn); err != nil {
			if errors.Is(err, db.ErrCanceled) || c.Err() != nil {
				canceled("dump")
			}
			fail(err, "could not reach the server")
		}
	}
	// no local pg_dump new enough for the server: the server's image in
	// Docker, when Docker answers (pgdump/docker.go)
	tools, err := pgdump.LocateOrDocker(c, pgdump.BinDir(flagDumpPGBin, cfg), major, format == pgdump.Split)
	if err != nil {
		if flagDumpDryRun {
			// still worth showing the command: the binary is a detail
			tools = pgdump.Tools{Dump: "pg_dump", Restore: "pg_restore"}
		} else {
			fail(err, "no usable pg_dump")
		}
	}

	progress := func(string) {}
	if term.IsTerminal(os.Stderr.Fd()) {
		progress = func(s string) { fmt.Fprintln(os.Stderr, s) }
	}
	run := &pgdump.Run{Name: conn, ServerMajor: major, Tools: tools, Conn: lc, Opts: opts,
		Stdout: os.Stdout, Stderr: os.Stderr, Progress: progress}

	if flagDumpDryRun {
		// The connection is shown as the service file holds it, less
		// sslpassword — the password never went into it.
		var shown []string
		for _, kv := range lc.Settings {
			if kv[0] != "sslpassword" {
				shown = append(shown, kv[0]+"="+kv[1])
			}
		}
		pw := ""
		if lc.Password != "" {
			pw = "; the password in PGPASSWORD"
		}
		fmt.Printf("# service %s: %s%s\n", "dbc", strings.Join(shown, " "), pw)
		for _, l := range run.Commands() {
			fmt.Println(l)
		}
		return nil
	}

	progress(fmt.Sprintf("dumping %s (PostgreSQL %d) with pg_dump %s from %s", conn, major, tools.Version, tools.Dump))
	if err = run.Do(c); err != nil {
		if c.Err() != nil {
			canceled("dump")
		}
		fail(err, "dump failed")
	}
	if flagOut != "" {
		fmt.Printf("dumped %s (%s) to %s — restore with %s%s\n", conn, format, flagOut, pgdump.RestoreHint(format, flagOut), run.RestoreNote())
	}
	return nil
}
