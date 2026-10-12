package tutorial

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Lessons is the tutorial, one lesson per doc section. Titles are the
// doc's headings exactly (TestLessonsMatchDocHeadings), and Files are the
// embedded copies of its code blocks (TestWorkspaceMatchesDoc).
//
// Commands are the doc's, in an order that works on a fresh workspace:
// §6 runs the pipeline before it previews one fragment, because that
// fragment's lookup reads a table the earlier fragment writes (the doc's
// own warning). Checks look at state only (tables, files, run records),
// never at output text, which carries timings and run ids.
func Lessons() []Lesson {
	return []Lesson{
		{
			N:     0,
			Title: "Set up a workspace",
			Summary: `A workspace is a directory with a dbc.toml. Every *_dir setting in it is
relative to the file, so scripts, pipelines, jobs, run records and output
files all stay together. It defines two connections on embedded engines
(no server needed): shop (SQLite) and warehouse (bytdb). Then seed the
shop: the emails are messy on purpose, and the later lessons clean them.`,
			Files: []string{"dbc.toml", "seed.sql"},
			Commands: []Command{
				{Args: []string{"-f", "seed.sql"}, Note: "runs on default_connection (shop)"},
				{Args: []string{"-t", "markdown", "SELECT * FROM orders"}},
			},
			Checks: []Check{
				{What: "shop has 4 customers", Test: rowsEqual("shop", "customers", 4),
					Hint: "run `dbc -f seed.sql` here. If it says a table exists with other rows, delete shop.db and seed again"},
				{What: "shop has 6 orders", Test: rowsEqual("shop", "orders", 6),
					Hint: "run `dbc -f seed.sql` here. If it says a table exists with other rows, delete shop.db and seed again"},
			},
		},
		{
			N:     1,
			Title: "Your first script",
			Summary: `A script is a Go file with one function, Run(s *sdb.S) error, interpreted
(no compile step; the standard library, no third-party imports). s.Query
takes a connection name, s.Print writes progress, and s.Show emits a
result. The comment above "package main" is the script's description.
Run it by name: dbc looks in scripts_dir.`,
			Files: []string{"scripts/order_summary.go"},
			Commands: []Command{
				{Args: []string{"script", "order_summary"}},
				{Args: []string{"scripts"}, Note: "your scripts, then the built-in examples"},
			},
			Checks: []Check{
				{What: "scripts/order_summary.go checks clean",
					Test: all(filesExist("scripts/order_summary.go"), clean("script", "--check", "order_summary")),
					Hint: "write scripts/order_summary.go (step 1 --run writes it); `dbc script --check order_summary` shows what's wrong"},
			},
		},
		{
			N:     2,
			Title: "Reports and files",
			Summary: `s.Export writes a result to a file. A relative path lands in files_dir
(here data/), never the shell's directory, so a script gives the same result
from a shell, the TUI, dbc web or a schedule. Use s.Path for your own files.
A script takes no arguments: knobs are constants at the top, a settings
file, or an environment variable.`,
			Files: []string{"scripts/region_report.go"},
			Commands: []Command{
				{Args: []string{"script", "region_report"}},
			},
			Checks: []Check{
				{What: "the region CSVs and the HTML overview are in data/reports/",
					Test: filesExist("data/reports/orders_amer.csv", "data/reports/orders_emea.csv",
						"data/reports/orders_none.csv", "data/reports/overview.html"),
					Hint: "run `dbc script region_report`; relative paths land in files_dir (data/), not the current directory"},
			},
		},
		{
			N:     3,
			Title: "Check before you run",
			Summary: `dbc script --check parses and compiles a script without connecting to
anything, checks Run's signature, and lints for interpreter bugs. It never
runs the script, so it's safe on scripts that write data, and it works as a
CI gate. bad.go shows the pitfall it warns about: a two-value assignment
into a map element, which the interpreter silently drops.`,
			Files: []string{"bad.go"},
			Commands: []Command{
				{Args: []string{"script", "--check", "order_summary", "region_report"}, Note: "silent when clean; exit 1 on an error"},
				{Args: []string{"script", "--check", "bad.go"}, Note: "a warning, not an error: exit 0"},
			},
			Checks: []Check{
				{What: "every script in scripts/ checks clean", Test: scriptsClean,
					Hint: "`dbc script --check NAME` names the file, line and column of each problem"},
			},
		},
		{
			N:     4,
			Title: "One-command ETL: `dbc copy`",
			Summary: `dbc copy moves one table between connections, across engines, with no
script. --create makes the destination table, --truncate empties it first,
--where filters on the source, and a second name renames it. The load is
one transaction: a copy that fails leaves the destination exactly as it was.`,
			Commands: []Command{
				{Args: []string{"copy", "--from", "shop", "--to", "warehouse", "--create", "orders"}},
				{Args: []string{"copy", "--from", "shop", "--to", "warehouse", "--create", "--truncate",
					"--where", "status = 'paid'", "orders", "paid_orders"}},
				{Args: []string{"copy", "--from", "shop", "--to", "warehouse", "orders"}, ExpectFail: true,
					Note: "fails on purpose: the rows are there already, so the primary key refuses them"},
				{Args: []string{"-c", "warehouse", "-t", "csv", "SELECT count(*) FROM orders"},
					Note: "still exactly 6: the failed copy changed nothing"},
			},
			Checks: []Check{
				{What: "warehouse has 6 orders", Test: rowsEqual("warehouse", "orders", 6),
					Hint: "run `dbc copy --from shop --to warehouse --create orders`; add --truncate to reload a table that has rows"},
				{What: "warehouse has 4 paid_orders", Test: rowsEqual("warehouse", "paid_orders", 4),
					Hint: "run the copy with --where \"status = 'paid'\" orders paid_orders"},
			},
		},
		{
			N:     5,
			Title: "ETL in a script: `Copy`, `Reader`, `Writer`",
			Summary: `Three levels of control. s.Copy copies a table or query, with an optional
per-row Transform. s.Reader streams rows uncapped and typed. s.Writer loads
rows you produce, all in one transaction that commits at Close. The script
cleans the customers' emails on the way in, then joins warehouse lookups to
shop facts — a join no single server can run. The "DDL …" lines are the DDL
log: every CREATE, TRUNCATE and the like a script runs.`,
			Files: []string{"scripts/load_warehouse.go"},
			Commands: []Command{
				{Args: []string{"script", "load_warehouse"}},
				{Args: []string{"-c", "warehouse", "-t", "markdown", "SELECT * FROM customers ORDER BY id"}},
			},
			Checks: []Check{
				{What: "warehouse has 4 clean customers (trimmed, lower-case emails; no NULL region)",
					Test: all(rowsEqual("warehouse", "customers", 4), cleanCustomers),
					Hint: "run `dbc script load_warehouse`; its Transform trims and lower-cases email and fills a NULL region"},
				{What: "warehouse has 6 order_facts", Test: rowsEqual("warehouse", "order_facts", 6),
					Hint: "run `dbc script load_warehouse`; its Writer loads order_facts"},
			},
		},
		{
			N:     6,
			Title: "Pipelines: ETL as data",
			Summary: `A pipeline describes ETL as data: fragments, run in order, each one
source → transforms → sinks (or one action). In exchange for writing JSON
you get parameters (-p key=value), previews that write nothing, validation
before anything runs, run records, and a canvas in dbc web. shop-sync loads
clean customers, gates on the result, exports paid orders to a dated CSV
and records the export in an audit table.`,
			Files: []string{"pipelines/shop-sync.json"},
			Commands: []Command{
				{Args: []string{"pipelines"}, Note: "yours, then the examples"},
				{Args: []string{"pipeline", "check", "shop-sync"}, Note: "silent when clean; exit 1 on an error"},
				{Args: []string{"pipeline", "run", "shop-sync", "-p", "min_total=30"}},
				{Args: []string{"pipeline", "run", "shop-sync", "--preview", "2", "--fragment", "big_orders"},
					Note: "writes nothing; after the real run, since its lookup reads dim_customer"},
			},
			Checks: []Check{
				{What: "shop-sync checks clean", Test: clean("pipeline", "check", "shop-sync"),
					Hint: "`dbc pipeline check shop-sync` names the node and field that's wrong"},
				{What: "warehouse has 4 rows in dim_customer", Test: rowsEqual("warehouse", "dim_customer", 4),
					Hint: "run `dbc pipeline run shop-sync`; a preview writes nothing"},
				{What: "the export is logged in load_log", Test: rowsAtLeast("warehouse", "load_log", 1),
					Hint: "run `dbc pipeline run shop-sync`; its log fragment writes load_log"},
				{What: "a dated CSV is in data/exports/", Test: fileMatches("data/exports/big_orders_*.csv"),
					Hint: "run `dbc pipeline run shop-sync`; csv.write's relative path lands in files_dir (data/)"},
			},
		},
		{
			N:     7,
			Title: "Your own node kind: a plugin",
			Summary: `A plugin is a Go file in plugins_dir: a descriptor (sdb.Plugin) and plain
functions for its kind — no interface, no build step. order.tier is a
transform that grades a numeric column small, medium or large. Once
dbc plugins --check is clean it's a node kind like any built-in.`,
			Files: []string{"plugins/order_tier.go", "pipelines/order-tiers.json"},
			Commands: []Command{
				{Args: []string{"plugins", "--check"}, Note: "every file in plugins_dir; exit 1 on an error"},
				{Args: []string{"pipeline", "run", "order-tiers"}},
			},
			Checks: []Check{
				{What: "plugins/ checks clean", Test: clean("plugins", "--check"),
					Hint: "`dbc plugins --check` prints path:line:col: msg for each problem"},
				{What: "order.tier is a node kind", Test: pluginListed("order.tier"),
					Hint: "write plugins/order_tier.go (step 7 --run writes it); plugins_dir is plugins/"},
				{What: "order-tiers checks clean", Test: clean("pipeline", "check", "order-tiers"),
					Hint: "`dbc pipeline check order-tiers` names what's wrong"},
			},
		},
		{
			N:     8,
			Title: "Jobs: pipelines in a dependency graph",
			Summary: `A job runs pipelines as a DAG with one root: a step starts when every step
in its "after" has succeeded. nightly-shop rebuilds the warehouse first,
then runs the export and the revenue report side by side. Jobs run by hand,
on a cron schedule (while dbc web runs), from a webhook, from the system
crontab via dbc job run, or from a script. script.run makes any saved
script a pipeline step.`,
			Files: []string{"pipelines/warehouse-facts.json", "pipelines/revenue-report.json", "jobs/nightly-shop.json"},
			Commands: []Command{
				{Args: []string{"job", "check", "nightly-shop"}, Note: "the DAG, the cron lines, and every pipeline it runs"},
				{Args: []string{"jobs"}, Note: "schedule, next fire, last run"},
				{Args: []string{"job", "run", "nightly-shop", "-p", "min_total=100"}},
			},
			Checks: []Check{
				{What: "nightly-shop checks clean", Test: clean("job", "check", "nightly-shop"),
					Hint: "`dbc job check nightly-shop` checks the job and every pipeline it runs"},
				{What: "nightly-shop has a succeeded run", Test: runRecord("nightly-shop", ""),
					Hint: "run `dbc job run nightly-shop`; `dbc runs --job nightly-shop` lists its runs"},
				{What: "the revenue report is in data/exports/", Test: fileMatches("data/exports/revenue_*.csv"),
					Hint: "run `dbc job run nightly-shop`; its revenue step writes the CSV"},
			},
		},
		{
			N:     9,
			Title: "Orchestrating from a script",
			Summary: `A script can do everything above with Go control flow around it: run a
saved pipeline by name with params, build one in code (ThenFunc is a
transform written as a Go func — the one thing JSON can't express), and
run a whole job and wait for it.`,
			Files: []string{"scripts/orchestrate.go"},
			Commands: []Command{
				{Args: []string{"script", "orchestrate"}},
			},
			Checks: []Check{
				{What: "scripts/orchestrate.go checks clean",
					Test: all(filesExist("scripts/orchestrate.go"), clean("script", "--check", "orchestrate")),
					Hint: "`dbc script --check orchestrate` shows what's wrong"},
				{What: "the script ran nightly-shop", Test: runRecord("nightly-shop", "script"),
					Hint: "run `dbc script orchestrate`; its s.RunJob leaves a run record with trigger script"},
			},
		},
		{
			N:     10,
			Title: "Run records",
			Summary: `Every pipeline or job run, from any process (a shell, cron, dbc web's
scheduler, the TUI), leaves a JSON record in runs_dir. dbc reads them back
as a list, as a tree (dbc run show ID), or as SQL tables: runs, pipelines,
fragments and nodes, so "which node got slow this week?" is one query.`,
			Commands: []Command{
				{Args: []string{"runs", "--limit", "5"}},
				{Args: []string{"runs", "--sql", "SELECT step, status, seconds, rows FROM pipelines ORDER BY seconds DESC LIMIT 5"}},
			},
			Checks: []Check{
				{What: "runs/ has run records", Test: hasRuns,
					Hint: "run any pipeline or job (lesson 6 or 8); each run leaves a record in runs/"},
			},
		},
		{
			N:     11,
			Title: "The same things in the UIs",
			Summary: `Everything above is plain files, so the TUI and the browser workbench see
it at once. Start one in this directory:

                    TUI (dbc)                        Browser (dbc web)
  Scripts           Ctrl+O: Enter runs, e edits      Ctrl+O: a Go tab, live checking
  Pipelines, jobs   Ctrl+J: run, edit, monitor       canvases: drag nodes, ▶ Run
  Runs              Alt+J                            Alt+R: DAG → fragments → nodes
  Schedules         —                                cron triggers fire while it runs`,
		},
		{
			N:     12,
			Title: "Pitfalls and quick fixes",
			Summary: `  A map is missing values      v, _ := x.(T); m[k] = v   (--check warns)
  go.transform returns no rows  v := s + "!"; b.Rows[i][c] = v
  os.Getenv empty in dbc web    set it before starting dbc web, or use params
  A file isn't where expected   relative paths are in files_dir: s.Path, $PWD/… in -p
  Preview fails at a lookup     previews write nothing: run once, or --fragment
  bytdb refuses a copy          create the destination first, with a primary key
  A Query result is cut off     s.Query is capped at max_rows: s.Reader streams
  Placeholder errors            ? for SQLite/MySQL; $1 for Postgres/bytdb
  Third-party import fails      stdlib + sdb only: write a plugin, or use SQL`,
		},
	}
}

// cleanCustomers passes when every warehouse customer's email is trimmed
// and lower case and none has a NULL region: what load_warehouse's
// Transform does. Checked in Go, not SQL, so it doesn't depend on which
// string functions bytdb has.
func cleanCustomers(e *Env) error {
	rows, err := e.query("warehouse", "SELECT id, email, region FROM customers ORDER BY id")
	if err != nil {
		return err
	}
	for _, r := range rows {
		email, _ := r["email"].(string)
		if email != strings.ToLower(strings.TrimSpace(email)) {
			return fmt.Errorf("customer %v's email %q isn't trimmed and lower case", r["id"], email)
		}
		if r["region"] == nil {
			return fmt.Errorf("customer %v has a NULL region", r["id"])
		}
	}
	return nil
}

// pluginListed passes when `dbc plugins -t json` lists the node kind.
func pluginListed(name string) func(*Env) error {
	return func(e *Env) error {
		out, errs, code, err := e.dbc("plugins", "-t", "json")
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("%s", lastLine(errs, "dbc plugins failed"))
		}
		var kinds []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(out), &kinds); err != nil {
			return fmt.Errorf("dbc plugins -t json: %v", err)
		}
		for _, k := range kinds {
			if k.Name == name {
				return nil
			}
		}
		return fmt.Errorf("dbc plugins doesn't list %s", name)
	}
}

// hasRuns passes when runs_dir holds at least one record.
func hasRuns(e *Env) error {
	recs, err := e.runs("--limit", "1")
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return fmt.Errorf("no run records yet")
	}
	return nil
}
