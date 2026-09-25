package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/workspace"
)

// Each browser tab is a WORKSPACE: its own workspace.Workspace — active
// connection, pinned session, run slot, last result — and its own SSE
// stream. So a BEGIN in one tab does not leak into another, exactly as two
// TUI windows behave.
//
// A tab's life:
//
//	POST /api/v1/ws ──► open: new Workspace + SSE hub, id → the page
//	GET  …/events   ──► stream attached (the page's EventSource)
//	  commands (POST …/run, …/cancel) start Jobs; their events go out on it
//	stream detached (tab closed, laptop asleep) ── idle clock starts
//	  a new stream within the grace period reattaches: same session, same
//	  open transaction (a reload keeps the id in sessionStorage)
//	idle > releaseAfter (conn_idle_timeout) ──► Close: run stopped, session
//	  released (its transaction rolled back); the workspace stays usable
//	idle > forgetAfter (a day) ──► forgotten; its id now answers 404 and the
//	  page opens a new one
//
// Why a clock rather than closing on disconnect: EventSource drops and
// reconnects on its own — a flaky network, a sleeping laptop, a reload —
// and a transaction the user left open must survive that.

// forgetAfter is how long a tab with no stream is kept at all.
const forgetAfter = 24 * time.Hour

// hub owns the open tabs.
type hub struct {
	mu   sync.Mutex
	tabs map[string]*tab

	// newWorkspace builds a tab's workspace, wired to send its mid-run
	// events (a script's output) to the tab's stream.
	newWorkspace func(sink func(workspace.Event)) *workspace.Workspace

	releaseAfter time.Duration // 0: never release an idle tab's session
	forgetAfter  time.Duration
}

// tab is one browser tab's workspace and stream.
type tab struct {
	id  string
	ws  *workspace.Workspace
	sse *rweb.SSEHub

	// sendMu serializes sends to sse. rweb's SSEHub (v0.1.31) updates each
	// client's drop counter under its READ lock, so two broadcasts at once
	// race on it — and a tab has several senders: the request handler, the
	// run's ticker, the Job's goroutine. One lock per tab also gives the
	// page a single order of events.
	sendMu sync.Mutex

	// the grid's cache of the last result and its sort order (grid.go);
	// rvSeq numbers results, so a copy can tell it is still looking at
	// the one the page drew
	viewMu sync.Mutex
	rv     resultView
	rvSeq  int

	// reaper state, guarded by hub.mu
	idleSince time.Time // zero while a stream is attached
	released  bool      // the idle release has run since the last attach
}

func newHub(newWS func(sink func(workspace.Event)) *workspace.Workspace, releaseAfter time.Duration) *hub {
	return &hub{
		tabs: map[string]*tab{}, newWorkspace: newWS,
		releaseAfter: releaseAfter, forgetAfter: forgetAfter,
	}
}

// open makes a new tab. The workspace does no IO yet: the page asks for a
// connect once its stream is attached, so that connect's events have
// somewhere to go.
func (h *hub) open() *tab {
	t := &tab{id: newID(), idleSince: time.Now()}
	t.sse = rweb.NewSSEHub(rweb.SSEHubOptions{
		// A run's events come in bursts (notes, then the outcome); 256
		// absorbs any burst a browser tab could fall behind on. A client
		// that stays full through 3 sends is gone, and is dropped; its
		// EventSource reconnects if it was merely slow.
		ChannelSize: 256,
		MaxDropped:  3,
		// a comment line every 20 s keeps an idle stream from being cut by
		// anything in between, and lets a dead peer be noticed
		HeartbeatInterval: 20 * time.Second,
	})
	t.ws = h.newWorkspace(t.sink)
	h.mu.Lock()
	h.tabs[t.id] = t
	h.mu.Unlock()
	return t
}

// get finds a tab by id.
func (h *hub) get(id string) (*tab, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tabs[id]
	if !ok {
		return nil, notFound("no workspace %q — it was closed or dbc web restarted", id)
	}
	return t, nil
}

// reap applies the idle rules to every tab; run periodically. Releasing
// waits on the session (a statement still returning from its cancel), so it
// runs off the lock, on its own goroutine per tab.
func (h *hub) reap(now time.Time) {
	var release, forget []*tab
	h.mu.Lock()
	for id, t := range h.tabs {
		if t.sse.ClientCount() > 0 {
			t.idleSince, t.released = time.Time{}, false
			continue
		}
		if t.idleSince.IsZero() {
			t.idleSince = now
		}
		idle := now.Sub(t.idleSince)
		switch {
		case idle >= h.forgetAfter:
			delete(h.tabs, id)
			forget = append(forget, t)
		case h.releaseAfter > 0 && idle >= h.releaseAfter && !t.released:
			t.released = true
			release = append(release, t)
		}
	}
	h.mu.Unlock()
	for _, t := range release {
		go t.ws.Close()
	}
	for _, t := range forget {
		go func() { t.ws.Close(); t.sse.Close() }()
	}
}

// closeAll stops and releases every tab's workspace — the shutdown path.
// Each Close waits for its statement to return from the cancel, so they run
// in parallel, and the whole thing gives up after grace: a driver that
// never answers its cancel must not keep dbc from exiting.
func (h *hub) closeAll(grace time.Duration) (timedOut bool) {
	h.mu.Lock()
	tabs := make([]*tab, 0, len(h.tabs))
	for _, t := range h.tabs {
		tabs = append(tabs, t)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range tabs {
		t.ws.Stop() // cancel everything first, so no Close waits on another's run
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.ws.Close()
			t.sse.Close()
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(grace):
		return true
	}
}

// count is the number of open tabs, for the health check.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tabs)
}

// ---------------------------------------------------------------------------
// Sending to a tab
// ---------------------------------------------------------------------------

// send puts one event on the tab's stream as {"type": typ, "data": data},
// the SSE hub's JSON wrapping, so the page has a single onmessage switch.
func (t *tab) send(typ string, data any) {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	t.sse.BroadcastAny(typ, data)
}

// logLine is a log event's data.
type logLine struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// levelNames are the page's CSS class suffixes for workspace.Level.
var levelNames = map[workspace.Level]string{
	workspace.Info: "info", workspace.Ok: "ok", workspace.Warn: "warn",
	workspace.Err: "err", workspace.Accent: "accent", workspace.Muted: "muted",
}

// notes sends the workspace's own words to the tab's log, so the browser
// and the terminal say the same thing.
func (t *tab) notes(ns []workspace.Note) {
	for _, n := range ns {
		t.send("log", logLine{Level: levelNames[n.Level], Text: n.Text})
	}
}

// sink receives the workspace's mid-run events (a script's s.Print and
// s.Show) from the run's own goroutine.
func (t *tab) sink(e workspace.Event) {
	switch e := e.(type) {
	case *workspace.ScriptPrint:
		t.send("log", logLine{Level: "info", Text: e.Text})
	case *workspace.ScriptShow:
		t.send("result", nil) // the page fetches it: see handleResult
	}
}

// ---------------------------------------------------------------------------
// Running a Start
// ---------------------------------------------------------------------------

// tickEvery is how often a running run's status line is refreshed.
const tickEvery = 250 * time.Millisecond

// launch logs a Start's notes and runs its Job on a goroutine, delivering
// the event it returns to the tab. A run (Gen > 0) also gets a ticker, which
// stops by itself once the workspace reports the run over.
func (s *Server) launch(t *tab, st workspace.Start) {
	t.notes(st.Notes)
	if st.Job == nil {
		return
	}
	if st.Gen > 0 {
		t.send("busy", busyEvent{Busy: true, Tag: st.Tag, Status: t.ws.RunningStatus()})
		go func() {
			tick := time.NewTicker(tickEvery)
			defer tick.Stop()
			for range tick.C {
				status, running := t.ws.Ticking(st.Gen)
				if !running {
					return
				}
				t.send("tick", busyEvent{Busy: true, Tag: st.Tag, Status: status})
			}
		}()
	}
	// the Job must be CALLED on the new goroutine: `go s.deliver(t,
	// st.Job())` would evaluate st.Job() right here, on the request's
	// goroutine, holding the POST open for the whole run
	go func() { s.deliver(t, st.Job()) }()
}

// busyEvent is sent when a run starts, and ticks while it runs.
type busyEvent struct {
	Busy   bool   `json:"busy"`
	Tag    string `json:"tag"`
	Status string `json:"status"`
}

// connEvent reports the tab's connection after a connect lands.
type connEvent struct {
	Active  string   `json:"active"`
	Changed bool     `json:"changed"`
	Failed  bool     `json:"failed"`
	Status  string   `json:"status,omitempty"`
	Tables  []tabRef `json:"tables"`
}

// tabRef is a sidebar row for a table or view. QName is the name to put in
// SQL: schema-qualified only when the catalog spans several schemas, the
// TUI's rule (tui/sidebar.go) — "public.cats" is noise when there is only
// public, and necessary when there is also audit.cats.
type tabRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	QName  string `json:"qname"`
	View   bool   `json:"view,omitempty"`
}

// runEvent reports a run that landed. The rows are not in it: a result can
// be thousands of rows, and the page fetches it (GET …/result) only when
// HasResult says there is one — which also means a page that missed this
// event can still catch up.
type runEvent struct {
	Tag       string `json:"tag"`
	Conn      string `json:"conn"`
	OK        bool   `json:"ok"`
	Stopped   bool   `json:"stopped"`
	Status    string `json:"status"`
	HasResult bool   `json:"hasResult"`
	// Stateful raises the page's "session state" badge: the pinned session
	// may hold a transaction, SET values or temp tables that closing it
	// would lose. It is db.Session.Stateful, which errs on the side of yes
	// — any statement that is not a plain read sets it, and nothing clears
	// it but a new session — so the badge says "may hold", never
	// "transaction open".
	Stateful bool `json:"stateful"`
}

// deliver draws a landed event: the workspace has already updated itself,
// and this tells the page. It runs on the Job's goroutine.
func (s *Server) deliver(t *tab, ev workspace.Event) {
	switch ev := ev.(type) {
	case *workspace.Connected:
		if ev.Stale {
			return // superseded by a newer connect, which will report
		}
		t.notes(ev.Notes)
		t.send("conn", connEvent{
			Active: t.ws.Active(), Changed: ev.Changed, Failed: ev.Err != nil,
			Status: ev.Status, Tables: tables(t.ws),
		})
		if ev.Release != nil {
			go func() { s.deliver(t, ev.Release()) }()
		}
	case *workspace.RunDone:
		if ev.Stale {
			return
		}
		t.notes(ev.Notes)
		out := runEvent{
			Tag: ev.Tag, Conn: ev.Conn, OK: ev.Err == nil,
			Stopped: errors.Is(ev.Err, db.ErrCanceled), Status: ev.Status,
			HasResult: ev.Result != nil,
		}
		if r := ev.Result; r != nil {
			shown := s.shown(r)
			out.Status = workspace.ResultStatus(r, s.cfg.MaxRows, shown)
			if shown < len(r.Rows) { // the TUI's words for the same cap
				t.send("log", logLine{Level: "warn", Text: fmt.Sprintf(
					"showing the first %d of %d rows — the rest are fetched, and go into an export (max_display_rows)",
					shown, len(r.Rows))})
			}
		}
		// Session takes the session lock; the run has just released it,
		// and this is the Job's goroutine, so waiting here holds up nobody.
		_, out.Stateful = t.ws.Session()
		t.send("run", out)
	case *workspace.SessionReleased:
		t.notes(ev.Notes)
	}
}

// tables is the active connection's catalog as sidebar rows.
func tables(ws *workspace.Workspace) []tabRef {
	cat := ws.Catalog()
	if cat == nil {
		return []tabRef{}
	}
	refs := db.TableRefs(cat.Rows)
	schemas := map[string]bool{}
	for _, r := range refs {
		schemas[r.Schema] = true
	}
	out := make([]tabRef, len(refs))
	for i, r := range refs {
		q := r.Name
		if len(schemas) > 1 && r.Schema != "" {
			q = r.Schema + "." + r.Name
		}
		out[i] = tabRef{Schema: r.Schema, Name: r.Name, QName: q, View: r.View}
	}
	return out
}

// newID mints a tab id: random, so one tab cannot guess another's.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail on supported platforms
	return hex.EncodeToString(b)
}
