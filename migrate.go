package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/migrate"
	"github.com/rohanthewiz/dbc/model"
)

// The migrate subcommand. Its verbs are goose's, so a hand used to
// `goose postgres "$DSN" up` can type `dbc --driver postgres --dsn "$DSN" migrate up`
// and nothing else changes — the files, the version table, the meaning.
//
//	dbc migrate status            every migration with its applied date
//	dbc migrate version           highest applied version
//	dbc migrate up                apply all pending
//	dbc migrate up-by-one         apply the next one
//	dbc migrate up-to VERSION     apply pending up to and including VERSION
//	dbc migrate down              roll back the latest
//	dbc migrate down-to VERSION   roll back everything newer than VERSION
//	dbc migrate redo              down then up on the latest
//	dbc migrate create NAME       write a timestamped empty migration file
//
// The connection comes from -c, --dsn/--driver, or the config default, as for
// a headless query. The directory comes from --dir, else the connection's
// `migrations` setting in the config, else the working directory.
//
// `status` and `version` are results like any other: -t json works, so a
// deploy script can check the schema state without parsing text.
const migrateUsage = `usage: dbc [-c conn | --driver d --dsn s] [--dir path] migrate <command>

commands:
  status           show each migration and when it was applied
  version          print the highest applied version
  up               apply every pending migration
  up-by-one        apply the next pending migration
  up-to VERSION    apply pending migrations through VERSION
  down             roll back the most recent migration
  down-to VERSION  roll back migrations newer than VERSION (0 = all)
  redo             roll back the latest migration and apply it again
  create NAME      create an empty migration file in the migrations dir
`

func runMigrate(cfg *config.Config, mgr *db.Manager, args []string, f export.Format) {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(os.Stderr, migrateUsage)
		os.Exit(2)
	}
	cmd := args[0]
	conn := pickConn(cfg)
	dir := migrationsDir(cfg, conn)

	// create needs no database at all, so it goes before the connection opens
	if cmd == "create" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: dbc migrate create <name>")
			os.Exit(2)
		}
		path, err := migrate.Create(dir, strings.Join(args[1:], "_"))
		if err != nil {
			fail(err, "could not create migration")
		}
		fmt.Println("created", path)
		return
	}

	m := openMigrator(mgr, cfg, conn, dir)
	ctx, stop := interruptible()
	defer stop()

	var err error
	switch cmd {
	case "status":
		var sts []migrate.Status
		if sts, err = m.Statuses(ctx); err == nil {
			emit([]*model.Result{statusResult(conn, sts)}, f)
		}
	case "version":
		var v int64
		if v, err = m.Version(ctx); err == nil {
			emit([]*model.Result{{
				Conn: conn, Query: "migrate version",
				Columns: []string{"version"},
				Rows:    [][]string{{strconv.FormatInt(v, 10)}},
				Raw:     [][]any{{v}},
			}}, f)
		}
	case "up":
		_, err = m.Up(ctx)
	case "up-by-one":
		_, err = m.UpByOne(ctx)
	case "up-to":
		var v int64
		if v, err = versionArg(args); err == nil {
			_, err = m.UpTo(ctx, v)
		}
	case "down":
		_, err = m.Down(ctx)
	case "down-to":
		var v int64
		if v, err = versionArg(args); err == nil {
			_, err = m.DownTo(ctx, v)
		}
	case "redo":
		err = m.Redo(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate command %q\n\n%s", cmd, migrateUsage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			canceled("migration")
		}
		fail(err, "migrate "+cmd+" failed")
	}
}

// migrationsDir applies the precedence: flag, then the connection's config
// entry, then the working directory (which is what goose defaults to).
func migrationsDir(cfg *config.Config, conn string) string {
	if flagDir != "" {
		return flagDir
	}
	if cc, ok := cfg.ConnByName(conn); ok && cc.Migrations != "" {
		return cc.Migrations
	}
	return "."
}

// openMigrator loads the migration files and opens the connection, failing
// with the file problem first: a bad migration file should not need a
// reachable database to be reported.
func openMigrator(mgr *db.Manager, cfg *config.Config, conn, dir string) *migrate.Migrator {
	migs, err := migrate.Load(dir)
	if err != nil {
		fail(err, "could not load migrations")
	}
	if len(migs) == 0 {
		fmt.Fprintf(os.Stderr, "no migrations found in %s\n", dir)
		os.Exit(2)
	}
	cc, _ := cfg.ConnByName(conn)
	d, err := migrate.DialectFor(cc.Driver)
	if err != nil {
		fail(err, "conn "+conn)
	}
	dbh, err := mgr.DB(conn)
	if err != nil {
		fail(err, "could not open connection")
	}
	m := migrate.NewWith(dbh, d, migs)
	m.AllowMissing = flagMissing
	m.Log = func(s string) { fmt.Println(s) }
	return m
}

func versionArg(args []string) (int64, error) {
	if len(args) < 2 {
		return 0, serr.New("usage: dbc migrate " + args[0] + " <version>")
	}
	v, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || v < 0 {
		return 0, serr.New("version must be a non-negative integer", "got", args[1])
	}
	return v, nil
}

// statusResult shapes the status report as a Result so it renders through the
// ordinary exporters — a text table on a terminal, JSON for a script.
func statusResult(conn string, sts []migrate.Status) *model.Result {
	r := &model.Result{
		Conn: conn, Query: "migrate status",
		Columns: []string{"version", "applied", "applied_at", "migration"},
	}
	for _, s := range sts {
		state := "pending"
		if s.Applied {
			state = "applied"
		}
		r.Rows = append(r.Rows, []string{strconv.FormatInt(s.Version, 10), state, s.AppliedAt, s.Name})
		r.Raw = append(r.Raw, []any{s.Version, s.Applied, s.AppliedAt, s.Name})
	}
	return r
}
