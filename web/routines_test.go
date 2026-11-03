package web

import (
	"testing"
)

// The Tables / Routines switch on a driver that stores none (SQLite): a
// "routines" event at once, the list empty rather than null (so the page
// says "no routines", not "loading…"), the state saying the switch is on
// and the driver has none to offer. A DDL for a routine the tab does not
// list is refused (409: the page's list is stale), not read.
func TestRoutinesSwitchWithoutRoutines(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	st0 := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st0.ShowRoutines || st0.HasRoutines || st0.Routines != nil {
		t.Fatalf("before: on %v, has %v, list %+v", st0.ShowRoutines, st0.HasRoutines, st0.Routines)
	}

	e.api("POST", "/api/v1/ws/"+id+"/routines", `{"on":true}`, 200)
	ev, _ := s.await(t, "routines")
	r := decodeData[routinesEvent](t, testEnvelope{Data: ev.Data})
	if !r.On || r.Active != "demo-sqlite" || r.Routines == nil || len(r.Routines) != 0 {
		t.Errorf("routines event: %+v", r)
	}
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.ShowRoutines || st.Routines == nil {
		t.Errorf("state: on %v, list %+v", st.ShowRoutines, st.Routines)
	}

	e.api("POST", "/api/v1/ws/"+id+"/ddl", `{"schema":"main","name":"nope","kind":"function","args":""}`, 409)

	e.api("POST", "/api/v1/ws/"+id+"/routines", `{"on":false}`, 200)
	ev, _ = s.await(t, "routines")
	if r := decodeData[routinesEvent](t, testEnvelope{Data: ev.Data}); r.On || r.Routines != nil {
		t.Errorf("off: %+v", r)
	}
}

// The sidebar's sections: Tables open and Views folded as a tab starts,
// each fold remembered for the tab (the state says so after a POST
// /section), an unknown section refused; Routines through /section is the
// routines switch, with its "routines" event. A view's DDL is asked for by
// name, and a table named as one is refused in words, not read.
func TestSidebarSections(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if !st.TablesOpen || st.ViewsOpen || st.ShowRoutines {
		t.Fatalf("as a tab starts: tables %v, views %v, routines %v", st.TablesOpen, st.ViewsOpen, st.ShowRoutines)
	}

	e.api("POST", "/api/v1/ws/"+id+"/section", `{"section":"views","open":true}`, 200)
	e.api("POST", "/api/v1/ws/"+id+"/section", `{"section":"tables","open":false}`, 200)
	st = decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st.TablesOpen || !st.ViewsOpen {
		t.Errorf("after the folds: tables %v, views %v", st.TablesOpen, st.ViewsOpen)
	}
	e.api("POST", "/api/v1/ws/"+id+"/section", `{"section":"indexes","open":true}`, 400)

	e.api("POST", "/api/v1/ws/"+id+"/section", `{"section":"routines","open":true}`, 200)
	ev, _ := s.await(t, "routines")
	if r := decodeData[routinesEvent](t, testEnvelope{Data: ev.Data}); !r.On || r.Routines == nil {
		t.Errorf("routines through /section: %+v", r)
	}

	e.api("POST", "/api/v1/ws/"+id+"/viewddl", `{"schema":"main","name":"cats"}`, 400)
	e.api("POST", "/api/v1/ws/"+id+"/viewddl", `{"schema":"main","name":"nope"}`, 400)
}
