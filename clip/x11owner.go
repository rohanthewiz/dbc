//go:build linux || freebsd || openbsd || netbsd || dragonfly

package clip

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/rohanthewiz/serr"
)

// X11: dbc owns the CLIPBOARD selection itself, so one copy offers the HTML
// flavor AND the text flavors a terminal asks for. xclip cannot: it answers
// one target per run (see rich_unix.go).
//
// HOW AN X11 CLIPBOARD WORKS. Nothing is stored in the server. "Copying" means
// becoming the selection's OWNER; "pasting" means the pasting app asks the
// owner, through the server, to convert the selection to a target type it
// names, and the owner writes the bytes into a property on the pasting app's
// window. So the owner must stay alive and answering until someone else
// copies — which is why xclip forks into the background, and why this does
// too:
//
//	dbc (TUI or headless)                 dbc __dbc-clip-x11 (detached helper)
//	─────────────────────                 ────────────────────────────────────
//	writeX11 ── exec, payload JSON on stdin ──▶ connect, create a hidden window,
//	                                           SetSelectionOwner(CLIPBOARD)
//	         ◀──────── "ok\n" on stdout ────── owner confirmed
//	returns nil                               serves SelectionRequests …
//	(may exit; the clipboard survives)        … until SelectionClear (someone
//	                                           else copied), then exits
//
// WHY RE-EXEC AND NOT A GOROUTINE. A goroutine owner dies with dbc, and a
// headless `dbc … script` export exits right after copying. Go cannot fork a
// running process, so the helper is the same binary started again with a
// sentinel argument; init below catches it before main or any test runs.
//
// WHY PURE GO (jezek/xgb) AND NOT Xlib. dbc builds with CGO_ENABLED=0, and the
// X11 wire protocol is small enough for the handful of requests an owner
// makes.
//
// On Wayland desktops this also reaches native apps when wl-copy is missing:
// XWayland bridges the X11 clipboard to the Wayland one, all targets intact.

const (
	x11HelperArg = "__dbc-clip-x11"
	// x11HelperEnv must ALSO be set for the helper to run: an argument alone
	// could be typed by a user or passed by something that execs any binary
	// with odd arguments, and the env var can only come from writeX11.
	x11HelperEnv = "DBC_CLIP_X11_HELPER"

	// x11Ready is the helper's one-line success report. Anything else on its
	// first stdout line is the error text.
	x11Ready = "ok"

	// x11StartTimeout bounds how long writeX11 waits for that line. Owning
	// the selection is a few round trips to a local server; a helper that
	// has not answered in this long is stuck (a dead display over ssh -X),
	// and the copy falls back to xclip or plain text.
	x11StartTimeout = 3 * time.Second

	// x11XferIdle drops an incremental transfer whose requestor stopped
	// reading (it crashed without destroying its window, or never deletes
	// the property). Without it a lost helper could never exit.
	x11XferIdle = 30 * time.Second
)

// init turns this process into the clipboard helper when writeX11 started
// it. It runs before main (and before a test binary's TestMain), so every
// binary that links clip can serve — dbc itself and each package's tests.
func init() {
	if len(os.Args) == 2 && os.Args[1] == x11HelperArg && os.Getenv(x11HelperEnv) == "1" {
		os.Exit(serveX11(os.Stdin, os.Stdout))
	}
}

// x11Payload is what the parent hands the helper. JSON for the same reason as
// on macOS: no argument-size limit and no quoting to get wrong.
type x11Payload struct {
	Text string `json:"text"`
	HTML string `json:"html"`
}

// writeX11 starts a helper that owns the clipboard with c's flavors, and
// returns once the helper reports it is the owner.
func writeX11(c Content) error {
	// A helper never launches another: if something in the helper path ever
	// reached here, the result would be a fork loop, not a clipboard.
	if os.Getenv(x11HelperEnv) != "" {
		return serr.New("clipboard helper cannot start another helper")
	}
	exe, err := os.Executable()
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}
	payload, err := json.Marshal(x11Payload{Text: c.Text, HTML: c.HTML})
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}

	cmd := exec.Command(exe, x11HelperArg)
	cmd.Env = append(os.Environ(), x11HelperEnv+"=1")
	cmd.Stdin = strings.NewReader(string(payload))
	// No stderr: after "ok" the helper outlives us, and anything it wrote to
	// the TUI's terminal would land on top of the screen.
	cmd.Dir = "/" // don't pin whatever directory dbc was started in
	// Setsid puts the helper in its own session, with no controlling
	// terminal: closing the terminal tab (SIGHUP) or Ctrl+C in a headless
	// run (SIGINT to the foreground process group) would otherwise take the
	// clipboard down with dbc.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}
	if err = cmd.Start(); err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case s := <-line:
		if s == x11Ready {
			// Reap it whenever it exits (the next copy clears it), so a
			// long TUI session does not collect one zombie per copy. If dbc
			// exits first, init adopts it.
			go func() { _ = cmd.Wait() }()
			return nil
		}
		_ = cmd.Wait()
		if s == "" {
			s = "helper exited without reporting"
		}
		return serr.New(s, "op", "clipboard-x11")
	case <-time.After(x11StartTimeout):
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return serr.New("clipboard helper did not start in time", "op", "clipboard-x11")
	}
}

// serveX11 is the helper's whole life: read the payload, take the selection,
// report, serve until another client takes it. It returns the exit status.
func serveX11(in io.Reader, out *os.File) int {
	var p x11Payload
	if err := json.NewDecoder(in).Decode(&p); err != nil {
		fmt.Fprintf(out, "reading clipboard payload: %v\n", err)
		return 1
	}
	o, err := newX11Owner(p)
	if err != nil {
		fmt.Fprintf(out, "%v\n", err)
		return 1
	}
	fmt.Fprintln(out, x11Ready)
	// Closing stdout is what lets the parent's reader goroutine finish and
	// its later Wait return; the helper has nothing more to say.
	_ = out.Close()
	o.serve()
	o.conn.Close()
	return 0
}

// x11Offer is one target the owner converts to: the property type the reply
// carries and its bytes.
type x11Offer struct {
	typ  xproto.Atom
	data []byte
}

// x11Xfer is one INCR (incremental) transfer in progress. ICCCM §2.7.2: a
// value too big for one request goes out in chunks, each written after the
// requestor deletes the previous one, ending with a zero-length chunk.
type x11Xfer struct {
	typ  xproto.Atom
	data []byte
	off  int
	last time.Time // last progress, for x11XferIdle
}

type x11XferKey struct {
	win  xproto.Window
	prop xproto.Atom
}

// x11EvErr is one item from the connection: an event or an X error.
type x11EvErr struct {
	ev  xgb.Event
	err xgb.Error
}

type x11Owner struct {
	conn   *xgb.Conn
	win    xproto.Window
	time   xproto.Timestamp // when we took the selection
	events chan x11EvErr    // closed when the connection drops

	clipboard, targets, timestamp, incr xproto.Atom

	order  []xproto.Atom // TARGETS reply order: richest first, as toolkits expect
	offers map[xproto.Atom]x11Offer
	chunk  int // max bytes in one ChangeProperty before INCR

	xfers map[x11XferKey]*x11Xfer
	lost  bool // SelectionClear seen; exit once transfers finish
}

// newX11Owner connects to $DISPLAY and takes CLIPBOARD with p's content.
func newX11Owner(p x11Payload) (*x11Owner, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, serr.Wrap(err, "op", "clipboard-x11", "display", os.Getenv("DISPLAY"))
	}
	o := &x11Owner{conn: conn, xfers: map[x11XferKey]*x11Xfer{}}
	if err = o.setup(p); err != nil {
		conn.Close()
		return nil, err
	}
	return o, nil
}

func (o *x11Owner) setup(p x11Payload) error {
	// The event pump starts first: taking a timestamp below waits on an
	// event, and the channel lets every wait here carry a timeout.
	o.events = make(chan x11EvErr, 16)
	go func() {
		for {
			ev, err := o.conn.WaitForEvent()
			if ev == nil && err == nil {
				close(o.events) // connection closed
				return
			}
			o.events <- x11EvErr{ev, err}
		}
	}()

	names := []string{"CLIPBOARD", "TARGETS", "TIMESTAMP", "INCR", "UTF8_STRING",
		"TEXT", "text/plain;charset=utf-8", "text/plain", "text/html", "_DBC_CLIP"}
	atoms := make(map[string]xproto.Atom, len(names))
	// Send every InternAtom before reading any reply: one round trip's
	// latency instead of ten.
	cookies := make([]xproto.InternAtomCookie, len(names))
	for i, n := range names {
		cookies[i] = xproto.InternAtom(o.conn, false, uint16(len(n)), n)
	}
	for i, ck := range cookies {
		r, err := ck.Reply()
		if err != nil {
			return serr.Wrap(err, "op", "clipboard-x11", "atom", names[i])
		}
		atoms[names[i]] = r.Atom
	}
	o.clipboard, o.targets, o.timestamp, o.incr =
		atoms["CLIPBOARD"], atoms["TARGETS"], atoms["TIMESTAMP"], atoms["INCR"]

	// The offers. Every text target carries c.Text; which one a client asks
	// for depends on its toolkit (GTK: UTF8_STRING or text/plain;charset=
	// utf-8, Qt: text/plain, xterm: UTF8_STRING then STRING). TEXT is
	// answered as UTF8_STRING, which ICCCM allows and GTK and xclip both do.
	// STRING is Latin-1 by definition, so it gets a real conversion rather
	// than UTF-8 bytes a strict client would show as mojibake.
	utf8 := atoms["UTF8_STRING"]
	text := []byte(p.Text)
	o.offers = map[xproto.Atom]x11Offer{}
	add := func(target, typ xproto.Atom, data []byte) {
		o.order = append(o.order, target)
		o.offers[target] = x11Offer{typ, data}
	}
	if p.HTML != "" {
		add(atoms["text/html"], atoms["text/html"], []byte(p.HTML))
	}
	add(utf8, utf8, text)
	add(atoms["text/plain;charset=utf-8"], atoms["text/plain;charset=utf-8"], text)
	add(atoms["text/plain"], atoms["text/plain"], text)
	add(atoms["TEXT"], utf8, text)
	add(xproto.AtomString, xproto.AtomString, latin1(p.Text))

	// A request's length field is 16 bits of 4-byte units, so one request
	// tops out at MaximumRequestLength*4 bytes (256 KiB without BIG-REQUESTS,
	// which xgb does not enable). A quarter of that per chunk follows xclip
	// and leaves the requestor's own buffers comfortable.
	o.chunk = max(int(xproto.Setup(o.conn).MaximumRequestLength), 1024)

	// An unmapped InputOnly window: never drawn, exists only so there is an
	// owner for the server to route requests to. PropertyChange on it is for
	// the timestamp trick below.
	screen := xproto.Setup(o.conn).DefaultScreen(o.conn)
	wid, err := xproto.NewWindowId(o.conn)
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}
	o.win = wid
	if err = xproto.CreateWindowChecked(o.conn, 0, o.win, screen.Root, 0, 0, 1, 1, 0,
		xproto.WindowClassInputOnly, 0, xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}

	// ICCCM §2.1 wants a real server timestamp for SetSelectionOwner, not
	// CurrentTime — clients use it to tell which copy is newer, and it is
	// what the TIMESTAMP target returns. The standard way to get one is to
	// touch a property on our own window and read the time off the
	// PropertyNotify the server sends back.
	xproto.ChangeProperty(o.conn, xproto.PropModeAppend, o.win, atoms["_DBC_CLIP"],
		xproto.AtomString, 8, 0, nil)
	if o.time, err = o.awaitTimestamp(); err != nil {
		return err
	}

	xproto.SetSelectionOwner(o.conn, o.win, o.clipboard, o.time)
	// SetSelectionOwner has no reply and can silently lose to a newer
	// timestamp, so ask who owns it now: only a yes is worth reporting "ok".
	r, err := xproto.GetSelectionOwner(o.conn, o.clipboard).Reply()
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-x11")
	}
	if r.Owner != o.win {
		return serr.New("could not take the X11 clipboard", "op", "clipboard-x11")
	}
	return nil
}

// awaitTimestamp waits for the PropertyNotify on our window.
func (o *x11Owner) awaitTimestamp() (xproto.Timestamp, error) {
	deadline := time.After(x11StartTimeout)
	for {
		select {
		case e, ok := <-o.events:
			if !ok {
				return 0, serr.New("X connection closed", "op", "clipboard-x11")
			}
			if pn, is := e.ev.(xproto.PropertyNotifyEvent); is && pn.Window == o.win {
				return pn.Time, nil
			}
		case <-deadline:
			return 0, serr.New("no timestamp from the X server", "op", "clipboard-x11")
		}
	}
}

// serve answers requests until the selection is lost and every transfer has
// finished, or the connection drops.
func (o *x11Owner) serve() {
	tick := time.NewTicker(x11XferIdle / 3)
	defer tick.Stop()
	for !(o.lost && len(o.xfers) == 0) {
		select {
		case e, ok := <-o.events:
			if !ok {
				return
			}
			o.handle(e)
		case now := <-tick.C:
			for k, x := range o.xfers {
				if now.Sub(x.last) > x11XferIdle {
					o.endXfer(k)
				}
			}
		}
	}
}

func (o *x11Owner) handle(e x11EvErr) {
	if e.err != nil {
		// Our requests are unchecked, so their errors arrive here. The one
		// that matters is BadWindow: a requestor went away mid-transfer.
		// Drop its transfers; everything else has no one to report to.
		if we, is := e.err.(xproto.WindowError); is {
			for k := range o.xfers {
				if uint32(k.win) == we.BadId() {
					delete(o.xfers, k)
				}
			}
		}
		return
	}
	switch ev := e.ev.(type) {
	case xproto.SelectionRequestEvent:
		o.answer(ev)
	case xproto.PropertyNotifyEvent:
		if ev.State == xproto.PropertyDelete {
			o.nextChunk(x11XferKey{ev.Window, ev.Atom})
		}
	case xproto.SelectionClearEvent:
		// Someone else copied. Transfers already under way still finish —
		// the requestor asked while the content was ours.
		if ev.Selection == o.clipboard {
			o.lost = true
		}
	}
}

// answer converts the selection for one request. ICCCM §2.2: write the reply
// into the requestor's property, then send SelectionNotify naming that
// property, or naming None to refuse.
func (o *x11Owner) answer(ev xproto.SelectionRequestEvent) {
	prop := ev.Property
	if prop == xproto.AtomNone {
		prop = ev.Target // obsolete clients: ICCCM says use the target
	}
	switch offer, ok := o.offers[ev.Target]; {
	case ev.Selection != o.clipboard || o.lost:
		prop = xproto.AtomNone
	case ev.Target == o.targets:
		list := append([]xproto.Atom{o.targets, o.timestamp}, o.order...)
		buf := make([]byte, 4*len(list))
		for i, a := range list {
			xgb.Put32(buf[4*i:], uint32(a))
		}
		xproto.ChangeProperty(o.conn, xproto.PropModeReplace, ev.Requestor, prop,
			xproto.AtomAtom, 32, uint32(len(list)), buf)
	case ev.Target == o.timestamp:
		buf := make([]byte, 4)
		xgb.Put32(buf, uint32(o.time))
		xproto.ChangeProperty(o.conn, xproto.PropModeReplace, ev.Requestor, prop,
			xproto.AtomInteger, 32, 1, buf)
	case ok && len(offer.data) > o.chunk:
		// Too big for one request: announce INCR with a lower bound on the
		// size, then feed chunks as the requestor deletes each one. Watching
		// its window for PropertyNotify is how we see those deletes.
		xproto.ChangeWindowAttributes(o.conn, ev.Requestor, xproto.CwEventMask,
			[]uint32{xproto.EventMaskPropertyChange})
		buf := make([]byte, 4)
		xgb.Put32(buf, uint32(len(offer.data)))
		xproto.ChangeProperty(o.conn, xproto.PropModeReplace, ev.Requestor, prop,
			o.incr, 32, 1, buf)
		o.xfers[x11XferKey{ev.Requestor, prop}] = &x11Xfer{typ: offer.typ, data: offer.data, last: time.Now()}
	case ok:
		xproto.ChangeProperty(o.conn, xproto.PropModeReplace, ev.Requestor, prop,
			offer.typ, 8, uint32(len(offer.data)), offer.data)
	default:
		// MULTIPLE lands here too. Toolkits fall back to single requests
		// when it is refused, and serving it means parsing atom pairs out
		// of the requestor's property for no user-visible gain.
		prop = xproto.AtomNone
	}

	notify := xproto.SelectionNotifyEvent{
		Time:      ev.Time,
		Requestor: ev.Requestor,
		Selection: ev.Selection,
		Target:    ev.Target,
		Property:  prop,
	}
	xproto.SendEvent(o.conn, false, ev.Requestor, xproto.EventMaskNoEvent, string(notify.Bytes()))
}

// nextChunk writes the next piece of an INCR transfer after the requestor
// deleted the previous one. The piece after the last real one is empty,
// which tells the requestor the transfer is complete.
func (o *x11Owner) nextChunk(k x11XferKey) {
	x, ok := o.xfers[k]
	if !ok {
		return // a property delete that is not one of our transfers
	}
	n := min(o.chunk, len(x.data)-x.off)
	xproto.ChangeProperty(o.conn, xproto.PropModeReplace, k.win, k.prop,
		x.typ, 8, uint32(n), x.data[x.off:x.off+n])
	if n == 0 {
		o.endXfer(k)
		return
	}
	x.off += n
	x.last = time.Now()
}

// endXfer forgets a transfer and, when it was the last one to that window,
// stops listening to the window's property changes.
func (o *x11Owner) endXfer(k x11XferKey) {
	delete(o.xfers, k)
	for other := range o.xfers {
		if other.win == k.win {
			return
		}
	}
	xproto.ChangeWindowAttributes(o.conn, k.win, xproto.CwEventMask, []uint32{0})
}

// latin1 converts s for the STRING target, which ICCCM defines as ISO
// 8859-1. Runes outside it become '?': visible, rather than silently wrong.
func latin1(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			r = '?'
		}
		b = append(b, byte(r))
	}
	return b
}
