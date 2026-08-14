package cats

import (
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// eventServer is a fake cats event stream: it acks a subscription and then
// writes whatever frames the test hands it.
type eventServer struct {
	path string
	ln   net.Listener

	mu    sync.Mutex
	conns []net.Conn
	subs  int // how many subscriptions have been accepted
}

func newEventServer(t *testing.T) *eventServer {
	t.Helper()
	path := sockPath(t, "e.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &eventServer{path: path, ln: ln}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Read the subscribe request, ack it, and keep the connection
			// open for frames.
			var req map[string]any
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			if _, err := conn.Write([]byte(`{"ok":true}` + "\n")); err != nil {
				conn.Close()
				continue
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.subs++
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *eventServer) subCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs
}

// emit writes one event frame to every live connection.
func (s *eventServer) emit(name string, data any) {
	raw, _ := json.Marshal(data)
	line, _ := json.Marshal(Event{Name: name, Data: raw})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_, _ = c.Write(append(line, '\n'))
	}
}

// hangUp closes the live connections, as a restarting cats would.
func (s *eventServer) hangUp() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

// collector gathers events off the reader goroutine.
type collector struct {
	mu   sync.Mutex
	evs  []Event
	ups  int
	down int
}

func (c *collector) on(ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evs = append(c.evs, ev)
}

func (c *collector) onState(up bool, _ error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if up {
		c.ups++
	} else {
		c.down++
	}
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.evs)
}

func (c *collector) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.evs))
	for i, e := range c.evs {
		out[i] = e.Name
	}
	return out
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStreamDeliversEvents(t *testing.T) {
	srv := newEventServer(t)
	c := &collector{}
	s := Subscribe(srv.path, SubscribeFilter{}, c.on, c.onState)
	defer s.Close()

	waitUntil(t, "the subscription", func() bool { return srv.subCount() == 1 })
	srv.emit(EventThemeChanged, ConfigTheme{Name: "cats-green"})
	waitUntil(t, "the theme frame", func() bool { return c.count() == 1 })

	if got := c.names()[0]; got != EventThemeChanged {
		t.Errorf("event name = %q", got)
	}
	var theme ThemeChangedEvent
	if err := json.Unmarshal(c.evs[0].Data, &theme); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if theme.Name != "cats-green" {
		t.Errorf("payload = %+v", theme)
	}
}

// json.Decoder reads ahead into its own buffer. A second decoder for the pump
// would leave an event that arrived alongside the ack sitting in the
// handshake decoder, unread forever.
func TestStreamSeesAnEventBundledWithTheAck(t *testing.T) {
	path := sockPath(t, "e.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		var req map[string]any
		_ = json.NewDecoder(conn).Decode(&req)
		// One write: the ack and the first event in the same packet.
		_, _ = conn.Write([]byte(`{"ok":true}` + "\n" +
			`{"event":"theme_changed","data":{"name":"x"}}` + "\n"))
	}()

	c := &collector{}
	s := Subscribe(path, SubscribeFilter{}, c.on, c.onState)
	defer s.Close()

	waitUntil(t, "the bundled event", func() bool { return c.count() == 1 })
}

// A subscription that dies silently is a feature that stops working forever
// and never says so, so the loop reconnects for as long as it is open.
func TestStreamReconnectsAfterTheServerHangsUp(t *testing.T) {
	reconnectMin, reconnectMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reconnectMin, reconnectMax = 500*time.Millisecond, 30*time.Second })

	srv := newEventServer(t)
	c := &collector{}
	s := Subscribe(srv.path, SubscribeFilter{}, c.on, c.onState)
	defer s.Close()

	waitUntil(t, "the first subscription", func() bool { return srv.subCount() == 1 })
	srv.hangUp()
	waitUntil(t, "the resubscription", func() bool { return srv.subCount() >= 2 })

	srv.emit(EventPaneAgent, PaneAgentEvent{Pane: 3, Agent: "claude", State: "blocked"})
	waitUntil(t, "an event on the new connection", func() bool { return c.count() >= 1 })
}

// A socket that is not there yet must be retried, not given up on — cats may
// start after dbc does.
func TestStreamRetriesARefusedDial(t *testing.T) {
	reconnectMin, reconnectMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { reconnectMin, reconnectMax = 500*time.Millisecond, 30*time.Second })

	c := &collector{}
	s := Subscribe(sockPath(t, "later.sock"), SubscribeFilter{}, c.on, c.onState)
	defer s.Close()

	waitUntil(t, "repeated dial attempts", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.down >= 2
	})
}

// Close has to interrupt a blocked read, and must not return before the
// reader is gone — otherwise a callback could fire into a stopped app.
func TestCloseInterruptsAndWaits(t *testing.T) {
	srv := newEventServer(t)
	c := &collector{}
	s := Subscribe(srv.path, SubscribeFilter{}, c.on, c.onState)

	waitUntil(t, "the subscription", func() bool { return srv.subCount() == 1 })

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on a read it should have interrupted")
	}

	// Nothing may be delivered after Close returns.
	before := c.count()
	srv.emit(EventThemeChanged, ConfigTheme{Name: "late"})
	time.Sleep(50 * time.Millisecond)
	if after := c.count(); after != before {
		t.Errorf("an event was delivered after Close: %d then %d", before, after)
	}
}

func TestCloseIsSafeTwiceAndOnNil(t *testing.T) {
	var s *Stream
	s.Close() // must not panic

	srv := newEventServer(t)
	live := Subscribe(srv.path, SubscribeFilter{}, func(Event) {}, nil)
	live.Close()
	live.Close()
}

// A frame with no event name is something newer than this build, not a
// desync: skip it and keep reading.
func TestStreamIgnoresUnnamedFrames(t *testing.T) {
	path := sockPath(t, "e.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		var req map[string]any
		_ = json.NewDecoder(conn).Decode(&req)
		_, _ = conn.Write([]byte(`{"ok":true}` + "\n" +
			`{"something":"new"}` + "\n" +
			`{"event":"focus_changed","data":{"pane":4}}` + "\n"))
	}()

	c := &collector{}
	s := Subscribe(path, SubscribeFilter{}, c.on, c.onState)
	defer s.Close()

	waitUntil(t, "the event after the unknown frame", func() bool { return c.count() == 1 })
	if got := c.names()[0]; got != EventFocusChanged {
		t.Errorf("the unnamed frame was not skipped cleanly: got %q", got)
	}
}
