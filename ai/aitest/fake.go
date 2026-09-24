// Package aitest is a scripted ACP agent for testing code that uses package
// ai, without spawning or signing in to a real one.
package aitest

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/rohanthewiz/dbc/ai"
)

// Fake is an ACP agent on the far side of in-memory pipes. It completes the
// handshake with a two-model roster, and answers each prompt by streaming the
// chunks Reply returns for it (by default, "echo: " plus the prompt's last
// line). A prompt containing "SLOW" streams nothing until it is cancelled.
type Fake struct {
	// Reply scripts the answer to a prompt, as the chunks to stream.
	Reply func(prompt string) []string

	mu      sync.Mutex
	prompts []string
	cancels int

	out     *io.PipeWriter
	writeMu sync.Mutex
	cancel  chan struct{}
}

// Start runs a Chat against a new Fake.
func Start(agent ai.Agent, opt ai.Options, f *Fake) *ai.Chat {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	f.out = a2cW
	f.cancel = make(chan struct{}, 4)
	go f.serve(bufio.NewReader(c2aR))
	return ai.StartPipes(agent, opt, a2cR, c2aW)
}

// Prompts returns the prompt texts received so far.
func (f *Fake) Prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// Cancels returns how many session/cancel notifications arrived.
func (f *Fake) Cancels() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

type msg struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (f *Fake) write(v any) {
	b, _ := json.Marshal(v)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.out.Write(append(b, '\n'))
}

func (f *Fake) reply(id *int64, result any) {
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *Fake) serve(in *bufio.Reader) {
	defer f.out.Close()
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		var m msg
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			f.reply(m.ID, map[string]any{"protocolVersion": 1})
		case "session/new":
			f.reply(m.ID, map[string]any{"sessionId": "s1", "models": map[string]any{
				"currentModelId": "fast",
				"availableModels": []any{
					map[string]any{"modelId": "fast", "name": "Fast Model", "_meta": map[string]any{"copilotUsage": "0x"}},
					map[string]any{"modelId": "smart", "name": "Smart Model", "_meta": map[string]any{"copilotUsage": "1x"}},
				},
			}})
		case "session/set_model":
			f.reply(m.ID, map[string]any{})
		case "session/cancel":
			f.mu.Lock()
			f.cancels++
			f.mu.Unlock()
			f.cancel <- struct{}{}
		case "session/prompt":
			go f.turn(m)
		}
	}
}

func (f *Fake) turn(m msg) {
	var p struct {
		Prompt []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	text := ""
	if len(p.Prompt) > 0 {
		text = p.Prompt[0].Text
	}
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	f.mu.Unlock()

	if strings.Contains(text, "SLOW") {
		<-f.cancel
		f.reply(m.ID, map[string]any{"stopReason": "cancelled"})
		return
	}
	chunks := []string{"echo: " + lastLine(text)}
	if f.Reply != nil {
		chunks = f.Reply(text)
	}
	for _, c := range chunks {
		f.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
			"sessionId": "s1",
			"update": map[string]any{"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{"type": "text", "text": c}},
		}})
	}
	f.reply(m.ID, map[string]any{"stopReason": "end_turn"})
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
