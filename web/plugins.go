package web

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// Plugin files: the user's pipeline plugins in plugins_dir — Go files,
// each one kind of node — listed in the scripts browser, edited in script
// tabs, checked as they are typed, and loaded into the registry the
// pipeline palette draws (GET /api/v1/plugins, pipelines.go).
//
// THE STORE is the scripts protocol (scripts.go) over another directory:
// the page names a file, never a path; a save names the revision it was
// made from; a delete is a move into .trash.
//
//	GET    /api/v1/plugin-files                   the list: files (with what each loaded as), examples, trash
//	GET    /api/v1/plugin-files/:name             {text, rev}
//	PUT    /api/v1/plugin-files/:name?win=        {text, base, fit?} → {rev, renamed?} | {conflict, text, rev}
//	POST   /api/v1/plugin-files/:name/rename?win= {to}
//	DELETE /api/v1/plugin-files/:name?win=        to .trash → {id}
//	POST   /api/v1/plugin-trash/:id/restore       {to?} → {name}
//	GET    /api/v1/plugin-examples/:name          a built-in example's text
//	POST   /api/v1/plugin-check                   {name, text} → {diags}  (script.CheckPlugin, unsaved text)
//
// A PLUGIN FILE IS A SCRIPT TAB with the name "plugin:<file>" — a name no
// script can have (":" is not in the script-name alphabet), so one tab
// strip, one editor, one draft scheme and one conflict protocol serve
// both; the page sends a plugin tab's requests here instead of to
// /api/v1/scripts. The store's changes are told to every window as the
// "scripts" event, under that same prefixed name, so an open plugin tab
// meets a newer file exactly as a script tab does.
//
// LOADING. The registry is process-wide (package script's loader swaps the
// user set in whole). The server loads plugins_dir at start, again after
// every change it makes, and every couple of seconds in case an editor
// outside changed it (pluginWatch: a stat of the directory, nothing more
// when nothing changed). A reload that happened is told to every window
// as "plugins", and the pipeline tabs fetch the registry again — the
// palette's Yours section, the inspector's forms, the check's verdicts.

// pluginPrefix marks a script tab's name as a plugin file's.
const pluginPrefix = "plugin:"

// validPluginTab reports whether a script tab's name is a plugin file's:
// the prefix, then a name the plugin store accepts.
func validPluginTab(name string) bool {
	file, ok := strings.CutPrefix(name, pluginPrefix)
	return ok && userdata.ValidPluginName(file)
}

// pluginWatchEvery is how often the server looks for plugin files changed
// outside it.
var pluginWatchEvery = 2 * time.Second

// syncPlugins reloads the plugins when plugins_dir changed, and tells the
// windows when it did. It is called at start, after every change through
// the API, and by pluginWatch.
func (s *Server) syncPlugins() {
	if !script.SyncPlugins(s.cfg.PluginsDir) {
		return
	}
	probs := pipeline.PluginProblems()
	for _, p := range probs {
		s.opt.Logf("plugin file %s did not load: %s", config.TildePath(p.File), p.Err)
	}
	if s.hub != nil {
		s.hub.broadcast("plugins", map[string]int{"problems": len(probs)})
	}
}

// pluginWatch calls syncPlugins until ctx ends (Run's lifetime).
func (s *Server) pluginWatch(ctx context.Context) {
	t := time.NewTicker(pluginWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncPlugins()
		}
	}
}

// pluginFile is one file in the list: the store's info, and what the
// loader made of it — the plugin's name and kind, or why it did not load.
type pluginFile struct {
	userdata.PluginInfo
	Plugin string        `json:"plugin,omitempty"`
	Kind   pipeline.Kind `json:"kind,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// pluginFilesList is GET /api/v1/plugin-files: what the scripts browser's
// Plugins section draws.
type pluginFilesList struct {
	Dir      string                     `json:"dir"`
	Short    string                     `json:"short"`
	Plugins  []pluginFile               `json:"plugins"`
	Examples []scripts.PluginExample    `json:"examples"`
	Trash    []userdata.PluginTrashInfo `json:"trash"`
}

func (s *Server) handlePluginFiles(ctx rweb.Context) error {
	s.syncPlugins() // a list is a moment to notice an edit made elsewhere
	infos, err := userdata.ListPlugins(s.cfg.PluginsDir)
	if err != nil {
		return fail(ctx, err)
	}
	loaded := map[string]pipeline.Plugin{}
	for _, p := range pipeline.Plugins() {
		if p.File != "" {
			loaded[p.File] = p
		}
	}
	probs := map[string]pipeline.PluginProblem{}
	for _, p := range pipeline.PluginProblems() {
		probs[p.File] = p
	}
	out := pluginFilesList{Dir: s.cfg.PluginsDir, Short: config.TildePath(s.cfg.PluginsDir),
		Plugins: []pluginFile{}, Examples: nonNil(scripts.PluginExamples()),
		Trash: nonNil(userdata.ListPluginTrash(s.cfg.PluginsDir))}
	for _, in := range infos {
		f := pluginFile{PluginInfo: in}
		path := filepath.Join(s.cfg.PluginsDir, in.Name)
		if p, ok := loaded[path]; ok {
			f.Plugin, f.Kind = p.Name, p.Kind
		} else if pr, ok := probs[path]; ok {
			f.Plugin, f.Error = pr.Name, pr.Err
		}
		out.Plugins = append(out.Plugins, f)
	}
	return ok(ctx, out)
}

// pluginName is the request's :name, checked as a script name is.
func pluginName(ctx rweb.Context) (string, error) {
	name := ctx.Request().PathParam("name")
	if !userdata.ValidPluginName(name) {
		return name, badRequest("not a plugin file name: %q (letters, digits, '.', '-' or '_', ending in .go)", name)
	}
	return name, nil
}

// pluginErr maps the store's errors onto statuses, in a plugin's words.
func (s *Server) pluginErr(err error, name string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return notFound("no plugin file %s in %s", name, config.TildePath(s.cfg.PluginsDir))
	case errors.Is(err, userdata.ErrScriptExists):
		return conflict("%s already has a plugin file named %s", config.TildePath(s.cfg.PluginsDir), name)
	case errors.Is(err, userdata.ErrBadScriptName):
		return badRequest("not a plugin file name: %q", name)
	}
	return err
}

// pluginChanged tells the windows a plugin file changed — as a "scripts"
// event under its tab name, see the file comment — and reloads the
// registry, which tells them "plugins" when the load changed.
func (s *Server) pluginChanged(ev scriptsEvent) {
	ev.Name = pluginPrefix + ev.Name
	if ev.To != "" {
		ev.To = pluginPrefix + ev.To
	}
	s.hub.broadcast("scripts", ev)
	s.syncPlugins()
}

func (s *Server) handlePluginRead(ctx rweb.Context) error {
	name, err := pluginName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	text, rev, err := userdata.ReadPlugin(s.cfg.PluginsDir, name)
	if err != nil {
		return fail(ctx, s.pluginErr(err, name))
	}
	return ok(ctx, scriptText{Text: text, Rev: rev})
}

// handlePluginSave is PUT /api/v1/plugin-files/:name?win=: the scripts
// save, and then the reload, so the answer's caller can read the file's
// load result (pluginStatus) right after.
//
// fit, on a create (base ""), says the text is a copy — an example copied,
// a file duplicated — whose Plugin.Name another plugin may already have:
// it is then made the file's own (script.FitPluginName) before the write,
// as the TUI's copy does, and the answer's renamed says from what to what.
// As written, the copy would not load.
func (s *Server) handlePluginSave(ctx rweb.Context) error {
	name, err := pluginName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req struct {
		scriptSave
		Fit bool `json:"fit"`
	}
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxScriptBytes {
		return fail(ctx, badRequest("the plugin file is %d KB; files save up to %d KB", len(req.Text)>>10, userdata.MaxScriptBytes>>10))
	}
	var renamed *pluginRenamed
	if req.Fit && req.Base == "" {
		s.syncPlugins() // taken is asked of the registry: as the files are now
		var from, to string
		if req.Text, from, to = script.FitPluginName(req.Text, name, script.PluginNameTaken); to != "" {
			renamed = &pluginRenamed{From: from, To: to}
		}
	}
	rev, isConflict, err := userdata.SavePlugin(s.cfg.PluginsDir, name, req.Text, req.Base)
	if err != nil {
		return fail(ctx, s.pluginErr(err, name))
	}
	if isConflict {
		text, _, _ := userdata.ReadPlugin(s.cfg.PluginsDir, name)
		return ok(ctx, scriptSaved{Rev: rev, Conflict: true, Text: text})
	}
	s.pluginChanged(scriptsEvent{Op: "saved", Name: name, Rev: rev, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, pluginSaved{scriptSaved: scriptSaved{Rev: rev}, Load: s.pluginStatus(name), Renamed: renamed})
}

// pluginSaved is a plugin save's answer: the scripts answer, what the
// loader made of the saved file, for the log, and the Name a fit changed.
type pluginSaved struct {
	scriptSaved
	Load    pluginLoad     `json:"load"`
	Renamed *pluginRenamed `json:"renamed,omitempty"`
}

// pluginRenamed is the Plugin.Name a fitted copy had, and the one it got.
type pluginRenamed struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// pluginLoad is one file's load result: its plugin, or why there is none.
type pluginLoad struct {
	Plugin string        `json:"plugin,omitempty"`
	Kind   pipeline.Kind `json:"kind,omitempty"`
	Error  string        `json:"error,omitempty"`
}

// pluginStatus is what the last load made of file name.
func (s *Server) pluginStatus(name string) pluginLoad {
	path := filepath.Join(s.cfg.PluginsDir, name)
	for _, p := range pipeline.Plugins() {
		if p.File == path {
			return pluginLoad{Plugin: p.Name, Kind: p.Kind}
		}
	}
	for _, p := range pipeline.PluginProblems() {
		if p.File == path {
			return pluginLoad{Plugin: p.Name, Error: p.Err}
		}
	}
	return pluginLoad{Error: "not loaded"}
}

func (s *Server) handlePluginRename(ctx rweb.Context) error {
	name, err := pluginName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	var req struct {
		To string `json:"to"`
	}
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.To), pluginPrefix))
	if !userdata.ValidPluginName(req.To) {
		return fail(ctx, badRequest("not a plugin file name: %q (letters, digits, '.', '-' or '_', ending in .go)", req.To))
	}
	if err = userdata.RenamePlugin(s.cfg.PluginsDir, name, req.To); err != nil {
		if errors.Is(err, userdata.ErrScriptExists) {
			return fail(ctx, s.pluginErr(err, req.To))
		}
		return fail(ctx, s.pluginErr(err, name))
	}
	s.pluginChanged(scriptsEvent{Op: "renamed", Name: name, To: req.To, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": pluginPrefix + req.To})
}

func (s *Server) handlePluginTrash(ctx rweb.Context) error {
	name, err := pluginName(ctx)
	if err != nil {
		return fail(ctx, err)
	}
	id, err := userdata.TrashPlugin(s.cfg.PluginsDir, name)
	if err != nil {
		return fail(ctx, s.pluginErr(err, name))
	}
	s.pluginChanged(scriptsEvent{Op: "trashed", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"id": id})
}

func (s *Server) handlePluginRestore(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	var req struct {
		To string `json:"to"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	req.To = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.To), pluginPrefix))
	if req.To != "" && !userdata.ValidPluginName(req.To) {
		return fail(ctx, badRequest("not a plugin file name: %q (letters, digits, '.', '-' or '_', ending in .go)", req.To))
	}
	if !slices.ContainsFunc(userdata.ListPluginTrash(s.cfg.PluginsDir), func(t userdata.PluginTrashInfo) bool { return t.ID == id }) {
		return fail(ctx, notFound("no trashed plugin file %q", id))
	}
	name, err := userdata.RestorePlugin(s.cfg.PluginsDir, id, req.To)
	if err != nil {
		return fail(ctx, s.pluginErr(err, cmp.Or(req.To, id)))
	}
	s.pluginChanged(scriptsEvent{Op: "restored", Name: name, ID: id, Win: ctx.Request().QueryParam("win")})
	return ok(ctx, map[string]string{"name": pluginPrefix + name})
}

func (s *Server) handlePluginExample(ctx rweb.Context) error {
	ex, found := scripts.PluginExampleByName(ctx.Request().PathParam("name"))
	if !found {
		return fail(ctx, notFound("no example plugin %q", ctx.Request().PathParam("name")))
	}
	return ok(ctx, map[string]string{"name": ex.Name, "desc": ex.Desc, "text": ex.Text})
}

// handlePluginCheck is POST /api/v1/plugin-check: script.CheckPlugin on
// the editor's text, saved or not — parse, the descriptor and the funcs
// its kind needs read off the syntax tree, the lints, yaegi's compile
// pass. It runs nothing, not even var Plugin; what only running can tell
// (a computed name) the load reports, on save.
func (s *Server) handlePluginCheck(ctx rweb.Context) error {
	var req scriptCheck
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if len(req.Text) > userdata.MaxScriptBytes {
		return fail(ctx, badRequest("the plugin file is %d KB; files are checked up to %d KB", len(req.Text)>>10, userdata.MaxScriptBytes>>10))
	}
	name := strings.TrimPrefix(cmp.Or(req.Name, "plugin.go"), pluginPrefix)
	return ok(ctx, map[string]any{"diags": nonNil(script.CheckPlugin(name, req.Text))})
}
