package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/theme"
)

// The schema as an entity-relationship diagram, in the browser. The Tables
// heading's ⧉ ERD button draws the whole connection; a table's right-click
// "Diagram around it" (or e on a selected table) draws it and its
// neighbours. Either opens a dialog (app.js, erdView) that shows the
// picture and offers its three forms:
//
//	GET …/erd?table=&depth=&views=  ─► {title, tables, rels, text}
//	      the dialog's header, and the Mermaid source for ⧉ Copy — asked
//	      first, so a refusal (no connection, an unknown table) arrives as
//	      words rather than as a broken image
//	GET …/erd.png|.jpg ?…&theme=light&download=1   the picture (erd.Picture)
//	GET …/erd.mmd      ?…&download=1               the Mermaid source
//
// Each request reads the catalog afresh (workspace.Diagram), on the pool
// and outside the run slot, so the diagram is current, drawing it never
// waits for a running query, and nothing is kept server-side between the
// dialog's requests. The reads are three catalog queries; a dialog costs
// two or three sets of them, which is nothing next to drawing the picture.

// erdSelection reads the diagram's selection from the query: table (one
// name, as the sidebar shows it), depth (hops around it, default 1, -1 for
// its whole connected group) and views=1.
func erdSelection(q rweb.ItfRequest) erd.Selection {
	sel := erd.Selection{Depth: 1, Views: q.QueryParam("views") == "1"}
	if t := strings.TrimSpace(q.QueryParam("table")); t != "" {
		sel.Tables = []string{t}
	}
	if d, err := strconv.Atoi(q.QueryParam("depth")); err == nil {
		sel.Depth = d
	}
	return sel
}

// handleERD is the diagram's description and Mermaid source.
func (s *Server) handleERD(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	sc, err := t.ws.Diagram(context.Background(), erdSelection(ctx.Request()))
	if err != nil {
		return fail(ctx, err)
	}
	return ok(ctx, map[string]any{
		"title":  sc.Title(),
		"tables": len(sc.Tables),
		"rels":   len(sc.Rels),
		"text":   sc.Mermaid(),
		"what":   "the diagram as Mermaid",
	})
}

// erdFile is one of the diagram's downloadable renderings.
type erdFile struct {
	ext, mime string
	render    func(s *erd.Schema, opt erd.Options) ([]byte, error)
}

var (
	erdPNG     = erdFile{"png", "image/png", (*erd.Schema).PNG}
	erdJPEG    = erdFile{"jpg", "image/jpeg", (*erd.Schema).JPEG}
	erdMermaid = erdFile{"mmd", "text/plain; charset=utf-8", func(s *erd.Schema, _ erd.Options) ([]byte, error) {
		return []byte(s.Mermaid()), nil
	}}
)

// handleERDFile serves the diagram as a picture or as Mermaid source.
// ?theme=light draws it in the light palette, so what is saved is what the
// dialog showed; ?download=1 makes it an attachment named as the TUI and
// the shell name their files (erd-<conn>-<stamp>.<ext>).
//
// As with a plan's pictures, the bytes are drawn in Go from the catalog —
// nothing of the request reaches them but the selection and the theme —
// and are served as the type they are, under the nosniff header every
// response carries, so a picture cannot be read as a page.
func (s *Server) handleERDFile(f erdFile) rweb.Handler {
	return func(ctx rweb.Context) error {
		t, err := s.hub.get(ctx.Request().PathParam("id"))
		if err != nil {
			return fail(ctx, err)
		}
		q := ctx.Request()
		sc, err := t.ws.Diagram(context.Background(), erdSelection(q))
		if err != nil {
			return fail(ctx, err)
		}
		opt := erd.Options{Palette: theme.Default()}
		if q.QueryParam("theme") == "light" {
			opt.Palette = theme.Light()
		}
		body, err := f.render(sc, opt)
		if err != nil {
			return fail(ctx, err)
		}
		h := ctx.Response()
		h.SetHeader("Content-Type", f.mime)
		h.SetHeader("Cache-Control", "no-store")
		if q.QueryParam("download") == "1" {
			h.SetHeader("Content-Disposition", `attachment; filename="erd-`+fileSafe(sc.Conn)+"-"+
				time.Now().Format("20060102-150405")+"."+f.ext+`"`)
		}
		ctx.SetStatus(http.StatusOK)
		return ctx.Bytes(body)
	}
}
