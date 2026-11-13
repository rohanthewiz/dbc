package web

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/workspace"
)

// completeAnswerT is a completion answer as the page reads it.
type completeAnswerT struct {
	Items      []completeItem `json:"items"`
	Incomplete bool           `json:"incomplete"`
	Note       string         `json:"note"`
}

func (a completeAnswerT) has(label string) bool {
	for _, it := range a.Items {
		if it.Label == label {
			return true
		}
	}
	return false
}

// slowReads makes the schema reads completion requests start wait for
// release, and shortens how long a request waits for one, so that a read
// "slower than completeWait" is certain rather than timed. It restores
// both, and lets any held read go, when the test ends.
func slowReads(t *testing.T) (release func()) {
	t.Helper()
	prevWait, prevLoad, prevConn := completeWait, loadCompletions, loadConnCompletions
	gate := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	completeWait = 50 * time.Millisecond
	loadCompletions = func(ws *workspace.Workspace) error {
		<-gate
		return ws.LoadCompletions(context.Background())
	}
	loadConnCompletions = func(c *workspace.ConnCompletions, conn string) error {
		<-gate
		return c.Load(context.Background(), conn)
	}
	t.Cleanup(func() {
		release()
		completeWait, loadCompletions, loadConnCompletions = prevWait, prevLoad, prevConn
	})
	return release
}

// N-275: a completion request does not wait out a slow schema read. Past
// completeWait it answers with the dialect's vocabulary, marked incomplete,
// and the read goes on; once it lands, the next request gets the schema.
// Each request that came in meanwhile answered promptly, so none of them
// held a browser connection for the read's length.
func TestCompleteDoesNotWaitOutASlowRead(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.open()
	release := slowReads(t)
	const body = `{"buffer":"SELECT coa FROM c","caret":17}`
	ask := func() (completeAnswerT, time.Duration) {
		t.Helper()
		start := time.Now()
		a := decodeData[completeAnswerT](t, e.api("POST", "/api/v1/ws/"+id+"/complete", body, 200))
		return a, time.Since(start)
	}

	// while the read is held: three asks in a row each answer within about
	// completeWait, with the vocabulary and no tables
	for i := range 3 {
		a, took := ask()
		if took > 2*time.Second {
			t.Fatalf("ask %d took %v while the schema was being read: it waited for the read", i, took)
		}
		if !a.Incomplete || a.has("cats") || a.Note != "" {
			t.Fatalf("ask %d while reading: incomplete=%v cats=%v note=%q, want incomplete, no tables, no note",
				i, a.Incomplete, a.has("cats"), a.Note)
		}
	}
	// the vocabulary is there (a function's name, at the caret after "coa")
	vocab := decodeData[completeAnswerT](t, e.api("POST", "/api/v1/ws/"+id+"/complete", `{"buffer":"SELECT coa","caret":10}`, 200))
	if !vocab.has("coalesce") || !vocab.Incomplete {
		t.Fatalf("while reading, the vocabulary: coalesce=%v incomplete=%v", vocab.has("coalesce"), vocab.Incomplete)
	}

	// the read lands: the schema's tables are offered, and the list is
	// whole again
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a, _ := ask()
		if a.has("cats") && !a.Incomplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read landed but the answer still lacks the schema: %+v", a)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A quick read is waited for, as before: the first ask on a cold cache
// answers in full when the read lands within completeWait.
func TestCompleteWaitsForAQuickRead(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.open()
	prev := completeWait
	completeWait = 30 * time.Second // the sqlite read is milliseconds: it lands well inside this
	t.Cleanup(func() { completeWait = prev })
	a := decodeData[completeAnswerT](t, e.api("POST", "/api/v1/ws/"+id+"/complete", `{"buffer":"SELECT * FROM c","caret":15}`, 200))
	if !a.has("cats") || a.Incomplete {
		t.Fatalf("first ask on a cold cache: cats=%v incomplete=%v, want the table and a whole list", a.has("cats"), a.Incomplete)
	}
}

// conn-complete (a pipeline field's editor) answers the same way.
func TestConnCompleteDoesNotWaitOutASlowRead(t *testing.T) {
	e := newTestEnv(t)
	release := slowReads(t)
	const body = `{"conn":"demo-sqlite","buffer":"SELECT * FROM c","caret":15}`
	start := time.Now()
	a := decodeData[completeAnswerT](t, e.api("POST", "/api/v1/conn-complete", body, 200))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("conn-complete took %v while the schema was being read", took)
	}
	if !a.Incomplete || a.has("cats") {
		t.Fatalf("while reading: incomplete=%v cats=%v, want incomplete and no tables", a.Incomplete, a.has("cats"))
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a = decodeData[completeAnswerT](t, e.api("POST", "/api/v1/conn-complete", body, 200))
		if a.has("cats") && !a.Incomplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read landed but conn-complete still lacks the schema: %+v", a)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// an unknown connection is answered at once, with the reason
	a = decodeData[completeAnswerT](t, e.api("POST", "/api/v1/conn-complete", `{"conn":"nope","buffer":"SELECT coa","caret":10}`, 200))
	if a.Incomplete || !a.has("coalesce") || a.Note == "" {
		t.Fatalf("unknown connection: incomplete=%v coalesce=%v note=%q", a.Incomplete, a.has("coalesce"), a.Note)
	}
}
