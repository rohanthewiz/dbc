package workspace

import (
	"context"
	"errors"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// catalogTimeout bounds the catalog query a connect runs for the sidebar: a
// huge catalog must not hold the connect open.
const catalogTimeout = 10 * time.Second

// Switch is picking a connection: a connect with a line for the log, and
// nothing at all when name is already active and its catalog loaded.
func (w *Workspace) Switch(name string) Start {
	w.mu.Lock()
	same := name == w.active && w.catalog != nil
	w.mu.Unlock()
	if same {
		return Start{}
	}
	st := w.Connect(name)
	if st.Job != nil {
		st.Notes = append([]Note{notef(Info, "connecting to %s…", name)}, st.Notes...)
	}
	return st
}

// Connect opens the connection and fetches its catalog for a sidebar. The
// catalog goes through the pool, not the pinned session: it is the app's
// query, and must not land inside a transaction the user has open.
//
// The connect runs under its own context, so Cancel can abandon it —
// without that, only connect_timeout bounds it, and with connect_timeout =
// "0" an unreachable host would hold "connecting…" until the OS gave up on
// the TCP connect, over a minute later. Starting a connect cancels any still
// in flight: the user has changed their mind, and the older one must not
// land after the newer one and switch the connection back.
//
//	pick A ──► connGen=1, dial A ───────────── (canceled) ──► lands Stale
//	pick B ──► connGen=2, cancel A, dial B ─► lands, active = B
//
// The Job's event is a *Connected.
func (w *Workspace) Connect(name string) Start {
	if name == "" {
		return Start{}
	}
	driver := ""
	if cc, ok := w.cfg.ConnByName(name); ok {
		driver = cc.Driver
	}
	ctx, cancel := context.WithCancel(context.Background())

	w.mu.Lock()
	if w.connCancel != nil {
		w.connCancel()
	}
	w.connGen++
	gen := w.connGen
	w.connCancel, w.connName = cancel, name
	w.mu.Unlock()

	mgr := w.mgr
	job := func() Event {
		defer cancel() // releases the context; the landing may already have
		ev := &Connected{Name: name}
		if _, err := mgr.DBContext(ctx, name); err != nil {
			ev.Err = err
		} else if q, err := db.TablesQuery(driver); err == nil {
			tctx, tcancel := context.WithTimeout(ctx, catalogTimeout)
			ev.Catalog, _ = mgr.RunContext(tctx, name, q)
			tcancel()
		}
		w.landConnect(ev, gen)
		return ev
	}
	return Start{Job: job}
}

// landConnect installs a connect's outcome, unless a newer connect has
// superseded it.
func (w *Workspace) landConnect(ev *Connected, gen int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.connGen {
		ev.Stale = true // superseded by a later connect, which canceled this one
		return
	}
	w.connCancel = nil
	if errors.Is(ev.Err, db.ErrCanceled) {
		ev.Notes = append(ev.Notes, notef(Warn, "connect to %s canceled", ev.Name))
		ev.Status = "connect canceled"
		return
	}
	if ev.Err != nil {
		ev.Notes = append(ev.Notes, notef(Err, "connect failed: %s", serr.StringFromErr(ev.Err)))
		return
	}
	ev.Changed = ev.Name != w.active
	w.active = ev.Name
	w.setCatalogLocked(ev.Catalog)
	if ev.Changed {
		ev.Notes = append(ev.Notes, notef(Ok, "connected to %s", ev.Name))
		ev.Status = "connected"
		// issued only after active has moved to Name, so a run started in
		// the meantime is already on Name and its session is left alone
		ev.Release = w.releaseJob(ev.Name)
	}
}

// setCatalogLocked installs a connection's catalog and its index.
func (w *Workspace) setCatalogLocked(tables *model.Result) {
	w.catalog, w.tableIdx = tables, nil
	if tables != nil {
		w.tableIdx = db.NewTableIndex(db.TableRefs(tables.Rows))
	}
}

// CancelConnect abandons the connect in flight. It reports false when there
// is none. The connect still lands, as canceled.
func (w *Workspace) CancelConnect() (Note, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelConnectLocked()
}

func (w *Workspace) cancelConnectLocked() (Note, bool) {
	if w.connCancel == nil {
		return Note{}, false
	}
	w.connCancel()
	return notef(Warn, "canceling connect to %s…", w.connName), true
}

// releaseJob closes the session pinned to any connection other than keep, so
// switching connections ends the old session — rolling back what it left
// open — then and there, rather than holding its transaction and locks open
// until the next run happens to replace it.
//
// It is a Job, off the UI goroutine, because it takes sessMu, which a run in
// flight holds for as long as its statement takes.
func (w *Workspace) releaseJob(keep string) Job {
	return func() Event {
		w.sessMu.Lock()
		defer w.sessMu.Unlock()
		if w.sess == nil || w.sessFor == keep {
			return nil
		}
		ev := &SessionReleased{Conn: w.sessFor, Stateful: w.sess.Stateful()}
		// A session that only ever ran queries goes quietly: nothing was lost.
		if ev.Stateful {
			ev.Notes = []Note{notef(Warn,
				"left %s: its session was closed — any open transaction was rolled back, SET values and temp tables are gone",
				ev.Conn)}
		}
		w.dropSessionLocked()
		return ev
	}
}
