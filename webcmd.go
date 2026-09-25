package main

// dbc web — the workbench in a browser (package web).
//
//	dbc web                        serve on 127.0.0.1:8450 (or a free port) and open the browser
//	dbc web --no-open              … and only print the link
//	dbc web --listen 127.0.0.1:9000
//	dbc web --secret s3cret        a fixed secret instead of a fresh one ($DBC_WEB_SECRET)
//
// The terminal prints a login link carrying this launch's secret; opening it
// signs that browser in. Ctrl+C stops the server, canceling every tab's run
// and releasing its session (rolling back what it left open).

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/tui"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/web"
)

var (
	flagListen string
	flagNoOpen bool
	flagSecret string
)

func webCommand() *cli.Command {
	return &cli.Command{
		Name:  "web",
		Usage: "open the workbench in a browser (a local web server)",
		Description: "Serves dbc's workbench on loopback and opens the browser on it. Only a browser that " +
			"opened the printed link (or a client sending the secret as a Bearer token) can use it. " +
			"Tabs and layout are kept in ~/.config/dbc/web.bytdb; query history and assistant conversations are shared with the TUI.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Usage: "`ADDR` to listen on (default " + web.DefaultListen +
				", or a free port when it is taken)", Destination: &flagListen},
			&cli.BoolFlag{Name: "no-open", Usage: "print the link instead of opening the browser", Destination: &flagNoOpen},
			&cli.StringFlag{Name: "secret", Sources: cli.EnvVars("DBC_WEB_SECRET"),
				Usage: "the login `SECRET` (default: a fresh random one per launch)", Destination: &flagSecret},
		},
		Action: webAction,
	}
}

func webAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("web")
	if cmd.Args().Len() > 0 {
		usage("usage: dbc web [--listen ADDR] [--no-open] [--secret S]")
	}
	if flagListen != "" && !loopback(flagListen) {
		fmt.Fprintln(os.Stderr, "warning: listening on "+flagListen+
			" makes dbc web reachable from the network, over plain HTTP; the secret is all that stands guard")
	}

	// The browser lists every connection, like the TUI, so every demo is
	// opened up front and one that cannot be is dropped with a warning.
	cfg, mgr := setup(demoAll)
	defer mgr.Close()
	warnConfig(cfg)

	store, err := web.OpenStore(web.StateFile())
	if err != nil {
		// another dbc web holds the file, most likely: run without it
		fmt.Fprintf(os.Stderr, "warning: tabs and layout will not be saved this session (%v)\n", err)
	}
	defer store.Close()

	srv, err := web.New(cfg, mgr, web.Options{
		Listen:  flagListen,
		Secret:  flagSecret,
		Store:   store,
		History: userdata.LoadHistory(userdata.HistoryFile()),
		// the TUI's archive, so a conversation had in either is offered in both
		ChatsDir: userdata.ChatsDir(),
		Ready: func(login string) {
			fmt.Fprintf(os.Stderr, "dbc web is serving — open this link to sign in:\n\n  %s\n\nCtrl+C stops it.\n", login)
			if !flagNoOpen {
				tui.OpenURL(login)
			}
		},
		Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	})
	if err != nil {
		fail(err, "could not start dbc web")
	}
	if err = srv.Run(); err != nil {
		fail(err, "dbc web stopped")
	}
	return nil
}

// loopback reports whether addr names a loopback host.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
