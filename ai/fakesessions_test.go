package ai_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/ai/aitest"
)

// One Fake serving several chats at once, as dbc web's tests have it (one
// Fake for every chat a test server starts): each chat's answers come on
// its own pipe, and nothing is shared between the sessions' goroutines
// (N-333). Before, every Start overwrote the Fake's pipe and cancel
// channel under the previous session's goroutine — a data race under
// -race, and an old session could write into the new chat's pipe. Run it
// with -race.
func TestOneFakeManyChats(t *testing.T) {
	f := &aitest.Fake{}
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			c := aitest.Start(ai.Agents()[0], ai.Options{Dir: "/w"}, f)
			defer c.Close()
			deadline := time.After(10 * time.Second)
			next := func() (ai.Event, bool) {
				select {
				case e := <-c.Events():
					return e, true
				case <-deadline:
					return ai.Event{}, false
				}
			}
			for e, ok := next(); e.Kind != ai.EventReady; e, ok = next() {
				if !ok || e.Kind == ai.EventExit {
					errs <- fmt.Errorf("chat %d: no ready (%+v)", i, e)
					return
				}
			}
			want := fmt.Sprintf("hello from %d", i)
			go func() { _ = c.Send(want) }()
			var got strings.Builder
			for e, ok := next(); e.Kind != ai.EventTurnDone; e, ok = next() {
				if !ok {
					errs <- fmt.Errorf("chat %d: no answer, got %q so far", i, got.String())
					return
				}
				if e.Kind == ai.EventText {
					got.WriteString(e.Text)
				}
			}
			if got.String() != "echo: "+want {
				errs <- fmt.Errorf("chat %d answered %q, want %q", i, got.String(), "echo: "+want)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if len(f.Prompts()) != n {
		t.Errorf("%d prompts, want %d", len(f.Prompts()), n)
	}
}
