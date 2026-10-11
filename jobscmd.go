package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sqlsplit"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/web"
)

// The job and run commands: the headless face of package jobs, so a job
// drawn in dbc web (or written by hand) runs from cron and CI, and every
// run's record — whoever ran it — can be listed, queried and read.
//
//	dbc jobs                                   list jobs_dir and the examples: schedule, next fire, last run
//	dbc job run NAME [--param k=v]…            run one (a path, a name, or an example) and wait for it
//	dbc job check NAME…                        diagnostics, the pipelines included; exit 1 on an error
//	dbc runs [--job N | --pipeline N]          the run records, newest first
//	    [--status S] [--since 7d] [--limit N]
//	dbc runs --sql "SELECT …"                  the records as tables (runs, pipelines, fragments, nodes)
//	                                           in a throwaway bytdb, queried like any connection
//	dbc run show ID                            one run as a tree: job → pipelines → fragments → nodes
//	dbc run cancel ID [--url U] [--secret S]   stop a run of a running dbc web, through its API
//
// `dbc job run` runs the job in this process, on an engine of its own,
// and writes its record to runs_dir like every run. Its lines stream as a
// pipeline run's do (stdout in text, stderr when -t json has stdout), each
// with its step; at the end, text prints the run's tree and -t json the
// whole record. Exit 0 when the job succeeded, 1 when not, 130 when
// interrupted (the run is canceled first, so every sink rolls back).

func jobsCommand() *cli.Command {
	return &cli.Command{
		Name:   "jobs",
		Usage:  "list the jobs in jobs_dir and the examples (run one with `dbc job run NAME`)",
		Action: jobsAction,
	}
}

func jobCommand() *cli.Command {
	return &cli.Command{
		Name:  "job",
		Usage: "run or check a job: a DAG of pipelines (dbc job help)",
		Commands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "run a job headless and wait for it: a file, a NAME from jobs_dir, or an example",
				ArgsUsage: "<file.json|NAME>",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "param", Aliases: []string{"p"}, Usage: "set a job parameter, `NAME=VALUE` (repeatable)",
						Destination: &flagParams},
				},
				Action: jobRunAction,
			},
			{
				Name:      "check",
				Usage:     "check jobs (and the pipelines they run) without running them; exit 1 on an error",
				ArgsUsage: "<file.json|NAME>...",
				Action:    jobCheckAction,
			},
		},
	}
}

var (
	flagRunsJob      string
	flagRunsPipeline string
	flagRunsStatus   string
	flagRunsSince    string
	flagRunsLimit    int
	flagRunsSQL      string
)

func runsCommand() *cli.Command {
	return &cli.Command{
		Name:  "runs",
		Usage: "list the run records in runs_dir, newest first, or query them with --sql",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "job", Usage: "only the runs of job `NAME`", Destination: &flagRunsJob},
			&cli.StringFlag{Name: "pipeline", Usage: "only the runs of pipeline `NAME` (run alone, not as a job's step)",
				Destination: &flagRunsPipeline},
			&cli.StringFlag{Name: "status", Usage: "only runs that ended `STATUS`: succeeded, failed, canceled, interrupted, running",
				Destination: &flagRunsStatus},
			&cli.StringFlag{Name: "since", Usage: "only runs started since `WHEN`: 7d, 36h, 90m, or a date (2026-10-01)",
				Destination: &flagRunsSince},
			&cli.IntFlag{Name: "limit", Value: 50, Usage: "at most `N` runs (0: all)", Destination: &flagRunsLimit},
			&cli.StringFlag{Name: "sql", Usage: "query the records with `SQL`: tables runs, pipelines, fragments, nodes",
				Destination: &flagRunsSQL},
		},
		Action: runsAction,
	}
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "show one run's record, or stop a run of dbc web (dbc run help)",
		Commands: []*cli.Command{
			{
				Name:      "show",
				Usage:     "show a run as a tree — job, pipelines, fragments, nodes — or its record with -t json",
				ArgsUsage: "<run id>",
				Action:    runShowAction,
			},
			{
				Name:  "cancel",
				Usage: "stop a run of a running dbc web (a scheduled job, a run started in the browser)",
				Description: "Asks the dbc web at --url to stop the run, through its API, with the secret dbc web was " +
					"started with (dbc web --secret, or $DBC_WEB_SECRET for both). It waits for the run to end and " +
					"prints how it ended. A run that a `dbc job run` is running is stopped where it runs (Ctrl+C there).",
				ArgsUsage: "<run id>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "url", Value: "http://" + web.DefaultListen, Sources: cli.EnvVars("DBC_WEB_URL"),
						Usage: "the dbc web to ask, `URL` ($DBC_WEB_URL)", Destination: &flagCancelURL},
					&cli.StringFlag{Name: "secret", Sources: cli.EnvVars("DBC_WEB_SECRET"),
						Usage: "dbc web's login `SECRET` ($DBC_WEB_SECRET)", Destination: &flagCancelSecret},
				},
				Action: runCancelAction,
			},
		},
	}
}

// newHeadlessEngine is an engine over cfg's runs dir, with the records a
// dead process left behind settled first. sink may be nil.
func newHeadlessEngine(cfg *config.Config, mgr *db.Manager, sink func(jobs.Event)) *jobs.Engine {
	// Signalable: this process runs the one run and Ctrl+C stops it, so
	// the record names the process for `dbc run cancel` and dbc web's
	// Runs view to interrupt (jobs signal.go)
	e := jobs.New(cfg, mgr, jobs.Options{RunsDir: cfg.RunsDir, RunsKeep: cfg.RunsKeep, Sink: sink, Signalable: true})
	if _, err := e.Recover(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not settle old run records: %v\n", err)
	}
	return e
}

func jobsAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("jobs")
	if cmd.Args().Present() {
		usage("usage: dbc jobs")
	}
	cfg := loadConfigOnly()
	f := outFormat()
	infos, err := userdata.ListJobs(cfg.JobsDir)
	if err != nil {
		fail(err, "could not list jobs")
	}
	var exs []scripts.Job
	for _, ex := range scripts.Jobs() {
		if !slices.ContainsFunc(infos, func(in userdata.JobInfo) bool { return in.Name == ex.Name }) {
			exs = append(exs, ex)
		}
	}
	fmt.Fprintf(os.Stderr, "%d job(s) in %s, and %d built-in example(s)\n", len(infos), cfg.JobsDir, len(exs))
	e := jobs.New(cfg, nil, jobs.Options{RunsDir: cfg.RunsDir})
	r := &model.Result{Query: "dbc jobs",
		Columns: []string{"name", "schedule", "next", "last run", "pipelines", "description", "kind"}}
	row := func(file, text, desc, kind string) {
		var sched, next, last string
		var nextRaw, lastRaw any
		n := 0
		if spec, err := jobs.ParseJob(text); err == nil {
			n = len(spec.Pipelines)
			sched = strings.Join(spec.Triggers.Schedule, "; ")
			if spec.Triggers.TZ != "" && sched != "" {
				sched += " (" + spec.Triggers.TZ + ")"
			}
			// examples are never scheduled: their cron line is there to copy
			if t := spec.NextFire(time.Now()); !t.IsZero() && kind == "job" {
				next, nextRaw = t.Local().Format("2006-01-02 15:04"), t.Format(time.RFC3339)
			}
			if lr, ok := e.LastRun(jobs.KindJob, spec.Name); ok {
				last = lr.Status + " " + lr.Started.Local().Format("2006-01-02 15:04")
				lastRaw = map[string]any{"id": lr.ID, "status": lr.Status, "started": lr.Started}
			}
		}
		r.Rows = append(r.Rows, []string{file, sched, next, last, strconv.Itoa(n), desc, kind})
		r.Raw = append(r.Raw, []any{file, sched, nextRaw, lastRaw, n, desc, kind})
	}
	for _, in := range infos {
		text, _, _ := userdata.ReadJob(cfg.JobsDir, in.Name)
		row(in.Name, text, in.Desc, "job")
	}
	for _, ex := range exs {
		row(ex.Name, ex.Text, ex.Desc, "example")
	}
	emit([]*model.Result{r}, f)
	return nil
}

// findJob resolves a job argument, or exits with usage.
func findJob(cfg *config.Config, arg string) config.JobRef {
	ref, err := cfg.FindJob(arg)
	if err != nil {
		usage(err.Error())
	}
	return ref
}

func jobRunAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("job run")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc job run [--param k=v]... <file.json|NAME>")
	}
	params, err := parseParams(flagParams)
	if err != nil {
		usage(err.Error())
	}
	cfg, mgr := setup(demoLazy)
	loadPlugins(cfg)
	defer mgr.Close()
	warnConfig(cfg)
	ref, err := cfg.FindJob(cmd.Args().First())
	if err != nil {
		mgr.Close()
		usage(err.Error())
	}
	if ref.Example != nil {
		fmt.Fprintf(os.Stderr, "running the built-in example %s (none of that name in %s)\n", ref.Example.Name, cfg.JobsDir)
	}
	text, err := ref.Source()
	if err != nil {
		fail(err, "could not read the job")
	}
	spec, err := jobs.ParseJob(text)
	if err != nil {
		fail(err, "not a job")
	}
	f := outFormat()
	out := newRunOutput(f)
	// a fan-out holds a reader and a writer per running fragment: the
	// in-memory SQLite demo's pool (3 by default) would make max_parallel
	// fragments on it wait for each other, so it gets room for them
	mgr.SetMemoryPool(2 + 3*spec.Policy.Parallel())
	e := newHeadlessEngine(cfg, mgr, func(ev jobs.Event) {
		switch ev := ev.(type) {
		case *jobs.Logged:
			text := ev.Line.Text
			if ev.Pipeline != "" {
				text = "[" + ev.Pipeline + "] " + text
			}
			fmt.Fprintln(out.log, text)
		case *jobs.Preview:
			out.show(ev.Result)
		case *jobs.Notice:
			fmt.Fprintln(os.Stderr, ev.Text)
		}
	})
	runCtx, stop := interruptible()
	defer stop()
	head, err := e.StartJob(jobs.JobRequest{Spec: spec, Params: params, Trigger: jobs.TriggerCLI, Source: ref.FileName()})
	if err != nil {
		fail(err, "the job did not start")
	}
	go func() {
		<-runCtx.Done()
		_ = e.Cancel(head.ID)
	}()
	fin, err := e.Wait(context.Background(), head.ID)
	e.Close(30 * time.Second)
	if f != export.JSON {
		out.finish()
	}
	if err != nil {
		fail(err, "lost the run")
	}
	if f == export.JSON {
		// the record is the document, the results a preview showed
		// inside it (runJSONResults)
		doc := struct {
			*jobs.Run
			Results json.RawMessage `json:"results,omitempty"`
		}{&fin, runJSONResults(out.results)}
		writeRunJSON(doc, "the run")
	} else {
		fmt.Fprint(out.log, fin.Tree())
	}
	switch fin.Status {
	case pipeline.Succeeded:
		return nil
	case pipeline.Canceled:
		canceled("job")
	}
	logToStderr(fmt.Sprintf("job %s %s: %s", fin.Name, fin.Status, fin.Error))
	os.Exit(1)
	return nil
}

// logToStderr is a final one-line failure for a command whose details were
// already printed (fail would log the error's whole serr context again).
func logToStderr(msg string) { fmt.Fprintln(os.Stderr, msg) }

func jobCheckAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("job check")
	if !cmd.Args().Present() {
		usage("usage: dbc job check <file.json|NAME>...")
	}
	cfg := loadConfigOnly()
	loadPlugins(cfg)
	var refs []config.JobRef
	for _, a := range cmd.Args().Slice() {
		refs = append(refs, findJob(cfg, a))
	}
	var conns []string
	for _, c := range cfg.Conns() {
		conns = append(conns, c.Name)
	}
	type fileDiag struct {
		File string `json:"file"`
		pipeline.Diag
	}
	all := []fileDiag{}
	bad := false
	for _, ref := range refs {
		text, err := ref.Source()
		if err != nil {
			fail(err, "could not read the job")
		}
		var diags []pipeline.Diag
		spec, err := jobs.ParseJob(text)
		if err != nil {
			diags = []pipeline.Diag{{Severity: pipeline.SevError, Msg: err.Error()}}
		} else {
			diags = jobs.CheckJob(spec, jobs.CheckOptions{Find: jobs.PipelineFinder(cfg.PipelinesDir), Conns: conns})
		}
		bad = bad || pipeline.HasError(diags)
		for _, d := range diags {
			if flagFormat == "json" {
				all = append(all, fileDiag{File: ref.Label(), Diag: d})
			} else {
				fmt.Printf("%s: %s\n", ref.Label(), d)
			}
		}
	}
	if flagFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(all); err != nil {
			fail(err, "could not write the diagnostics")
		}
	}
	if bad {
		os.Exit(1)
	}
	return nil
}

// parseSince reads --since: a span back from now (7d, 36h, 90m — d is a
// day of 24 hours) or a date or date-time in local time.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since wants a span (7d, 36h, 90m) or a date (2026-10-01), not %q", s)
}

func runsAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("runs")
	if cmd.Args().Present() {
		usage("usage: dbc runs [--job N | --pipeline N] [--status S] [--since 7d] [--limit N] [--sql SQL]")
	}
	if flagRunsJob != "" && flagRunsPipeline != "" {
		usage("--job and --pipeline: one or the other")
	}
	since, err := parseSince(flagRunsSince, time.Now())
	if err != nil {
		usage(err.Error())
	}
	filter := userdata.RunFilter{Status: flagRunsStatus, Since: since}
	switch {
	case flagRunsJob != "":
		filter.Kind, filter.Name = jobs.KindJob, strings.TrimSuffix(flagRunsJob, ".json")
	case flagRunsPipeline != "":
		filter.Kind, filter.Name = jobs.KindPipeline, strings.TrimSuffix(flagRunsPipeline, ".json")
	}
	cfg := loadConfigOnly()
	f := outFormat()
	e := jobs.New(cfg, nil, jobs.Options{RunsDir: cfg.RunsDir})
	if flagRunsSQL != "" {
		runsSQL(cfg, e, filter, flagRunsSQL, f)
		return nil
	}
	filter.Limit = flagRunsLimit
	heads, err := e.History(filter)
	if err != nil {
		fail(err, "could not list the runs")
	}
	if f == export.JSON && flagOut == "" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(nonNilSlice(heads))
	}
	r := &model.Result{Query: "dbc runs",
		Columns: []string{"id", "kind", "name", "trigger", "started", "took", "status", "rows", "error"}}
	for _, h := range heads {
		took := ""
		if !h.Ended.IsZero() {
			took = h.Ended.Sub(h.Started).Round(time.Millisecond).String()
		}
		r.Rows = append(r.Rows, []string{h.ID, h.Kind, h.Name, h.Trigger, h.Started.Local().Format("2006-01-02 15:04:05"),
			took, h.Status, strconv.FormatInt(h.Rows, 10), h.Error})
		r.Raw = append(r.Raw, []any{h.ID, h.Kind, h.Name, h.Trigger, h.Started, took, h.Status, h.Rows, h.Error})
	}
	emit([]*model.Result{r}, f)
	return nil
}

func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func runShowAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("run show")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc run show <run id>")
	}
	id := cmd.Args().First()
	if !userdata.ValidRunID(id) {
		usage(fmt.Sprintf("%q is not a run id (20261009-020000-7f3a; dbc runs lists them)", id))
	}
	cfg := loadConfigOnly()
	e := jobs.New(cfg, nil, jobs.Options{RunsDir: cfg.RunsDir})
	r, ok := e.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "no run %s in %s\n", id, cfg.RunsDir)
		os.Exit(1)
	}
	if outFormat() == export.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	fmt.Print(r.Tree())
	return nil
}

var flagCancelURL, flagCancelSecret string

// runCancelAction is `dbc run cancel ID`: a run belongs to the process
// whose engine runs it. A `dbc job run` or `dbc pipeline run` on this
// machine names itself in the record, and is stopped here by interrupting
// it (cancelLocal: no dbc web, no secret). For a scheduled job or a run
// started in the browser that process is dbc web — so this command asks
// dbc web, over its API, as curl would:
//
//	dbc run cancel ──record names a local process──► SIGINT ──► it cancels the run
//	               │ else
//	               ──POST /api/v1/runs/ID/cancel──► dbc web ──► its engine cancels the run
//	               ◄──── 200 | 404 no such run | 409 another process runs it
//	               ──GET /api/v1/runs/ID (until it has ended, ≤ 15s)──► how it ended
//
// WHY THE SECRET IS ASKED FOR. dbc web keeps its launch secret in memory
// only (web/auth.go: nothing on disk, a restart signs everyone out), so
// there is no file to find it in; a dbc web meant to be driven from a
// shell is started with --secret (or $DBC_WEB_SECRET), and this command
// reads the same variable. Exit 0 when the run is stopped (or had already
// ended), 1 otherwise.
func runCancelAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("run cancel")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc run cancel <run id> [--url URL] [--secret SECRET]")
	}
	id := cmd.Args().First()
	if !userdata.ValidRunID(id) {
		usage(fmt.Sprintf("%q is not a run id (20261009-020000-7f3a; dbc runs lists them)", id))
	}
	if cancelLocal(ctx, id) {
		return nil
	}
	if flagCancelSecret == "" {
		fmt.Fprintln(os.Stderr, "dbc run cancel needs dbc web's secret: start dbc web with --secret S (or $DBC_WEB_SECRET), "+
			"and give the same here (--secret S, or the same $DBC_WEB_SECRET)")
		os.Exit(1)
	}
	base := strings.TrimRight(flagCancelURL, "/")
	c := &webClient{base: base, secret: flagCancelSecret, http: &http.Client{Timeout: 10 * time.Second}}
	// a run that has ended already is said so, not "stopped": the record
	// first, then the cancel only for one that runs
	read := func() (jobs.Run, int, error) {
		var r jobs.Run
		data, status, err := c.call(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id))
		if err == nil {
			err = json.Unmarshal(data, &r)
		}
		return r, status, err
	}
	r, status, err := read()
	if err == nil && r.Status != pipeline.Running && r.Status != pipeline.Queued {
		fmt.Printf("run %s (%s %s) had already ended: %s\n", id, r.Kind, r.Name, r.Status)
		return nil
	}
	if err == nil {
		_, status, err = c.call(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/cancel")
	}
	if err != nil {
		switch {
		case status == 0:
			fmt.Fprintf(os.Stderr, "no dbc web answers at %s (%v) — is it running? --url or $DBC_WEB_URL names another\n", base, err)
		case status == http.StatusUnauthorized:
			fmt.Fprintf(os.Stderr, "dbc web at %s refused the secret — give the one it was started with\n", base)
		default:
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(1)
	}
	followCancel(ctx, id, func() (jobs.Run, bool) {
		r, _, err := read()
		return r, err == nil
	})
	return nil
}

// cancelLocal stops run id when its record names a process on this
// machine that an interrupt stops (a `dbc job run` or `dbc pipeline run`:
// jobs.Run.PID), and follows it to its end; false when the record names
// none — the run is dbc web's, or ended, or unknown here — and the caller
// asks dbc web instead.
func cancelLocal(ctx context.Context, id string) bool {
	cfg := loadConfigOnly()
	e := jobs.New(cfg, nil, jobs.Options{RunsDir: cfg.RunsDir})
	r, ok := e.Get(id)
	if !ok || r.PID == 0 || (r.Status != pipeline.Running && r.Status != pipeline.Queued) {
		return false
	}
	if err := e.Cancel(id); err != nil {
		return false // another machine's, or no signal here (Windows): dbc web's path says why
	}
	followCancel(ctx, id, func() (jobs.Run, bool) { return e.Get(id) })
	return true
}

// followCancel follows a run asked to stop to the end it reaches — it
// rolls its sinks back and ends on its own goroutine, in its own process —
// and says how it ended, waiting at most 15 s.
func followCancel(ctx context.Context, id string, read func() (jobs.Run, bool)) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if r, ok := read(); ok {
			switch r.Status {
			case pipeline.Running, pipeline.Queued:
			case pipeline.Canceled:
				fmt.Printf("stopped run %s (%s %s) — dbc run show %s\n", id, r.Kind, r.Name, id)
				return
			default:
				fmt.Printf("run %s (%s %s) had already ended: %s\n", id, r.Kind, r.Name, r.Status)
				return
			}
		}
		if time.Now().After(deadline) {
			fmt.Printf("asked run %s to stop; it is still rolling back — dbc run show %s says when it has\n", id, id)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// webClient calls a running dbc web's API with its secret as a Bearer
// token — the door web/auth.go keeps for curl and scripts.
type webClient struct {
	base, secret string
	http         *http.Client
}

// call makes one request and unwraps dbc web's envelope ({success, data,
// error}): the data on success; on failure an error in the server's own
// words, with the HTTP status (0 when nothing answered).
func (c *webClient) call(ctx context.Context, method, path string) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if json.Unmarshal(body, &env) != nil || !env.Success {
		msg := env.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return nil, resp.StatusCode, fmt.Errorf("dbc web: %s (%s)", msg, resp.Status)
	}
	return env.Data, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// dbc runs --sql
// ---------------------------------------------------------------------------

// runsTables are the tables `dbc runs --sql` queries: the records,
// flattened one level per table and keyed back to the run.
var runsTables = []string{
	`CREATE TABLE runs (id TEXT, kind TEXT, name TEXT, trigger TEXT, by TEXT, status TEXT,
	  started TIMESTAMP, ended TIMESTAMP, seconds DOUBLE PRECISION, rows BIGINT, error TEXT, PRIMARY KEY (id))`,
	`CREATE TABLE pipelines (run_id TEXT, step TEXT, pipeline TEXT, status TEXT,
	  started TIMESTAMP, ended TIMESTAMP, seconds DOUBLE PRECISION, rows BIGINT, error TEXT, PRIMARY KEY (run_id, step))`,
	`CREATE TABLE fragments (run_id TEXT, step TEXT, fragment TEXT, status TEXT, started TIMESTAMP, ended TIMESTAMP,
	  seconds DOUBLE PRECISION, rows BIGINT, direct BOOLEAN, error TEXT, PRIMARY KEY (run_id, step, fragment))`,
	`CREATE TABLE nodes (run_id TEXT, step TEXT, fragment TEXT, node TEXT, plugin TEXT, rows_in BIGINT, rows_out BIGINT,
	  batches BIGINT, seconds DOUBLE PRECISION, error TEXT, PRIMARY KEY (run_id, step, fragment, node))`,
}

// runsSQL loads the records that pass filter into a throwaway bytdb —
// runs ⟶ pipelines ⟶ fragments ⟶ nodes, each row carrying its run_id —
// and runs sql on it, so "which fragment is slowest this month" is one
// statement:
//
//	dbc runs --sql "SELECT step, fragment, avg(seconds) FROM fragments GROUP BY 1, 2 ORDER BY 3 DESC"
//
// Why a database file and not the records' JSON: SQL is the language the
// rest of dbc speaks, and bytdb is the embedded engine dbc already has
// (with no in-memory mode, so a temp file, removed before the results are
// written out).
func runsSQL(cfg *config.Config, e *jobs.Engine, filter userdata.RunFilter, sql string, f export.Format) {
	stmts := sqlsplit.Split(sql)
	if len(stmts) == 0 {
		usage("--sql holds no statement")
	}
	heads, err := e.History(filter)
	if err != nil {
		fail(err, "could not list the runs")
	}
	tmp, err := os.MkdirTemp("", "dbc-runs-")
	if err != nil {
		fail(err, "could not make a scratch directory")
	}
	const conn = "dbc-runs"
	qcfg := &config.Config{MaxRows: cfg.MaxRows, DefaultConnection: conn,
		Connections: []config.Connection{{Name: conn, Driver: "bytdb", DSN: filepath.Join(tmp, "runs.bytdb")}}}
	mgr := db.NewManager(qcfg)
	cleanup := func() {
		mgr.Close()
		_ = os.RemoveAll(tmp)
	}
	results, err := loadAndQuery(mgr, conn, e, heads, stmts)
	cleanup()
	if err != nil {
		fail(err, "query failed")
	}
	emit(results, f)
}

// loadAndQuery fills the tables and runs the statements.
func loadAndQuery(mgr *db.Manager, conn string, e *jobs.Engine, heads []jobs.Summary, stmts []sqlsplit.Stmt) ([]*model.Result, error) {
	ctx, stop := interruptible()
	defer stop()
	exec := func(q string, args ...any) error {
		_, err := mgr.RunContext(ctx, conn, q, args...)
		return err
	}
	for _, ddl := range runsTables {
		if err := exec(ddl); err != nil {
			return nil, err
		}
	}
	at := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t
	}
	secs := func(a, b time.Time) any {
		if a.IsZero() || b.IsZero() {
			return nil
		}
		return b.Sub(a).Seconds()
	}
	for _, h := range heads {
		r, ok := e.Get(h.ID)
		if !ok {
			continue
		}
		var rows int64
		for _, p := range r.Pipelines {
			rows += p.Rows()
		}
		if err := exec(`INSERT INTO runs VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			r.ID, r.Kind, r.Name, r.Trigger, r.By, string(r.Status), at(r.Started), at(r.Ended),
			secs(r.Started, r.Ended), rows, r.Error); err != nil {
			return nil, err
		}
		for _, p := range r.Pipelines {
			if err := exec(`INSERT INTO pipelines VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				r.ID, p.ID, p.Pipeline, string(p.Status), at(p.Started), at(p.Ended), secs(p.Started, p.Ended),
				p.Rows(), p.Error); err != nil {
				return nil, err
			}
			for _, fr := range p.Fragments {
				if err := exec(`INSERT INTO fragments VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
					r.ID, p.ID, fr.Name, string(fr.Status), at(fr.Started), at(fr.Ended), secs(fr.Started, fr.Ended),
					fr.Rows, fr.Direct, fr.Error); err != nil {
					return nil, err
				}
				for _, n := range fr.Nodes {
					if err := exec(`INSERT INTO nodes VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
						r.ID, p.ID, fr.Name, n.ID, n.Plugin, n.In, n.Out, n.Batches, n.Elapsed.Seconds(), n.Error); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "%d run(s) loaded as tables runs, pipelines, fragments, nodes\n", len(heads))
	var results []*model.Result
	for _, st := range stmts {
		res, err := mgr.RunContext(ctx, conn, st.Text)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}
