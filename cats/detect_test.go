package cats

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// sockPath returns a unix socket path in a deliberately SHORT temp
// directory. t.TempDir() cannot be used here: it embeds the test's name, and
// sun_path is 104 bytes on macOS — a descriptive test name plus the system
// TMPDIR overruns it, and the failure ("bind: invalid argument") reads like a
// permissions problem rather than a length one.
func sockPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// recorder collects what a fake server received. Every connection is served
// on its own goroutine while the test reads from another, so the requests
// live behind a mutex rather than in a bare slice.
type recorder struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (r *recorder) add(req map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// all returns a copy, so a caller can range over it while the server is still
// accepting.
func (r *recorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.reqs...)
}

func (r *recorder) at(t *testing.T, i int) map[string]any {
	t.Helper()
	all := r.all()
	if i >= len(all) {
		t.Fatalf("wanted request %d, only %d arrived", i, len(all))
	}
	return all[i]
}

// serveOnce answers each connection with one canned response, so a test can
// stand in for cats without a protocol implementation. A nil reply is the
// fire-and-forget hook socket: read the request, say nothing.
func serveOnce(t *testing.T, path string, reply func(req map[string]any) any) *recorder {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })

	got := &recorder{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req map[string]any
				dec := json.NewDecoder(conn)
				// UseNumber because the hook seq is seeded from UnixNano
				// (~1.8e18), which is past float64's 53-bit mantissa: decoded
				// as a float, two consecutive seq values compare EQUAL. The
				// real server parses it as uint64, so a test that lost the
				// precision would be measuring its own decoder.
				dec.UseNumber()
				if err := dec.Decode(&req); err != nil {
					return
				}
				got.add(req)
				if reply == nil {
					return
				}
				line, _ := json.Marshal(reply(req))
				_, _ = conn.Write(append(line, '\n'))
			}()
		}
	}()
	return got
}

// okReply builds the control socket's success envelope around a payload.
func okReply(data any) map[string]any {
	raw, _ := json.Marshal(data)
	return map[string]any{"ok": true, "data": json.RawMessage(raw)}
}

// inPane sets the four environment variables cats gives a pane.
func inPane(t *testing.T, handle, control, hook string) {
	t.Helper()
	t.Setenv(EnvMarker, "1")
	t.Setenv(EnvPaneID, handle)
	t.Setenv(EnvControlSocket, control)
	t.Setenv(EnvHookSocket, hook)
}

func TestDetectEnvOutsideCatsIsTier0(t *testing.T) {
	t.Setenv(EnvMarker, "")
	t.Setenv(EnvPaneID, "")
	t.Setenv(EnvControlSocket, "")
	t.Setenv(EnvHookSocket, "")

	c := DetectEnv()
	if c.InCats || c.Tier1() {
		t.Fatalf("expected Tier 0 outside cats, got %+v", c)
	}
	if c.Reason == "" {
		t.Error("a Tier-0 verdict should carry a reason")
	}
}

// The marker alone is not enough: a process that outlived its pane can
// inherit it, and without a handle the hook API has no address to report to.
func TestDetectEnvNeedsBothMarkerAndHandle(t *testing.T) {
	t.Setenv(EnvMarker, "1")
	t.Setenv(EnvPaneID, "")
	t.Setenv(EnvControlSocket, "")
	t.Setenv(EnvHookSocket, "")
	if DetectEnv().InCats {
		t.Error("marker without a pane handle should not read as in-cats")
	}

	t.Setenv(EnvMarker, "")
	t.Setenv(EnvPaneID, "w1:p3")
	if DetectEnv().InCats {
		t.Error("pane handle without the marker should not read as in-cats")
	}
}

func TestDetectEnvInPane(t *testing.T) {
	hook := sockPath(t, "h.sock")
	serveOnce(t, hook, nil) // exists, so Hooks should be true

	inPane(t, "w1:p3", "/nonexistent/control.sock", hook)
	c := DetectEnv()

	if !c.InCats {
		t.Fatal("expected InCats")
	}
	if !c.Hooks {
		t.Error("an existing hook socket should set Hooks")
	}
	if c.Control {
		t.Error("DetectEnv must not decide Control — only Probe can")
	}
	if c.PaneIDKnown {
		t.Error(`the public "w1:p3" form carries no internal id`)
	}
}

// A socket FILE proves nothing: a crashed server leaves one behind. Only a
// ping reply may set Tier 1.
func TestProbeNeedsAnAnswerNotJustASocket(t *testing.T) {
	ProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = 500 * time.Millisecond })

	dead := sockPath(t, "d.sock")
	ln, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // the file survives; nothing is listening

	if _, err := os.Stat(dead); err != nil {
		t.Skip("this platform removes the socket file on close")
	}

	inPane(t, "p_7", dead, "")
	c := DetectEnv().Probe()
	if c.Tier1() {
		t.Error("a dead socket must not reach Tier 1")
	}
	if c.Reason == "" {
		t.Error("a failed probe should say why")
	}
}

func TestProbeReachesTier1(t *testing.T) {
	ctl := sockPath(t, "c.sock")
	serveOnce(t, ctl, func(map[string]any) any {
		return okReply(Pong{Protocol: 1, Service: "catway"})
	})

	inPane(t, "p_26", ctl, "")
	c := DetectEnv().Probe()

	if !c.Tier1() {
		t.Fatalf("expected Tier 1, got %+v", c)
	}
	if c.Service != "catway" || c.Protocol != 1 {
		t.Errorf("ping self-identification lost: %q/%d", c.Service, c.Protocol)
	}
	if c.Reason != "" {
		t.Errorf("Tier 1 should carry no reason, got %q", c.Reason)
	}
	if !c.PaneIDKnown || c.PaneID != 26 {
		t.Errorf(`"p_26" should decode locally, got %d/%v`, c.PaneID, c.PaneIDKnown)
	}
}

func TestParsePaneHandle(t *testing.T) {
	cases := []struct {
		handle string
		want   uint32
		ok     bool
	}{
		{"p_0", 0, true},
		{"p_289", 289, true},
		{"w1:p3", 0, false}, // public form: encodes workspace, not the internal id
		{"p_", 0, false},    // no number
		{"p_-1", 0, false},  // not unsigned
		{"", 0, false},      // absent
		{"p_99x", 0, false}, // trailing junk
	}
	for _, c := range cases {
		got, ok := ParsePaneHandle(c.handle)
		if got != c.want || ok != c.ok {
			t.Errorf("ParsePaneHandle(%q) = %d,%v; want %d,%v", c.handle, got, ok, c.want, c.ok)
		}
	}
}

// Every socket read in this package must be bounded, or a wedged server turns
// a background goroutine into a permanent leak.
func TestDialAlwaysHasADeadline(t *testing.T) {
	path := sockPath(t, "s.sock")
	serveOnce(t, path, nil) // accepts, never answers

	conn, err := dial(path, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 1)
	start := time.Now()
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the read to time out")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("read was not bounded by the dial timeout: waited %s", elapsed)
	}
}
