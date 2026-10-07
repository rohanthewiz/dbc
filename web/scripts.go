package web

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/sdb/sdbapi"
	"github.com/rohanthewiz/dbc/userdata"
	"github.com/rohanthewiz/dbc/workspace"
)

// Scripts: the Go files in scripts_dir — listed, read, written, renamed,
// trashed and restored here, checked without running, and run as the TUI's
// Ctrl+O picker does. A script runs through workspace.RunScript — the run
// slot, cancel and history rules of any run — and its mid-run output goes
// where the TUI puts it: s.Print lines to the log, s.Show results to the
// grid (hub.go's sink), each as it happens.
//
// THE PAGE NAMES A SCRIPT, NEVER A PATH. Every :name is checked with
// userdata.ValidScriptName before anything touches the disk, and the store
// joins it to the resolved scripts_dir itself; a trash :id is checked the
// same way (userdata.parseTrashID). Nothing from the browser can reach a
// file outside scripts_dir or its .trash.
//
//	GET    /api/v1/scripts                    the browser's list: scripts,
//	                                          examples, templates, trash
//	GET    /api/v1/scripts/:name              {text, rev}
//	PUT    /api/v1/scripts/:name?win=         {text, base} → {rev} | {conflict, text, rev}
//	POST   /api/v1/scripts/:name/rename       {to}
//	DELETE /api/v1/scripts/:name              to .trash → {id}
//	POST   /api/v1/script-trash/:id/restore   {to?} → {name}
//	POST   /api/v1/script-check               {name, text} → {diags}  (unsaved text)
//	GET    /api/v1/script-examples/:name      a built-in example's text
//	GET    /api/v1/script-templates/:name     a template, filled with connections
//	GET    /api/v1/script-api                 the sdb API, for completion
//	POST   /api/v1/ws/:id/script              {name}: run the saved file
//	POST   /api/v1/ws/:id/show-result         {i}: the grid to the run's ith s.Show
//
// Why script-check and not scripts/check: rweb's router is a radix tree
// that does not backtrack. With a literal child such as "examples/" beside
// :name under /api/v1/scripts/, a script whose name shares that literal's
// first letters (export_report.go, trash.go) walks into the literal's
// branch, fails there, and is a 404 instead of falling back to :name. So
// nothing but :name sits under /api/v1/scripts/
// (TestScriptNamesBesideRouteWords).
//
// Saves follow the console protocol (consoles.go): a PUT names the revision
// it was made from, and a file changed since — by another window, the TUI,
// vim — is not overwritten; the answer carries the file's text instead.
// Every change is told to every window as a "scripts" event, so an open
// browser list redraws and a script tab notices its file moved on.

// scriptFiles lists the scripts, by file name, sorted. A directory that
// cannot be read lists nothing, and handleScripts reports why.
func (s *Server) scriptFiles() []string {
	infos, _ := userdata.ListScripts(s.cfg.ScriptsDir)
	names := make([]string, len(infos))
	for i, in := range infos {
		names[i] = in.Name
	}
	return names
}

// scriptsList is GET /api/v1/scripts: everything the scripts browser draws.
// dir is the resolved, absolute directory (config.ResolveScriptsDir) and
// short its ~-form, for the title: the page always says where it looked.
type scriptsList struct {
	Dir       string                `json:"dir"`
	Short     string                `json:"short"`
	Scripts   []userdata.ScriptInfo `json:"scripts"`
	Examples  []scripts.Example     `json:"examples"`  // name and desc; the text by its own route
	Templates []scripts.Template    `json:"templates"` // the New menu, in order
	Trash     []userdata.TrashInfo  `json:"trash"`     // newest first
}

// handleScripts lists the scripts, the built-in examples, the templates and
// the trash. Empty lists are [] rather than null, so the page can take
// .length of each without a guard.
func (s *Server) handleScripts(ctx rweb.Context) error {
	infos, err := userdata.ListScripts(s.cfg.ScriptsDir)
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, scriptsList{
		Dir: s.cfg.ScriptsDir, Short: config.TildePath(s.cfg.ScriptsDir),
		Scripts:   nonNil(infos),
		Examples:  nonNil(scripts.Examples()),
		Templates: nonNil(scripts.Templates()),
		Trash:     nonNil(userdata.ListTrash(s.cfg.ScriptsDir)),
	})
}

// nonNil makes a nil slice an empty one, for JSON's [] over null.
func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

// scriptName is the request's :name, checked. A name the store would
// refuse is a 400 here, before any file is touched.
func scriptName(ctx rweb.Context) (string, error) {
	name := ctx.Request().PathParam("name")
	if !userdata.ValidScriptName(name) {
		return name, badRequest("not a script name: %q (letters, digits, '.', '-' or '_', ending in .go)", name)
	}
	return name, nil
}

// scriptErr maps the store's errors onto statuses: a missing file is a
// 404, a name already taken a 409, anything else falls through to fail's
// 500 (an unreadable directory, a full disk).
func (s *Server) scriptErr(err error, name string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notFound("no script %s in %s", name, config.TildePath(s.cfg.ScriptsDir))
	case errors.Is(err, userdata.ErrScriptExists):
		return conflict("%s already has a script named %s", config.TildePath(s.cfg.ScriptsDir), name)
	case errors.Is(err, userdata.ErrBadScriptName):
		return badRequest("not a script name: %q", name)
	}
	return err
}

// scriptText is a script as the page loads it.
type scriptText struct {
	Text string `json:"text"`
	Rev  string `json:"rev"`
}

// handleScriptRead is GET /api/v1/scripts/:name.
func (s *Server) handleScriptRead(ctx rweb.Context) error {
	name, err := scriptName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	text, rev, err := userdata.ReadScript(s.cfg.ScriptsDir, name)
	if err != nil {
		return fail(ctx, s.scriptErr(err, name))
	}
	return ok(ctx, scriptText{Text: text, Rev: rev})
}

// scriptSave is a PUT's body. Base "" creates: it is refused (409) when the
// name is taken, so New and Duplicate never overwrite a script.
type scriptSave struct {
	Text string `json:"text"`
	Base string `json:"base"`
}

// scriptSaved is a PUT's answer: the new revision, or — Conflict — the
// file's text and revision, which were not overwritten. A file deleted
// since the base was read is a conflict with rev "" and no text; saving
// again with base "" puts it back.
type scriptSaved struct {
	Rev      string `json:"rev"`
	Conflict bool   `json:"conflict,omitempty"`
	Text     string `json:"text,omitempty"`
}

// scriptsEvent is the window-level "scripts" event: one script changed.
// Win is the window that changed it, which already knows. A save carries
// the new revision but not the text — a script can be a megabyte, and only
// a window with that script open wants it; it fetches the text itself.
type scriptsEvent struct {
	Op   string `json:"op"` // "saved", "renamed", "trashed" or "restored"
	Name string `json:"name"`
	To   string `json:"to,omitempty"`  // renamed: the new name
	Rev  string `json:"rev,omitempty"` // saved: the new revision
	ID   string `json:"id,omitempty"`  // trashed, restored: the trash id
	Win  string `json:"win"`
}

// handleScriptSave is PUT /api/v1/scripts/:name?win=<window>. The
// compare-and-write is the store's (userdata.SaveScript), under its own
// lock; this only maps the outcome and tells the other windows.
func (s *Server) handleScriptSave(ctx rweb.Context) error {
	name, err := scriptName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req scriptSave
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxScriptBytes {
		return fail(ctx, badRequest("the script is %d KB; scripts save up to %d KB", len(req.Text)>>10, userdata.MaxScriptBytes>>10))
	}
	rev, isConflict, err := userdata.SaveScript(s.cfg.ScriptsDir, name, req.Text, req.Base)
	if err != nil {
		return fail(ctx, s.scriptErr(err, name))
	}
	if isConflict {
		// the file's text now, for the page to load as an undoable edit;
		// "" when it was deleted (rev "" says so)
		text, _, _ := userdata.ReadScript(s.cfg.ScriptsDir, name)
		return ok(ctx, scriptSaved{Rev: rev, Conflict: true, Text: text})
	}
	s.hub.broadcast("scripts", scriptsEvent{Op: "saved", Name: name, Rev: rev, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, scriptSaved{Rev: rev})
}

// handleScriptRename is POST /api/v1/scripts/:name/rename?win= {to}.
func (s *Server) handleScriptRename(ctx rweb.Context) error {
	name, err := scriptName(ctx)
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
	if !userdata.ValidScriptName(req.To) {
		return fail(ctx, badRequest("not a script name: %q (letters, digits, '.', '-' or '_', ending in .go)", req.To))
	}
	if err = userdata.RenameScript(s.cfg.ScriptsDir, name, req.To); err != nil {
		if errors.Is(err, userdata.ErrScriptExists) {
			return fail(ctx, s.scriptErr(err, req.To))
		}
		return fail(ctx, s.scriptErr(err, name))
	}
	s.hub.broadcast("scripts", scriptsEvent{Op: "renamed", Name: name, To: req.To, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": req.To})
}

// handleScriptTrash is DELETE /api/v1/scripts/:name?win=: a move into
// .trash, not a delete. The answer's id is what restore takes, so the page
// can offer an Undo right away.
func (s *Server) handleScriptTrash(ctx rweb.Context) error {
	name, err := scriptName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	id, err := userdata.TrashScript(s.cfg.ScriptsDir, name)
	if err != nil {
		return fail(ctx, s.scriptErr(err, name))
	}
	s.hub.broadcast("scripts", scriptsEvent{Op: "trashed", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"id": id})
}

// handleScriptRestore is POST /api/v1/script-trash/:id/restore?win= {to}.
// to "" restores under the old name; a taken name is a 409, and the page
// asks for another.
func (s *Server) handleScriptRestore(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	var req struct {
		To string `json:"to"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(req.To)
	if req.To != "" && !userdata.ValidScriptName(req.To) {
		return fail(ctx, badRequest("not a script name: %q (letters, digits, '.', '-' or '_', ending in .go)", req.To))
	}
	// the id is checked by the store; one it cannot parse is not in the
	// trash, whatever it names
	if !slices.ContainsFunc(userdata.ListTrash(s.cfg.ScriptsDir), func(t userdata.TrashInfo) bool { return t.ID == id }) {
		return fail(ctx, notFound("no trashed script %q", id))
	}
	name, err := userdata.RestoreScript(s.cfg.ScriptsDir, id, req.To)
	if err != nil {
		return fail(ctx, s.scriptErr(err, cmp.Or(req.To, id)))
	}
	s.hub.broadcast("scripts", scriptsEvent{Op: "restored", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": name})
}

// scriptCheck is a check's body: the editor's text, saved or not, so the
// markers follow typing. Name is only for messages; it need not exist.
type scriptCheck struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// handleScriptCheck is POST /api/v1/script-check. script.Check never runs
// the script (no Run, no init, no initializers) and costs about 2 ms, so
// the page can call it on every pause in typing. Diags is [] when clean.
func (s *Server) handleScriptCheck(ctx rweb.Context) error {
	var req scriptCheck
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxScriptBytes {
		return fail(ctx, badRequest("the script is %d KB; scripts are checked up to %d KB", len(req.Text)>>10, userdata.MaxScriptBytes>>10))
	}
	return ok(ctx, map[string]any{"diags": nonNil(script.Check(cmp.Or(req.Name, "script.go"), req.Text))})
}

// handleScriptExample is GET /api/v1/script-examples/:name: a built-in
// sample's text, read-only. Duplicate is the page PUTting it under a new
// name with base "".
func (s *Server) handleScriptExample(ctx rweb.Context) error {
	ex, found := scripts.ExampleByName(ctx.Request().PathParam("name"))
	if !found {
		return fail(ctx, notFound("no example %q", ctx.Request().PathParam("name")))
	}
	return ok(ctx, map[string]string{"name": ex.Name, "desc": ex.Desc, "text": ex.Text})
}

// handleScriptTemplate is GET /api/v1/script-templates/:name?conn=: a
// template's text with connection names put in (scripts.Fill). The tab's
// connection, when the page passes one, goes first; the default connection
// next, then the rest in config order — so a copy template made from a tab
// on prod copies from prod into the next connection. The page then PUTs it
// with base "" under the name the user chose.
func (s *Server) handleScriptTemplate(ctx rweb.Context) error {
	text, found := scripts.Fill(ctx.Request().PathParam("name"), s.templateConns(ctx.Request().QueryParam("conn")))
	if !found {
		return fail(ctx, notFound("no template %q", ctx.Request().PathParam("name")))
	}
	return ok(ctx, map[string]string{"text": text})
}

// templateConns orders the connection names for a template: first, if it
// is a connection; the default; the rest. Each name once. The rule lives in
// config.ConnOrder, which the TUI's browser fills its templates through too.
func (s *Server) templateConns(first string) []string {
	return s.cfg.ConnOrder(first)
}

// handleScriptAPI is GET /api/v1/script-api: the sdb API as data, for
// the script editor's completion and hover (package sdbapi, generated from
// the sdb source and embedded). Served as the envelope's data, raw — it is
// already JSON.
func (s *Server) handleScriptAPI(ctx rweb.Context) error {
	return ok(ctx, json.RawMessage(sdbapi.JSON()))
}

type scriptReq struct {
	Name string `json:"name"`
}

// handleScript runs a script by name. Busy → 409, as for any run; the
// output and the outcome arrive on the stream.
func (s *Server) handleScript(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req scriptReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if !slices.Contains(s.scriptFiles(), req.Name) {
		return fail(ctx, notFound("no script %q in %s", req.Name, s.cfg.ScriptsDir))
	}
	st, err := t.ws.RunScript(filepath.Join(s.cfg.ScriptsDir, req.Name))
	if err != nil {
		if r, isRefusal := asRefusal(err); isRefusal {
			t.notes(append(st.Notes, r.Note))
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"tag": st.Tag})
}

// handleShowResult is POST /api/v1/ws/:id/show-result {i}: the grid back
// on result i (0-based, among the kept ones) of the last script run's
// s.Show results — the results bar's "Result 1 · 2 · 3". The pick is the
// workspace's (it becomes LastResult), so the grid, its copies and
// exports, and the assistant all see the same result; the page then
// fetches it as after any result.
func (s *Server) handleShowResult(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req struct {
		I int `json:"i"`
	}
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if err = t.ws.ShowScriptResult(req.I); err != nil {
		return fail(ctx, err)
	}
	// the status bar's summary of the picked result, as a run's "run"
	// event carries for its own
	r := t.ws.LastResult()
	return ok(ctx, map[string]any{"sets": scriptSets(t.ws), "status": workspace.ResultStatus(r, s.cfg.MaxRows, s.shown(r))})
}
