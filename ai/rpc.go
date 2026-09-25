package ai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The JSON-RPC 2.0 transport the Agent Client Protocol rides on.
//
// PROVENANCE. Ported from ced (github.com/rohanthewiz/ced,
// internal/lsp/client.go, acp.go, ndjson.go), where it has carried Copilot
// chat since July 2026. The correlation, the per-request goroutines and the
// teardown order are unchanged, because those are the parts that took ced
// several sessions to get right.
//
// TWO DIALECTS. Chat is ACP, which frames newline-delimited JSON. Copilot
// sign-in (signin.go) has to run the same binary as an LSP server, which
// frames with Content-Length headers — so both survive here, chosen per
// connection. The envelope, correlation and threading are identical; only
// send and the reader differ.
//
//	caller ──Call/Notify──► rpcConn ──stdin──► agent process
//	  ▲                        │
//	  │   onNotify / onRequest │◄──stdout── readLoop goroutine
//	  └── the Chat turns these into Events on a channel; nothing here
//	      touches UI state
//
// FRAMING. ACP: one JSON object per line — json.Marshal never emits a raw
// newline inside an object, so body+"\n" is always exactly one record. LSP:
// "Content-Length: N\r\n\r\n" then exactly N bytes of body.
//
// THREADING. Call and Notify are safe from any goroutine (writes are
// serialized by writeMu). The read loop runs on its own goroutine: it
// resolves pending Calls, hands notifications to onNotify, and starts a NEW
// goroutine per agent→client request, because a request handler may block
// (ced's permission prompt waits on the user) and the loop must keep
// draining streamed notifications meanwhile.

// message is the JSON-RPC envelope, used in both directions. Which fields are
// set decides the shape: ID+Method = request, ID+Result/Error = response,
// Method alone = notification.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object of a failed response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// errConnClosed is what every Call gets once the connection is gone.
var errConnClosed = errors.New("agent connection closed")

// rpcConn is one JSON-RPC connection to one agent process.
type rpcConn struct {
	writeMu sync.Mutex
	w       io.Writer
	r       *bufio.Reader

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *message
	closed  bool

	// lsp selects Content-Length framing instead of newline-delimited JSON.
	// Fixed before the read loop starts; never changed after.
	lsp bool

	// onNotify receives agent→client notifications. Called on the read-loop
	// goroutine, so it must hand off rather than block for long.
	onNotify func(method string, params json.RawMessage)

	// onRequest answers agent→client REQUESTS: its result is marshaled into
	// the response, its error into a JSON-RPC error. Each call runs on its
	// own goroutine. Nil answers every request with method-not-found.
	onRequest func(method string, params json.RawMessage) (any, error)

	// onExit fires once when the read loop ends (process exit, pipe closed,
	// or a protocol error), on the read-loop goroutine.
	onExit func(err error)

	// cmd is the spawned process; nil for connections over test pipes.
	cmd *exec.Cmd
}

// newRPCConn wraps a reader/writer pair (the agent's stdout/stdin) and starts
// the read loop. The hooks are parameters rather than fields set afterwards
// because the read loop reads them from its first message on; assigning them
// later would race the very goroutine that uses them. Split from startRPC so
// tests can drive the protocol over in-memory pipes.
func newRPCConn(r io.Reader, w io.Writer,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) *rpcConn {
	return newConn(r, w, false, onNotify, onRequest, onExit)
}

// newLSPConn is newRPCConn with LSP's Content-Length framing.
func newLSPConn(r io.Reader, w io.Writer,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) *rpcConn {
	return newConn(r, w, true, onNotify, onRequest, onExit)
}

// newConn builds a connection in either dialect. The dialect is set before
// the read loop starts, for the same reason the hooks are.
func newConn(r io.Reader, w io.Writer, lsp bool,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) *rpcConn {
	c := &rpcConn{
		w:         w,
		r:         bufio.NewReader(r),
		pending:   map[int64]chan *message{},
		lsp:       lsp,
		onNotify:  onNotify,
		onRequest: onRequest,
		onExit:    onExit,
	}
	go c.readLoop()
	return c
}

// startRPC launches bin with args in dir and wires a connection to its stdio.
//
// The agent inherits dbc's environment unchanged: its credentials live in its
// own store (Copilot's under ~/.config/github-copilot), or in variables such
// as ANTHROPIC_API_KEY that the user set for it, never in dbc's config.
//
// Its stderr is discarded. The TUI owns the terminal, so anything the agent
// logged there would be drawn over the UI, and agent logs are not something a
// database user can act on.
func startRPC(dir, bin string, args []string,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) (*rpcConn, error) {
	return spawn(dir, bin, args, false, onNotify, onRequest, onExit)
}

// spawn is startRPC in either dialect.
func spawn(dir, bin string, args []string, lsp bool,
	onNotify func(string, json.RawMessage),
	onRequest func(string, json.RawMessage) (any, error),
	onExit func(error)) (*rpcConn, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	c := newConn(stdout, stdin, lsp, onNotify, onRequest, onExit)
	c.cmd = cmd
	return c, nil
}

// call sends a request and blocks until its response arrives or timeout
// passes, then decodes the result into result (skipped when nil). The timeout
// is always explicit: ACP calls range from a 30-second session/new to a
// prompt turn that can legitimately stream for many minutes.
func (c *rpcConn) call(method string, params, result any, timeout time.Duration) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errConnClosed
	}
	c.nextID++
	id := c.nextID
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(&message{JSONRPC: "2.0", ID: &id, Method: method, Params: marshalParams(params)}); err != nil {
		c.forget(id)
		return err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp == nil {
			return errConnClosed
		}
		if resp.Error != nil {
			return fmt.Errorf("%s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-timer.C:
		c.forget(id)
		return fmt.Errorf("%s timed out after %s", method, timeout)
	}
}

// forget drops a pending call that will never be answered usefully.
func (c *rpcConn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// notify sends a fire-and-forget notification.
func (c *rpcConn) notify(method string, params any) error {
	return c.send(&message{JSONRPC: "2.0", Method: method, Params: marshalParams(params)})
}

// close tears the connection down: close the agent's stdin (which a
// well-behaved agent takes as the end of the session) and kill the process
// after a grace period if it has not exited. Reaping happens on a goroutine,
// so close never blocks the UI. Idempotent.
func (c *rpcConn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	if wc, ok := c.w.(io.Closer); ok {
		_ = wc.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		proc := c.cmd
		go func() {
			done := make(chan struct{})
			go func() { _ = proc.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = proc.Process.Kill()
				<-done
			}
		}()
	}
}

// send frames and writes one message, serialized so concurrent callers cannot
// interleave their bytes.
func (c *rpcConn) send(m *message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if c.lsp {
		// header and body in one Write, so a reader on the far side never
		// sees a header without the body that follows it
		body = append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	} else {
		body = append(body, '\n')
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(body)
	return err
}

// marshalParams pre-encodes params so the envelope marshal cannot fail half
// way. Params are always this package's own values, so a failure is a
// programming error; null keeps the wire valid regardless.
func marshalParams(params any) json.RawMessage {
	if params == nil {
		return nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// readLoop drains the agent's messages until the pipe closes, routing each to
// the call it answers, the notification hook, or a request handler.
func (c *rpcConn) readLoop() {
	var loopErr error
	read := readLine
	if c.lsp {
		read = readFramed
	}
	for {
		m, err := read(c.r)
		if err != nil {
			if err != io.EOF {
				loopErr = err
			}
			break
		}
		switch {
		case m.ID != nil && m.Method != "":
			c.serveRequest(m)
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		}
	}

	// The connection is over: fail every in-flight call now rather than at
	// its timeout — a prompt turn's timeout is many minutes — and tell the
	// owner.
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- nil
	}
	c.mu.Unlock()
	if c.onExit != nil {
		c.onExit(loopErr)
	}
}

// serveRequest answers one agent→client request on its own goroutine.
// JSON-RPC correlates by id, so answering out of order is legal.
func (c *rpcConn) serveRequest(m *message) {
	go func() {
		if c.onRequest == nil {
			_ = c.send(&message{JSONRPC: "2.0", ID: m.ID,
				Error: &rpcError{Code: -32601, Message: "method not supported: " + m.Method}})
			return
		}
		res, err := c.onRequest(m.Method, m.Params)
		if err != nil {
			// -32601 (method not found) is the honest code for a request
			// this client declines; the message says why.
			_ = c.send(&message{JSONRPC: "2.0", ID: m.ID,
				Error: &rpcError{Code: -32601, Message: err.Error()}})
			return
		}
		raw, _ := json.Marshal(res)
		_ = c.send(&message{JSONRPC: "2.0", ID: m.ID, Result: raw})
	}()
}

// readLine parses one newline-delimited message. Blank lines are skipped
// (some agents emit them as keep-alives). A line that is not valid JSON is a
// hard error: once a record boundary is wrong there is no way to find the
// next one. A last record arriving without its newline right before EOF is
// parsed rather than dropped.
func readLine(r *bufio.Reader) (*message, error) {
	for {
		line, err := r.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		var m message
		if uerr := json.Unmarshal(trimmed, &m); uerr != nil {
			return nil, fmt.Errorf("bad message from agent: %w", uerr)
		}
		return &m, nil
	}
}

// readFramed parses one Content-Length-framed message. Headers other than
// Content-Length (Content-Type) are skipped. A missing or malformed length
// is a hard error for the same reason a bad NDJSON line is: past it, there
// is no way to find where the next message starts. EOF between messages is
// a clean end (io.EOF); EOF inside one is not.
func readFramed(r *bufio.Reader) (*message, error) {
	length, headers := -1, 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && line == "" && headers == 0 {
				return nil, io.EOF
			}
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if headers == 0 {
				continue // a stray blank line between messages
			}
			break // the blank line that ends the header block
		}
		headers++
		if name, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length %q from agent", strings.TrimSpace(v))
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("message from agent has no Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("bad message from agent: %w", err)
	}
	return &m, nil
}
