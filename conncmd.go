package main

import (
	"context"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
)

// `dbc conns` lists the connections a run can name with -c (N-287): the
// config file's, the ones added in dbc web or the TUI (connections.toml),
// the ad-hoc --dsn one, or the built-in demos when there is no config.
// Before it, the names were found by opening the config file — and the
// saved ones not even there.
//
//	$ dbc conns
//	name      driver    default  from
//	--------  --------  -------  --------------------
//	local-pg  postgres  yes      config
//	lite      sqlite             added in dbc web/TUI
//
// No DSN is shown: a DSN may hold a password (or decrypt to one), and the
// list is the kind of thing pasted into a chat. --format applies, so a
// script can read it as JSON or CSV. Nothing is connected.
func connsCommand() *cli.Command {
	return &cli.Command{
		Name:    "conns",
		Aliases: []string{"connections"},
		Usage:   "list the connections -c can name (no DSNs shown)",
		Action:  connsAction,
	}
}

func connsAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("conns")
	if cmd.Args().Present() {
		usage("usage: dbc conns")
	}
	cfg, mgr := setup(demoLazy) // the saved and ad-hoc ones too; opens nothing
	defer mgr.Close()
	warnConfig(cfg)
	emit([]*model.Result{connsResult(cfg)}, outFormat())
	return nil
}

// connsResult is the list as a result, for any --format.
func connsResult(cfg *config.Config) *model.Result {
	def := defaultConnName(cfg)
	r := &model.Result{Conn: "config", Query: "dbc conns", Columns: []string{"name", "driver", "default", "from"}}
	for _, cn := range cfg.Conns() {
		isDef := ""
		if cn.Name == def {
			isDef = "yes"
		}
		row := []string{cn.Name, cn.Driver, isDef, connSource(cn)}
		raw := make([]any, len(row))
		for i, v := range row {
			raw[i] = v
		}
		r.Rows = append(r.Rows, row)
		r.Raw = append(r.Raw, raw)
	}
	return r
}

// defaultConnName is the connection a run with no -c uses (pickConn's
// order, without its usage error): $DBC_CONN, --dsn's, default_connection,
// or the only one.
func defaultConnName(cfg *config.Config) string {
	switch {
	case flagDSN != "":
		return adHocConnName
	case strings.TrimSpace(os.Getenv(connEnv)) != "":
		return strings.TrimSpace(os.Getenv(connEnv))
	case cfg.DefaultConnection != "":
		return cfg.DefaultConnection
	}
	if cs := cfg.Conns(); len(cs) == 1 {
		return cs[0].Name
	}
	return ""
}

// connSource says where a connection is defined.
func connSource(cn config.Connection) string {
	switch {
	case cn.Demo:
		return "built-in demo"
	case cn.Web:
		return "added in dbc web/TUI"
	case cn.Name == adHocConnName && flagDSN != "":
		return "--dsn"
	}
	return "config"
}
