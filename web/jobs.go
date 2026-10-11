package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// Jobs: DAGs of pipelines (package jobs), the JSON specs in jobs_dir, run
// by the server's engine — by hand from the page, on their cron lines by
// the server's scheduler, and from outside through the webhook. The jobs
// tab that draws them is Phase 4; these routes are what it, curl and a
// cron on another machine use.
//
// THE STORE is the pipelines' protocol (pipelines.go), for jobs:
//
//	GET    /api/v1/jobs                     the list: jobs (next fire, last run), examples, trash
//	GET    /api/v1/jobs/:name               {text, rev}
//	PUT    /api/v1/jobs/:name?win=          {text, base} → {rev} | {conflict, text, rev}
//	POST   /api/v1/jobs/:name/rename?win=   {to}
//	DELETE /api/v1/jobs/:name?win=          to .trash → {id}
//	POST   /api/v1/job-trash/:id/restore    {to?} → {name}
//	GET    /api/v1/job-examples/:name       a built-in example's text
//	POST   /api/v1/job-check                {text} → {diags, fires, layout}  (each diag's line and
//	                                        col in the text; the next five fires per cron line;
//	                                        where the jobs tab draws each step)
//	GET    /api/v1/jobs/:name/layout        the saved job's (or the example's) layout
//
// Every change to a job tells every window ("jobs") and the scheduler,
// which reads the directory again at once.
//
// RUNNING ONE — and the webhook, which is the same route:
//
//	POST   /api/v1/jobs/:name/run           {params, ws?} → {run}
//
//	  the page (session cookie)        trigger "manual"; ws, when given, is
//	                                   the run's origin (its Stop)
//	  Authorization: Bearer <secret>   trigger "webhook" — allowed only for a
//	                                   job whose triggers say "webhook": true,
//	                                   403 otherwise: the secret opens the whole
//	                                   API, but a job runs from outside only
//	                                   when it says so
//
//	  409 when the job is running (overlap: skip) or a queued run already
//	  waits; 400 when it does not check out. GET /api/v1/runs/:id follows
//	  the run, from memory or its record.
//
// RUNS, with the record on disk now, list across processes — a cron's
// `dbc job run` included:
//
//	GET    /api/v1/runs?kind=&name=&status=&since=&limit=   adds {runs}: the history, newest first
//	GET    /api/v1/runs/:id                 the record, and for a job its layout (as it ran)
//	POST   /api/v1/runs/:id/cancel          409 when another process runs it (`dbc run cancel`)
//
// THE LAYOUT is package jobs' (layout.go, over package dag): the server
// places the cards and the page draws them, so the jobs tab's canvas and
// the Runs view's run page put each step where the other does.

// jobsList is GET /api/v1/jobs.
type jobsList struct {
	Dir      string                   `json:"dir"`
	Short    string                   `json:"short"`
	Jobs     []jobRow                 `json:"jobs"`
	Examples []jobRow                 `json:"examples"`
	Trash    []userdata.SpecTrashInfo `json:"trash"`
}

// jobRow is one job as the list shows it: the file's facts, when the
// scheduler fires it next (examples are never scheduled), and its last run.
type jobRow struct {
	userdata.JobInfo
	Next *time.Time    `json:"next,omitempty"`
	Last *jobs.Summary `json:"last,omitempty"`
}

func (s *Server) handleJobs(ctx rweb.Context) error {
	infos, err := userdata.ListJobs(s.cfg.JobsDir)
	if err != nil {
		return fail(ctx, err)
	}
	var fires map[string]time.Time
	if s.sched != nil {
		fires = s.sched.NextFires()
	}
	out := jobsList{Dir: s.cfg.JobsDir, Short: config.TildePath(s.cfg.JobsDir), Jobs: []jobRow{}, Examples: []jobRow{},
		Trash: nonNil(userdata.ListJobTrash(s.cfg.JobsDir))}
	for _, in := range infos {
		row := jobRow{JobInfo: in}
		if t, ok := fires[in.Name]; ok {
			row.Next = &t
		}
		row.Last = s.lastJobRun(strings.TrimSuffix(in.Name, ".json"))
		out.Jobs = append(out.Jobs, row)
	}
	for _, ex := range scripts.Jobs() {
		row := jobRow{JobInfo: userdata.JobInfo{Name: ex.Name, Desc: ex.Desc}}
		if spec, err := jobs.ParseJob(ex.Text); err == nil {
			row.Pipelines, row.Schedule, row.Webhook = len(spec.Pipelines), spec.Triggers.Schedule, spec.Triggers.Webhook
			row.Last = s.lastJobRun(spec.Name)
		}
		out.Examples = append(out.Examples, row)
	}
	return ok(ctx, out)
}

// lastJobRun is a job's newest run, live or recorded; nil when none.
func (s *Server) lastJobRun(name string) *jobs.Summary {
	if last, ok := s.jobs.LastRun(jobs.KindJob, name); ok {
		return &last
	}
	return nil
}

// jobName is the request's :name, checked before any file is touched.
func jobName(ctx rweb.Context) (string, error) {
	name := ctx.Request().PathParam("name")
	if !userdata.ValidJobName(name) {
		return name, badRequest("not a job name: %q (letters, digits, '.', '-' or '_', ending in .json)", name)
	}
	return name, nil
}

// jobErr maps the store's errors onto statuses, as pipelineErr does.
func (s *Server) jobErr(err error, name string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notFound("no job %s in %s", name, config.TildePath(s.cfg.JobsDir))
	case errors.Is(err, userdata.ErrJobExists):
		return conflict("%s already has a job named %s", config.TildePath(s.cfg.JobsDir), name)
	case errors.Is(err, userdata.ErrBadJobName):
		return badRequest("not a job name: %q", name)
	}
	return err
}

// jobsChanged tells the windows and the scheduler a job file changed.
func (s *Server) jobsChanged(ev scriptsEvent) {
	s.hub.broadcast("jobs", ev)
	if s.sched != nil {
		s.sched.Reload()
	}
}

func (s *Server) handleJobRead(ctx rweb.Context) error {
	name, err := jobName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	text, rev, err := userdata.ReadJob(s.cfg.JobsDir, name)
	if err != nil {
		return fail(ctx, s.jobErr(err, name))
	}
	return ok(ctx, scriptText{Text: text, Rev: rev})
}

// handleJobSave is PUT /api/v1/jobs/:name: the pipelines' save. A job that
// does not check out saves (its diags say what is missing), but it must
// be a JSON object.
func (s *Server) handleJobSave(ctx rweb.Context) error {
	name, err := jobName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req scriptSave
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxJobBytes {
		return fail(ctx, badRequest("the job is %d KB; jobs save up to %d KB", len(req.Text)>>10, userdata.MaxJobBytes>>10))
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal([]byte(req.Text), &obj); err != nil {
		return fail(ctx, badRequest("not saved — the job is not a JSON object: %v", err))
	}
	rev, isConflict, err := userdata.SaveJob(s.cfg.JobsDir, name, req.Text, req.Base)
	if err != nil {
		return fail(ctx, s.jobErr(err, name))
	}
	if isConflict {
		text, _, _ := userdata.ReadJob(s.cfg.JobsDir, name)
		return ok(ctx, scriptSaved{Rev: rev, Conflict: true, Text: text})
	}
	s.jobsChanged(scriptsEvent{Op: "saved", Name: name, Rev: rev, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, scriptSaved{Rev: rev})
}

func (s *Server) handleJobRename(ctx rweb.Context) error {
	name, err := jobName(ctx)
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
	if !userdata.ValidJobName(req.To) {
		return fail(ctx, badRequest("not a job name: %q (letters, digits, '.', '-' or '_', ending in .json)", req.To))
	}
	if err = userdata.RenameJob(s.cfg.JobsDir, name, req.To); err != nil {
		if errors.Is(err, userdata.ErrJobExists) {
			return fail(ctx, s.jobErr(err, req.To))
		}
		return fail(ctx, s.jobErr(err, name))
	}
	s.jobsChanged(scriptsEvent{Op: "renamed", Name: name, To: req.To, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": req.To})
}

func (s *Server) handleJobTrash(ctx rweb.Context) error {
	name, err := jobName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	id, err := userdata.TrashJob(s.cfg.JobsDir, name)
	if err != nil {
		return fail(ctx, s.jobErr(err, name))
	}
	s.jobsChanged(scriptsEvent{Op: "trashed", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"id": id})
}

func (s *Server) handleJobRestore(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	var req struct {
		To string `json:"to"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(req.To)
	if req.To != "" && !userdata.ValidJobName(req.To) {
		return fail(ctx, badRequest("not a job name: %q (letters, digits, '.', '-' or '_', ending in .json)", req.To))
	}
	if !slices.ContainsFunc(userdata.ListJobTrash(s.cfg.JobsDir), func(t userdata.SpecTrashInfo) bool { return t.ID == id }) {
		return fail(ctx, notFound("no trashed job %q", id))
	}
	name, err := userdata.RestoreJob(s.cfg.JobsDir, id, req.To)
	if err != nil {
		return fail(ctx, s.jobErr(err, cmp.Or(req.To, id)))
	}
	s.jobsChanged(scriptsEvent{Op: "restored", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": name})
}

func (s *Server) handleJobExample(ctx rweb.Context) error {
	ex, found := scripts.JobByName(ctx.Request().PathParam("name"))
	if !found {
		return fail(ctx, notFound("no example job %q", ctx.Request().PathParam("name")))
	}
	return ok(ctx, map[string]string{"name": ex.Name, "desc": ex.Desc, "text": ex.Text})
}

// cronFires is one cron line's next fires, for an editor to show under it:
// what the line means, in times.
type cronFires struct {
	Expr  string      `json:"expr"`
	Next  []time.Time `json:"next"`
	Error string      `json:"error,omitempty"`
}

// handleJobCheck is POST /api/v1/job-check: a job's text, saved or not,
// checked — its pipelines found as a run would find them — with each cron
// line's next five fire times.
func (s *Server) handleJobCheck(ctx rweb.Context) error {
	var req struct {
		Text string `json:"text"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxJobBytes {
		return fail(ctx, badRequest("the job is %d KB; jobs are checked up to %d KB", len(req.Text)>>10, userdata.MaxJobBytes>>10))
	}
	spec, err := jobs.ParseJob(req.Text)
	if err != nil {
		// no layout: the page keeps the last one it had while the text
		// is half-typed (the JSON view). The error is placed as a
		// pipeline's is: a syntax error at its offset, an unknown key
		// where it is written.
		line, col := pipeline.ParseErrorAt(req.Text, err)
		return ok(ctx, map[string]any{"diags": []pipeDiag{{Diag: pipeline.Diag{Severity: pipeline.SevError,
			Msg: strings.TrimPrefix(err.Error(), "json: ")}, Line: line, Col: col}}, "fires": []cronFires{}})
	}
	var conns []string
	for _, c := range s.cfg.Conns() {
		conns = append(conns, c.Name)
	}
	// each finding placed in the text (jobs.Locate, which the TUI's
	// file:line:col shares), for the JSON view's markers
	found := jobs.CheckJob(spec, jobs.CheckOptions{Find: jobs.PipelineFinder(s.cfg.PipelinesDir), Conns: conns})
	diags := make([]pipeDiag, len(found))
	for i, d := range found {
		diags[i] = pipeDiag{Diag: d}
		diags[i].Line, diags[i].Col = jobs.Locate(req.Text, d.Where)
	}
	fires := []cronFires{}
	loc, lerr := spec.Location()
	for _, expr := range spec.Triggers.Schedule {
		cf := cronFires{Expr: expr, Next: []time.Time{}}
		switch sc, err := jobs.ParseCron(expr, loc); {
		case lerr != nil:
			cf.Error = lerr.Error()
		case err != nil:
			cf.Error = err.Error()
		default:
			cf.Next = append(cf.Next, sc.NextN(time.Now(), 5)...)
		}
		fires = append(fires, cf)
	}
	return ok(ctx, map[string]any{"diags": diags, "fires": fires, "layout": spec.Layout()})
}

// handleJobLayout is GET /api/v1/jobs/:name/layout: where the jobs tab
// draws a saved job's steps (the user's file, else the example of that
// name). The tab itself asks job-check, which lays out the text being
// edited; this is for a caller with only a name.
func (s *Server) handleJobLayout(ctx rweb.Context) error {
	name, err := jobName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	spec, _, err := jobs.LoadJob(s.cfg.JobsDir, name)
	if err != nil {
		if strings.Contains(err.Error(), "no such job") {
			return fail(ctx, notFound("no job %s in %s, and no example of that name", name, config.TildePath(s.cfg.JobsDir)))
		}
		return fail(ctx, badRequest("%s does not parse: %s", name, strings.TrimPrefix(err.Error(), "json: ")))
	}
	return ok(ctx, spec.Layout())
}

// jobRunReq is POST /api/v1/jobs/:name/run's body; empty is fine.
type jobRunReq struct {
	WS     string            `json:"ws"`
	Params map[string]string `json:"params"`
}

// handleJobRun runs a job — the user's file, else the example of that
// name — by hand from the page, or as the webhook; see the top.
func (s *Server) handleJobRun(ctx rweb.Context) error {
	name, err := jobName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req jobRunReq
	if len(ctx.Request().Body()) > 0 {
		if err = decode(ctx, &req); err != nil {
			return fail(ctx, err)
		}
	}
	if req.WS != "" {
		if _, err := s.hub.get(req.WS); err != nil {
			return fail(ctx, err)
		}
	}
	spec, file, err := jobs.LoadJob(s.cfg.JobsDir, name)
	if err != nil {
		if strings.Contains(err.Error(), "no such job") {
			return fail(ctx, notFound("no job %s in %s, and no example of that name", name, config.TildePath(s.cfg.JobsDir)))
		}
		return fail(ctx, badRequest("%s does not parse: %s", name, strings.TrimPrefix(err.Error(), "json: ")))
	}
	trigger, by := jobs.TriggerManual, ""
	if s.auth.checkBearer(ctx.Request().Header("Authorization")) {
		if !spec.Triggers.Webhook {
			return fail(ctx, forbidden("job %s does not take webhook runs — set \"webhook\": true in its triggers", spec.Name))
		}
		trigger, by = jobs.TriggerWebhook, ctx.ClientIP()
	}
	run, err := s.jobs.StartJob(jobs.JobRequest{Spec: spec, Params: req.Params, Trigger: trigger, By: by,
		Origin: req.WS, Source: file})
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

// forbidden is a request the user's own settings turn away.
func forbidden(format string, args ...any) error {
	r := badRequest(format, args...).(*reqError)
	r.status = http.StatusForbidden
	return r
}

// runsFilter reads GET /api/v1/runs's history filters; ok false when none
// was given (the page's catch-up asks for no history).
func runsFilter(ctx rweb.Context) (f userdata.RunFilter, given bool, err error) {
	q := ctx.Request().QueryParam
	f.Kind, f.Name, f.Status = q("kind"), strings.TrimSuffix(q("name"), ".json"), q("status")
	given = f.Kind != "" || f.Name != "" || f.Status != "" || q("since") != "" || q("limit") != "" || q("history") != ""
	if v := q("since"); v != "" {
		t, perr := time.Parse(time.RFC3339, v)
		if perr != nil {
			d, derr := time.ParseDuration(v)
			if derr != nil {
				return f, given, badRequest("since is an RFC 3339 time or a duration such as 168h, not %q", v)
			}
			t = time.Now().Add(-d)
		}
		f.Since = t
	}
	f.Limit = 100
	if v := q("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 0 {
			return f, given, badRequest("limit is a number, not %q", v)
		}
		f.Limit = n
	}
	return f, given, nil
}
