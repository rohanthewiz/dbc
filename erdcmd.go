package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/erd"
	"github.com/rohanthewiz/dbc/theme"
	"github.com/rohanthewiz/dbc/tui"
)

// The erd subcommand: the connection's schema as an entity-relationship
// diagram, headless.
//
//	dbc erd                              Mermaid erDiagram source, to stdout
//	dbc erd -t markdown >> SCHEMA.md     … in a ```mermaid fence, which GitHub draws
//	dbc erd -t png -o schema.png         the diagram as a picture (jpeg too)
//	dbc erd --open                       … straight into the image viewer
//	dbc erd --table orders -t png -o o.png   orders and its neighbours
//	dbc erd --table orders --depth 2 …   … two foreign-key hops out
//	dbc erd --views …                    views too (tables only by default)
//	dbc erd -t png --theme light …       on paper rather than slate
//
// The default format is Mermaid, as the global -t defaults to "text" and
// Mermaid IS the diagram's text form; it is also safe on a terminal, where
// the pictures are refused unless written with -o or piped.
//
// Exit status: 0 fine, 1 reading the schema failed, 2 bad usage (an unknown
// --table included), 130 Ctrl+C.

var (
	flagERDTables []string
	flagERDDepth  int
	flagERDViews  bool
	flagERDOpen   bool
	flagERDTheme  string
)

func erdCommand() *cli.Command {
	return &cli.Command{
		Name:  "erd",
		Usage: "draw the connection's schema as an entity-relationship diagram (Mermaid, PNG or JPEG)",
		Description: "Reads the tables, columns and keys of the connection -c names and draws them as an " +
			"entity-relationship diagram. --format mermaid (the default) is erDiagram source for a pull request or " +
			"a wiki; markdown is the same in a ```mermaid fence; png and jpeg are a picture (written with -o, or " +
			"piped), dark unless --theme light or the config's plan_theme asks for paper. --table narrows it to " +
			"those tables and their neighbours within --depth foreign-key hops.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "table",
				Usage:       "centre the diagram on `TABLE` (repeatable, or comma-separated), as the sidebar names it",
				Destination: &flagERDTables},
			&cli.IntFlag{Name: "depth", Value: 1,
				Usage: "with --table: foreign-key hops to include around it; -1 for all it connects to", Destination: &flagERDDepth},
			&cli.BoolFlag{Name: "views", Usage: "include views (a view named by --table is always shown)",
				Destination: &flagERDViews},
			&cli.BoolFlag{Name: "open",
				Usage: "write the picture (png unless -t jpeg) to a temp file and open it", Destination: &flagERDOpen},
			&cli.StringFlag{Name: "theme",
				Usage:       "palette of a png or jpeg: light|dark (default: the config's plan_theme, else dark)",
				Destination: &flagERDTheme},
		},
		Action: erdAction,
	}
}

// erdFormat is one of the diagram's renderings.
type erdFormat string

const (
	erdMermaid  erdFormat = "mermaid"
	erdMarkdown erdFormat = "markdown"
	erdPNG      erdFormat = "png"
	erdJPEG     erdFormat = "jpeg"
)

// parseERDFormat reads --format for erd. "text", the global flag's
// default, means Mermaid: it is the diagram's text form.
func parseERDFormat(s string) (erdFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text", "mermaid", "mmd":
		return erdMermaid, nil
	case "markdown", "md":
		return erdMarkdown, nil
	case "png":
		return erdPNG, nil
	case "jpeg", "jpg":
		return erdJPEG, nil
	}
	return "", fmt.Errorf("erd renders mermaid, markdown, png or jpeg — not %q", s)
}

// ext is the file extension a rendering is saved under.
func (f erdFormat) ext() string {
	switch f {
	case erdPNG:
		return "png"
	case erdJPEG:
		return "jpg"
	case erdMarkdown:
		return "md"
	}
	return "mmd"
}

func (f erdFormat) binary() bool { return f == erdPNG || f == erdJPEG }

func erdAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("erd")
	if cmd.Args().Present() {
		usage("usage: dbc erd [-c conn] [-t mermaid|markdown|png|jpeg] [-o file] [--table T] [--depth N]")
	}
	f, err := parseERDFormat(flagFormat)
	if err != nil {
		usage(err.Error())
	}
	if flagERDOpen {
		// --open is for looking at the picture; a text format opens as
		// nothing useful, so the picture is what it means
		if !f.binary() {
			f = erdPNG
		}
	}
	// Everything checkable is checked before the catalog is read, as
	// explain checks before it runs: a mistake should cost nothing.
	// --theme with a text format is refused rather than ignored, since a
	// flag that silently does nothing reads as a bug.
	if flagERDTheme != "" {
		if _, err = theme.ByName(flagERDTheme); err != nil {
			usage("--theme: " + err.Error())
		}
		if !f.binary() {
			usage(fmt.Sprintf("--theme colors a png or jpeg — %s has no palette to change", f))
		}
	}
	if f.binary() && flagOut == "" && !flagERDOpen && term.IsTerminal(os.Stdout.Fd()) {
		usage(fmt.Sprintf("%s is binary — write it with -o schema.%s, or pipe it", f, f.ext()))
	}
	var tables []string
	for _, t := range flagERDTables {
		for n := range strings.SplitSeq(t, ",") {
			if n = strings.TrimSpace(n); n != "" {
				tables = append(tables, n)
			}
		}
	}

	cfg, mgr := setup(demoForRun)
	defer mgr.Close()
	warnConfig(cfg)
	conn := pickConn(cfg)

	c, stop := interruptible()
	defer stop()
	s, err := mgr.Schema(c, conn)
	if err != nil {
		if errors.Is(err, db.ErrCanceled) {
			canceled("erd")
		}
		fail(err, "reading the schema failed")
	}
	s, missing := s.Select(erd.Selection{Tables: tables, Depth: flagERDDepth, Views: flagERDViews})
	if len(missing) > 0 {
		usage(fmt.Sprintf("no table %s on %s", strings.Join(missing, ", "), conn))
	}

	name := cfg.PlanTheme
	if flagERDTheme != "" {
		name = flagERDTheme
	}
	pal, _ := theme.ByName(name) // both validated already
	out, err := renderERD(s, f, pal)
	if err != nil {
		fail(err, "render failed")
	}
	if flagERDOpen {
		path, err := s.WriteFile(erd.Dir(), f.ext(), out)
		if err != nil {
			fail(err, "could not save the diagram")
		}
		tui.OpenURL("file://" + path)
		fmt.Fprintln(os.Stderr, "opened the diagram — saved as "+path)
	}
	switch {
	case flagOut != "":
		if err = os.WriteFile(flagOut, out, 0o644); err != nil {
			fail(err, "write failed")
		}
		fmt.Printf("wrote the diagram (%s, %s) to %s\n", f, strings.TrimPrefix(s.Title(), "dbc · "), flagOut)
	case !flagERDOpen:
		_, _ = os.Stdout.Write(out)
	}
	return nil
}

// renderERD renders the diagram in a headless format. Markdown is the
// Mermaid source in a ```mermaid fence, which GitHub, GitLab and most wikis
// draw in place.
func renderERD(s *erd.Schema, f erdFormat, pal theme.Palette) ([]byte, error) {
	switch f {
	case erdPNG:
		return s.PNG(erd.Options{Palette: pal})
	case erdJPEG:
		return s.JPEG(erd.Options{Palette: pal})
	case erdMarkdown:
		return []byte("```mermaid\n" + s.Mermaid() + "```\n"), nil
	}
	return []byte(s.Mermaid()), nil
}
