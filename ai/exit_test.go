package ai_test

import (
	"errors"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/ai"
	"github.com/rohanthewiz/dbc/ai/aitest"
)

// A refused handshake is one EventExit, and it is the refusal: closing the
// connection after session/new fails also ends the read loop, whose own
// "exited" used to win the race now and then (N-091) — a UI reading up to
// the first EventExit then never offered sign-in. Many rounds, since the
// race is one of goroutine scheduling.
func TestRefusedHandshakeIsOneAuthExit(t *testing.T) {
	for i := range 300 {
		c := aitest.Start(ai.Agents()[0], ai.Options{Dir: "/w"}, &aitest.Fake{SignedOut: true})
		var e ai.Event
		select {
		case e = <-c.Events():
		case <-time.After(5 * time.Second):
			t.Fatalf("round %d: no event", i)
		}
		if e.Kind != ai.EventExit || !errors.Is(e.Err, ai.ErrAuthRequired) {
			t.Fatalf("round %d: first event = %+v, want an Exit matching ErrAuthRequired", i, e)
		}
		if i%30 == 0 { // a second exit would follow within moments
			select {
			case e2 := <-c.Events():
				t.Fatalf("round %d: a second event after Exit: %+v", i, e2)
			case <-time.After(20 * time.Millisecond):
			}
		}
		c.Close()
	}
}
