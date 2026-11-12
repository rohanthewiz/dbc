package e2e

import (
	"path/filepath"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// introspectOff: a connection added with Introspect "off" connects without
// reading its catalog. The Tables list says so — neither "loading tables…"
// nor an empty list — with a refresh link, and the click on it lists the
// tables. The edit form opens on the saved level.
//
// The connection is lite2's file under another name, so there is a table
// (dogs) for the refresh to find. It is removed again at the end: the steps
// after this one count on the sidebar's connections as they were.
func introspectOff(t *testing.T, e *env, p *rod.Page) {
	waitConnected(t, p, "lite")
	p.MustElement("#conn-add").MustClick()
	waitFor(t, p, "the + menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Add a connection…")
	waitFor(t, p, "the form", `() => !!document.getElementById("cf-introspect")`)

	// the select offers the default and the three levels, on the default
	if got := evalStr(t, p, `() => {
	  const s = document.getElementById("cf-introspect");
	  return [...s.options].map((o) => o.value).join(",") + "|" + s.value;
	}`); got != ",full,names,off|" {
		t.Fatalf("the Introspect select = %q, want the default and three levels, on the default", got)
	}

	p.MustElement("#cf-driver").MustSelect("sqlite")
	p.MustElement("#cf-mode-f").MustClick()
	p.MustElement("#cf-name").MustInput("litequiet")
	p.MustElement("#cf-file").MustInput(filepath.Join(e.home, "lite2.db"))
	eval(t, p, `() => {
	  const s = document.getElementById("cf-introspect");
	  s.value = "off";
	  s.dispatchEvent(new Event("change", { bubbles: true }));
	}`)
	p.MustElementR(".modal button", "^Save$").MustClick()
	waitFor(t, p, "the form closed", `() => !document.querySelector(".modal")`)

	// connected, and told that nothing was read: one row, with the link
	waitFor(t, p, "the unread Tables list", `() => {
	  const li = [...document.querySelectorAll("#tables li")];
	  return dbc.state.active === "litequiet" && li.length === 1 &&
	    li[0].classList.contains("none") && li[0].textContent === "not read — refresh lists the tables" &&
	    !!li[0].querySelector("button.linkish") &&
	    !!document.querySelector('#conns .conn-item.active[data-conn="litequiet"][data-introspect="off"]');
	}`)
	// the connection works meanwhile: that is what off is for
	seq := gridSeq(t, p)
	eval(t, p, `() => { dbc.editor.setText("SELECT count(*) AS n FROM dogs"); dbc.cmd.run(); }`)
	waitResult(t, p, seq, "n")
	if n := evalStr(t, p, `() => String(document.querySelectorAll("#tables li.none").length)`); n != "1" {
		t.Fatalf("a run on the unread connection changed the Tables list (%s unread rows)", n)
	}

	p.MustElement("#tables li.none button.linkish").MustClick()
	waitFor(t, p, "dogs listed after the refresh", `() =>
	  [...document.querySelectorAll("#tables li")].some((li) => li.dataset.table === "dogs" || li.textContent.includes("dogs")) &&
	  !document.querySelector("#tables li.none button.linkish")`)

	// the edit form starts from the saved level
	rightClick(t, p, `#conns .conn-item[data-conn="litequiet"]`)
	menuPick(t, p, "Edit…")
	waitFor(t, p, "the edit form on off", `() => {
	  const s = document.getElementById("cf-introspect");
	  return !!s && s.value === "off";
	}`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the form closed", `() => !document.querySelector(".modal")`)

	// back to lite, and the connection gone again
	clickAt(t, p, `#conns .conn-item[data-conn="lite"]`, proto.InputMouseButtonLeft)
	waitConnected(t, p, "lite")
	eval(t, p, `() => dbc.api("DELETE", "/api/v1/conns/litequiet")`)
	waitFor(t, p, "litequiet removed", `() => !document.querySelector('#conns .conn-item[data-conn="litequiet"]')`)
}
