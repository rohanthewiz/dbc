package ui

import (
	"sync"

	"github.com/rohanthewiz/dbc/cats"
)

// The seam between the cats client (package cats — sockets, no UI) and this
// tview app.
//
// THE THREADING RULE, which every function in every cats*.go file obeys: a
// goroutine started here may touch only the values it was handed and
// a.catsPost. Everything that reads or writes an App field happens inside the
// closure catsPost hands to the UI goroutine. tview's QueueUpdateDraw is
// dbc's equivalent of a single-threaded event loop's PostEvent, and the same
// discipline applies — App fields have exactly one mutator, the UI goroutine.
//
// TIER 0 IS THE ZERO VALUE. catsState's zero value is "not in cats, nothing
// connected", which is also what every failure path produces, so there is no
// enabled flag to check and no cleanup to do when detection fails. Outside
// cats this whole file costs four getenv calls at startup.
//
// WHAT THE HOOK REPORTER IS FOR. It is the only part of the integration that
// matters when the user is not looking at the screen: cats turns a
// working→idle transition into a "finished" badge, toast, and phone push. A
// four-minute query that ends while you are in another window is the whole
// point, and it is why the reporter is armed from the environment alone —
// before, and independently of, the control-socket probe that decides Tier 1.

// catsState is everything this process knows about its cats host, plus the
// handles it talks back through. It hangs off App as one field so the
// integration adds one line to the struct rather than eight.
type catsState struct {
	caps     cats.Caps
	client   *cats.Client   // nil below Tier 1
	reporter *cats.Reporter // nil without a hook socket; independent of client
	stream   *cats.Stream   // nil below Tier 1; the event subscription

	// self is this pane's internal id, which control commands address panes
	// by. The environment only carries the public handle, so it costs a
	// pane.list to learn — done once during the startup probe. Its consumer
	// is the agent picker, which must leave dbc's own pane off the list.
	self   uint32
	selfOK bool

	// asking holds the phrase for a question dbc raised that the user did
	// NOT ask for. See catsAsking: it is deliberately empty today.
	asking string

	// The last report, so transitions are reported and steady state is not.
	lastState, lastStatus string

	// stopping is closed once the app is going away, so a late result from a
	// background call does not try to post onto an event loop that has
	// stopped reading.
	stopping chan struct{}
	stopOnce sync.Once
}

// catsInit detects the host and arms the reporter. Called from Run before the
// event loop starts.
//
// The detection is deliberately in two halves. The env sniff is free, so it
// runs inline and immediately decides whether the reporter can exist at all;
// the socket probe is IO against a server that might be wedged, so it runs on
// a goroutine and posts its answer back. The reporter does not wait for the
// probe: the hook socket is a different address, and reporting state is worth
// doing even when the control API is unreachable.
func (a *App) catsInit() {
	a.cats.stopping = make(chan struct{})
	env := cats.DetectEnv()
	a.cats.caps = env
	a.cats.reporter = cats.NewReporter(env.HookSocket, env.PaneHandle)

	// Claim the pane immediately: this first report is what puts "dbc" in
	// cats' sidebar, and it also establishes the state the first transition
	// will be measured against.
	a.catsReportNow()

	// Independent of cats: the title and cwd are for whatever terminal this
	// is, and cats is only the host that does the most with them.
	a.hostIdentInit()

	if !env.InCats || env.ControlSocket == "" {
		return
	}
	go func() {
		caps := env.Probe()
		// Resolving our own pane id rides the probe rather than costing a
		// second round trip later: the picker that needs it is opened by a
		// keystroke, which must not dial a socket.
		var self uint32
		var selfOK bool
		if caps.Tier1() {
			if id, err := cats.NewClient(caps.ControlSocket).ResolvePane(caps.PaneHandle); err == nil {
				self, selfOK = id, true
			}
		}
		a.catsPost(func() { a.catsReady(caps, self, selfOK) })
	}()
}

// catsReady installs the probe's verdict. Runs on the UI goroutine.
func (a *App) catsReady(caps cats.Caps, self uint32, selfOK bool) {
	a.cats.caps, a.cats.self, a.cats.selfOK = caps, self, selfOK
	if !caps.Tier1() {
		// Silent degradation, but not invisible: a user inside a cats pane
		// whose socket did not answer is the one person who might wonder,
		// and one log line is where they would look. Outside cats there is
		// nothing to explain, so nothing is said.
		if caps.InCats && caps.Reason != "" {
			a.logf(tagMuted+"cats: %s — running standalone", caps.Reason)
		}
		return
	}
	a.cats.client = cats.NewClient(caps.ControlSocket)
	a.logf(tagOk+"cats: connected"+tagOff+" — pane %s, host %s",
		caps.PaneHandle, caps.Service)
	a.catsSubscribe()
}

// catsPost hands a closure to the UI goroutine. It is the ONLY way a cats
// goroutine may reach App state.
//
// The stopping check keeps a late arrival from parking on tview's update
// queue after the event loop has stopped draining it. It is a best-effort
// check rather than a lock: the loser of the race is a goroutine that blocks
// while the process is already exiting, which costs nothing, whereas a lock
// held across a queue send could deadlock against the loop itself.
func (a *App) catsPost(f func()) {
	select {
	case <-a.cats.stopping:
		return
	default:
	}
	a.app.QueueUpdateDraw(f)
}

// catsTier1 is the one question every Tier-1 feature asks. A client exists
// only when the probe said so, but both halves are checked because the stream
// can report the link down later without the client being torn down.
func (a *App) catsTier1() bool {
	return a.cats.client != nil && a.cats.caps.Tier1()
}

// catsSelfState maps dbc's run state onto the hook API's vocabulary, in
// priority order.
//
// There is no honest `blocked` source today, and that is a deliberate reading
// of what the state means rather than an omission: blocked is a question dbc
// raised that the user did not ask for. Every modal dbc opens today —
// export, history, scripts — was opened by the keystroke the user just
// pressed, and paging someone about a dialog they summoned is how a
// notification channel earns being muted. A future modal that INTERRUPTS (a
// dropped connection asking whether to reconnect) is the case that should
// call catsAsking, which is why the branch exists ahead of any caller.
func (a *App) catsSelfState() (state, status string) {
	if a.cats.asking != "" {
		return cats.StateBlocked, a.cats.asking
	}
	if a.busy.Load() {
		a.runMu.Lock()
		tag := a.runTag
		a.runMu.Unlock()
		return cats.StateWorking, tag
	}
	return cats.StateIdle, ""
}

// catsReportNow publishes the current state if it differs from the last one
// published. Call from the UI goroutine.
//
// Reporting only on change is noise control, not economy: every report is a
// potential toast or phone push, and a channel that fires when nothing
// happened is one the user switches off. The cost when nothing changed is two
// string comparisons.
func (a *App) catsReportNow() {
	if a.cats.reporter == nil {
		return
	}
	state, status := a.catsSelfState()
	if state == a.cats.lastState && status == a.cats.lastStatus {
		return
	}
	a.cats.lastState, a.cats.lastStatus = state, status
	a.cats.reporter.ReportState(state, status)
}

// catsAfterTransition is the one call every run-state change makes: tell the
// host what we are doing, and put the same fact in the terminal's title.
//
// It exists as one function because both halves answer the same question from
// the same three call sites (beginRun, endRun, a completed connect), and a
// single seam is what keeps the two from drifting apart as sites are added.
func (a *App) catsAfterTransition() {
	a.catsReportNow()
	a.hostIdentSync()
}

// catsAsking marks that dbc has raised a question the user did not ask for,
// with the phrase cats should show beside the pane — which is also the phrase
// that lands on a phone, so it is written as a message ("connection lost")
// rather than as a UI fact ("modal open").
//
// It has no caller today by design; see catsSelfState for the rule that
// decides when it should get one. catsAskingDone is its other half.
func (a *App) catsAsking(reason string) {
	a.cats.asking = reason
	a.catsAfterTransition()
}

// catsAskingDone clears the mark once the question has been answered.
func (a *App) catsAskingDone() {
	if a.cats.asking == "" {
		return
	}
	a.cats.asking = ""
	a.catsAfterTransition()
}

// catsStopping closes the stopping channel exactly once. Both the quit path
// and the shutdown path reach it, and either may be first.
func (a *App) catsStopping() {
	if a.cats.stopping == nil {
		return
	}
	a.cats.stopOnce.Do(func() { close(a.cats.stopping) })
}

// catsClose hands the pane back. Called after the event loop has returned.
//
// Order is load-bearing: the stream stops FIRST, and Close waits for its
// reader to be gone, so no callback is still in flight posting onto a loop
// that has stopped. Only then is the pane released — which blocks briefly, as
// it must, because the process is about to exit and a goroutine posted here
// would be killed before it ever dialed.
func (a *App) catsClose() {
	a.catsStopping()
	a.cats.stream.Close() // nil-safe
	a.cats.stream = nil
	a.cats.reporter.Release() // nil-safe
}
