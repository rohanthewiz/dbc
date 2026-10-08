package workspace

import (
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// On a driver that stores no routines (SQLite) the list switches on and
// reads as empty at once — no Job, nothing to wait for — and a connect
// with it on lands an empty list too rather than leaving it unread, which
// a UI would show as "loading…" for good. A DDL is refused.
func TestShowRoutinesWithoutRoutines(t *testing.T) {
	w := newTestWorkspace(t)
	if w.RoutinesShown() || w.Routines() != nil {
		t.Fatal("the routines list starts on")
	}
	if j := w.ShowRoutines(true); j != nil {
		t.Fatal("a Job for a driver without routines")
	}
	if !w.RoutinesShown() || w.Routines() == nil || len(w.Routines()) != 0 {
		t.Fatalf("on: shown %v, routines %#v", w.RoutinesShown(), w.Routines())
	}
	if j := w.ShowRoutines(true); j != nil {
		t.Fatal("a repeat made a Job")
	}

	// a refresh lands a catalog, and with it the (empty) list again
	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	ev := st.Job().(*Connected)
	if ev.Routines != nil || w.Routines() == nil {
		t.Fatalf("refresh: job %v, routines %#v", ev.Routines != nil, w.Routines())
	}

	if w.ShowRoutines(false) != nil || w.RoutinesShown() || w.Routines() != nil {
		t.Fatal("off kept the list")
	}

	_, err = w.RoutineDDL(model.Routine{Name: "f", Kind: model.RoutineFunction})
	refusal(t, err, Invalid)
}

// With the list off, no landing makes a Routines job: a sidebar showing
// tables costs no routine read.
func TestRoutinesOffMakesNoJob(t *testing.T) {
	w := newTestWorkspace(t)
	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if ev := st.Job().(*Connected); ev.Routines != nil || w.Routines() != nil {
		t.Fatal("a routines read with the list off")
	}
}
