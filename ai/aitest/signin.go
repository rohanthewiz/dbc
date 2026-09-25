package aitest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/rohanthewiz/dbc/ai"
)

// The fake's language-server side: just enough of Copilot's LSP mode for
// ai.BeginSignIn — initialize, signIn and the finishDeviceFlow command —
// over Content-Length framing, as the real server speaks it.

// FakeUserCode is the device code the fake hands out.
const FakeUserCode = "ABCD-1234"

// SignIn runs ai.BeginSignInPipes against the fake. It has ai.BeginSignIn's
// signature, so a test can stand it in for the real one.
func (f *Fake) SignIn(_ ai.Agent, dir string) (*ai.SignIn, error) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	go f.serveLSP(bufio.NewReader(c2sR), s2cW)
	return ai.BeginSignInPipes(dir, s2cR, c2sW)
}

// SignIns returns how many device codes the fake has handed out.
func (f *Fake) SignIns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signIns
}

func (f *Fake) serveLSP(in *bufio.Reader, out *io.PipeWriter) {
	defer out.Close()
	var writeMu sync.Mutex
	reply := func(id *int64, result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	for {
		m, err := readFramed(in)
		if err != nil {
			return
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"capabilities": map[string]any{}})
		case "signIn":
			f.mu.Lock()
			out := f.SignedOut
			if out {
				f.signIns++
			}
			f.mu.Unlock()
			if !out {
				reply(m.ID, map[string]any{"status": "AlreadySignedIn", "user": "octocat"})
				continue
			}
			reply(m.ID, map[string]any{
				"status": "PromptUserDeviceFlow", "userCode": FakeUserCode,
				"verificationUri": "https://github.com/login/device",
				"command": map[string]any{"command": "github.copilot.finishDeviceFlow",
					"title": "Sign in with GitHub", "arguments": []any{}},
			})
		case "workspace/executeCommand":
			st := f.SignInResult
			if st.Status == "" {
				st = ai.AuthStatus{Status: "OK", User: "octocat"}
			}
			if st.SignedIn() {
				f.mu.Lock()
				f.SignedOut = false
				f.mu.Unlock()
			}
			reply(m.ID, st)
		}
	}
}

// readFramed reads one Content-Length-framed message.
func readFramed(r *bufio.Reader) (*msg, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if n < 0 {
				continue
			}
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if n, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return nil, err
			}
		}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m msg
	return &m, json.Unmarshal(body, &m)
}
