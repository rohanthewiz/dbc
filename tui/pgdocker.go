package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/pgdocker"
)

// "Postgres in Docker…" in the connections menu: pick a supported major
// version, and dbc starts it in a container (package pgdocker) and adds the
// connection to it — the same steps and the same connection as dbc web's.
//
//	menu row ─► tea.Cmd: Check + Statuses ─► pgDockerListMsg
//	        ─► version menu (each row: its container's state, if any)
//	pick ─► tea.Cmd: Start ── progress lines via m.send ──► the log
//	                       └► pgDockerStartedMsg ─► Register (inline, as a
//	                          form's Save is), refresh the list, connect
//
// Docker is asked about off the UI goroutine every time, even for the
// version menu: a docker CLI call is tens of milliseconds when Docker is up
// and seconds when its VM is waking, too slow to make the menu wait on.
//
// Stop is the connections menu's row on a connection it made
// (pgdocker.MajorOf): it stops that container, off the UI goroutine, and
// drops the connection's pool, whose connections the stop has cut. It is
// refused while dbc is on the connection, as Remove is — a stop would
// pull the server out from under the session and whatever transaction it
// holds — so disconnecting (x) comes first.
//
//	row ─► in use? ─ yes ─► the row says why (dim), picking it logs that
//	     └► tea.Cmd: Docker.Stop ─► pgDockerStoppedMsg ─► mgr.Drop, log
//
// One start at a time: a pull can take minutes, and two picks racing would
// only race two registrations of their results. While one is out the
// version rows stay visible and say why they cannot run.

// newPGDocker makes the Docker driver; tests swap in one over a fake CLI.
var newPGDocker = pgdocker.New

// pgDockerListMsg answers the menu row: Docker's state, for the version menu
// opened where the connections menu was.
type pgDockerListMsg struct {
	x, y int
	sts  map[string]pgdocker.Status
	err  error
}

// pgDockerProgressMsg is a slow step of a start, said as it begins.
type pgDockerProgressMsg string

// pgDockerStoppedMsg is a stop's outcome, for the connection conn.
type pgDockerStoppedMsg struct {
	conn, container string
	wasRunning      bool
	err             error
}

// pgDockerStartedMsg is a start's outcome.
type pgDockerStartedMsg struct {
	res pgdocker.Result
	err error
}

// pgDockerItem is the connections menu's row for it.
func (m *Model) pgDockerItem(x, y int) menuItem {
	return menuItem{label: "Postgres in Docker…", act: func(m *Model) tea.Cmd { return m.pgDockerList(x, y) }}
}

// pgDockerList asks Docker whether it runs and which of dbc's containers
// exist, for the version menu.
func (m *Model) pgDockerList(x, y int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		d := newPGDocker(nil)
		if err := d.Check(ctx); err != nil {
			return pgDockerListMsg{x: x, y: y, err: err}
		}
		sts, err := d.Statuses(ctx)
		return pgDockerListMsg{x: x, y: y, sts: sts, err: err}
	}
}

// pgDockerMenu is the version picker: the supported majors newest first,
// each with its container's state on the right. A version whose support
// ends within the year says so, as a reason to prefer a newer one.
func (m *Model) pgDockerMenu(msg pgDockerListMsg) tea.Cmd {
	if msg.err != nil {
		m.log(logWarn, msg.err.Error())
		return nil
	}
	busy := ""
	if m.pgDockerBusy != "" {
		busy = "PostgreSQL " + m.pgDockerBusy + " is still starting — wait for it to finish"
	}
	now := time.Now()
	items := []menuItem{heading("start postgres in docker")}
	for i, v := range pgdocker.Supported(now) {
		label := "PostgreSQL " + v.Major
		switch {
		case i == 0:
			label += " (newest)"
		case v.EOL.Before(now.AddDate(1, 0, 0)):
			label += " (support ends " + v.EOL.Format("Jan 2006") + ")"
		}
		major := v.Major
		items = append(items, menuItem{label: label, key: msg.sts[major].Text(), why: busy,
			act: func(m *Model) tea.Cmd { return m.pgDockerStart(major) }})
	}
	m.openMenu(msg.x, msg.y, items)
	return nil
}

// pgDockerStart starts major's container off the UI goroutine. Its slow
// steps (a pull, the wait for the server) reach the log as they begin, by
// m.send, the way a script's prints do.
func (m *Model) pgDockerStart(major string) tea.Cmd {
	m.pgDockerBusy = major
	m.logf(logInfo, "starting PostgreSQL %s in Docker…", major)
	send := m.send
	return func() tea.Msg {
		d := newPGDocker(func(s string) { send(pgDockerProgressMsg(s)) })
		res, err := d.Start(context.Background(), major)
		return pgDockerStartedMsg{res: res, err: err}
	}
}

// pgDockerStarted adds (or updates) the connection to a started server and
// connects to it, as a saved form does.
func (m *Model) pgDockerStarted(msg pgDockerStartedMsg) tea.Cmd {
	m.pgDockerBusy = ""
	if msg.err != nil {
		m.logf(logErr, "PostgreSQL %s did not start: %s", msg.res.Major, msg.err.Error())
		return nil
	}
	r := msg.res
	reg, err := pgdocker.Register(m.connEditor(), r, m.connInUse(true))
	if err != nil {
		m.logf(logErr, "PostgreSQL %s is running in container %s on 127.0.0.1:%d, but its connection was not saved: %s",
			r.Major, r.Container, r.Port, connErrText(err))
		return nil
	}
	m.refreshConns()
	what := "connection " + reg.Name
	switch {
	case reg.Added:
		what = "added connection " + reg.Name
	case reg.Updated:
		what = "updated connection " + reg.Name + " to the new container"
	}
	how := "running"
	if r.Created {
		how = "started"
	}
	m.logf(logOk, "PostgreSQL %s %s in container %s on 127.0.0.1:%d — %s", r.Major, how, r.Container, r.Port, what)
	for _, w := range reg.Warnings {
		m.log(logWarn, w)
	}
	if r.Created {
		m.log(logInfo, fmt.Sprintf("its data is kept in the Docker volume %s; stop it with: docker stop %s",
			pgdocker.VolumeName(r.Major), r.Container))
	}
	return m.setActive(reg.Name)
}

// pgDockerStopItem is the connections menu's Stop row for target, when
// target is a connection made for a Postgres in Docker container.
func (m *Model) pgDockerStopItem(target string) (menuItem, bool) {
	major, err := pgdocker.StopTarget(m.connEditor(), target)
	if err != nil {
		return menuItem{}, false
	}
	why := ""
	if err := m.connInUse(false)(target); err != nil {
		why = err.Error()
	}
	return menuItem{label: "■ Stop container " + pgdocker.ContainerName(major), why: why,
		act: func(m *Model) tea.Cmd { return m.pgDockerStop(target) }}, true
}

// pgDockerStop stops target's container. The in-use rule is asked again
// here, on the UI goroutine: the menu was drawn a moment ago, and a connect
// may have started since.
func (m *Model) pgDockerStop(target string) tea.Cmd {
	major, err := pgdocker.StopTarget(m.connEditor(), target)
	if err == nil {
		err = m.connInUse(false)(target)
	}
	if err != nil {
		m.log(logWarn, connErrText(err))
		return nil
	}
	name := pgdocker.ContainerName(major)
	m.logf(logInfo, "stopping container %s…", name)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		was, err := newPGDocker(nil).Stop(ctx, major)
		return pgDockerStoppedMsg{conn: target, container: name, wasRunning: was, err: err}
	}
}

// pgDockerStopped reports a stop. The pool goes either way: a stopped
// server has cut its connections, and a failed stop leaves nothing worse
// for the next connect to open afresh.
func (m *Model) pgDockerStopped(msg pgDockerStoppedMsg) tea.Cmd {
	m.mgr.Drop(msg.conn)
	switch {
	case msg.err != nil:
		m.logf(logErr, "could not stop %s: %s", msg.container, msg.err.Error())
	case !msg.wasRunning:
		m.logf(logInfo, "container %s was not running", msg.container)
	default:
		m.logf(logOk, "stopped container %s — Postgres in Docker… starts it again, its data intact", msg.container)
	}
	return nil
}
