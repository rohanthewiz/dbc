// dbc — a TUI database client with Go scripting.
//
//	dbc                            launch the TUI
//	dbc "SELECT * FROM cats"       run one query headless (uses -c / default conn)
//	dbc script scripts/loop.go     run a Go script headless
//
// Flags (before positional args):
//
//	-config path   config file (default: ./dbc.toml, ~/.config/dbc/config.toml)
//	-c name        connection name for a one-off query
//	-f format      headless output format: text|csv|tsv|markdown|html|json
//	-o file        write headless output to a file instead of stdout
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/sdb"
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

func runQueryHeadless(cfg *config.Config, mgr *db.Manager, stmt string) {
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
	res, err := mgr.Run(conn, stmt)
	if err != nil {
		fail(err, "query failed")
	}
	out, err := export.Render(res, f)
	if err != nil {
		fail(err, "render failed")
	}
	if *flagOut != "" {
		if err = os.WriteFile(*flagOut, []byte(out), 0644); err != nil {
			fail(serr.Wrap(err, "path", *flagOut), "write failed")
		}
		fmt.Printf("wrote %d rows to %s\n", len(res.Rows), *flagOut)
		return
	}
	fmt.Print(out)
}

func runScriptHeadless(mgr *db.Manager, path string) {
	s := sdb.New(mgr,
		func(r *model.Result) {
			fmt.Printf("-- %s │ %d rows in %s\n", r.Conn, len(r.Rows), r.Duration)
			fmt.Println(export.TextTable(r))
		},
		func(msg string) {
			fmt.Println(msg)
		})
	if err := script.Run(path, s); err != nil {
		fail(err, "script failed")
	}
}

func fail(err error, msg string) {
	logger.LogErr(err, msg)
	logger.CloseLog()
	os.Exit(1)
}
