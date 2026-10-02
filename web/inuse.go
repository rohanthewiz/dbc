package web

// Which connections the OTHER query tabs are on.
//
// A query tab's sidebar marks only its own connection (the bright bar on the
// active row). Every other query tab — in this window or another — holds a
// session on its connection too, and keeps that connection's pool open past
// this tab's Disconnect (handleDisconnect counts them with tabsOn). Without
// a mark, "the row without the bar" read as "not connected" when another tab
// still was. So the hub tells every window which connection each query tab
// is on, and the page dims-bars the rows other tabs are on (app.js
// markInUse).
//
// WHY A SNAPSHOT, NOT A DELTA: a tab moves between connections from several
// goroutines (a request, a connect's Job, a tab close, the reaper), and a
// missed or reordered delta would leave a mark wrong until the next reload.
// The whole list is a handful of entries, so every change sends all of it,
// and the page just redraws from the latest.
//
// WHY THE PAGE FILTERS OUT ITS OWN TAB: which query tab is on screen is the
// page's to know — switching tabs is not a round trip — so the snapshot lists
// every tab, and the page leaves out the one it is showing.
//
//	connect / disconnect / tab closed / window forgotten
//	   └─► hub.announceInUse ─► per window: "inuse" {tabs: [{conn, ws?}, …]}
//	                                ws is set for the window's own tabs (the
//	                                page names them by title), "" for another
//	                                window's (a tab id is that window's to use)
//	page boot / stream back after a drop ─► GET /api/v1/win/:id → inUse

// inUseTab is one query tab that is on (or dialing) a connection.
type inUseTab struct {
	Conn string `json:"conn"`         // as the workspace names it: "<conn>/<db>" for another database
	WS   string `json:"ws,omitempty"` // the query tab, when it is in the receiving window
}

// inUseEvent is the "inuse" event's data: every query tab on a connection.
type inUseEvent struct {
	Tabs []inUseTab `json:"tabs"`
}

// on is the connection t holds: its active one, or — mid-dial with none yet
// — the one it is dialing, which tabsOn counts the same way, since a dial
// that lands is a pool open.
func (t *tab) on() string {
	if a := t.ws.Active(); a != "" {
		return a
	}
	if name, dialing := t.ws.Connecting(); dialing {
		return name
	}
	return ""
}

// inUseFor is the snapshot as window w receives it. The tabs are gathered
// under hub.mu and asked outside it, as tabsOn does, so hub.mu is never held
// while taking a workspace's lock.
func (h *hub) inUseFor(w *window) inUseEvent {
	h.mu.Lock()
	all := make([]*tab, 0, len(h.tabs))
	for _, t := range h.tabs {
		all = append(all, t)
	}
	h.mu.Unlock()
	return inUse(all, w)
}

// inUse builds w's view of tabs: every tab on a connection, with the ids of
// w's own. Tabs is never nil, so the page always gets a list to iterate.
func inUse(tabs []*tab, w *window) inUseEvent {
	ev := inUseEvent{Tabs: []inUseTab{}}
	for _, t := range tabs {
		conn := t.on()
		if conn == "" {
			continue
		}
		u := inUseTab{Conn: conn}
		if t.win == w {
			u.WS = t.id
		}
		ev.Tabs = append(ev.Tabs, u)
	}
	return ev
}

// announceInUse sends every window its snapshot. Call it after a tab's
// connection changes — never while holding hub.mu or a workspace's lock.
//
// inUseMu makes "read the tabs, send" one step, so two announcements racing
// (a connect landing as another tab closes) reach each stream in the order
// they read the tabs: the last snapshot a page gets is the newest, whichever
// change finished last. The sends do not block (BroadcastRaw drops for a
// stream that is full), so holding it across them is cheap.
func (h *hub) announceInUse() {
	h.inUseMu.Lock()
	defer h.inUseMu.Unlock()
	h.mu.Lock()
	all := make([]*tab, 0, len(h.tabs))
	for _, t := range h.tabs {
		all = append(all, t)
	}
	wins := make([]*window, 0, len(h.wins))
	for _, w := range h.wins {
		wins = append(wins, w)
	}
	h.mu.Unlock()
	for _, w := range wins {
		w.send("inuse", "", inUse(all, w))
	}
}
