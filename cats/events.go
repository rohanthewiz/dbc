// The event stream: the one control-socket call that does not close after a
// reply. events.subscribe gets an ack response, then Event frames on the same
// connection until somebody hangs up — so dbc learns that the host's theme
// changed, or that a sibling agent went from working to blocked, without
// polling for any of it.
//
// THE STREAM MUST SURVIVE A CATS RESTART. This is the difference between the
// stream and every other call in this package: a unary call that fails is a
// feature that didn't happen, but a subscription that fails silently is a
// feature that stops working forever and never says so. The reader loop
// therefore reconnects with capped exponential backoff, indefinitely, until
// Close — a cats that comes back up five minutes later is picked up
// automatically.
//
// Three rules the loop keeps, each one a bug it would otherwise be:
//
//  1. NO READ DEADLINE once subscribed. Events are sparse — minutes can pass
//     between them — so a deadline would turn an idle stream into a
//     reconnect loop. The handshake IS bounded; only the pump is not.
//  2. The callback runs on the READER goroutine and must not touch App
//     state. It is expected to do exactly one thing: post a closure onto the
//     UI goroutine. See ui/cats_glue.go.
//  3. Close must interrupt a blocked read, which closing the connection is
//     the only way to do. The live connection is therefore held under a
//     mutex so Close can reach it.

package cats

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"time"
)

// Event names (cats internal/app/events.go).
const (
	EventPaneExited   = "pane_exited"
	EventPaneAgent    = "pane_agent"
	EventPaneNotify   = "pane_notify"
	EventPaneAdded    = "pane_added"
	EventPaneRemoved  = "pane_removed"
	EventFocusChanged = "focus_changed"

	// EventThemeChanged is the one SESSION-scoped event: it names no pane, so
	// cats emits it with pane 0 and a pane-filtered subscription would never
	// see it. dbc subscribes by event name and never by pane, so that costs
	// nothing here — it is written down because a later narrowing of the
	// filter to "our own pane" would silently lose the theme.
	EventThemeChanged = "theme_changed"
)

// ThemeChangedEvent is EventThemeChanged's payload: the host's EFFECTIVE
// appearance after the change — resolved, with the user's per-key overrides
// already folded in, which is exactly what config.get's theme section carries.
//
// It is deliberately the same type. The event and the startup poll are two
// deliveries of one answer, so they decode into one struct and feed one
// consumer; a second shape would be a second place for the host→dbc palette
// mapping to drift.
type ThemeChangedEvent = ConfigTheme

// Backoff bounds for the reconnect loop. The first retry is quick because the
// overwhelmingly common cause is a cats that is restarting and will be back in
// a second; the cap keeps a permanently-dead socket down to two wakeups a
// minute. No jitter: there is exactly one subscriber per process, so there is
// no thundering herd to spread out, and a deterministic sequence is one a test
// can assert on.
//
// Package vars so a test can collapse the window instead of sleeping through
// it. They are not settings.
var (
	reconnectMin = 500 * time.Millisecond
	reconnectMax = 30 * time.Second
)

// Event is one frame from the stream: the event name and its raw payload.
// Decoding is the consumer's job (see the typed payloads below) because the
// glue dispatches on the name anyway.
type Event struct {
	Name string          `json:"event"`
	Data json.RawMessage `json:"data,omitempty"`
}

// PaneAgentEvent is EventPaneAgent's payload: a pane's detected agent identity
// or activity state changed. State is idle|working|blocked|unknown; Agent is
// "" for a plain shell.
type PaneAgentEvent struct {
	Pane  uint32 `json:"pane"`
	Agent string `json:"agent"`
	State string `json:"state"`
}

// PaneNotifyEvent is EventPaneNotify's payload: the moment cats itself
// considers notification-worthy — an agent hit a blocker ("attention") or a
// background run finished ("finished").
type PaneNotifyEvent struct {
	Pane    uint32 `json:"pane"`
	Agent   string `json:"agent"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// PaneRefEvent is the payload of the structural events (added, removed,
// focus_changed): they name a pane and nothing more. For focus_changed, Pane
// is the newly focused pane (0 for none).
type PaneRefEvent struct {
	Pane   uint32 `json:"pane"`
	Handle string `json:"handle,omitempty"`
}

// SubscribeFilter narrows a subscription server-side. Both fields are
// optional: no pane means every pane, no events means every name. dbc
// subscribes to the names it acts on rather than filtering client-side, so an
// idle process is not woken by traffic it would only discard.
type SubscribeFilter struct {
	Pane   *uint32  `json:"pane,omitempty"`
	Events []string `json:"events,omitempty"`
}

// Stream is a live subscription with its reader goroutine. Create one with
// Subscribe and stop it with Close; it needs no other handling — a dropped
// connection is the loop's own problem, not the caller's.
type Stream struct {
	socket  string
	filter  SubscribeFilter
	on      func(Event)
	onState func(up bool, err error)

	mu     sync.Mutex
	conn   net.Conn // the live connection, so Close can interrupt a blocked read
	closed bool

	stop      chan struct{}
	closeOnce sync.Once
	done      chan struct{} // closed when the reader goroutine returns
}

// Subscribe starts a subscription and returns immediately; the connection is
// made on the returned Stream's own goroutine, so a dead socket costs the
// caller nothing.
//
// on receives every matching event. onState (may be nil) is called when the
// connection comes up or goes down — the stream is the only thing that
// notices a cats that died AFTER startup detection said Tier 1.
//
// Both callbacks run on the reader goroutine. Neither may touch App state.
func Subscribe(socket string, filter SubscribeFilter, on func(Event), onState func(up bool, err error)) *Stream {
	s := &Stream{
		socket: socket, filter: filter, on: on, onState: onState,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go s.run()
	return s
}

// Close stops the subscription and waits for its goroutine to finish, so a
// closed Stream can never deliver another event — which is what lets the app
// tear one down during shutdown without racing a callback that posts onto an
// event loop that has stopped. Safe to call twice, and on a nil Stream (the
// Tier-0 case, where nothing was ever subscribed).
func (s *Stream) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		s.mu.Lock()
		s.closed = true
		if s.conn != nil {
			_ = s.conn.Close() // unblocks the pump's read
		}
		s.mu.Unlock()
	})
	<-s.done
}

// run is the reconnect loop: connect, pump until the connection dies, wait out
// the backoff, repeat — until Close.
func (s *Stream) run() {
	defer close(s.done)
	delay := reconnectMin
	for {
		err := s.session()
		if s.stopped() {
			return
		}
		if err == nil {
			// A clean end with no error means the server closed the stream
			// (a restart, a config reload). Treat it as the transient thing
			// it is: retry from the shortest delay.
			delay = reconnectMin
		}
		select {
		case <-s.stop:
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > reconnectMax {
			delay = reconnectMax
		}
	}
}

// session runs one connection end to end: handshake, then pump events until
// the connection fails. It returns nil when the server hung up cleanly and an
// error when the connect or handshake failed.
func (s *Stream) session() error {
	conn, err := dial(s.socket, ProbeTimeout)
	if err != nil {
		s.state(false, err)
		return err
	}
	defer conn.Close()

	// ONE decoder for the whole session, ack and events alike. A second
	// decoder for the pump would be a silent event-eater: json.Decoder reads
	// ahead into its own buffer, so a server that writes the ack and a first
	// event in quick succession leaves that event sitting in the handshake
	// decoder's buffer, where a fresh reader can never see it.
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := s.handshake(conn, dec); err != nil {
		s.state(false, err)
		return err
	}
	// Publish the connection so Close can interrupt the read below, and bail
	// out if Close already happened while we were shaking hands.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.conn = conn
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.conn = nil
		s.mu.Unlock()
	}()

	s.state(true, nil)
	// Rule 1: no deadline on the pump. dial set one for the handshake; clear
	// it now or the first quiet minute would look like a failure.
	_ = conn.SetDeadline(time.Time{})
	s.pump(dec)
	s.state(false, nil)
	return nil
}

// handshake sends the subscribe request and reads its ack, under the dial's
// deadline. A refused subscription (unknown event name, streaming disabled) is
// an error like any other — it reconnects, which is right: the server that
// refuses today may be a different build tomorrow.
func (s *Stream) handshake(conn net.Conn, dec *json.Decoder) error {
	line, err := json.Marshal(request{ID: "dbc-events", Method: MethodSubscribe, Params: s.filter})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return err
	}
	var ack response
	if err := dec.Decode(&ack); err != nil {
		return err
	}
	if !ack.OK {
		msg := ack.Error
		if msg == "" {
			msg = "subscribe refused"
		}
		return &subscribeError{msg}
	}
	return nil
}

// pump reads Event frames off the session decoder until the connection ends,
// handing each to the callback. Frames that carry no event name are skipped
// rather than treated as a desync: the two frame shapes never interleave on
// this transport, so an unnamed object is something new the server sends that
// this build predates, and ignoring it is forward compatibility.
func (s *Stream) pump(dec *json.Decoder) {
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return // connection closed, server gone, or Close() cut it
		}
		if ev.Name == "" {
			continue // not an event frame; ignore rather than resync
		}
		if s.on != nil && !s.stopped() {
			s.on(ev)
		}
	}
}

// state reports a connection transition to the caller's hook, if any.
func (s *Stream) state(up bool, err error) {
	if s.onState != nil && !s.stopped() {
		s.onState(up, err)
	}
}

// stopped reports whether Close has been called.
func (s *Stream) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// subscribeError is a server-side refusal of the subscription, distinct from a
// transport failure only in its message.
type subscribeError struct{ msg string }

func (e *subscribeError) Error() string { return "cats: events.subscribe: " + e.msg }
