package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/userdata"
)

// The scripts subcommand lists the Go scripts in scripts_dir, and says on
// stderr which directory that is. It is the shell's answer to "where does
// dbc look?", which before scripts_dir was resolved (config/scripts.go)
// depended on the directory dbc started in.
//
//	dbc scripts             name · modified · description, as a table
//	dbc scripts -t json     the same as a JSON array, for tooling
//
// The listing is a model.Result, so every -t format and -o work as they do
// for a query, through the same renderers. Nothing is connected: only the
// config is read.
func scriptsCommand() *cli.Command {
	return &cli.Command{
		Name:   "scripts",
		Usage:  "list the Go scripts in scripts_dir (run one with `dbc script NAME`)",
		Action: scriptsAction,
	}
}

func scriptsAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("scripts")
	if cmd.Args().Present() {
		usage("usage: dbc scripts")
	}
	demo, err := config.ParseDemoEngine(flagDemo)
	if err != nil {
		fail(err, "bad --demo engine")
	}
	// LoadDemo rather than setup: setup opens a db.Manager (and, with no
	// config, seeds a demo), none of which a listing needs.
	cfg, err := config.LoadDemo(flagConfig, demo)
	if err != nil {
		fail(err, "could not load config")
	}
	warnConfig(cfg)
	f := outFormat()

	infos, err := userdata.ListScripts(cfg.ScriptsDir)
	if err != nil {
		fail(err, "could not list scripts")
	}
	// stderr, so `-t json` on stdout stays one parseable document
	fmt.Fprintf(os.Stderr, "%d script(s) in %s\n", len(infos), cfg.ScriptsDir)
	emit([]*model.Result{scriptsResult(infos)}, f)
	return nil
}

// scriptsResult shapes the listing as a result. Rows hold the text forms;
// Raw the typed ones for JSON (size a number, modified an RFC 3339 time).
func scriptsResult(infos []userdata.ScriptInfo) *model.Result {
	r := &model.Result{Query: "dbc scripts", Columns: []string{"name", "modified", "size", "description"}}
	for _, in := range infos {
		mod := in.Mod.Local().Format("2006-01-02 15:04")
		r.Rows = append(r.Rows, []string{in.Name, mod, fmt.Sprint(in.Size), in.Desc})
		r.Raw = append(r.Raw, []any{in.Name, in.Mod.Format(time.RFC3339), in.Size, in.Desc})
	}
	return r
}
