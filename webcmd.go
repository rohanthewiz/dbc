package main

// dbc web — the workbench in a browser (package web).
//
//	dbc web                        serve on 127.0.0.1:8450 (or a free port) and open the browser
//	dbc web --no-open              … and only print the link
//	dbc web --listen 127.0.0.1:9000
//	dbc web --secret s3cret        a fixed secret instead of a fresh one ($DBC_WEB_SECRET)
//	dbc web --exit-on-eof          stop when stdin closes (hidden; the macOS app, macapp/)
//
// The terminal prints a login link carrying this launch's secret; opening it
// signs that browser in. Ctrl+C stops the server, canceling every tab's run
// and releasing its session (rolling back what it left open).

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/tui"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/web"
)

var (
	flagListen string
	flagNoOpen bool
	flagSecret string
	flagEOF    bool
)

func webCommand() *cli.Command {
	return &cli.Command{
		Name:  "web",
		Usage: "open the workbench in a browser (a local web server)",
		Description: "Serves dbc's workbench on loopback and opens the browser on it. Only a browser that " +
			"opened the printed link (or a client sending the secret as a Bearer token) can use it. " +
			"Tabs and layout are kept in ~/.config/dbc/web.bytdb. Connections added in the browser go to ~/.config/dbc/connections.toml, " +
			"which every dbc reads; query history and assistant conversations are shared with the TUI too.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Usage: "`ADDR` to listen on (default " + web.DefaultListen +
				", or a free port when it is taken)", Destination: &flagListen},
			&cli.BoolFlag{Name: "no-open", Usage: "print the link instead of opening the browser", Destination: &flagNoOpen},
			&cli.StringFlag{Name: "secret", Sources: cli.EnvVars("DBC_WEB_SECRET"),
				Usage: "the login `SECRET` (default: a fresh random one per launch)", Destination: &flagSecret},
			// plumbing for a wrapper that owns the process, not for people:
			// see exitOnEOF
			&cli.BoolFlag{Name: "exit-on-eof", Hidden: true, Destination: &flagEOF,
				Usage: "stop when stdin reaches EOF (the parent holding it has gone)"},
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
		Listen: flagListen,
		Secret: flagSecret,
		Store:  store,
		// the file setup merged: the browser's adds, edits and removals go there
		Conns:   config.OpenSaved(config.SavedFile()),
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
	if flagEOF {
		go exitOnEOF(os.Stdin, interruptSelf)
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

// exitOnEOF reads r until it ends, then calls stop. It is how the macOS app
// (macapp/DbcApp.swift) ties this server's life to its own: the app hands
// dbc web the read end of a pipe as stdin and keeps the write end. The
// kernel closes that end when the app exits by any road — Quit, a crash,
// Force Quit, kill -9 — so EOF arrives even when the app never had the
// chance to send a signal, and no orphaned server is left holding
// web.bytdb and its database sessions.
//
//	DbcApp ──(pipe, write end)──► dbc web stdin ── EOF ──► stop()
//
// Anything written into the pipe is discarded: only its end matters. A read
// error counts as the end too, since there is nothing left to wait for.
func exitOnEOF(r io.Reader, stop func()) {
	_, _ = io.Copy(io.Discard, r)
	stop()
}

// interruptSelf sends this process SIGTERM, which rweb already treats like
// Ctrl+C: the listener closes, Run returns and Shutdown releases every tab's
// session. Reusing that road keeps a single shutdown path rather than a
// second one to keep in step with it.
func interruptSelf() {
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}
