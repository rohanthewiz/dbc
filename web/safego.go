package web

import (
	"fmt"
	"runtime/debug"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"
)

// goSafe runs fn on a goroutine of its own, recovering a panic in it.
//
// rweb recovers a panic in a request handler, but not one in a goroutine a
// handler started — a Job delivering its outcome, a recount, a tab's
// workspace being closed, the assistant's pump — and an unrecovered panic
// anywhere ends the process: dbc web, with every tab's session and every
// open transaction in it (N-260). No such panic is known; this is the
// backstop. A recovered one is logged with its stack, and said in t's log
// when the work was a tab's (t may be nil).
func goSafe(t *tab, what string, fn func()) { go runSafe(t, what, fn, nil) }

// runSafe runs fn, turning a panic in it into a logged error, and then
// calls after with that error — for undoing what fn left half done (a run
// that never landed: Server.launch). after runs under the same guard.
func runSafe(t *tab, what string, fn func(), after func(error)) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		err := fmt.Errorf("internal error in %s: %v", what, r)
		logger.LogErr(serr.Wrap(err, "stack", string(debug.Stack())), "recovered a panic; dbc web keeps running")
		if t != nil {
			t.logOn("", "error", err.Error()+" — dbc web kept running; the stack is in its own output")
		}
		if after != nil {
			runSafe(t, what+" (its cleanup)", func() { after(err) }, nil)
		}
	}()
	fn()
}
