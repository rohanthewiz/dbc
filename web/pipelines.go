package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// Pipelines: the JSON specs in pipelines_dir, edited on a pipeline tab's
// canvas, checked as the user edits, previewed and run through the
// server's engine (package jobs) rather than a query tab's run slot.
//
// THE STORE is the scripts protocol (scripts.go) for .json files: the page
// names a pipeline, never a path; a save names the revision it was made
// from and a changed file is a conflict, not an overwrite; a delete is a
// move into .trash; every change is told to every window ("pipelines").
//
//	GET    /api/v1/pipelines                    the list: pipelines, examples, trash
//	GET    /api/v1/pipelines/:name              {text, rev}
//	PUT    /api/v1/pipelines/:name?win=         {text, base} → {rev} | {conflict, text, rev}
//	POST   /api/v1/pipelines/:name/rename?win=  {to}
//	DELETE /api/v1/pipelines/:name?win=         to .trash → {id}
//	POST   /api/v1/pipeline-trash/:id/restore   {to?} → {name}
//	GET    /api/v1/pipeline-examples/:name      a built-in example's text
//	GET    /api/v1/plugins                      the registry: the palette and the inspector
//
// THE CHECK runs on the editor's text, saved or not, on every pause in
// typing or canvas edit — pipeline.Check never touches a database:
//
//	POST   /api/v1/pipeline-check               {text} → {diags}  (where, line/col for the JSON view)
//
// RUNS belong to the engine (Server.jobs), which outlives any tab: a query
// tab is not busy while a pipeline it started runs, and two pipelines run
// at once. The tab is the run's ORIGIN — where its preview rows land and
// what its Stop stops:
//
//	POST   /api/v1/pipeline-preview  {ws, text, fragment, rows, params} → {run}
//	POST   /api/v1/pipeline-run      {ws, name, fragment, params}       → {run}
//	GET    /api/v1/pipeline-export/:name   the builder form, as a dbc script
//	GET    /api/v1/runs                    {running, recent}: a reload catches up
//	GET    /api/v1/runs/:id                one run's record, its log included
//	POST   /api/v1/runs/:id/cancel
//
//	engine event        window event      the page
//	─────────────────   ───────────────   ───────────────────────────────────────
//	RunStarted          job.run           the canvas's run state, the tab's busy mark
//	Progress            job.progress      node counters on the cards, lane states
//	Logged              job.line          the pipeline's log
//	Preview             job.preview  ┐    a note; the rows themselves land in the
//	                    + "result" ──┘    origin tab's workspace (ShowResult) and its
//	                      (to that tab)   grid fetches them as after a script's s.Show
//	State               job.state         a job's step started, ended, was skipped
//	Notice              job.notice        a line in the log on screen (the scheduler)
//	RunDone             job.done          the outcome, the status bar
//
// A preview runs the editor's text — it writes nothing, so an unsaved
// draft may be tried; a run runs the saved file (the page saves first),
// so what ran is what `dbc pipeline run` would run.
//
// Why pipeline-check and not pipelines/check: the radix router does not
// backtrack (see scripts.go), so nothing but :name sits under
// /api/v1/pipelines/.

// pipelinesList is GET /api/v1/pipelines: what the scripts browser's
// Pipelines section draws.
type pipelinesList struct {
	Dir       string                       `json:"dir"`
	Short     string                       `json:"short"`
	Pipelines []userdata.PipelineInfo      `json:"pipelines"`
	Examples  []scripts.Pipeline           `json:"examples"` // name and desc; the text by its own route
	Trash     []userdata.PipelineTrashInfo `json:"trash"`
}

func (s *Server) handlePipelines(ctx rweb.Context) error {
	infos, err := userdata.ListPipelines(s.cfg.PipelinesDir)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, pipelinesList{
		Dir: s.cfg.PipelinesDir, Short: config.TildePath(s.cfg.PipelinesDir),
		Pipelines: nonNil(infos), Examples: nonNil(scripts.Pipelines()),
		Trash: nonNil(userdata.ListPipelineTrash(s.cfg.PipelinesDir)),
	})
}

// pipelineName is the request's :name, checked before any file is touched.
func pipelineName(ctx rweb.Context) (string, error) {
	name := ctx.Request().PathParam("name")
	if !userdata.ValidPipelineName(name) {
		return name, badRequest("not a pipeline name: %q (letters, digits, '.', '-' or '_', ending in .json)", name)
	}
	return name, nil
}

// pipelineErr maps the store's errors onto statuses, as scriptErr does.
func (s *Server) pipelineErr(err error, name string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notFound("no pipeline %s in %s", name, config.TildePath(s.cfg.PipelinesDir))
	case errors.Is(err, userdata.ErrPipelineExists):
		return conflict("%s already has a pipeline named %s", config.TildePath(s.cfg.PipelinesDir), name)
	case errors.Is(err, userdata.ErrBadPipelineName):
		return badRequest("not a pipeline name: %q", name)
	}
	return err
}

func (s *Server) handlePipelineRead(ctx rweb.Context) error {
	name, err := pipelineName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	text, rev, err := userdata.ReadPipeline(s.cfg.PipelinesDir, name)
	if err != nil {
		return fail(ctx, s.pipelineErr(err, name))
	}
	return ok(ctx, scriptText{Text: text, Rev: rev})
}

// handlePipelineSave is PUT /api/v1/pipelines/:name?win=: the scripts
// protocol's save (scriptSave in, scriptSaved out). The text is not
// required to check out — a half-built pipeline saves, and its diags say
// what is missing — but it must be a JSON object, so the file is always
// one the canvas can open again.
func (s *Server) handlePipelineSave(ctx rweb.Context) error {
	name, err := pipelineName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req scriptSave
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxPipelineBytes {
		return fail(ctx, badRequest("the pipeline is %d KB; pipelines save up to %d KB", len(req.Text)>>10, userdata.MaxPipelineBytes>>10))
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal([]byte(req.Text), &obj); err != nil {
		return fail(ctx, badRequest("not saved — the pipeline is not a JSON object: %v", err))
	}
	rev, isConflict, err := userdata.SavePipeline(s.cfg.PipelinesDir, name, req.Text, req.Base)
	if err != nil {
		return fail(ctx, s.pipelineErr(err, name))
	}
	if isConflict {
		text, _, _ := userdata.ReadPipeline(s.cfg.PipelinesDir, name)
		return ok(ctx, scriptSaved{Rev: rev, Conflict: true, Text: text})
	}
	s.hub.broadcast("pipelines", scriptsEvent{Op: "saved", Name: name, Rev: rev, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, scriptSaved{Rev: rev})
}

func (s *Server) handlePipelineRename(ctx rweb.Context) error {
	name, err := pipelineName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req struct {
		To string `json:"to"`
	}
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(req.To)
	if !userdata.ValidPipelineName(req.To) {
		return fail(ctx, badRequest("not a pipeline name: %q (letters, digits, '.', '-' or '_', ending in .json)", req.To))
	}
	if err = userdata.RenamePipeline(s.cfg.PipelinesDir, name, req.To); err != nil {
		if errors.Is(err, userdata.ErrPipelineExists) {
			return fail(ctx, s.pipelineErr(err, req.To))
		}
		return fail(ctx, s.pipelineErr(err, name))
	}
	s.hub.broadcast("pipelines", scriptsEvent{Op: "renamed", Name: name, To: req.To, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": req.To})
}

func (s *Server) handlePipelineTrash(ctx rweb.Context) error {
	name, err := pipelineName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	id, err := userdata.TrashPipeline(s.cfg.PipelinesDir, name)
	if err != nil {
		return fail(ctx, s.pipelineErr(err, name))
	}
	s.hub.broadcast("pipelines", scriptsEvent{Op: "trashed", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"id": id})
}

func (s *Server) handlePipelineRestore(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	var req struct {
		To string `json:"to"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(req.To)
	if req.To != "" && !userdata.ValidPipelineName(req.To) {
		return fail(ctx, badRequest("not a pipeline name: %q (letters, digits, '.', '-' or '_', ending in .json)", req.To))
	}
	if !slices.ContainsFunc(userdata.ListPipelineTrash(s.cfg.PipelinesDir), func(t userdata.PipelineTrashInfo) bool { return t.ID == id }) {
		return fail(ctx, notFound("no trashed pipeline %q", id))
	}
	name, err := userdata.RestorePipeline(s.cfg.PipelinesDir, id, req.To)
	if err != nil {
		return fail(ctx, s.pipelineErr(err, cmp.Or(req.To, id)))
	}
	s.hub.broadcast("pipelines", scriptsEvent{Op: "restored", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": name})
}

func (s *Server) handlePipelineExample(ctx rweb.Context) error {
	ex, found := scripts.PipelineByName(ctx.Request().PathParam("name"))
	if !found {
		return fail(ctx, notFound("no example pipeline %q", ctx.Request().PathParam("name")))
	}
	return ok(ctx, map[string]string{"name": ex.Name, "desc": ex.Desc, "text": ex.Text})
}

// handlePlugins is GET /api/v1/plugins: every plugin with its fields —
// the palette lists them by kind, and the inspector draws a node's form
// from its plugin's fields, so no plugin needs page code of its own. The
// connections ride along for the conn fields' pickers.
func (s *Server) handlePlugins(ctx rweb.Context) error {
	ps := pipeline.Plugins()
	for i := range ps {
		ps[i].Fields = nonNil(ps[i].Fields)
	}
	conns := []string{}
	for _, c := range s.cfg.Conns() {
		conns = append(conns, c.Name)
	}
	return ok(ctx, map[string]any{"plugins": ps, "conns": conns})
}

// ---------------------------------------------------------------------------
// The check
// ---------------------------------------------------------------------------

// pipeDiag is a check finding as the page draws it: pipeline.Diag's where
// (which the canvas maps to a node, a lane or the pipeline), and — for the
// JSON view's markers — the line and column it points at in the text, 1-based,
// found from the where (0 when it could not be placed).
type pipeDiag struct {
	pipeline.Diag
	Line int `json:"line"`
	Col  int `json:"col"`
}

// checkText checks a pipeline's text: a parse error is one diag at its
// offset; a spec that parses gets pipeline.Check's findings, placed.
func (s *Server) checkText(text string) []pipeDiag {
	spec, err := pipeline.Parse(text)
	if err != nil {
		line, col := 1, 1
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		switch {
		case errors.As(err, &se):
			line, col = lineCol(text, int(se.Offset))
		case errors.As(err, &te):
			line, col = lineCol(text, int(te.Offset))
		default:
			// an unknown key names itself: find it
			if m := unknownFieldRe.FindStringSubmatch(err.Error()); m != nil {
				if i := strings.Index(text, `"`+m[1]+`"`); i >= 0 {
					line, col = lineCol(text, i)
				}
			}
		}
		return []pipeDiag{{Diag: pipeline.Diag{Severity: pipeline.SevError, Msg: strings.TrimPrefix(err.Error(), "json: ")}, Line: line, Col: col}}
	}
	diags := pipeline.Check(spec, pipeline.CheckOptions{Conns: s.checkConns(spec)})
	out := make([]pipeDiag, len(diags))
	for i, d := range diags {
		out[i] = pipeDiag{Diag: d}
		out[i].Line, out[i].Col = locate(text, d.Where)
	}
	return out
}

var unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)

// checkConns is the connection names a spec's conn fields may use: every
// configured one, and any the spec names that resolves another way — a
// "<conn>/<database>" onto another database of a configured server — so the
// check does not call a working name unknown.
func (s *Server) checkConns(spec *pipeline.Spec) []string {
	var names []string
	for _, c := range s.cfg.Conns() {
		names = append(names, c.Name)
	}
	for _, f := range spec.Fragments {
		for _, n := range f.Nodes {
			p, ok := pipeline.Lookup(n.Plugin)
			if !ok {
				continue
			}
			for k, v := range n.Cfg {
				fd, ok := p.Field(k)
				v = strings.TrimSpace(v)
				if !ok || fd.Type != pipeline.FieldConn || v == "" || slices.Contains(names, v) {
					continue
				}
				if _, found := s.cfg.ConnByName(v); found {
					names = append(names, v)
				}
			}
		}
	}
	return names
}

// lineCol is the 1-based line and column of byte offset off in text.
func lineCol(text string, off int) (int, int) {
	off = min(max(off, 0), len(text))
	before := text[:off]
	line := strings.Count(before, "\n") + 1
	col := off - strings.LastIndex(before, "\n")
	return line, col
}

// locate places a diag's where in the spec's text, for the JSON view: the
// fragment's "name", then within it the node's "id", then the field's key.
// It is a text search, not a JSON parse with positions — good enough to put
// a marker on the right line of a file in the shape Spec.JSON writes, and
// harmless when it misses (0, 0: the page marks nothing).
//
//	where "clean/dst.table" → "name": "clean" … "id": "dst" … "table":
func locate(text, where string) (int, int) {
	if where == "" {
		return 0, 0
	}
	at := 0
	find := func(needle *regexp.Regexp) bool {
		loc := needle.FindStringIndex(text[at:])
		if loc == nil {
			return false
		}
		at += loc[0]
		return true
	}
	quote := func(s string) string { return regexp.QuoteMeta(strings.ReplaceAll(s, `"`, `\"`)) }
	switch {
	case where == "name" || where == "fragments":
		if !find(regexp.MustCompile(`"` + where + `"\s*:`)) {
			return 0, 0
		}
		return lineCol(text, at)
	case strings.HasPrefix(where, "params."):
		if !find(regexp.MustCompile(`"params"\s*:`)) || !find(regexp.MustCompile(`"`+quote(strings.TrimPrefix(where, "params."))+`"\s*:`)) {
			return 0, 0
		}
		return lineCol(text, at)
	}
	frag, rest, _ := strings.Cut(where, "/")
	frag, edge, isEdge := strings.Cut(frag, ":")
	if !find(regexp.MustCompile(`"name"\s*:\s*"` + quote(frag) + `"`)) {
		return 0, 0
	}
	switch {
	case isEdge:
		// "edge a→b": the fragment's edges
		if from, to, ok := strings.Cut(strings.TrimPrefix(edge, "edge "), "→"); ok &&
			find(regexp.MustCompile(`\[\s*"`+quote(from)+`"\s*,\s*"`+quote(to)+`"\s*\]`)) {
			return lineCol(text, at)
		}
	case rest != "":
		node, field, _ := strings.Cut(rest, ".")
		if find(regexp.MustCompile(`"id"\s*:\s*"` + quote(node) + `"`)) {
			if field != "" {
				_ = find(regexp.MustCompile(`"` + quote(field) + `"\s*:`))
			}
		}
	}
	return lineCol(text, at)
}

func (s *Server) handlePipelineCheck(ctx rweb.Context) error {
	var req struct {
		Text string `json:"text"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxPipelineBytes {
		return fail(ctx, badRequest("the pipeline is %d KB; pipelines are checked up to %d KB", len(req.Text)>>10, userdata.MaxPipelineBytes>>10))
	}
	return ok(ctx, map[string]any{"diags": nonNil(s.checkText(req.Text))})
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// previewRows bounds a preview: the default when the page sends none, and
// the most it may ask for (every preview sink keeps that many rows in
// memory, and the grid shows them all).
const (
	previewRowsDefault = 50
	previewRowsMax     = 1000
)

// pipelinePreviewReq is POST /api/v1/pipeline-preview's body.
type pipelinePreviewReq struct {
	WS       string            `json:"ws"`   // the query tab whose grid shows the rows
	Name     string            `json:"name"` // the file the text is of, for the run's Source; may be ""
	Text     string            `json:"text"`
	Fragment string            `json:"fragment"`
	Rows     int               `json:"rows"`
	Params   map[string]string `json:"params"`
}

// handlePipelinePreview starts a preview of the editor's text: every sink
// a preview sink, the source stopped after rows rows, actions skipped —
// nothing is written. Its rows land in the asking tab's grid as they come.
func (s *Server) handlePipelinePreview(ctx rweb.Context) error {
	var req pipelinePreviewReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if _, err := s.hub.get(req.WS); err != nil {
		return fail(ctx, err)
	}
	spec, err := pipeline.Parse(req.Text)
	if err != nil {
		return fail(ctx, badRequest("the pipeline does not parse: %s", strings.TrimPrefix(err.Error(), "json: ")))
	}
	rows := req.Rows
	if rows <= 0 {
		rows = previewRowsDefault
	}
	rows = min(rows, previewRowsMax)
	source := ""
	if userdata.ValidPipelineName(req.Name) {
		source = req.Name
	}
	return s.startRun(ctx, jobs.Request{Spec: spec, Params: req.Params, Fragment: req.Fragment, PreviewRows: rows,
		Origin: req.WS, Source: source})
}

// pipelineRunReq is POST /api/v1/pipeline-run's body.
type pipelineRunReq struct {
	WS       string            `json:"ws"`
	Name     string            `json:"name"`
	Fragment string            `json:"fragment"`
	Params   map[string]string `json:"params"`
}

// handlePipelineRun runs a saved pipeline — or a built-in example, which
// is read-only but runnable, as `dbc pipeline run` finds one when no file
// of the user's shadows it.
func (s *Server) handlePipelineRun(ctx rweb.Context) error {
	var req pipelineRunReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if req.WS != "" {
		if _, err := s.hub.get(req.WS); err != nil {
			return fail(ctx, err)
		}
	}
	text, err := s.pipelineSource(req.Name)
	if err != nil {
		return fail(ctx, err)
	}
	spec, err := pipeline.Parse(text)
	if err != nil {
		return fail(ctx, badRequest("%s does not parse: %s", req.Name, strings.TrimPrefix(err.Error(), "json: ")))
	}
	return s.startRun(ctx, jobs.Request{Spec: spec, Params: req.Params, Fragment: req.Fragment, Origin: req.WS, Source: req.Name})
}

// pipelineSource is a pipeline's text by name: the user's file, else the
// example of that name.
func (s *Server) pipelineSource(name string) (string, error) {
	if !userdata.ValidPipelineName(name) {
		return "", badRequest("not a pipeline name: %q", name)
	}
	text, _, err := userdata.ReadPipeline(s.cfg.PipelinesDir, name)
	if err == nil {
		return text, nil
	}
	if ex, found := scripts.PipelineByName(name); found {
		return ex.Text, nil
	}
	return "", s.pipelineErr(err, name)
}

// startRun hands a request to the engine: a refusal for something already
// running is a 409, one for a spec that does not check out a 400 with the
// problems; started, the answer is the run's header.
func (s *Server) startRun(ctx rweb.Context, req jobs.Request) error {
	run, err := s.jobs.StartPipeline(req)
	switch {
	case errors.Is(err, jobs.ErrBusy):
		return fail(ctx, conflict("%s", err.Error()))
	case errors.Is(err, jobs.ErrClosed):
		return fail(ctx, conflict("dbc web is shutting down"))
	case err != nil:
		return fail(ctx, badRequest("%s", err.Error()))
	}
	return ok(ctx, map[string]any{"run": run})
}

// handlePipelineExport is GET /api/v1/pipeline-export/:name: the
// pipeline in the sdb builder's form, as a dbc script (pipeline.Gen) —
// what the canvas draws, as Go a script can run and change. The page saves
// it as a new script and opens it in a script tab.
func (s *Server) handlePipelineExport(ctx rweb.Context) error {
	name := ctx.Request().PathParam("name")
	text, err := s.pipelineSource(name)
	if err != nil {
		return fail(ctx, err)
	}
	spec, err := pipeline.Parse(text)
	if err != nil {
		return fail(ctx, badRequest("%s does not parse: %s", name, err.Error()))
	}
	src, err := pipeline.Gen(spec)
	if err != nil {
		return fail(ctx, badRequest("%s cannot be exported: %s", name, err.Error()))
	}
	return ok(ctx, map[string]string{"name": name, "text": src})
}

// handleRuns is GET /api/v1/runs: the runs going now and the ones that
// ended lately in this process, newest first, without their logs — what a
// page that just loaded needs to draw a running pipeline's state and its
// tab's busy mark. With a filter (kind, name, status, since, limit, or
// history=1) it adds "runs": the history from the records in runs_dir —
// every process's, a cron's `dbc job run` included — previews left out.
func (s *Server) handleRuns(ctx rweb.Context) error {
	f, history, err := runsFilter(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	out := map[string]any{"running": nonNil(s.jobs.Running()), "recent": nonNil(s.jobs.Recent())}
	if history {
		runs, err := s.jobs.History(f)
		if err != nil {
			return fail(ctx, err)
		}
		out["runs"] = nonNil(runs)
	}
	return ok(ctx, out)
}

// handleRunRecord is one run's record with its log: live, kept in memory,
// or read back from runs_dir.
func (s *Server) handleRunRecord(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	r, found := s.jobs.Get(id)
	if !found {
		return fail(ctx, notFound("no run %q (not running, and no record of it in %s)", id, config.TildePath(s.cfg.RunsDir)))
	}
	return ok(ctx, r)
}

func (s *Server) handleRunCancel(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	if err := s.jobs.Cancel(id); err != nil {
		return fail(ctx, notFound("no run %q", id))
	}
	return ok(ctx, map[string]string{"run": id})
}

// ---------------------------------------------------------------------------
// The engine's events
// ---------------------------------------------------------------------------

// jobLine is "job.line": one line of a run's log. Name is the pipeline's,
// which keys the pipeline tab's log on the page.
type jobLine struct {
	Run   string `json:"run"`
	Name  string `json:"name"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// jobPreview is "job.preview": rows a preview showed — a note for every
// window; the rows themselves go to the origin tab's grid.
type jobPreview struct {
	Run    string   `json:"run"`
	Name   string   `json:"name"`
	Origin string   `json:"origin"`
	Title  string   `json:"title"`
	Rows   int      `json:"rows"`
	Cols   []string `json:"cols"`
}

// onJob turns the engine's events into window events (see the table at
// the top). It runs on the run's goroutine, in the run's order.
func (s *Server) onJob(ev jobs.Event) {
	switch e := ev.(type) {
	case *jobs.RunStarted:
		s.hub.broadcast("job.run", map[string]any{"run": e.Run})
	case *jobs.Progress:
		s.hub.broadcast("job.progress", map[string]any{"run": e.Run, "name": e.Pipeline, "fragment": e.Fragment})
		// (the page knows each run's source and origin from job.run, or
		// from GET /api/v1/runs when it loaded mid-run)
	case *jobs.Logged:
		s.hub.broadcast("job.line", jobLine{Run: e.Run, Name: e.Pipeline, Level: e.Line.Level, Text: e.Line.Text})
	case *jobs.Preview:
		s.landPreview(e)
	case *jobs.State:
		// a job's step started, ended or was skipped; a queued run began
		s.hub.broadcast("job.state", e)
	case *jobs.Notice:
		// the scheduler's word — a fire skipped, missed, a job that does
		// not parse — or a record that could not be written; also said
		// where dbc web was started, since no page may be open at 02:00
		s.opt.Logf("%s", e.Text)
		s.hub.broadcast("job.notice", e)
	case *jobs.RunDone:
		s.hub.broadcast("job.done", map[string]any{"run": e.Run})
	}
}

// landPreview puts a preview's rows in the grid of the tab that asked for
// them (its workspace's result set, as a script's s.Show lands), and tells
// every window. A tab closed since is no error: the rows had nowhere to go.
func (s *Server) landPreview(e *jobs.Preview) {
	r := e.Result
	// a preview sink names its result "preview <what>" on the fragment
	// (pipeline's previewSink): <what> is the node a preview run put it
	// after, or a real preview sink's title — its "fragment/node" when it
	// has none. The title says the fragment once, either way.
	what := strings.TrimPrefix(r.Query, "preview ")
	if !strings.HasPrefix(what, r.Conn+"/") {
		what = r.Conn + "/" + what
	}
	title := "preview " + what
	s.hub.broadcast("job.preview", jobPreview{Run: e.Run, Name: e.Pipeline, Origin: e.Origin, Title: title,
		Rows: r.RowCount(), Cols: nonNil(r.Columns)})
	t, err := s.hub.get(e.Origin)
	if err != nil {
		return
	}
	if err = t.ws.ShowResult(e.Run, title, r); err != nil {
		t.logOn("", "warn", serr.UserMsgFromErr(err, err.Error()))
		return
	}
	t.send("result", resultEvent{Conn: t.ws.Active(), Sets: scriptSets(t.ws), resultTabsState: resultTabsOf(t.ws)})
}
