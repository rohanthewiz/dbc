package web

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/workspace"
)

// A browser tab is a WINDOW holding one or more QUERY TABS. Each query tab
// is a workspace: its own workspace.Workspace — active connection, pinned
// session, run slot, last result, plan — so a BEGIN in one query tab does
// not leak into another, exactly as two TUI windows behave. The window owns
// what there is one of per browser tab: the event stream, the assistant,
// and the idle clock.
//
//	window ─┬─ SSE stream (one EventSource)      events carry "ws": the query tab
//	        ├─ assistant (chat.go)               asks about the active query tab
//	        ├─ query tab 1 ── Workspace (session A)
//	        └─ query tab 2 ── Workspace (session B)
//
// WHY ONE STREAM PER WINDOW, NOT PER QUERY TAB: a browser allows about six
// HTTP/1.1 connections per host, and an EventSource holds one for good. A
// stream per query tab would let the seventh query tab — or three windows
// of three — starve every fetch the page makes. So every event goes out on
// the window's stream as {"type", "ws", "data"}, and the page routes it by
// ws: the active query tab draws it; a background one marks its tab busy or
// done and prefixes its log lines.
//
// A window's life:
//
//	POST /api/v1/ws        ──► a new window and its first query tab
//	POST /api/v1/ws {win}  ──► another query tab in that window
//	POST /api/v1/win/:id/tabs ──► the saved tabs it will show claimed, so
//	  no other window shows (and saves over) them — see claims.go
//	GET  /api/v1/win/:id/events ──► the window's stream attached
//	  commands (POST /api/v1/ws/:id/run, …/cancel) start Jobs; their events
//	  go out on the window's stream, tagged with the query tab
//	DELETE /api/v1/ws/:id  ──► a query tab closed: its session released
//	stream detached (browser tab closed, laptop asleep) ── idle clock starts
//	  a new stream within the grace period reattaches: same sessions, same
//	  open transactions (a reload keeps the ids in sessionStorage)
//	idle > releaseAfter (conn_idle_timeout) ──► every query tab's session
//	  released (transactions rolled back); the workspaces stay usable
//	idle > forgetAfter (a day) ──► the window and its query tabs forgotten;
//	  their ids now answer 404 and the page opens new ones
//
// Why a clock rather than closing on disconnect: EventSource drops and
// reconnects on its own — a flaky network, a sleeping laptop, a reload —
// and a transaction the user left open must survive that.

// forgetAfter is how long a window with no stream is kept at all.
const forgetAfter = 24 * time.Hour

// hub owns the open windows and their query tabs.
type hub struct {
	mu   sync.Mutex
	wins map[string]*window
	tabs map[string]*tab // every window's query tabs, by id

	// claims: saved tab key (web.bytdb) → the window showing it; see
	// claims.go. A window's claims stand only while it is live.
	claims     map[string]*window
	claimGrace time.Duration

	// newWorkspace builds a query tab's workspace, wired to send its mid-run
	// events (a script's output) to the window's stream.
	newWorkspace func(sink func(workspace.Event)) *workspace.Workspace
	// newChat builds a window's assistant (chat.go). It starts nothing: the
	// agent is spawned when the pane is first opened.
	newChat func(w *window) *assistant

	releaseAfter time.Duration // 0: never release an idle window's sessions
	forgetAfter  time.Duration
}

// window is one browser tab: its stream, its assistant, its idle clock.
type window struct {
	id string

	// sse is safe for concurrent sends, which matters: a window has many
	// senders — request handlers, each run's ticker, each Job's goroutine,
	// the assistant. (rweb before v0.1.32 raced on a per-client drop counter
	// here, and a per-window send mutex worked around it.) Each send is one
	// channel send per stream, so every stream still sees one order of events.
	sse *rweb.SSEHub

	chat *assistant // the assistant pane's conversation (chat.go)

	// reaper state, guarded by hub.mu
	idleSince time.Time // zero while a stream is attached
	released  bool      // the idle release has run since the last attach

	// claim state (claims.go). detached is when the last stream went away
	// (unix nanos; the window's birth before any did) — set from the SSE
	// hub's disconnect hook, under that hub's lock, hence atomic. handedOff,
	// guarded by hub.mu: the page said goodbye (pagehide), so its claims
	// are gone and a page booting on this window is its reload, not a copy.
	detached  atomic.Int64
	handedOff bool
}

// tab is one query tab: a workspace and the page's views of it.
type tab struct {
	id  string
	win *window
	ws  *workspace.Workspace

	// the grid's cache of the last result and its sort order (grid.go);
	// rvSeq numbers results, so a copy can tell it is still looking at
	// the one the page drew
	viewMu sync.Mutex
	rv     resultView
	rvSeq  int
	ps     planState // the Plan tab's plan and its "before" (plan.go)
}

func newHub(newWS func(sink func(workspace.Event)) *workspace.Workspace, releaseAfter time.Duration) *hub {
	return &hub{
		wins: map[string]*window{}, tabs: map[string]*tab{}, newWorkspace: newWS,
		claims: map[string]*window{}, claimGrace: claimGrace,
		releaseAfter: releaseAfter, forgetAfter: forgetAfter,
	}
}

// open makes a query tab: in window winID when it is given, else in a new
// window. The workspace does no IO yet: the page asks for a connect once
// the window's stream is attached, so that connect's events have somewhere
// to go.
func (h *hub) open(winID string) (*tab, error) {
	var w *window
	if winID != "" {
		var err error
		if w, err = h.window(winID); err != nil {
			return nil, err
		}
	} else {
		w = &window{id: newID(), idleSince: time.Now()}
		w.detached.Store(time.Now().UnixNano()) // claims.go: its grace runs from birth until a stream attaches
		w.sse = rweb.NewSSEHub(rweb.SSEHubOptions{
			// A run's events come in bursts (notes, then the outcome); 256
			// absorbs any burst a browser tab could fall behind on. A client
			// that stays full through 3 sends is gone, and is dropped; its
			// EventSource reconnects if it was merely slow.
			ChannelSize: 256,
			MaxDropped:  3,
			// a comment line every 20 s keeps an idle stream from being cut
			// by anything in between, and lets a dead peer be noticed
			HeartbeatInterval: 20 * time.Second,
			// a detached stream starts the window's claim grace (claims.go)
			OnDisconnect: w.onDetach,
		})
		w.chat = h.newChat(w)
	}
	t := &tab{id: newID(), win: w}
	t.ws = h.newWorkspace(t.sink)
	h.mu.Lock()
	defer h.mu.Unlock()
	if winID != "" && h.wins[winID] != w {
		return nil, notFound("no window %q — it was forgotten meanwhile", winID)
	}
	h.wins[w.id] = w
	h.tabs[t.id] = t
	return t, nil
}

// get finds a query tab by id.
func (h *hub) get(id string) (*tab, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tabs[id]
	if !ok {
		return nil, notFound("no workspace %q — it was closed or dbc web restarted", id)
	}
	return t, nil
}

// window finds a window by id.
func (h *hub) window(id string) (*window, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, ok := h.wins[id]
	if !ok {
		return nil, notFound("no window %q — it was forgotten or dbc web restarted", id)
	}
	return w, nil
}

// tabsOf lists a window's query tabs.
func (h *hub) tabsOf(w *window) []*tab {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tabsOfLocked(w)
}

func (h *hub) tabsOfLocked(w *window) []*tab {
	var out []*tab
	for _, t := range h.tabs {
		if t.win == w {
			out = append(out, t)
		}
	}
	return out
}

// close closes one query tab: its run stopped, its session released — the
// transaction it held rolled back — and its id forgotten. The window and
// its other query tabs are untouched. Releasing waits for a statement to
// return from its cancel, so it runs on its own goroutine.
func (h *hub) close(id string) (*tab, error) {
	h.mu.Lock()
	t, ok := h.tabs[id]
	delete(h.tabs, id)
	h.mu.Unlock()
	if !ok {
		return nil, notFound("no workspace %q — it was closed or dbc web restarted", id)
	}
	t.ws.Stop()
	go t.ws.Close()
	return t, nil
}

// reap applies the idle rules to every window; run periodically. Releasing
// waits on the sessions (a statement still returning from its cancel), so
// it runs off the lock, on its own goroutine per query tab.
func (h *hub) reap(now time.Time) {
	var release []*tab
	var forget []*window
	var forgetTabs [][]*tab
	h.mu.Lock()
	for id, w := range h.wins {
		if w.sse.ClientCount() > 0 {
			w.idleSince, w.released = time.Time{}, false
			continue
		}
		if w.idleSince.IsZero() {
			w.idleSince = now
		}
		idle := now.Sub(w.idleSince)
		switch {
		case idle >= h.forgetAfter:
			ts := h.tabsOfLocked(w)
			for _, t := range ts {
				delete(h.tabs, t.id)
			}
			delete(h.wins, id)
			h.dropClaimsLocked(w)
			forget, forgetTabs = append(forget, w), append(forgetTabs, ts)
		case h.releaseAfter > 0 && idle >= h.releaseAfter && !w.released:
			w.released = true
			release = append(release, h.tabsOfLocked(w)...)
		}
	}
	h.mu.Unlock()
	for _, t := range release {
		go t.ws.Close()
	}
	for i, w := range forget {
		go func() {
			w.chat.close()
			for _, t := range forgetTabs[i] {
				t.ws.Close()
			}
			w.sse.Close()
		}()
	}
}

// closeAll stops and releases every query tab's workspace — the shutdown
// path. Each Close waits for its statement to return from the cancel, so
// they run in parallel, and the whole thing gives up after grace: a driver
// that never answers its cancel must not keep dbc from exiting.
func (h *hub) closeAll(grace time.Duration) (timedOut bool) {
	h.mu.Lock()
	tabs := make([]*tab, 0, len(h.tabs))
	for _, t := range h.tabs {
		tabs = append(tabs, t)
	}
	wins := make([]*window, 0, len(h.wins))
	for _, w := range h.wins {
		wins = append(wins, w)
	}
	h.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range tabs {
		t.ws.Stop() // cancel everything first, so no Close waits on another's run
	}
	for _, t := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t.ws.Close()
		}()
	}
	for _, w := range wins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.chat.close() // saved, as quitting the TUI saves
			w.sse.Close()
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

// liveChat reports whether a saved conversation is the live one of some
// window's assistant — which must not be deleted from the list, since that
// window's next save would write it straight back.
func (h *hub) liveChat(id string) bool {
	h.mu.Lock()
	wins := make([]*window, 0, len(h.wins))
	for _, w := range h.wins {
		wins = append(wins, w)
	}
	h.mu.Unlock()
	for _, w := range wins {
		if w.chat.liveID() == id {
			return true
		}
	}
	return false
}

// count is the number of open query tabs, for the health check.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tabs)
}

// ---------------------------------------------------------------------------
// Sending
// ---------------------------------------------------------------------------

// wireEvent is one event as the page reads it. WS names the query tab it
// belongs to; "" is the window's own (the assistant's).
type wireEvent struct {
	Type string `json:"type"`
	WS   string `json:"ws,omitempty"`
	Data any    `json:"data"`
}

// send puts one event on the window's stream. It is the JSON rweb's
// BroadcastAny would write, plus "ws" — sent raw, as a "message" event, so
// the page keeps its single onmessage switch.
func (w *window) send(typ, ws string, data any) {
	b, err := json.Marshal(wireEvent{Type: typ, WS: ws, Data: data})
	if err != nil {
		return // every event type here is plain data; this cannot happen
	}
	w.sse.BroadcastRaw(rweb.SSEvent{Type: "message", Data: string(b)})
}

// send puts one of this query tab's events on its window's stream.
func (t *tab) send(typ string, data any) { t.win.send(typ, t.id, data) }

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
	// HasPlan: the result is itself a query plan (an EXPLAIN the user
	// typed), which the page opens in its Plan tab, as the TUI does.
	HasPlan bool `json:"hasPlan"`
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
		if ev.Plan != nil {
			t.setPlan(ev.Plan)
			out.HasPlan = true
			// the raw rows stay in the Results tab, one keypress (p) away
			t.send("log", logLine{Level: "accent",
				Text: "that result is a query plan — shown in the ◈ Plan tab (p switches back to the raw rows)"})
		}
		if ev.Script && ev.Err == nil {
			// a script's results reached the grid as it showed them (the
			// sink's "result" events); its outcome's status is that of the
			// last one shown, as in the TUI, or just that it completed
			out.Status = fmt.Sprintf("%s completed in %s", ev.Tag, ev.Elapsed.Round(time.Millisecond))
			if r := t.ws.LastResult(); r != nil {
				out.HasResult = true
				out.Status = workspace.ResultStatus(r, s.cfg.MaxRows, s.shown(r))
			}
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
	case *workspace.ExplainDone:
		if ev.Stale {
			return
		}
		s.deliverExplain(t, ev)
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
