package ai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeLSP is the Copilot language server's sign-in surface, scripted: it
// speaks Content-Length framing, asks the client for workspace/configuration
// in the middle of the handshake (as the real server does), and runs the
// device flow. finish decides how the flow ends; nil blocks until the
// client goes away.
type fakeLSP struct {
	signedIn bool
	finish   *AuthStatus

	calls   chan string          // every method the client sent, in order
	cfgResp chan json.RawMessage // the client's answer to our config request
}

func startFakeLSP(t *testing.T, f *fakeLSP) (*SignIn, error) {
	t.Helper()
	f.calls = make(chan string, 32)
	f.cfgResp = make(chan json.RawMessage, 1)
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	go f.serve(bufio.NewReader(c2sR), s2cW)
	return BeginSignInPipes(t.TempDir(), s2cR, c2sW)
}

func (f *fakeLSP) serve(in *bufio.Reader, out *io.PipeWriter) {
	defer out.Close()
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	cfgID := int64(900)
	for {
		m, err := readFramed(in)
		if err != nil {
			return
		}
		switch {
		case m.ID != nil && m.Method == "" && *m.ID == cfgID:
			f.cfgResp <- m.Result
			continue
		case m.Method != "":
			f.calls <- m.Method
		}
		switch m.Method {
		case "initialize":
			var p struct {
				InitializationOptions struct {
					EditorInfo struct{ Name string } `json:"editorInfo"`
				} `json:"initializationOptions"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.InitializationOptions.EditorInfo.Name == "" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"error": map[string]any{"code": -32602, "message": "editorInfo is required"}})
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "initialized":
			send(map[string]any{"jsonrpc": "2.0", "id": cfgID, "method": "workspace/configuration",
				"params": map[string]any{"items": []any{map[string]any{"section": "github"}, map[string]any{"section": "http"}}}})
		case "signIn":
			if f.signedIn {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"status": "AlreadySignedIn", "user": "octocat"}})
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"status": "PromptUserDeviceFlow", "userCode": "WXYZ-9876",
				"command": map[string]any{"command": "github.copilot.finishDeviceFlow", "arguments": []any{}},
			}})
		case "workspace/executeCommand":
			if f.finish == nil {
				continue // the user never enters the code
			}
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": f.finish})
		}
	}
}

// methods drains what the client sent so far.
func (f *fakeLSP) methods() []string {
	var out []string
	for {
		select {
		case m := <-f.calls:
			out = append(out, m)
		default:
			return out
		}
	}
}

// The whole device flow: handshake, a code to show, then the confirmation
// that the user entered it.
func TestSignInDeviceFlow(t *testing.T) {
	f := &fakeLSP{finish: &AuthStatus{Status: "OK", User: "octocat"}}
	s, err := startFakeLSP(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserCode != "WXYZ-9876" {
		t.Errorf("code = %q", s.UserCode)
	}
	if s.VerificationURI != fallbackVerifyURL {
		t.Errorf("a response without a URL should fall back to %s, got %q", fallbackVerifyURL, s.VerificationURI)
	}
	st, err := s.Wait()
	if err != nil || !st.SignedIn() || st.User != "octocat" {
		t.Fatalf("wait = %+v, %v", st, err)
	}
	got := strings.Join(f.methods(), " ")
	want := "initialize initialized workspace/didChangeConfiguration signIn workspace/executeCommand"
	if got != want {
		t.Errorf("methods = %s\nwant      %s", got, want)
	}
}

// The server's workspace/configuration gets one empty object per item it
// asked for — the server waits on that answer.
//
// The flow here is a device flow the user never finishes, so the connection
// stays open until the test closes it. An already-signed-in account would not
// do: the client answers server requests on their own goroutine, and it
// closes the connection as soon as signIn says AlreadySignedIn — which can
// land before the answer is written, dropping it. Nothing needs the answer
// then, so that is not a bug, but it made this test fail about half the time.
func TestSignInAnswersConfigurationRequests(t *testing.T) {
	f := &fakeLSP{}
	s, err := startFakeLSP(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case raw := <-f.cfgResp:
		if string(raw) != "[{},{}]" {
			t.Errorf("configuration answer = %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace/configuration was never answered")
	}
}

// An account that is already signed in comes back as such, with no code
// and no server left running.
func TestSignInAlreadySignedIn(t *testing.T) {
	s, err := startFakeLSP(t, &fakeLSP{signedIn: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.UserCode != "" || !s.Status.SignedIn() || s.Status.User != "octocat" {
		t.Fatalf("got %+v", s)
	}
	if st, err := s.Wait(); err != nil || !st.SignedIn() {
		t.Errorf("wait = %+v, %v", st, err)
	}
}

// A GitHub account without Copilot is its own ending.
func TestSignInNoSubscription(t *testing.T) {
	s, err := startFakeLSP(t, &fakeLSP{finish: &AuthStatus{Status: "NotAuthorized", User: "octocat"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Wait()
	if err != nil || st.SignedIn() || !st.NoSubscription() {
		t.Errorf("wait = %+v, %v", st, err)
	}
}

// Close while waiting on the user ends Wait at once, in plain words.
func TestSignInCloseStopsTheWait(t *testing.T) {
	s, err := startFakeLSP(t, &fakeLSP{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Wait(); done <- err }()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("wait after close = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Close")
	}
}

// Only an agent that says how can be signed in by dbc.
func TestSignInNeedsSignInArgs(t *testing.T) {
	a, _ := AgentByID("claude")
	if a.CanSignIn() {
		t.Fatal("claude should not offer dbc's sign-in")
	}
	if _, err := BeginSignIn(a, t.TempDir()); err == nil || !strings.Contains(err.Error(), "claude") {
		t.Errorf("err = %v", err)
	}
	if c, _ := AgentByID(CopilotID); !c.CanSignIn() {
		t.Error("copilot should offer dbc's sign-in")
	}
}

func TestReadFramed(t *testing.T) {
	cases := []struct {
		name, in string
		method   string
		err      string
	}{
		{"plain", "Content-Length: 17\r\n\r\n{\"method\":\"ping\"}", "ping", ""},
		{"extra header, any case", "content-length: 17\r\nContent-Type: x\r\n\r\n{\"method\":\"ping\"}", "ping", ""},
		{"stray blank line first", "\r\nContent-Length: 17\r\n\r\n{\"method\":\"ping\"}", "ping", ""},
		{"no length", "Content-Type: x\r\n\r\n{}", "", "no Content-Length"},
		{"bad length", "Content-Length: x\r\n\r\n{}", "", "bad Content-Length"},
		{"short body", "Content-Length: 40\r\n\r\n{}", "", io.ErrUnexpectedEOF.Error()},
		{"clean end", "", "", io.EOF.Error()},
		{"cut header", "Content-Len", "", io.ErrUnexpectedEOF.Error()},
	}
	for _, c := range cases {
		m, err := readFramed(bufio.NewReader(strings.NewReader(c.in)))
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case m.Method != c.method:
			t.Errorf("%s: method = %q", c.name, m.Method)
		}
	}
}

// A refusal for lack of sign-in is recognisable as such, not only readable.
func TestExplainMarksAuthErrors(t *testing.T) {
	c := &Chat{agent: Agents()[0]}
	err := c.explain(errors.New("session/new: Authentication required (code -32000)"))
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("%v should match ErrAuthRequired", err)
	}
	if errors.Is(c.explain(errors.New("boom")), ErrAuthRequired) {
		t.Error("an unrelated error matched ErrAuthRequired")
	}
}
