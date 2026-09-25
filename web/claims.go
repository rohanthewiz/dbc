package web

import (
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/rweb"
)

// Claims: which window (browser tab) is showing each saved query tab.
//
// The saved tabs (web.bytdb) are one set, but every browser tab of dbc web
// is its own window with its own sessions. Before claims, two windows both
// showed every saved tab, and editing one tab's text in both lost the
// earlier save — each window PUT its whole buffer, last write wins. Now a
// window CLAIMS the saved tabs it shows, and a tab one live window holds is
// not handed to another:
//
//	window A boots ─► POST /api/v1/win/A/tabs ─► every free saved tab, now A's
//	window B boots ─► POST /api/v1/win/B/tabs ─► none free: B starts a fresh
//	                                             tab of its own
//	A saves tab k  ─► PUT /api/v1/tabs/k?win=A ─► k is A's: saved
//	B saves tab k  ─► PUT /api/v1/tabs/k?win=B ─► 409: k is A's
//	A closes (pagehide) ─► POST /api/v1/win/A/release ─► A's tabs free again;
//	                       the next window to boot (or A, reloading) takes them
//
// A claim lasts only while its window is LIVE: it still exists, has not
// said goodbye (release), and has a stream attached — or had one within
// claimGrace. The grace covers a reload and a dropped connection's
// reconnect, where the stream is briefly gone but the window is not; a
// window the browser discarded, or a crash that never sent its release,
// stops holding its tabs once the grace runs out. Liveness is checked when
// a claim is contested, not kept up to date: a dead window's claims sit in
// the map, harmless, until another window takes the tab (or the reaper
// forgets the window).
//
// Why not a lock in the store: the store is shared by every window of ONE
// process (a second process already gets a memory-only store — see
// OpenStore), so the contest is between windows, which only the hub knows.
//
// A duplicated browser tab copies sessionStorage, so it arrives holding the
// original's window id. A boot claim spots that — see claimBoot — and the
// page opens a window of its own instead of sharing the original's.

// claimGrace is how long a window with no stream keeps its claims. A reload
// or an EventSource retry reattaches within seconds; a quarter minute is
// ample without making a closed-and-reopened browser tab wait long if its
// release never arrived.
const claimGrace = 15 * time.Second

// liveLocked reports whether w's claims still stand; hub.mu is held.
func (h *hub) liveLocked(w *window, now time.Time) bool {
	if w == nil || h.wins[w.id] != w || w.handedOff {
		return false
	}
	if w.sse.ClientCount() > 0 {
		return true
	}
	return now.Sub(time.Unix(0, w.detached.Load())) < h.claimGrace
}

// heldLocked is the live window, other than w, holding key; nil when none
// does. hub.mu is held.
func (h *hub) heldLocked(key string, w *window, now time.Time) *window {
	if o := h.claims[key]; o != nil && o != w && h.liveLocked(o, now) {
		return o
	}
	return nil
}

// claim gives window w every key no other live window holds, and reports
// which it got and which are held elsewhere. It also clears w's goodbye: a
// window that claims is showing tabs again (a reload, a restore from the
// back-forward cache).
func (h *hub) claim(w *window, keys []string) (got, held []string) {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	w.handedOff = false
	for _, k := range keys {
		if h.heldLocked(k, w, now) != nil {
			held = append(held, k)
			continue
		}
		h.claims[k] = w
		got = append(got, k)
	}
	return got, held
}

// claimBoot is claim for a page that is booting on window w — reattaching
// after a reload, or starting on a window it just opened. A window that
// still has a stream attached and never said goodbye is being shown by
// another page right now: a duplicated browser tab, which copied the
// original's sessionStorage and with it the window id. That is refused
// (409), and the page opens a window of its own. A reload is not caught by
// this: its old page's pagehide released the window first.
func (h *hub) claimBoot(w *window, keys []string) (got, held []string, err error) {
	h.mu.Lock()
	dup := !w.handedOff && w.sse.ClientCount() > 0
	h.mu.Unlock()
	if dup {
		return nil, nil, conflict("window %q is open in another browser tab", w.id)
	}
	got, held = h.claim(w, keys)
	return got, held, nil
}

// claimOne is the claim a save or a delete of one saved tab makes. winID is
// the window the request came from; "" (a script, an older page) or a
// window this server does not know (it restarted) claims nothing but is
// still turned away from a tab a live window holds. A known window takes
// the tab when it is free — a tab it just opened, typically.
func (h *hub) claimOne(winID, key string) error {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.wins[winID] // nil for "" or an unknown id
	if h.heldLocked(key, w, now) != nil {
		return conflict("that query tab is open in another browser tab of dbc web — its copy here is no longer saved")
	}
	if w != nil {
		h.claims[key] = w
		w.handedOff = false
	}
	return nil
}

// unclaim drops key's claim — its tab was deleted.
func (h *hub) unclaim(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.claims, key)
}

// release is a window's goodbye (its page is going away): every tab it
// holds is free at once, rather than after claimGrace, so a browser tab
// opened right after this one closed gets them.
func (h *hub) release(w *window) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w.handedOff = true
	for k, o := range h.claims {
		if o == w {
			delete(h.claims, k)
		}
	}
}

// heldElsewhere is the set of keys that a live window other than winID's
// holds — the tabs a layout write from winID must not speak for.
func (h *hub) heldElsewhere(winID string) map[string]bool {
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.wins[winID]
	out := map[string]bool{}
	for k := range h.claims {
		if h.heldLocked(k, w, now) != nil {
			out[k] = true
		}
	}
	return out
}

// dropClaimsLocked forgets every claim of a window the reaper forgot, so
// the map does not grow with dead windows. hub.mu is held.
func (h *hub) dropClaimsLocked(w *window) {
	for k, o := range h.claims {
		if o == w {
			delete(h.claims, k)
		}
	}
}

// onDetach is the window's stream-detach hook: it stamps the time, which
// starts claimGrace. It runs under the SSE hub's own lock (rweb calls
// OnDisconnect from inside Unregister), so it must not ask that hub
// anything — an atomic store only.
func (w *window) onDetach() { w.detached.Store(time.Now().UnixNano()) }

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// claimReq names the tabs to claim. Keys nil: every saved tab (a boot).
// Boot: the page is starting on this window, so a window already showing
// in another browser tab is refused (see claimBoot).
type claimReq struct {
	Keys []string `json:"keys"`
	Boot bool     `json:"boot"`
}

// claimResp is what a window got: the saved tabs it now holds (a boot
// draws them) and the keys another window holds.
type claimResp struct {
	Tabs    []Tab    `json:"tabs"`
	Claimed []string `json:"claimed"`
	Held    []string `json:"held"`
}

// handleClaim is POST /api/v1/win/:id/tabs.
func (s *Server) handleClaim(ctx rweb.Context) error {
	w, err := s.hub.window(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req claimReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	saved, err := s.store.Tabs()
	if err != nil {
		return fail(ctx, err)
	}
	keys := req.Keys
	if keys == nil {
		for _, t := range saved {
			keys = append(keys, t.ID)
		}
	}
	var got, held []string
	if req.Boot {
		if got, held, err = s.hub.claimBoot(w, keys); err != nil {
			return fail(ctx, err)
		}
	} else {
		got, held = s.hub.claim(w, keys)
	}
	out := claimResp{Tabs: []Tab{}, Claimed: got, Held: held}
	if out.Claimed == nil {
		out.Claimed = []string{}
	}
	if out.Held == nil {
		out.Held = []string{}
	}
	for _, t := range saved {
		if slices.Contains(got, t.ID) {
			out.Tabs = append(out.Tabs, t)
		}
	}
	return ok(ctx, out)
}

// handleRelease is POST /api/v1/win/:id/release, the page's pagehide. The
// body, when there is one, is the active tab's last save, made here before
// the release: sent as a separate request it could land after the release
// and claim the tab straight back. A window the server no longer has
// answers 200 all the same — there is nothing of it to release, and the
// save is made as an unknown window's would be.
func (s *Server) handleRelease(ctx rweb.Context) error {
	id := ctx.Request().PathParam("id")
	var req struct {
		Save *Tab `json:"save"`
	}
	if err := decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	if req.Save != nil {
		if err := s.saveTab(id, *req.Save); err != nil {
			return fail(ctx, err)
		}
	}
	if w, err := s.hub.window(id); err == nil {
		s.hub.release(w)
	}
	return ok(ctx, nil)
}

// mergeTabKeys is a layout write's "tabs" or "plans" value (comma-separated
// saved-tab keys) with the keys another live window holds put back. Each
// window knows only its own tabs, so its write would otherwise drop the
// other window's from the order (they would come back last on the next
// boot) and from the plans (their reload would land on the grid). Kept:
// the old value's keys held elsewhere, in their old order, after the
// writer's. Keys held by nobody are dropped with the writer's write, as
// before claims: a closed tab's key must not linger.
func mergeTabKeys(given, old string, elsewhere map[string]bool) string {
	out := splitKeys(given)
	for _, k := range splitKeys(old) {
		if elsewhere[k] && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return strings.Join(out, ",")
}

func splitKeys(v string) []string {
	var out []string
	for k := range strings.SplitSeq(v, ",") {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}
