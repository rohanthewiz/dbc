package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// DOWNLOADING IT (N-171). With dbc web serving another machine (--listen),
// the dialog's ⤓ Download writes a single-file dump (plain, custom, tar)
// into a temp directory of the server instead, and hands it to the page
// that asked once it is done:
//
//	POST /api/v1/dump {…, download: true} ─► {ok, token}; the dump runs as any
//	     dump runs, to <temp>/pg-….sql
//	the end ──► "dump" event {ready: token, name, size} ─► that page fetches
//	GET /api/v1/dump/file/:token ──► the file, as an attachment, once: then removed
//
// Not streamed as pg_dump writes it: rweb holds a response's body in
// memory, and only server-sent events go out a piece at a time. So the
// file is sent whole, and a dump over MaxDumpDownload stays on the server,
// at the path the log names. One never fetched is removed after
// dumpFileTTL, and every one at Shutdown.
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

// dumps holds the one running dump, and the finished downloads waiting
// for their page (ready, by token).
type dumps struct {
	mu    sync.Mutex
	job   *dumpJob
	ready map[string]readyDump
}

// readyDump is a downloaded dump's file, waiting to be fetched.
type readyDump struct {
	path string // in dir
	dir  string // the temp directory, removed with it
	name string // the download's file name
}

// MaxDumpDownload is the biggest dump the dialog's ⤓ Download sends: the
// response is held in memory whole (see DOWNLOADING IT).
const MaxDumpDownload = 512 << 20

// dumpFileTTL is how long a finished download waits for its page.
const dumpFileTTL = 30 * time.Minute

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
	// Download: write it to a temp file and hand it to the page (see
	// DOWNLOADING IT); Form's out is then ignored.
	Download bool `json:"download"`
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
	if req.Download && strings.TrimSpace(req.Form.Out) == "" {
		req.Form.Out = "download" // replaced below: a download's file is the server's temp one
	}
	opts, err := req.Form.Options()
	if err != nil {
		return refused(err)
	}
	var token, tmp string
	if req.Download {
		if opts.Format.ToDir() {
			return refused(errors.New("only a single-file dump (plain, custom, tar) downloads — a directory stays on the server"))
		}
		if tmp, err = os.MkdirTemp("", "dbc-download-"); err != nil {
			return fail(ctx, serr.Wrap(err))
		}
		opts.Out = filepath.Join(tmp, filepath.Base(pgdump.DefaultOut(req.Conn, opts.Format, time.Now())))
		token = newDumpToken()
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := prepareDump(c, s.cfg, s.mgr, req.Conn, opts)
	if err != nil && tmp != "" {
		_ = os.RemoveAll(tmp)
	}
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
		say := func(level, text string) {
			s.hub.broadcast("dump", map[string]any{"level": level, "text": text, "conn": req.Conn})
		}
		err := run.Report(runCtx, say)
		stop()
		s.dumps.mu.Lock()
		s.dumps.job = nil
		s.dumps.mu.Unlock()
		if tmp != "" {
			s.readyDownload(err, token, tmp, opts.Out, say)
		}
		s.hub.broadcast("dump", map[string]any{"running": s.dumps.state()})
	}()
	return ok(ctx, map[string]any{"ok": true, "out": opts.Out, "tool": run.Tools.Version, "token": token})
}

// readyDownload ends a download's dump: a failed one's temp directory
// removed; one too big to send left where it is, said; else the file kept
// under token for its page, which the "dump" event's ready tells to fetch
// it — and removed after dumpFileTTL if it never does.
func (s *Server) readyDownload(err error, token, dir, path string, say func(level, text string)) {
	if err != nil {
		_ = os.RemoveAll(dir)
		return
	}
	size := pgdump.Size(path)
	if size > MaxDumpDownload {
		say("warn", fmt.Sprintf("the dump is %s, more than dbc web sends to a browser (%s): it stays on the server, at %s",
			pgdump.HumanSize(size), pgdump.HumanSize(MaxDumpDownload), path))
		return
	}
	name := filepath.Base(path)
	s.dumps.mu.Lock()
	if s.dumps.ready == nil {
		s.dumps.ready = map[string]readyDump{}
	}
	s.dumps.ready[token] = readyDump{path: path, dir: dir, name: name}
	s.dumps.mu.Unlock()
	time.AfterFunc(dumpFileTTL, func() { s.dropDownload(token) })
	s.hub.broadcast("dump", map[string]any{"ready": token, "name": name, "size": size})
}

// dropDownload forgets a waiting download and removes its file; a no-op
// for a token already fetched or dropped.
func (s *Server) dropDownload(token string) {
	s.dumps.mu.Lock()
	r, ok := s.dumps.ready[token]
	delete(s.dumps.ready, token)
	s.dumps.mu.Unlock()
	if ok {
		_ = os.RemoveAll(r.dir)
	}
}

// handleDumpFile is GET /api/v1/dump/file/:token: a finished download's
// file, as an attachment, once — it is removed as it is sent.
func (s *Server) handleDumpFile(ctx rweb.Context) error {
	token := ctx.Request().PathParam("token")
	s.dumps.mu.Lock()
	r, found := s.dumps.ready[token]
	delete(s.dumps.ready, token)
	s.dumps.mu.Unlock()
	if !found {
		return fail(ctx, notFound("no dump waiting under that name — it was fetched already, or waited too long"))
	}
	defer os.RemoveAll(r.dir)
	body, err := os.ReadFile(r.path)
	if err != nil {
		return fail(ctx, serr.Wrap(err))
	}
	h := ctx.Response()
	h.SetHeader("Content-Type", "application/octet-stream")
	h.SetHeader("Content-Disposition", `attachment; filename="`+r.name+`"`)
	h.SetHeader("Cache-Control", "no-store")
	return ctx.Bytes(body)
}

// newDumpToken names a download: unguessable, since its route serves the
// dump to whoever holds it (and is signed in).
func newDumpToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
	defer s.dropDownloads() // after the dump: one stopped now ends as a failure, its file gone
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

// dropDownloads removes every download nobody fetched, at Shutdown: they
// are dbc's temp files, not the user's.
func (s *Server) dropDownloads() {
	s.dumps.mu.Lock()
	var waiting []string
	for token := range s.dumps.ready {
		waiting = append(waiting, token)
	}
	s.dumps.mu.Unlock()
	for _, token := range waiting {
		s.dropDownload(token)
	}
}
