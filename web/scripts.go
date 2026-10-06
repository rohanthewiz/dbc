package web

import (
	"path/filepath"
	"slices"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// Scripts: the Go files in scripts_dir, listed and run as the TUI's Ctrl+O
// picker does. A script runs through workspace.RunScript — the run slot,
// cancel and history rules of any run — and its mid-run output goes where
// the TUI puts it: s.Print lines to the log, s.Show results to the grid
// (hub.go's sink), each as it happens.
//
// THE PAGE NAMES A SCRIPT, NEVER A PATH. The browser picks from the list
// this file serves, and the name is checked against that list again at run
// time — a request cannot reach a .go file outside scripts_dir, and a
// script deleted since the list was drawn is a plain "not found".

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

// handleScripts lists the scripts. dir is the resolved, absolute directory
// (config.ResolveScriptsDir) and short its ~-form, for the picker's title:
// the page always says where it looked.
func (s *Server) handleScripts(ctx rweb.Context) error {
	if _, err := userdata.ListScripts(s.cfg.ScriptsDir); err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, map[string]any{"dir": s.cfg.ScriptsDir, "short": config.TildePath(s.cfg.ScriptsDir),
		"scripts": s.scriptFiles()})
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
