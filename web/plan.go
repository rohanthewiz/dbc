package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/workspace"
)

// Explain in the browser. An explain is a run like any other in the
// workspace (the run slot, Ctrl+K, the pinned session); what the web adds
// is the Plan tab, which is the standalone plan page's own view
// (explain/assets/plan.js), mounted in the results pane:
//
//	POST …/explain {buffer, caret, selection, analyze, again}
//	      │  refused → 409 / 400 at once
//	      ▼
//	Job ─► ExplainDone ─► deliver: tab.setPlan (keeps the last plan of the
//	                       same statement as the "before"), log the TUI's
//	                       words, SSE "explain" {hasPlan}
//	                              ▼
//	      GET …/plan ─► {doc, compare} ─► DbcPlan.mount(pane, doc, …)
//
// A plan also arrives in a result — an EXPLAIN the user typed and ran —
// which the workspace recognizes (RunDone.Plan); it lands the same way and
// the "run" event says hasPlan.
//
// The page can also be had whole: GET …/plan.html is the standalone page,
// the same file `dbc explain --open` writes, to open in its own tab, save,
// or send to someone. For someone who will not open a page, the plan is
// also a file: …/plan.pdf, plan.jpg and plan.png (the graph and findings
// drawn by explain.Picture) and plan.mmd (a Mermaid flowchart) — see
// handlePlanFile.

// planState is a tab's plan and the one it replaced, when that was a plan
// of the same statement — the "before" of a tuning session. Guarded by
// tab.viewMu; the plans themselves are never modified once landed.
type planState struct {
	plan, prev *explain.Plan
	seq        int // moves with the plan, so the page can tell a new one
}

// setPlan installs a newly landed plan, keeping the old one as its "before"
// when both explain the same statement.
func (t *tab) setPlan(p *explain.Plan) (prev *explain.Plan) {
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	if explain.SameSubject(t.ps.plan, p) {
		prev = t.ps.plan
	}
	t.ps = planState{plan: p, prev: prev, seq: t.ps.seq + 1}
	return prev
}

func (t *tab) planState() planState {
	t.viewMu.Lock()
	defer t.viewMu.Unlock()
	return t.ps
}

// explainEvent reports an explain that landed.
type explainEvent struct {
	Tag      string `json:"tag"`
	Conn     string `json:"conn"`
	OK       bool   `json:"ok"`
	Stopped  bool   `json:"stopped"`
	Status   string `json:"status"`
	HasPlan  bool   `json:"hasPlan"`
	Stateful bool   `json:"stateful"` // see runEvent
}

// deliverExplain draws a landed explain — the TUI's explainDone: on
// success the plan's gist goes to the log (headline, the comparison with
// the last plan of the statement, the findings) before the explain's own
// notes; on failure, the notes and the status say why.
func (s *Server) deliverExplain(t *tab, ev *workspace.ExplainDone) {
	out := explainEvent{Tag: ev.Tag, Conn: ev.Conn, OK: ev.Err == nil,
		Stopped: errors.Is(ev.Err, db.ErrCanceled), Status: ev.Status}
	if ev.Err == nil && ev.Plan != nil {
		prev := t.setPlan(ev.Plan)
		t.notes(workspace.PlanNotes(ev.Plan, prev, ev.Elapsed))
		out.HasPlan, out.Status = true, workspace.PlanStatus(ev.Plan)
	}
	t.notes(ev.Notes)
	_, out.Stateful = t.ws.Session()
	t.send("explain", out)
}

type explainReq struct {
	runReq
	Analyze bool `json:"analyze"`
	// Again re-explains the plan on show — its own statement, not the
	// editor's — the TUI's e and a keys: after adding an index, say, which
	// is the moment the before/after comparison exists for.
	Again bool `json:"again"`
}

// handleExplain is Ctrl+X / Ctrl+Shift+X from the editor, and "explain
// again" from the Plan tab.
func (s *Server) handleExplain(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req explainReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	var st workspace.Start
	if p := t.planState().plan; req.Again && p != nil && p.Statement != "" {
		// the TUI's reexplain: the plan's statement, on the active
		// connection — said out loud when that is not where it came from
		if active := t.ws.Active(); p.Conn != "" && p.Conn != active {
			t.send("log", logLine{Level: "warn", Text: "this plan is from " + p.Conn + " and " + active +
				" is active — explaining on " + active})
		}
		st, err = t.ws.Explain(p.Statement, "", req.Analyze)
	} else {
		ed := workspace.Editor{Text: req.Buffer, Caret: byteOffset(req.Buffer, req.Caret), Selection: req.Selection}
		st, err = t.ws.ExplainEditor(ed, req.Analyze)
	}
	if err != nil {
		t.notes(st.Notes)
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{r.Note})
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"tag": st.Tag})
}

// planOut is the plan for the Plan tab: the Document JSON the view mounts
// (the same document the standalone page embeds and `dbc explain -t json`
// prints), and how it compares with the plan it replaced.
type planOut struct {
	Seq     int             `json:"seq"`
	Doc     json.RawMessage `json:"doc"`
	Compare string          `json:"compare,omitempty"`
	Better  bool            `json:"better"`
	Status  string          `json:"status"`
	// Again says whether "explain again" can use the plan's own statement:
	// a plan detected in a result has none.
	Again bool `json:"again"`
}

func (s *Server) handlePlan(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	ps := t.planState()
	if ps.plan == nil {
		return ok(ctx, nil)
	}
	doc, err := ps.plan.JSON()
	if err != nil {
		return fail(ctx, err)
	}
	out := planOut{Seq: ps.seq, Doc: doc, Status: workspace.PlanStatus(ps.plan), Again: ps.plan.Statement != ""}
	out.Compare, out.Better = explain.Compare(ps.prev, ps.plan)
	return ok(ctx, out)
}

// handlePlanText is "copy the plan as text" (y) and "copy the engine's own
// output" (Y), the TUI's two plan copies: the text tree with its findings,
// under the statement, for a ticket or a chat; the engine's verbatim
// output for a DBA, a forum or explain.depesz.com. metric sizes the tree's
// bars as the view shows them.
func (s *Server) handlePlanText(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	p := t.planState().plan
	if p == nil {
		return fail(ctx, badRequest("no plan yet — Ctrl+X explains the statement under the caret"))
	}
	q := ctx.Request()
	if q.QueryParam("what") == "mermaid" {
		// the chart's source, for a pull request or a wiki page that draws
		// ```mermaid blocks
		return ok(ctx, map[string]string{"text": p.Mermaid(), "what": "the plan as a Mermaid chart"})
	}
	if q.QueryParam("what") == "raw" {
		if p.Raw == "" {
			return fail(ctx, badRequest("the engine's output was not kept for this plan"))
		}
		return ok(ctx, map[string]string{"text": p.Raw, "what": "the engine's plan output"})
	}
	metric := explain.Metric(q.QueryParam("metric"))
	valid := false
	for _, m := range p.Metrics() {
		valid = valid || m == metric
	}
	if !valid {
		metric = "" // the plan's best
	}
	text := p.Text(explain.TextOptions{Width: 120, Metric: metric, Insights: true})
	if p.Statement != "" {
		text = p.Statement + "\n\n" + text
	}
	return ok(ctx, map[string]string{"text": text, "what": "the plan"})
}

// handlePlanPage is the plan as the standalone page — the same file
// `dbc explain --open` writes — to open in a tab of its own, save, or send.
// ?download=1 makes it an attachment named as WriteHTML names its files.
//
// Its CSP is its own: the page runs one inline script (the view and its
// boot), so script-src allows exactly that script's hash and nothing else,
// and it fetches nothing, so connect-src is 'none'.
func (s *Server) handlePlanPage(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	p := t.planState().plan
	if p == nil {
		return plain(ctx, http.StatusNotFound, "no plan yet — explain a statement first")
	}
	page, err := p.HTML()
	if err != nil {
		return fail(ctx, err)
	}
	h := ctx.Response()
	h.SetHeader("Content-Security-Policy", "default-src 'none'; script-src "+explain.ScriptHash()+
		"; style-src 'unsafe-inline'; img-src data:; connect-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	if ctx.Request().QueryParam("download") == "1" {
		h.SetHeader("Content-Disposition", `attachment; filename="plan-`+fileSafe(p.Conn)+"-"+
			time.Now().Format("20060102-150405")+`.html"`)
	}
	return writePage(ctx, http.StatusOK, page)
}

// planFile is one of the plan's downloadable renderings.
type planFile struct {
	ext, mime string
	render    func(p *explain.Plan, opt explain.PictureOptions) ([]byte, error)
}

var (
	planPDF     = planFile{"pdf", "application/pdf", (*explain.Plan).PDF}
	planJPEG    = planFile{"jpg", "image/jpeg", (*explain.Plan).JPEG}
	planPNG     = planFile{"png", "image/png", (*explain.Plan).PNG}
	planMermaid = planFile{"mmd", "text/plain; charset=utf-8", func(p *explain.Plan, _ explain.PictureOptions) ([]byte, error) {
		return []byte(p.Mermaid()), nil
	}}
)

// handlePlanFile serves the plan as a PDF, a picture, or Mermaid source —
// the forms that travel where a page will not: a chat, a ticket, a slide,
// a pull request.
//
// The picture is drawn as the Plan tab shows it, so what is sent is what
// was on screen: ?metric= is the metric the view is sized by (one the plan
// lacks falls back to its best, as the text copy does), ?theme=light the
// view's light palette, and the before/after comparison rides in the
// header when there is one. ?download=1 makes it an attachment named as
// the page's download is; without it the browser shows it in a tab.
//
// The bytes are drawn in Go from the plan the tab holds — nothing from the
// request reaches them but those three choices — and are served with the
// type they are (the security headers every response carries include
// nosniff), so a picture cannot be read as a page.
func (s *Server) handlePlanFile(f planFile) rweb.Handler {
	return func(ctx rweb.Context) error {
		t, err := s.hub.get(ctx.Request().PathParam("id"))
		if err != nil {
			return fail(ctx, err)
		}
		ps := t.planState()
		if ps.plan == nil {
			return plain(ctx, http.StatusNotFound, "no plan yet — explain a statement first")
		}
		q := ctx.Request()
		opt := explain.PictureOptions{Metric: explain.Metric(q.QueryParam("metric")), Palette: theme.Default()}
		if q.QueryParam("theme") == "light" {
			opt.Palette = theme.Light()
		}
		opt.Compare, _ = explain.Compare(ps.prev, ps.plan)
		body, err := f.render(ps.plan, opt)
		if err != nil {
			return fail(ctx, err)
		}
		h := ctx.Response()
		h.SetHeader("Content-Type", f.mime)
		h.SetHeader("Cache-Control", "no-store")
		if q.QueryParam("download") == "1" {
			h.SetHeader("Content-Disposition", `attachment; filename="plan-`+fileSafe(ps.plan.Conn)+"-"+
				time.Now().Format("20060102-150405")+"."+f.ext+`"`)
		}
		ctx.SetStatus(http.StatusOK)
		return ctx.Bytes(body)
	}
}
