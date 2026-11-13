package e2e

import (
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// staleCompletionAborted: a completion request Monaco no longer wants is
// aborted, so it gives its connection back to the browser (N-275).
//
// The page shares six connections with the server, and the event streams
// hold some of them for good. A completion that waits on a slow schema
// read used to keep its connection until the read ended, even after Monaco
// had dropped it, and a few of those left Run and saves queued behind
// them. The server now answers within completeWait (web/complete.go, unit
// tested). This step checks the page's half: Monaco's cancellation token
// is tied to the fetch's AbortSignal (editor.js).
//
// The slow read is played in the page: /complete requests are held for
// 30 s before they are sent, and the held request rejects as soon as its
// signal aborts, the way the browser rejects a real one. Aborting is the
// only thing that settles a request within the 30 s. Two ways Monaco drops
// a request, both found by trying them in Chrome:
//
//	"c." (trigger character)  ask 1 held; no list on screen yet
//	Ctrl+Space                 ask 2 replaces ask 1 → ask 1 aborted;
//	                           the list shows "Loading…"
//	Escape                     the list closes → ask 2 aborted
//
// Escape alone right after "c." cancels nothing: until an answer or the
// Loading message is drawn, the suggest list is not visible, and Escape's
// binding in Monaco needs it to be. Typing while an ask is held asks
// nothing new either: Monaco filters the answer when it arrives. So
// the requests that pile up are those replaced by a new ask or dropped by
// a closed list, and those are the ones the page aborts.
func staleCompletionAborted(t *testing.T, _ *env, p *rod.Page) {
	eval(t, p, `() => {
	  const f = window.fetch; // the probe's wrapper, put back at the end
	  const s = window.__compl = { asked: 0, signalled: 0, aborted: 0, restore: () => { window.fetch = f; } };
	  window.fetch = function (input, init) {
	    const url = typeof input === "string" ? input : input.url;
	    if (!/\/complete$/.test(url)) return f.apply(this, arguments);
	    s.asked++;
	    const sig = init && init.signal;
	    if (sig) s.signalled++;
	    return new Promise((res, rej) => {
	      const send = setTimeout(() => f(input, init).then(res, rej), 30000);
	      if (sig) sig.addEventListener("abort", () => {
	        s.aborted++;
	        clearTimeout(send);
	        rej(new DOMException("The operation was aborted.", "AbortError"));
	      });
	    });
	  };
	  dbc.editor.setText("SELECT  FROM cats c");
	  const ed = monaco.editor.getEditors()[0];
	  ed.setPosition({ lineNumber: 1, column: 8 });
	  ed.focus();
	}`)
	defer eval(t, p, `() => { window.__compl.restore(); dbc.editor.setText(""); }`)

	// "." is a trigger character: Monaco asks, and the ask is held
	p.Keyboard.MustType(input.KeyC, input.Period)
	waitFor(t, p, "a completion request in flight", `() => window.__compl.asked >= 1`)
	if n := evalNum(t, p, `() => window.__compl.signalled`); n < 1 {
		t.Fatal("the completion request carries no AbortSignal: nothing can abort it")
	}

	// Ctrl+Space asks again: the held ask is replaced, and aborted
	if err := p.Keyboard.Press(input.ControlLeft); err != nil {
		t.Fatal(err)
	}
	p.Keyboard.MustType(input.Space)
	if err := p.Keyboard.Release(input.ControlLeft); err != nil {
		t.Fatal(err)
	}
	waitFor(t, p, "the replaced request aborted, its successor held",
		`() => window.__compl.asked >= 2 && window.__compl.aborted >= 1`)
	waitFor(t, p, "the suggest list open, loading", `() => !!document.querySelector(".suggest-widget.visible")`)

	// Escape closes the list: Monaco cancels its request, and the page aborts it
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the dropped request aborted", `() => window.__compl.aborted >= 2`)
	// every request asked is settled: none is left holding a connection
	if asked, aborted := evalNum(t, p, `() => window.__compl.asked`), evalNum(t, p, `() => window.__compl.aborted`); aborted != asked {
		t.Fatalf("%v completion requests asked, %v aborted: the rest still hold a connection", asked, aborted)
	}
}
