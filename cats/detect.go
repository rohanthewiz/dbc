// Package cats is dbc's client for the cats terminal multiplexer it may be
// running inside: the control socket (one JSON request per line, one response
// back), the event stream that socket can be upgraded to, and the separate
// hook socket a pane program reports its state on.
//
// It is stdlib only, and it obeys two house rules:
//
//   - NO UI STATE. Nothing in here touches the UI's model. Background
//     goroutines hand results to callbacks whose only job is to post a
//     message to the UI goroutine; the UI's Update is the sole mutator. See
//     tui/cats.go.
//   - SILENT DEGRADATION. Every failure — not inside cats, socket missing,
//     socket dead, cats restarted, protocol mismatch — ends at Tier 0, where
//     dbc behaves exactly as it does in any other terminal. No feature may
//     exist only at Tier 1.
//
// THE WIRE STRUCTS ARE HAND-COPIED MIRRORS of cats' own
// (internal/app/command_vocab.go, internal/app/events.go,
// cmd/catway/hooks.go), deliberately minimal — only the fields dbc reads or
// sends. dbc must NOT import the cats module: it has to stay buildable and
// installable on a machine that has never heard of cats, and a shared type
// package would make the database client's build depend on the multiplexer's.
//
// Detection is split in two on purpose:
//
//	DetectEnv()  reads four env vars. No IO, so startup can call it inline.
//	Caps.Probe() dials the control socket and pings. Milliseconds when the
//	             socket is live or absent, but a wedged listener could hold
//	             it for ProbeTimeout — so the app runs it on a goroutine and
//	             posts the answer back.
package cats

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// The environment cats gives every pane it spawns (cats
// internal/integration + cmd/catway's pane env). CATS_ENV is the "you are
// inside cats" marker; the other three are the addresses.
const (
	EnvMarker        = "CATS_ENV"
	EnvPaneID        = "CATS_PANE_ID"
	EnvControlSocket = "CATS_CONTROL_SOCKET"
	EnvHookSocket    = "CATS_SOCKET_PATH"
)

// ProbeTimeout bounds the ping round trip that decides Tier 1. Short on
// purpose: the socket may belong to a wedged server, and the right answer to
// that is to fall back to Tier 0 quickly, not to diagnose it. A package var
// so tests can collapse the window rather than sleep through it.
var ProbeTimeout = 500 * time.Millisecond

// Caps is what this process knows about its host. Zero value = Tier 0,
// which is also what every failure path produces, so a caller that ignores
// errors still gets correct behavior.
type Caps struct {
	// InCats reports the env marker plus a pane handle: we are running in a
	// cats pane. True even when the control socket is unusable — the hook
	// socket is a separate address and may still work.
	InCats bool

	// Control reports that the control socket answered a ping. This is the
	// Tier-1 gate: a socket FILE proves nothing (a crashed server leaves one
	// behind), and even a successful dial proves only that something is
	// listening. Only a reply identifies a live cats.
	Control bool

	// Hooks reports that a hook socket path was given and exists. Not
	// pinged: the hook API is one-shot fire-and-forget with no liveness
	// verb, and a reporter that silently no-ops is exactly the degradation
	// we want anyway.
	Hooks bool

	// PaneHandle is CATS_PANE_ID verbatim — the public "w1:p3" form, or the
	// "p_<raw>" fallback cats emits when a pane has no public id yet. It is
	// what the HOOK api addresses a pane by, so it is used as-is there.
	PaneHandle string

	// PaneID is the internal numeric id every CONTROL command addresses a
	// pane by, and PaneIDKnown reports whether we have it. Only the "p_<n>"
	// handle carries it inline; the public "w1:p3" form has to be resolved
	// against pane.list (Client.ResolvePane), which needs the socket that
	// Control is still being decided for at detection time.
	PaneID      uint32
	PaneIDKnown bool

	ControlSocket string
	HookSocket    string

	// Service and Protocol are the ping reply's self-identification, kept for
	// the log line and for future protocol gating. The service name is
	// deliberately NOT matched against a constant: catway answers "catway",
	// a sibling implementation answers its own name, and a client that
	// rejected an unfamiliar one would break on the very host it is meant to
	// work with.
	Service  string
	Protocol int

	// Reason explains a Tier-0 verdict in one phrase ("not running inside a
	// cats pane", "control socket not answering"). Empty at Tier 1. Nothing
	// branches on it — it exists so the degradation is silent but not
	// INVISIBLE when a user goes looking for why.
	Reason string
}

// Tier1 reports whether the full cats integration is available: we are in a
// pane AND the control socket answered. Everything gated on Tier 1 asks this
// question and nothing else.
func (c Caps) Tier1() bool { return c.InCats && c.Control }

// DetectEnv reads the pane environment and nothing else — no IO, so startup
// can call it inline and know immediately whether a probe is even worth
// scheduling. Control is always false here; Probe decides it.
func DetectEnv() Caps {
	c := Caps{
		PaneHandle:    strings.TrimSpace(os.Getenv(EnvPaneID)),
		ControlSocket: strings.TrimSpace(os.Getenv(EnvControlSocket)),
		HookSocket:    strings.TrimSpace(os.Getenv(EnvHookSocket)),
	}
	// Both halves are required. The marker alone could be inherited by a
	// process that outlived its pane; the handle alone would leave the hook
	// API unaddressable.
	c.InCats = strings.TrimSpace(os.Getenv(EnvMarker)) == "1" && c.PaneHandle != ""
	if !c.InCats {
		c.Reason = "not running inside a cats pane"
		return c
	}
	c.PaneID, c.PaneIDKnown = ParsePaneHandle(c.PaneHandle)
	if c.HookSocket != "" {
		if _, err := os.Stat(c.HookSocket); err == nil {
			c.Hooks = true
		}
	}
	if c.ControlSocket == "" {
		c.Reason = "no control socket in the environment"
	}
	return c
}

// Probe completes the detection: dial the control socket and ping it. The
// receiver is what DetectEnv returned; the result is the same value with
// Control (and Service/Protocol/Reason) filled in, so a caller can hand one
// value across the goroutine boundary and back.
//
// A returned Caps is always usable: every error path leaves Control false
// with a Reason, which is Tier 0.
func (c Caps) Probe() Caps {
	if !c.InCats || c.ControlSocket == "" {
		return c
	}
	pong, err := NewClient(c.ControlSocket).Ping()
	if err != nil {
		c.Reason = "control socket not answering: " + err.Error()
		return c
	}
	c.Control, c.Service, c.Protocol = true, pong.Service, pong.Protocol
	c.Reason = ""
	return c
}

// Detect is DetectEnv followed by Probe — the whole answer in one call, for
// callers that can afford the IO (tests, a future `dbc --cats-status`). The
// app deliberately does not use it; see the package comment.
func Detect() Caps { return DetectEnv().Probe() }

// ParsePaneHandle extracts the internal pane id from the "p_<n>" handle form
// cats uses when a pane has no public id assigned yet. The public "w1:p3"
// form encodes workspace and per-workspace index, not the internal id, so it
// reports false — resolving it takes a pane.list round trip.
func ParsePaneHandle(handle string) (uint32, bool) {
	raw, ok := strings.CutPrefix(handle, "p_")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// dial opens a unix connection with the given timeout applied to both the
// dial and the round trip that follows. One helper so every socket user in
// this package inherits the same bound — an unbounded read on a wedged
// server is how a background goroutine becomes a leak.
func dial(socket string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	conn, err := net.DialTimeout("unix", socket, timeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return conn, nil
}
