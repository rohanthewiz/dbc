package web

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/designer"
)

// Saved designs: a query design kept under a name, so it outlives the tab
// it was made in (the store's designs table, SavedDesign). A design tab is
// tied to one by Tab.DesignName; while it is, every save of the tab's
// design is written there too, as a console's text is — a design is a
// thing being shaped, not code to commit, so there is no Ctrl+S to forget.
//
//	GET  /api/v1/designs          the list, each with its design and SQL
//	POST /api/v1/designs          save {old, rev, name, conn, design, sql}
//	                              → {name, rev}: "" old makes one, a name
//	                              unlike old renames it (SaveDesign's table)
//	POST /api/v1/designs/delete   {name}
//
// The name rides in the body, not the path: it is a title ("Orders by
// month") with spaces and any letters, and the router hands a path
// parameter over still percent-encoded.
//
// Each change is broadcast as "designs", so an open list in any window
// follows it. A save refused because the design changed since the tab
// read it (another window saved it) is a 409 and the page lets go of the
// name, keeping the tab's copy (app.js designNameLost): two windows never
// overwrite each other's work without a word.

// maxDesignName bounds a name: it is the tab's title too, and a tab
// title's field takes 40.
const maxDesignName = 40

// validDesignName refuses what would make a poor title or list row: blank,
// padded, too long, control characters, a slash (a name might one day be a
// file's).
func validDesignName(name string) error {
	switch {
	case name == "":
		return badRequest("a design needs a name")
	case strings.TrimSpace(name) != name:
		return badRequest("a design's name does not start or end with a space")
	case utf8.RuneCountInString(name) > maxDesignName:
		return badRequest("a design's name is up to %d characters", maxDesignName)
	case strings.ContainsAny(name, `/\`) || strings.ContainsFunc(name, unicode.IsControl):
		return badRequest("a design's name has no slashes or control characters")
	}
	return nil
}

// designRow is a saved design as the list shows it.
type designRow struct {
	SavedDesign
	Rev string `json:"rev"`
	// Tables are the design's tables, by name, for the list's line ("orders,
	// customers") — read off the JSON, so a design that no longer parses is
	// still listed (and opens as an empty canvas, which says so).
	Tables []string `json:"tables"`
}

func (s *Server) handleDesigns(ctx rweb.Context) error {
	list, err := s.store.Designs()
	if err != nil {
		return fail(ctx, err)
	}
	rows := make([]designRow, 0, len(list))
	for _, d := range list {
		r := designRow{SavedDesign: d, Rev: d.Rev(), Tables: []string{}}
		var dd designer.Design
		if json.Unmarshal([]byte(d.Design), &dd) == nil {
			for _, t := range dd.Tables {
				r.Tables = append(r.Tables, t.Name)
			}
		}
		rows = append(rows, r)
	}
	return ok(ctx, map[string]any{"designs": rows})
}

// designSaveReq is a save: Old is the name the page's copy is saved under
// ("" for a new design), Rev the revision it was read or last written as.
type designSaveReq struct {
	Old    string `json:"old"`
	Rev    string `json:"rev"`
	Name   string `json:"name"`
	Conn   string `json:"conn"`
	Design string `json:"design"`
	SQL    string `json:"sql"`
}

func (s *Server) handleDesignSave(ctx rweb.Context) error {
	var req designSaveReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if err := validDesignName(req.Name); err != nil {
		return fail(ctx, err)
	}
	// the same checks a design tab's save makes (saveTab): a saved design
	// that would not parse would reopen as an empty canvas
	if len(req.Design) > maxDesign {
		return fail(ctx, badRequest("the design is %d KB; designs save up to %d KB", len(req.Design)>>10, maxDesign>>10))
	}
	if len(req.SQL) > maxBuffer {
		return fail(ctx, badRequest("the design's SQL is %d MB; up to %d MB is saved", len(req.SQL)>>20, maxBuffer>>20))
	}
	var d designer.Design
	if err := json.Unmarshal([]byte(req.Design), &d); err != nil {
		return fail(ctx, badRequest("not a design: %v", err))
	}
	saved, err := s.store.SaveDesign(req.Old, req.Rev, SavedDesign{Name: req.Name, Conn: req.Conn, Design: req.Design, SQL: req.SQL})
	switch {
	case errors.Is(err, errDesignExists), errors.Is(err, errDesignChanged):
		return fail(ctx, conflict("%s: %v", req.Name, err))
	case errors.Is(err, errNoDesign):
		return fail(ctx, notFound("%s: %v — it was deleted, or renamed, elsewhere", req.Old, err))
	case err != nil:
		return fail(ctx, err)
	}
	s.hub.broadcast("designs", map[string]string{"op": "saved", "name": saved.Name, "old": req.Old})
	return ok(ctx, map[string]any{"name": saved.Name, "rev": saved.Rev(), "updated": saved.Updated.Format(time.RFC3339)})
}

func (s *Server) handleDesignDelete(ctx rweb.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if req.Name == "" {
		return fail(ctx, badRequest("which design?"))
	}
	if err := s.store.DeleteDesign(req.Name); err != nil {
		return fail(ctx, err)
	}
	s.hub.broadcast("designs", map[string]string{"op": "deleted", "name": req.Name})
	return ok(ctx, nil)
}
