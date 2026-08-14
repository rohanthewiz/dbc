// The hook reporter: how dbc tells cats what it is doing, so cats can badge
// the pane, raise a toast, or push to a phone when a long query finishes —
// the same channel the coding-agent hooks use, and the reason dbc gets that
// whole notification story for ~150 lines rather than by growing a notifier
// of its own.
//
// This is a DIFFERENT socket from the control API (CATS_SOCKET_PATH, not
// CATS_CONTROL_SOCKET) with a different envelope, so it is available even
// when the control socket is not — a distinction worth keeping, because the
// attention story is the one that matters when you are not looking at the
// screen. It is also why the reporter is armed from the environment alone,
// before the control-socket probe has answered.
//
// Three design points:
//
//   - SOURCE IS "dbc", A CUSTOM SOURCE. The reserved "cats:<agent>" sources
//     (cats:claude, cats:codex …) have their state driven by cats' own
//     process detection, and a hook claiming one of those names gets its
//     state ignored. dbc is not one of those agents, so it takes an
//     unreserved name and receives real state authority for its pane.
//   - SEQ IS ALLOCATED AT CALL TIME, NOT AT SEND TIME, AND IS SEEDED FROM
//     THE CLOCK. Reports are sent on their own goroutines, so two of them
//     can reach the socket out of order; the server keeps a per-source
//     high-water mark and drops anything stale. Stamping the number in
//     ReportState — on the UI goroutine, in the order the transitions
//     actually happened — is what makes that mechanism protect us instead of
//     a race we lose at random. The seed matters just as much: the
//     high-water mark lives on the PANE and outlives this process, so a
//     counter starting at 1 would have every report from a re-launched dbc
//     in the same pane silently dropped as stale.
//   - SENDING IS FIRE AND FORGET. The reply is not read and failures are not
//     surfaced. A hook socket that is gone, wedged, or belongs to a cats that
//     forgot this pane must cost dbc nothing at all; there is no user-facing
//     decision that a failed status report could inform.
package cats

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Hook API methods (cats' pane.report_agent* API, cmd/catway/hooks.go).
const (
	methodReportAgent  = "pane.report_agent"
	methodReleaseAgent = "pane.release_agent"
)

// The four states the hook API accepts. Anything else is rejected by the
// server, so callers use these rather than raw strings.
const (
	StateIdle    = "idle"
	StateWorking = "working"
	StateBlocked = "blocked"
	StateUnknown = "unknown"
)

// SourceDBC is the reporter's source name and AgentDBC the label cats shows
// for the pane. Deliberately not "cats:dbc": that prefix names cats' own
// built-in agent integrations, whose state is detection-driven and whose hook
// reports are dropped.
const (
	SourceDBC = "dbc"
	AgentDBC  = "dbc"
)

// customStatusMax is where cats truncates a custom status. Trimming here too
// means what we send is what gets displayed — a status cut mid-word by the
// server would be our bug showing up in someone else's UI.
const customStatusMax = 32

// hookTimeout bounds a report's dial and write. Short: a status update that
// has not landed in a second is worthless anyway, and the goroutine holding
// it is a goroutine dbc is leaking.
const hookTimeout = time.Second

// hookRequest is the one-shot envelope: {"id","method","params"}. Note this
// is NOT the control socket's envelope — same request shape, different reply
// shape, different socket.
type hookRequest struct {
	ID     string     `json:"id"`
	Method string     `json:"method"`
	Params hookParams `json:"params"`
}

// hookParams is the superset of the methods' params. State rides
// pane.report_agent only; the rest identify who is reporting about what.
type hookParams struct {
	PaneID       string  `json:"pane_id"`
	Source       string  `json:"source"`
	Agent        string  `json:"agent"`
	State        string  `json:"state,omitempty"`
	CustomStatus string  `json:"custom_status,omitempty"`
	Seq          *uint64 `json:"seq,omitempty"`
}

// Reporter reports dbc's state to cats. A nil *Reporter is the Tier-0 form
// and every method on it is a no-op, so call sites need no availability
// check.
type Reporter struct {
	socket string
	pane   string // the PUBLIC handle from the environment; the hook API's addressing

	// seq is the per-source idempotency token, seeded from the clock and
	// incremented per report. Atomic because ReportState is called from the
	// UI goroutine while previous reports may still be in flight on their
	// own goroutines.
	seq atomic.Uint64
}

// NewReporter builds a reporter for a pane, or returns nil when either half
// of the address is missing. Callers keep the nil and go on.
//
// The sequence is seeded from the clock so this instance's numbers land above
// whatever a previous dbc in the same pane left as the server's high-water
// mark — see the file comment.
func NewReporter(socket, paneHandle string) *Reporter {
	if socket == "" || paneHandle == "" {
		return nil
	}
	r := &Reporter{socket: socket, pane: paneHandle}
	r.seq.Store(uint64(time.Now().UnixNano()))
	return r
}

// ReportState publishes a state transition. customStatus is the short phrase
// cats shows beside the pane ("query", "script report.go") — it is what turns
// a badge into a message worth acting on, so pass one whenever the state is
// anything but idle.
//
// Returns immediately: the write happens on its own goroutine.
func (r *Reporter) ReportState(state, customStatus string) {
	if r == nil {
		return
	}
	seq := r.seq.Add(1)
	go r.send(hookRequest{
		ID:     "dbc",
		Method: methodReportAgent,
		Params: hookParams{
			PaneID: r.pane, Source: SourceDBC, Agent: AgentDBC,
			State: state, CustomStatus: clampStatus(customStatus), Seq: &seq,
		},
	})
}

// Release withdraws dbc's claim on the pane, so the pane stops being labeled
// "dbc" the moment the process exits instead of wearing a stale badge until
// something else claims it.
//
// Unlike ReportState this one BLOCKS, briefly: it runs from shutdown, where a
// goroutine posted on the way out would be killed before it ever dialed.
// hookTimeout is the whole cost.
func (r *Reporter) Release() {
	if r == nil {
		return
	}
	seq := r.seq.Add(1)
	r.send(hookRequest{
		ID:     "dbc",
		Method: methodReleaseAgent,
		Params: hookParams{
			PaneID: r.pane, Source: SourceDBC, Agent: AgentDBC, Seq: &seq,
		},
	})
}

// send writes one request and does not wait for the reply. Every failure is
// swallowed — see the file comment.
func (r *Reporter) send(req hookRequest) {
	line, err := json.Marshal(req)
	if err != nil {
		return
	}
	conn, err := dial(r.socket, hookTimeout)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write(append(line, '\n'))
}

// clampStatus trims a custom status to what cats will actually display, and
// strips control bytes so a connection name or a filename in a status line
// cannot smuggle escape sequences into the host's UI — the same guard
// hostident.go puts on titles.
//
// The cap is 32 BYTES because that is the unit cats truncates in, and it
// backs off to a rune boundary because cats does not: a status cut at byte 32
// mid-character would arrive as invalid UTF-8 in someone else's UI. Doing the
// cut here, correctly, is what keeps the server's own truncation from ever
// firing.
func clampStatus(s string) string {
	s = strings.Map(func(rn rune) rune {
		if rn < 0x20 || rn == 0x7f {
			return -1
		}
		return rn
	}, strings.TrimSpace(s))
	if len(s) <= customStatusMax {
		return s
	}
	cut := customStatusMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}
