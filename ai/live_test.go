package ai

import (
	"os"
	"testing"
	"time"
)

// TestLiveHandshake connects to the real Copilot agent and lists its models,
// sending no prompt — so it spends no premium requests. It only runs when
// asked, because it needs the binary and a signed-in account:
//
//	DBC_AI_LIVE=1 go test -run Live -v ./ai
//
// DBC_AI_LIVE_PROMPT=1 additionally sends one short question and prints the
// streamed answer, which does use a request.
func TestLiveHandshake(t *testing.T) {
	if os.Getenv("DBC_AI_LIVE") == "" {
		t.Skip("set DBC_AI_LIVE=1 to talk to the real agent")
	}
	dir, _ := os.Getwd()
	c := Start(Agents()[0], Options{Dir: dir})
	defer c.Close()

	select {
	case e := <-c.Events():
		if e.Kind != EventReady {
			t.Fatalf("handshake: %+v", e)
		}
		t.Logf("ready: model %s of %d", e.ModelID, len(e.Models))
		for _, m := range e.Models {
			t.Logf("  %-28s %-24s %s", m.ID, m.Name, m.Usage)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("no handshake within 2 minutes")
	}

	if os.Getenv("DBC_AI_LIVE_PROMPT") == "" {
		return
	}
	p := Build("In one sentence: what does this do?", Context{
		Conn: "demo", Driver: "sqlite", Query: "SELECT breed, count(*) FROM cats GROUP BY breed",
	}, true)
	if err := c.Send(p.Text); err != nil {
		t.Fatal(err)
	}
	var answer string
	for {
		select {
		case e := <-c.Events():
			switch e.Kind {
			case EventText:
				answer += e.Text
			case EventTurnDone:
				t.Logf("answer (%s, err=%v): %s", e.StopReason, e.Err, answer)
				return
			case EventExit:
				t.Fatalf("exited: %v", e.Err)
			}
		case <-time.After(2 * time.Minute):
			t.Fatal("no answer within 2 minutes")
		}
	}
}

// TestLiveSignInStatus runs the real server in LSP mode and asks it to sign
// in. On a signed-in machine that answers "already signed in" and changes
// nothing, so it proves the LSP handshake and framing against the real thing
// for free. On a signed-out one it prints the device code, and
// DBC_AI_LIVE_SIGNIN=1 waits for it to be entered.
func TestLiveSignInStatus(t *testing.T) {
	if os.Getenv("DBC_AI_LIVE") == "" {
		t.Skip("set DBC_AI_LIVE=1 to talk to the real agent")
	}
	dir, _ := os.Getwd()
	s, err := BeginSignIn(Agents()[0], dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.UserCode == "" {
		t.Logf("already signed in: %+v", s.Status)
		return
	}
	t.Logf("enter %s at %s", s.UserCode, s.VerificationURI)
	if os.Getenv("DBC_AI_LIVE_SIGNIN") == "" {
		return
	}
	st, err := s.Wait()
	t.Logf("sign-in: %+v, %v", st, err)
}
