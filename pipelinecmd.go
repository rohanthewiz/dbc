package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/export"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sdb"
	"github.com/rohanthewiz/dbc/userdata"
)

// The pipeline commands: the headless face of package pipeline, so a
// pipeline drawn in dbc web or written by hand runs from a shell, cron or
// CI exactly as it does in the app.
//
//	dbc pipelines                              list pipelines_dir and the examples
//	dbc pipeline run NAME [--param k=v]…       run one (a path, a name, or an example)
//	    [--preview N] [--fragment F]           preview N rows, nothing written; or one fragment
//	dbc pipeline check NAME…                   diagnostics, as dbc script --check; exit 1 on an error
//	dbc pipeline export NAME [-o f.go]         the pipeline as a dbc script (the Builder form)
//	dbc plugins                                the node kinds and their fields
//
// A run's log lines (fragments starting and ending, DDL, a node's Logf)
// stream as a script's s.Print lines do: stdout in text, stderr when a
// machine-readable format has stdout. Results a preview sink shows go out
// as a script's s.Show results do. With -t json the run's stats are the
// document on stdout, so a job scheduler can read the outcome; in text,
// one line per fragment then the summary. Exit 0 on success, 1 on failure,
// 130 when interrupted.

func pipelinesCommand() *cli.Command {
	return &cli.Command{
		Name:   "pipelines",
		Usage:  "list the pipelines in pipelines_dir (run one with `dbc pipeline run NAME`)",
		Action: pipelinesAction,
	}
}

var (
	flagParams   []string
	flagPreview  int
	flagFragment string
)

func pipelineCommand() *cli.Command {
	return &cli.Command{
		Name:  "pipeline",
		Usage: "run, check or export a pipeline (dbc pipeline help)",
		Commands: []*cli.Command{
			{
				Name:      "run",
				Usage:     "run a pipeline headless: a file, a NAME from pipelines_dir, or an example",
				ArgsUsage: "<file.json|NAME>",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "param", Aliases: []string{"p"}, Usage: "set a parameter, `NAME=VALUE` (repeatable)",
						Destination: &flagParams},
					&cli.IntFlag{Name: "preview", Usage: "preview: stop each source after `N` rows and show them; nothing is written",
						Destination: &flagPreview},
					&cli.StringFlag{Name: "fragment", Usage: "run only the fragment called `NAME`",
						Destination: &flagFragment},
				},
				Action: pipelineRunAction,
			},
			{
				Name:      "check",
				Usage:     "check pipelines without running them; exit 1 on an error",
				ArgsUsage: "<file.json|NAME>...",
				Action:    pipelineCheckAction,
			},
			{
				Name:      "export",
				Usage:     "write a pipeline as a dbc script that builds and runs it",
				ArgsUsage: "<file.json|NAME>",
				Action:    pipelineExportAction,
			},
		},
	}
}

func pluginsCommand() *cli.Command {
	return &cli.Command{
		Name:   "plugins",
		Usage:  "list the pipeline node kinds (plugins) and their fields",
		Action: pluginsAction,
	}
}

// loadConfigOnly is LoadDemo for the commands that connect nothing.
func loadConfigOnly() *config.Config {
	demo, err := config.ParseDemoEngine(flagDemo)
	if err != nil {
		fail(err, "bad --demo engine")
	}
	cfg, err := config.LoadDemo(flagConfig, demo)
	if err != nil {
		fail(err, "could not load config")
	}
	warnConfig(cfg)
	return cfg
}

func pipelinesAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("pipelines")
	if cmd.Args().Present() {
		usage("usage: dbc pipelines")
	}
	cfg := loadConfigOnly()
	f := outFormat()
	infos, err := userdata.ListPipelines(cfg.PipelinesDir)
	if err != nil {
		fail(err, "could not list pipelines")
	}
	var exs []scripts.Pipeline
	for _, ex := range scripts.Pipelines() {
		if !slices.ContainsFunc(infos, func(in userdata.PipelineInfo) bool { return in.Name == ex.Name }) {
			exs = append(exs, ex)
		}
	}
	fmt.Fprintf(os.Stderr, "%d pipeline(s) in %s, and %d built-in example(s)\n", len(infos), cfg.PipelinesDir, len(exs))
	r := &model.Result{Query: "dbc pipelines", Columns: []string{"name", "modified", "fragments", "description", "kind"}}
	for _, in := range infos {
		mod := in.Mod.Local().Format("2006-01-02 15:04")
		r.Rows = append(r.Rows, []string{in.Name, mod, fmt.Sprint(in.Fragments), in.Desc, "pipeline"})
		r.Raw = append(r.Raw, []any{in.Name, in.Mod.Format(time.RFC3339), in.Fragments, in.Desc, "pipeline"})
	}
	for _, ex := range exs {
		n := 0
		if spec, err := pipeline.Parse(ex.Text); err == nil {
			n = len(spec.Fragments)
		}
		r.Rows = append(r.Rows, []string{ex.Name, "", fmt.Sprint(n), ex.Desc, "example"})
		r.Raw = append(r.Raw, []any{ex.Name, nil, n, ex.Desc, "example"})
	}
	emit([]*model.Result{r}, f)
	return nil
}

func pluginsAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("plugins")
	f := outFormat()
	if f == export.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(pipeline.Plugins())
	}
	r := &model.Result{Query: "dbc plugins", Columns: []string{"plugin", "kind", "label", "fields", "description"}}
	for _, p := range pipeline.Plugins() {
		var fields []string
		for _, fd := range p.Fields {
			s := fd.Name
			if fd.Required {
				s += "*"
			}
			fields = append(fields, s)
		}
		r.Rows = append(r.Rows, []string{p.Name, string(p.Kind), p.Label, strings.Join(fields, " "), p.Doc})
		r.Raw = append(r.Raw, []any{p.Name, string(p.Kind), p.Label, strings.Join(fields, " "), p.Doc})
	}
	emit([]*model.Result{r}, f)
	return nil
}

// parseParams reads --param NAME=VALUE flags.
func parseParams(flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range flags {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--param wants NAME=VALUE, got %q", kv)
		}
		out[k] = v
	}
	return out, nil
}

// findPipelines resolves every argument before any is used.
func findPipelines(cfg *config.Config, args []string) []config.PipelineRef {
	refs := make([]config.PipelineRef, len(args))
	for i, a := range args {
		var err error
		if refs[i], err = cfg.FindPipeline(a); err != nil {
			usage(err.Error())
		}
	}
	return refs
}

func pipelineRunAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("pipeline run")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc pipeline run [--param k=v]... [--preview N] [--fragment F] <file.json|NAME>")
	}
	params, err := parseParams(flagParams)
	if err != nil {
		usage(err.Error())
	}
	cfg, mgr := setup(demoLazy)
	defer mgr.Close()
	warnConfig(cfg)
	ref, err := cfg.FindPipeline(cmd.Args().First())
	if err != nil {
		mgr.Close()
		usage(err.Error())
	}
	if ref.Example != nil {
		fmt.Fprintf(os.Stderr, "running the built-in example %s (none of that name in %s)\n",
			ref.Example.Name, cfg.PipelinesDir)
	}
	text, err := ref.Source()
	if err != nil {
		fail(err, "could not read the pipeline")
	}
	spec, err := pipeline.Parse(text)
	if err != nil {
		fail(err, "not a pipeline")
	}
	runPipelineHeadless(cfg, mgr, spec, pipeline.Options{Params: params, PreviewRows: flagPreview, Fragment: flagFragment}, outFormat())
	return nil
}

// runPipelineHeadless runs spec and writes the outcome: the preview
// results as a script's results go (streamed or collected by format), the
// log as a script's Print lines go, and the stats as the final document
// (-t json) or a summary (text). It exits as runScriptHeadless does.
func runPipelineHeadless(cfg *config.Config, mgr *db.Manager, spec *pipeline.Spec, opt pipeline.Options, f export.Format) {
	ctx, stop := interruptible()
	defer stop()
	out := newHeadlessOutput(f)
	s := sdb.New(mgr, out.show, func(msg string) { fmt.Fprintln(out.log, msg) }).
		WithContext(ctx).WithPaths(sdb.Paths{ScriptsDir: cfg.ScriptsDir, PipelinesDir: cfg.PipelinesDir})
	s.LogDDL()
	st, err := s.RunPipelineSpec(spec, opt)
	s.Release()
	out.finish()
	if f == export.JSON {
		// the stats are the document: results a preview showed were
		// collected and are not repeated here (they went out as the
		// script path writes them, before this)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(st); encErr != nil {
			fail(encErr, "could not write the stats")
		}
	} else if st != nil {
		fmt.Fprint(out.log, runText(st))
	}
	if err != nil {
		if sdb.IsCanceled(err) {
			canceled("pipeline")
		}
		fail(err, "pipeline failed")
	}
}

// runText is the stats as text: one line per fragment (the summary line
// was logged by the run itself, just before).
func runText(st *pipeline.RunStats) string {
	var sb strings.Builder
	for _, fr := range st.Fragments {
		var nodes []string
		for _, n := range fr.Nodes {
			nodes = append(nodes, fmt.Sprintf("%s %d→%d", n.ID, n.In, n.Out))
		}
		line := fmt.Sprintf("  %-12s %-9s %8d rows %8s", fr.Name, fr.Status, fr.Rows, fr.Elapsed().Round(time.Millisecond))
		if fr.Direct {
			line += "  direct COPY"
		}
		if len(nodes) > 0 {
			line += "  " + strings.Join(nodes, ", ")
		}
		if fr.Error != "" {
			line += "  " + fr.Error
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// headlessOutput is where a headless script or pipeline's shown results
// and log lines go — the rules of runScriptHeadless, shared with it.
type headlessOutput struct {
	f       export.Format
	results []*model.Result
	stream  *blockStream
	log     io.Writer
	stop    func()
}

func newHeadlessOutput(f export.Format) *headlessOutput {
	o := &headlessOutput{f: f, log: scriptLog(f)}
	if scriptStreams(f) {
		o.stream = &blockStream{out: os.Stdout, notes: os.Stderr, f: f}
	}
	return o
}

// show takes one result: written at once when streaming, else kept.
func (o *headlessOutput) show(r *model.Result) {
	if o.stream != nil {
		o.stream.show(r, func() {
			if o.stop != nil {
				o.stop()
			}
		})
		return
	}
	o.results = append(o.results, r)
}

// finish writes the collected results, and fails on a stream that broke.
func (o *headlessOutput) finish() {
	if o.stream != nil && o.stream.err != nil {
		fail(o.stream.err, "render failed")
	}
	if len(o.results) > 0 {
		emitScript(o.results, o.f)
	}
}

func pipelineCheckAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("pipeline check")
	if err := checkScriptFlags(); err != nil {
		usage(err.Error())
	}
	if !cmd.Args().Present() {
		usage("usage: dbc pipeline check <file.json|NAME>...")
	}
	cfg := loadConfigOnly()
	refs := findPipelines(cfg, cmd.Args().Slice())
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
			fail(err, "could not read the pipeline")
		}
		var diags []pipeline.Diag
		spec, err := pipeline.Parse(text)
		if err != nil {
			diags = []pipeline.Diag{{Severity: pipeline.SevError, Msg: err.Error()}}
		} else {
			diags = pipeline.Check(spec, pipeline.CheckOptions{Conns: conns})
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

func pipelineExportAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("pipeline export")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc pipeline export [-o file.go] <file.json|NAME>")
	}
	cfg := loadConfigOnly()
	ref := findPipelines(cfg, cmd.Args().Slice())[0]
	text, err := ref.Source()
	if err != nil {
		fail(err, "could not read the pipeline")
	}
	spec, err := pipeline.Parse(text)
	if err != nil {
		fail(err, "not a pipeline")
	}
	src, err := pipeline.Gen(spec)
	if err != nil {
		fail(err, "cannot export")
	}
	if flagOut != "" {
		if err := os.WriteFile(flagOut, []byte(src), 0o644); err != nil {
			fail(err, "could not write "+flagOut)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", flagOut)
		return nil
	}
	_, err = io.WriteString(os.Stdout, src)
	if err != nil && !errors.Is(err, io.ErrClosedPipe) {
		fail(err, "could not write the script")
	}
	return nil
}
