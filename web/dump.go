package web

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/pgdump"
)

// "Dump database…" in the Connections menu: a Postgres connection dumped
// with pg_dump. What is checked, how the connection reaches pg_dump, which
// pg_dump runs and what is said are package pgdump's, shared with the CLI
// and the TUI; this is HTTP and the event stream.
//
//	GET  /api/v1/dump?conn=X ─► the dialog's suggested path, and the dump
//	                            running now (if any), for its Stop row
//	POST /api/v1/dump {conn, …pgdump.Form}
//	     ─► Form.Options, Prepare (asks the server its version, finds
//	        pg_dump): refused? ok: false with the reason, for the dialog
//	     ─► else the dump starts in the background; the answer is at once
//	        ok: true, and the dump goes on:
//	           every line ──► "dump" event {level, text}  ─► every window's log
//	           start, end ──► "dump" event {running}      ─► the menu's rows
//	POST /api/v1/dump/stop ─► cancel: pg_dump interrupted, partial output
//	                          removed; the end comes as events like any
//
// WHY IN THE BACKGROUND. A dump takes as long as the database is big —
// minutes, hours — and a request held that long ties the dialog to one
// page load: a reload would lose the outcome, and nothing could stop it.
// So the POST answers once the dump has started, and the dump reports on
// the event stream, which every window has and a reload reconnects.
//
// The dump writes on the machine dbc web runs on, as dbc's own files
// (consoles, scripts) are: dbc web is a local workbench.
//
// One dump at a time, across windows, as in the TUI: dumps are long and
// heavy on the server, and a second is more likely a double click than a
// plan. Shutdown stops a running one, so pg_dump does not outlive dbc.

// prepareDump readies a dump; tests swap in one that needs no server.
var prepareDump = pgdump.Prepare

// dumpJob is the dump running now.
type dumpJob struct {
	conn    string
	out     string
	format  pgdump.Format
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{} // closed once the dump has ended and cleaned up
}

// dumpState is the running dump as events and GET carry it.
type dumpState struct {
	Conn    string `json:"conn"`
	Out     string `json:"out"`
	Format  string `json:"format"`
	Started string `json:"started"` // RFC 3339
}

// dumps holds the one running dump.
type dumps struct {
	mu  sync.Mutex
	job *dumpJob
}

func (d *dumps) state() *dumpState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.job == nil {
		return nil
	}
	return &dumpState{Conn: d.job.conn, Out: d.job.out, Format: string(d.job.format),
		Started: d.job.started.Format(time.RFC3339)}
}

// handleDumpInfo is GET /api/v1/dump?conn=X.
func (s *Server) handleDumpInfo(ctx rweb.Context) error {
	conn := ctx.Request().QueryParam("conn")
	out := map[string]any{"running": s.dumps.state()}
	if conn != "" {
		// plain's; the dialog moves it to another format's suffix as the
		// format changes, as pgdump.SwapSuffix does in the TUI
		out["out"] = pgdump.DefaultOut(conn, pgdump.Plain, time.Now())
	}
	return ok(ctx, out)
}

// dumpReq is POST /api/v1/dump's body: the connection, and the dialog's
// fields as pgdump.Form has them.
type dumpReq struct {
	Conn string `json:"conn"`
	pgdump.Form
}

// handleDumpStart is POST /api/v1/dump.
func (s *Server) handleDumpStart(ctx rweb.Context) error {
	var req dumpReq
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if req.Conn == "" {
		return fail(ctx, badRequest("name the connection to dump"))
	}
	if st := s.dumps.state(); st != nil {
		return fail(ctx, conflict("a dump of %s is still running — one at a time", st.Conn))
	}
	refused := func(err error) error {
		return ok(ctx, map[string]any{"ok": false, "error": serr.UserMsgFromErr(err, err.Error())})
	}
	opts, err := req.Form.Options()
	if err != nil {
		return refused(err)
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := prepareDump(c, s.cfg, s.mgr, req.Conn, opts)
	switch {
	case errors.Is(err, pgdump.ErrNoConn):
		return fail(ctx, badRequest("%s", err.Error()))
	case err != nil:
		return refused(err)
	}

	job := &dumpJob{conn: req.Conn, out: opts.Out, format: opts.Format, started: time.Now(), done: make(chan struct{})}
	runCtx, stop := context.WithCancel(context.Background())
	job.cancel = stop
	s.dumps.mu.Lock()
	if other := s.dumps.job; other != nil { // another window's press, between the check above and here
		s.dumps.mu.Unlock()
		stop()
		return fail(ctx, conflict("a dump of %s is still running — one at a time", other.conn))
	}
	s.dumps.job = job
	s.dumps.mu.Unlock()
	s.hub.broadcast("dump", map[string]any{"running": s.dumps.state()})

	go func() {
		defer close(job.done)
		_ = run.Report(runCtx, func(level, text string) {
			s.hub.broadcast("dump", map[string]any{"level": level, "text": text, "conn": req.Conn})
		})
		stop()
		s.dumps.mu.Lock()
		s.dumps.job = nil
		s.dumps.mu.Unlock()
		s.hub.broadcast("dump", map[string]any{"running": s.dumps.state()})
	}()
	return ok(ctx, map[string]any{"ok": true, "out": opts.Out, "tool": run.Tools.Version})
}

// handleDumpStop is POST /api/v1/dump/stop: interrupt the running dump.
// Its end is reported on the event stream like any end.
func (s *Server) handleDumpStop(ctx rweb.Context) error {
	s.dumps.mu.Lock()
	job := s.dumps.job
	s.dumps.mu.Unlock()
	if job == nil {
		return ok(ctx, map[string]any{"ok": false, "error": "no dump is running"})
	}
	job.cancel()
	return ok(ctx, map[string]any{"ok": true, "conn": job.conn})
}

// stopDump stops a running dump for Shutdown, waiting up to grace for it
// to end, so pg_dump does not outlive dbc and its partial output is
// removed rather than left looking like a dump.
func (s *Server) stopDump(grace time.Duration) {
	s.dumps.mu.Lock()
	job := s.dumps.job
	s.dumps.mu.Unlock()
	if job == nil {
		return
	}
	s.opt.Logf("stopping the dump of %s…", job.conn)
	job.cancel()
	select {
	case <-job.done:
	case <-time.After(grace):
	}
}
