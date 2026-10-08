package web

import (
	"context"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/pgdocker"
)

// "Postgres in Docker…" in the Connections menu: start a supported
// PostgreSQL major in a local container and add the connection to it. The
// Docker side and the connection it gets are package pgdocker's, shared
// with the TUI (tui/pgdocker.go); this is HTTP.
//
//	GET  /api/v1/pgdocker ─► Check + Statuses: the versions for the dialog's
//	                         dropdown, each with its container's state, or
//	                         why Docker cannot be used
//	POST /api/v1/pgdocker ─► Start (pull, create or start, wait) + Register,
//	                         "conns" to every window; the page then connects
//	POST /api/v1/pgdocker/stop {conn}
//	                      ─► StopTarget (one Register made), refused while a
//	                         query tab in any window is on it; Docker.Stop,
//	                         then the connection's pool dropped
//
// A start answers when the server accepts connections — after a pull, a
// minute or more on a first use. rweb sets no write timeout, so the request
// simply waits; the dialog says why it may.
//
// Docker failing (not installed, not running, a port or name clash) is the
// answer, not a server fault: it comes back as ok: false with docker's
// words, as a connection test's failure does, rather than as a 500 that
// would be logged as dbc's own.

// newPGDocker makes the Docker driver; tests swap in one over a fake CLI.
var newPGDocker = pgdocker.New

// handlePGDocker is GET /api/v1/pgdocker.
func (s *Server) handlePGDocker(ctx rweb.Context) error {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type version struct {
		Major  string `json:"major"`
		EOL    string `json:"eol"`    // "2030-11-14"
		Status string `json:"status"` // "running on :5518", "stopped", ""
	}
	d := newPGDocker(nil)
	if err := d.Check(c); err != nil {
		return ok(ctx, map[string]any{"ok": false, "error": err.Error()})
	}
	sts, err := d.Statuses(c)
	if err != nil {
		return ok(ctx, map[string]any{"ok": false, "error": err.Error()})
	}
	var vs []version
	for _, v := range pgdocker.Supported(time.Now()) {
		vs = append(vs, version{Major: v.Major, EOL: v.EOL.Format(time.DateOnly), Status: sts[v.Major].Text()})
	}
	return ok(ctx, map[string]any{"ok": true, "versions": vs})
}

// handlePGDockerStart is POST /api/v1/pgdocker {major}.
func (s *Server) handlePGDockerStart(ctx rweb.Context) error {
	var req struct {
		Major string `json:"major"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if !pgdocker.Known(req.Major) {
		return fail(ctx, badRequest("PostgreSQL %q is not a version dbc offers", req.Major))
	}
	if !s.pgDockerOne.TryLock() {
		return fail(ctx, conflict("another Postgres in Docker start is still running — wait for it to finish"))
	}
	defer s.pgDockerOne.Unlock()

	// the slow steps, said once the start is done: the page logs them
	var steps []string
	d := newPGDocker(func(st string) { steps = append(steps, st) })
	res, err := d.Start(context.Background(), req.Major)
	if err != nil {
		return ok(ctx, map[string]any{"ok": false, "error": err.Error(), "steps": steps})
	}
	reg, err := pgdocker.Register(s.connEditor(), res, s.inUseRefusal(true))
	if err != nil {
		// the server runs; only its connection is missing, which the page
		// says with the port so it can be added by hand
		return ok(ctx, map[string]any{"ok": false, "steps": steps, "container": res.Container, "port": res.Port,
			"error": "PostgreSQL " + res.Major + " is running in container " + res.Container +
				", but its connection was not saved: " + connErr(err).Error()})
	}
	list := s.connList()
	s.hub.broadcast("conns", list)
	list["ok"] = true
	list["steps"] = steps
	list["name"] = reg.Name
	list["added"] = reg.Added
	list["updated"] = reg.Updated
	list["warnings"] = reg.Warnings
	list["container"] = res.Container
	list["volume"] = pgdocker.VolumeName(res.Major)
	list["port"] = res.Port
	list["created"] = res.Created
	return ok(ctx, list)
}

// handlePGDockerStop is POST /api/v1/pgdocker/stop {conn}: stop the
// container behind a connection Register made. It is refused (409) while
// a query tab is on the connection, as a removal is: the stop would cut
// that tab's session and any transaction it holds. The page offers the row
// only on such a connection and dims it while its own tab is on it; this
// check is the one that counts, across windows.
//
// Like a start, Docker failing is the answer (ok: false), not a 500.
func (s *Server) handlePGDockerStop(ctx rweb.Context) error {
	var req struct {
		Conn string `json:"conn"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	major, err := pgdocker.StopTarget(s.connEditor(), req.Conn)
	if err != nil {
		return fail(ctx, connErr(err))
	}
	if err = s.inUseRefusal(false)(req.Conn); err != nil {
		return fail(ctx, err)
	}
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	name := pgdocker.ContainerName(major)
	was, err := newPGDocker(nil).Stop(c, major)
	// a stopped server has cut the pool's connections; a failed stop
	// leaves nothing worse for the next connect to open afresh
	s.mgr.Drop(req.Conn)
	if err != nil {
		return ok(ctx, map[string]any{"ok": false, "container": name, "error": err.Error()})
	}
	return ok(ctx, map[string]any{"ok": true, "container": name, "was_running": was})
}
