package tui

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
)

// The jobs engine in the TUI. The Model owns one engine (package jobs) for
// the session, as dbc web's Server owns its own: the pipelines and jobs run
// from the Ctrl+J browser (pipes.go) and a script's s.RunJob run on it,
// beside the query tabs' runs and never in a tab's run slot — a load that
// takes an hour does not hold a tab, and a tab's Stop does not reach it.
// The TUI does not schedule: dbc web is the process that fires cron lines.
//
//	Ctrl+J ─Enter─► startPipeline / startJob ─► Engine ─go─► the run's goroutines
//	                                                │ Sink(ev): never blocks
//	                                                ▼
//	                                       jobPump: a FIFO, one goroutine
//	                                                │ m.send(jobMsg)
//	                                                ▼
//	Update ─► jobEvent ─┬─ Logged ──► the log, "[nightly › copy] …"
//	                    ├─ Preview ─► the grid of the tab it was started from
//	                    ├─ RunStarted / State / RunDone ─► liveRuns: the
//	                    │                 status bar's "● nightly 3m12s"
//	                    └─ every event ─► the run monitor and the Runs list
//	                                      (runs.go), when open, redrawn
//
// WHY A PUMP. Bubble Tea's Program.Send blocks until the event loop takes
// the message, and the engine sends some events from inside the very call
// that started the run — RunStarted, from StartPipeline — which Update
// made. A sink calling Send directly would wait on the loop it runs in,
// forever. The engine also sends while holding a run's event lock, so a
// Send that waited on a busy UI would stall the run itself. The pump takes
// each event into an unbounded queue at once, and hands the queue to Send
// in order from a goroutine of its own. (The engine coalesces progress
// counters to one event per 250ms per run, so the queue stays short.)
//
// WHAT IS DRAWN FROM WHERE. Draw paths never call the engine: the status
// bar reads liveRuns, which the events keep; the monitor and the Runs list
// hold a copy of the records they show, re-read when an event for them
// lands (and every 2s for a run another process is running, which sends
// this engine no events).

// jobMsg is one engine event, delivered to Update through the pump.
type jobMsg struct{ ev jobs.Event }

// jobTickMsg is the jobs' 1s refresh: elapsed times in the status bar, the
// monitor and the Runs list, and (every other tick) a re-read of runs only
// the disk knows about. seq numbers the chain, so a stale one dies.
type jobTickMsg struct{ seq int }

// jobTickEvery is the refresh period: elapsed times are shown to the
// second. A var for tests, whose harness runs a tick's command — a sleep
// that long — in line.
var jobTickEvery = time.Second

// jobPump carries engine events to the program in order without ever
// blocking the engine. See WHY A PUMP above.
type jobPump struct {
	mu     sync.Mutex
	q      []jobs.Event
	wake   chan struct{} // capacity 1: "the queue has something"
	stop   chan struct{}
	closed bool
}

// newJobPump starts the pump's goroutine, which hands each event to send.
func newJobPump(send func(tea.Msg)) *jobPump {
	p := &jobPump{wake: make(chan struct{}, 1), stop: make(chan struct{})}
	go p.loop(send)
	return p
}

// push queues ev and returns at once: the engine's Sink.
func (p *jobPump) push(ev jobs.Event) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.q = append(p.q, ev)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default: // already woken; the loop takes the whole queue
	}
}

// loop sends what is queued, oldest first, until the pump closes.
func (p *jobPump) loop(send func(tea.Msg)) {
	for {
		select {
		case <-p.stop:
			return
		case <-p.wake:
		}
		p.mu.Lock()
		batch := p.q
		p.q = nil
		p.mu.Unlock()
		for _, ev := range batch {
			send(jobMsg{ev: ev})
		}
	}
}

// close stops the pump; events pushed after it are dropped (the program
// has gone, and nothing would draw them).
func (p *jobPump) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.stop)
	}
}

// initJobs builds the session's engine. Records go to runs_dir, as dbc
// web's and `dbc job run`'s do, so the three list each other's runs —
// unless persistence is off (tests), when they stay in memory. A record a
// crashed process left "running" is settled first, as dbc web does at
// start, and the count said in the log.
func (m *Model) initJobs(noPersist bool) {
	runsDir := m.cfg.RunsDir
	if noPersist {
		runsDir = ""
	}
	m.runsDir = runsDir
	m.runFrom, m.runNames = map[string]int{}, map[string]string{}
	// the sink reads m.send when an event comes, not now: Run installs
	// Program.Send after New returns
	m.jobPump = newJobPump(func(msg tea.Msg) { m.send(msg) })
	m.jobs = jobs.New(m.cfg, m.mgr, jobs.Options{Sink: m.jobPump.push, RunsDir: runsDir, RunsKeep: m.cfg.RunsKeep})
	if runsDir != "" {
		if n, err := m.jobs.Recover(); err != nil {
			m.jobsNote = "could not settle old run records: " + serr.StringFromErr(err)
		} else if n > 0 {
			m.jobsNote = fmt.Sprintf("%s left running by a process that is gone marked interrupted (⌥J lists the runs)",
				plural(n, "run record"))
		}
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// jobEvent draws one engine event (see the diagram at the top).
func (m *Model) jobEvent(ev jobs.Event) tea.Cmd {
	switch e := ev.(type) {
	case *jobs.RunStarted:
		m.trackRun(e.Run)
		m.runNames[e.Run.ID] = e.Run.Name
		verb := "started"
		if e.Run.Status == pipeline.Queued {
			verb = "queued behind the run before it (overlap: queue)"
		}
		m.logf(logMuted, "%s %s %s — run %s · ⌥J lists the runs", e.Run.Kind, e.Run.Name, verb, e.Run.ID)
	case *jobs.State:
		if e.Pipeline == "" { // a queued run starting
			m.setLiveStatus(e.Run, e.Status)
		}
	case *jobs.Logged:
		m.log(levelKind(e.Line.Level), m.runPrefix(e.Run, e.Pipeline)+e.Line.Text)
	case *jobs.Preview:
		m.landPreview(e)
	case *jobs.Notice:
		m.log(levelKind(e.Level), e.Text)
	case *jobs.RunDone:
		m.untrackRun(e.Run.ID)
		delete(m.runFrom, e.Run.ID)
		// the run's own log has said how it went, with its numbers (the
		// runner's summary, a job's step summary); the status bar says it
		// once more where the eye is — unless a query is running there
		if !m.ws.Busy() {
			m.setStatus(fmt.Sprintf("%s %s %s %s", runGlyph(e.Run.Status), e.Run.Kind, e.Run.Name, e.Run.Status))
		}
	}
	m.runsChanged(ev)
	return m.ensureJobTick()
}

// levelKind is a run log level as a log line's color.
func levelKind(level string) logKind {
	switch level {
	case "err":
		return logErr
	case "warn":
		return logWarn
	case "ok":
		return logOk
	}
	return logInfo
}

// runPrefix is a run's lines' tag in the log: the job and its step, or the
// pipeline — "[nightly › copy] ", "[nightly] ", "[copy-cats] ". The log
// is shared by everything the connection on screen does, so a run's lines
// say whose they are.
func (m *Model) runPrefix(run, pipe string) string {
	name := m.runNames[run]
	switch {
	case name == "":
		return ""
	case pipe == "" || pipe == name:
		return "[" + name + "] "
	}
	return "[" + name + " › " + pipe + "] "
}

// landPreview puts a preview sink's rows in the grid of the query tab the
// run was started from — the tab on screen when its run started from a
// script (s.RunJob) or the tab is gone — as dbc web lands them in the
// asking tab. The tab's workspace keeps them as a script's s.Show results
// are kept (Workspace.ShowResult), so a tab in the background shows them
// when it comes back, marked • meanwhile.
func (m *Model) landPreview(e *jobs.Preview) {
	r := e.Result
	if r == nil {
		return
	}
	// a preview sink names its result "preview <what>" (the pipeline's
	// previewSink): the title says the connection once, as the web's does
	what := strings.TrimPrefix(r.Query, "preview ")
	if !strings.HasPrefix(what, r.Conn+"/") {
		what = r.Conn + "/" + what
	}
	title := "preview " + what
	t := m.active()
	if key, ok := m.runFrom[e.Run]; ok {
		if from := m.tabByKey(key); from != nil {
			t = from
		}
	}
	ws := t.ws
	if t == m.active() {
		ws = m.ws
	}
	if err := ws.ShowResult(e.Run, title, r); err != nil {
		m.refused(err)
		return
	}
	if t == m.active() {
		m.showResult(r)
		return
	}
	t.done = true
	m.logf(logInfo, "%s: %s in %s (⌥%d goes there)", title, plural(r.RowCount(), "row"), t.title, slices.Index(m.tabs, t)+1)
}

// ---------------------------------------------------------------------------
// The live runs, for the status bar
// ---------------------------------------------------------------------------

// trackRun adds a run that started to liveRuns (newest first).
func (m *Model) trackRun(r jobs.Run) {
	r.Log = nil
	m.liveRuns = slices.Insert(slices.DeleteFunc(m.liveRuns, func(x jobs.Run) bool { return x.ID == r.ID }), 0, r)
}

// untrackRun drops a run that ended.
func (m *Model) untrackRun(id string) {
	m.liveRuns = slices.DeleteFunc(m.liveRuns, func(x jobs.Run) bool { return x.ID == id })
}

// setLiveStatus notes a live run's own state (a queued run starting).
func (m *Model) setLiveStatus(id string, s pipeline.Status) {
	for i := range m.liveRuns {
		if m.liveRuns[i].ID == id {
			m.liveRuns[i].Status = s
			if s == pipeline.Running {
				m.liveRuns[i].Started = time.Now()
			}
		}
	}
}

// jobIndicator is the status bar's word on the runs going in this
// process: "● nightly 3m12s", "+2" after it when there are more, "" when
// none. The newest leads.
func (m *Model) jobIndicator() string {
	if len(m.liveRuns) == 0 {
		return ""
	}
	r := m.liveRuns[0]
	s := runGlyph(r.Status) + " " + r.Name + " "
	if r.Status == pipeline.Queued {
		s += "queued"
	} else {
		s += shortDur(time.Since(r.Started))
	}
	if n := len(m.liveRuns) - 1; n > 0 {
		s += fmt.Sprintf(" +%d", n)
	}
	return s
}

// jobIndicatorClick opens what the status bar's indicator names: the
// run's monitor, or the Runs list when several are going.
func (m *Model) jobIndicatorClick() tea.Cmd {
	switch len(m.liveRuns) {
	case 0:
		return nil
	case 1:
		return m.openMonitor(m.liveRuns[0].ID, nil)
	}
	return m.openRuns("", "")
}

// shortDur is a running time to the second: 9s, 3m12s, 1h04m.
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// runGlyph is a state's mark, as the run tree (`dbc run show`) and dbc
// web draw it: ○ queued, ● running, ✓ ✗ ■ done, ↷ skipped, ⚠ interrupted.
func runGlyph(s pipeline.Status) string {
	switch s {
	case pipeline.Succeeded:
		return "✓"
	case pipeline.Failed:
		return "✗"
	case pipeline.Canceled:
		return "■"
	case pipeline.Skipped:
		return "↷"
	case pipeline.Running:
		return "●"
	case pipeline.Interrupted:
		return "⚠"
	}
	return "○"
}

// runStyle is a state's color.
func (m *Model) runStyle(s pipeline.Status) Style {
	switch s {
	case pipeline.Succeeded:
		return m.st.ok
	case pipeline.Failed, pipeline.Interrupted:
		return m.st.err
	case pipeline.Running:
		return m.st.accent
	case pipeline.Canceled:
		return m.st.warn
	}
	return m.st.muted
}

// ---------------------------------------------------------------------------
// The tick
// ---------------------------------------------------------------------------

// jobTickNeeded reports whether anything on screen moves with time: a run
// going here, or a view of runs open (a run another process runs moves
// too, seen only by re-reading).
func (m *Model) jobTickNeeded() bool {
	if len(m.liveRuns) > 0 {
		return true
	}
	switch m.modal.(type) {
	case *runsModal, *runMonitor:
		return true
	}
	return false
}

// ensureJobTick arms the 1s refresh when something needs it and it is not
// armed. One chain at a time: jobTickSeq names the live one.
func (m *Model) ensureJobTick() tea.Cmd {
	if m.jobTicking || !m.jobTickNeeded() {
		return nil
	}
	m.jobTicking = true
	m.jobTickSeq++
	return m.jobTickCmd()
}

func (m *Model) jobTickCmd() tea.Cmd {
	seq := m.jobTickSeq
	return tea.Tick(jobTickEvery, func(time.Time) tea.Msg { return jobTickMsg{seq: seq} })
}

// jobTick redraws what moves with time (the frame after any Update does),
// re-reads every 2s the runs only the disk can tell about, and re-arms
// itself while it is needed.
func (m *Model) jobTick(msg jobTickMsg) tea.Cmd {
	if msg.seq != m.jobTickSeq {
		return nil
	}
	if !m.jobTickNeeded() {
		m.jobTicking = false
		return nil
	}
	m.jobTicks++
	disk := m.jobTicks%2 == 0
	switch md := m.modal.(type) {
	case *runsModal:
		md.tick(m, disk)
	case *runMonitor:
		if disk {
			md.refreshElsewhere(m)
		}
	case *pipesModal:
		md.build(m) // a running spec's "● 3m12s"
	}
	return m.jobTickCmd()
}

// ---------------------------------------------------------------------------
// Starting runs
// ---------------------------------------------------------------------------

// startPipeline runs a pipeline of the browser's: text is its spec, file
// its name ("orders.json"), params the values asked for. Started, the
// monitor opens on it; refused (a check error, the pipeline already
// running), the log says why and the browser stays.
func (m *Model) startPipeline(file, text string, params map[string]string) tea.Cmd {
	spec, err := pipeline.Parse(text)
	if err != nil {
		m.logf(logErr, "%s does not parse: %s — e edits it", file, strings.TrimPrefix(err.Error(), "json: "))
		return nil
	}
	head, err := m.jobs.StartPipeline(jobs.Request{Spec: spec, Params: params, Trigger: jobs.TriggerManual,
		By: "terminal", Source: file})
	return m.started(head, err)
}

// startJob runs a job of the browser's, as startPipeline does a pipeline.
func (m *Model) startJob(file, text string, params map[string]string) tea.Cmd {
	spec, err := jobs.ParseJob(text)
	if err != nil {
		m.logf(logErr, "%s does not parse: %s — e edits it", file, strings.TrimPrefix(err.Error(), "json: "))
		return nil
	}
	head, err := m.jobs.StartJob(jobs.JobRequest{Spec: spec, Params: params, Trigger: jobs.TriggerManual,
		By: "terminal", Source: file})
	return m.started(head, err)
}

// started lands a start's answer: the run tied to the tab on screen (its
// previews land there), and its monitor open — or the refusal said.
func (m *Model) started(head jobs.Run, err error) tea.Cmd {
	if err != nil {
		if id := jobs.BusyRun(err); id != "" {
			m.logf(logWarn, "%s — ⌥J lists the runs (run %s)", serr.UserMsgFromErr(err, err.Error()), id)
		} else {
			m.logf(logErr, "did not start: %s", serr.StringFromErr(err))
		}
		return nil
	}
	m.runFrom[head.ID] = m.active().key
	// the engine's RunStarted is on its way through the pump; tracking the
	// run now means the monitor and the status bar have it on this frame
	m.trackRun(head)
	m.runNames[head.ID] = head.Name
	return m.openMonitor(head.ID, nil)
}

// paramSpec is a parameter a run may be given: its name, its default and
// what it is for.
type paramSpec struct {
	name, def, doc string
}

// paramsOf is a spec's parameters by name.
func paramsOf(ps map[string]pipeline.Param) []paramSpec {
	var out []paramSpec
	for _, k := range slices.Sorted(maps.Keys(ps)) {
		out = append(out, paramSpec{name: k, def: ps[k].Default, doc: ps[k].Doc})
	}
	return out
}

// askParams asks for a run's parameter values, one prompt each, then calls
// run with them. all asks every one (each offered with its default); else
// only those with no default, which a run cannot do without — so a spec
// whose params all have defaults runs at once. Cancelling any prompt goes
// back to the browser, nothing run.
//
//	Enter ─► params with no default? ─ none ─► run(values)
//	                └ some ─► prompt 1 ─► prompt 2 ─► … ─► run(values)
//	                            └ Esc ─► the browser again, on the spec
func (m *Model) askParams(what string, ps []paramSpec, all bool, back func(m *Model), run func(m *Model, values map[string]string) tea.Cmd) tea.Cmd {
	var ask []paramSpec
	for _, p := range ps {
		if all || p.def == "" {
			ask = append(ask, p)
		}
	}
	values := map[string]string{}
	var next func(m *Model, i int) tea.Cmd
	next = func(m *Model, i int) tea.Cmd {
		if i == len(ask) {
			return run(m, values)
		}
		p := ask[i]
		hint := p.doc
		if p.def == "" {
			hint = strings.TrimSpace(hint + " — no default: a value is needed")
		} else if hint == "" {
			hint = "default " + p.def
		}
		title := fmt.Sprintf("Run %s · %s", what, p.name)
		if len(ask) > 1 {
			title += fmt.Sprintf(" (%d of %d)", i+1, len(ask))
		}
		pr := m.openPromptCmd(title, strings.TrimPrefix(hint, "— "), "Run", p.def, func(m *Model, typed string) (tea.Cmd, error) {
			if typed == "" && p.def == "" {
				return nil, fmt.Errorf("%s has no default — type a value", p.name)
			}
			values[p.name] = typed
			return next(m, i+1), nil
		})
		pr.cancel = back
		return nil
	}
	return next(m, 0)
}

// ---------------------------------------------------------------------------
// Quitting
// ---------------------------------------------------------------------------

// runsGoingWord is "job nightly" or "2 runs", for the quit messages.
func (m *Model) runsGoingWord() string {
	if len(m.liveRuns) == 1 {
		return m.liveRuns[0].Kind + " " + m.liveRuns[0].Name
	}
	return plural(len(m.liveRuns), "run")
}

// stopJobsForQuit stops what the engine is running as the TUI quits — the
// runs would otherwise go on loading with nobody to see them, and stop
// half-way when the process exits. Close cancels each run, so every
// fragment's sinks roll back, and waits for their records to be written
// (canceled). The log is gone by now; stderr says what was stopped.
func (m *Model) stopJobsForQuit() {
	if m.jobs == nil {
		return
	}
	going := m.jobs.Running()
	late := m.jobs.Close(30 * time.Second)
	m.jobPump.close()
	for _, r := range going {
		fmt.Fprintf(os.Stderr, "%s %s (run %s) was still running, and was stopped — it rolled back\n", r.Kind, r.Name, r.ID)
	}
	if late {
		fmt.Fprintln(os.Stderr, "a run was still rolling back after 30s; its record may read interrupted")
	}
}
