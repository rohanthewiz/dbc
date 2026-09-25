// dbc web — the connection list's own UI: adding a connection (with a test
// before saving), removing one added here, and redrawing the sidebar list
// when either happens in any window.
//
// The list itself is rendered by the server (pages/workbench.go) and only
// redrawn here after a change — draw builds the same markup, so keep the
// two in step. Connecting is app.js's (dbc.cmd.connect); a click on a
// connection goes there directly.
//
// What the server knows about a connection and the page does not: its DSN.
// The form sends one, and nothing ever sends one back.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;
  const conns = document.getElementById("conns");

  // Placeholders show each driver's DSN shape — the same examples as
  // dbc.example.toml. ${VAR} is expanded from dbc web's environment, which
  // keeps a password out of web.bytdb.
  const DRIVERS = [
    ["postgres", "postgres://user:${PGPASS}@localhost:5432/mydb?sslmode=disable"],
    ["mysql", "user:${MYSQL_PASS}@tcp(localhost:3306)/mydb?parseTime=true"],
    ["sqlite", "file:scratch.db"],
    ["bytdb", "notes.bytdb"],
  ];

  // ── the list ───────────────────────────────────────────────────────────
  // draw replaces the list with list (from GET /api/v1/conns or a "conns"
  // event). The active and connecting marks are app.js's; they are carried
  // across by name, so a redraw mid-connect does not lose "…".
  function draw(list) {
    const marks = new Map();
    for (const b of conns.querySelectorAll(".conn-item")) marks.set(b.dataset.conn, b.className);
    conns.replaceChildren();
    for (const c of list) {
      const b = el("button", { class: marks.get(c.name) || "conn-item", type: "button",
        "data-conn": c.name, "data-driver": c.driver,
        "data-saved": c.saved ? "1" : null, title: c.saved ? c.name + " — added here; right-click to remove" : null },
        el("span", "name", c.name), el("span", "driver", c.driver));
      conns.append(el("li", null, b));
    }
  }

  // ── adding ─────────────────────────────────────────────────────────────
  // openAdd shows the form. Test runs the DSN through POST /conns/test and
  // says what happened in the result box; Save adds it (whether or not it
  // was tested — a database that is down right now may still be worth
  // adding) and switches the query tab to it.
  function openAdd() {
    const name = el("input", { type: "text", id: "cf-name", autocomplete: "off", spellcheck: "false",
      maxlength: "64", placeholder: "prod-reports" });
    const driver = el("select", { id: "cf-driver" });
    for (const [d] of DRIVERS) driver.append(el("option", { value: d }, d));
    // a text field, not a password one: a DSN is a URL to read and fix, and
    // a password need not be in it at all — ${VAR} keeps it in the env
    const dsn = el("input", { type: "text", id: "cf-dsn", class: "mono", autocomplete: "off",
      spellcheck: "false", autocapitalize: "off" });
    const aiRows = el("input", { type: "checkbox", id: "cf-airows" });
    const result = el("div", { class: "connresult", "aria-live": "polite", hidden: "hidden" });

    const placeholder = () => { dsn.placeholder = DRIVERS.find(([d]) => d === driver.value)[1]; };
    driver.addEventListener("change", placeholder);
    placeholder();

    const form = el("div", "connform",
      el("label", { for: "cf-name" }, "Name"), name,
      el("label", { for: "cf-driver" }, "Driver"), driver,
      el("label", { for: "cf-dsn" }, "DSN"), dsn,
      el("span", "hint full", "${VAR} is read from dbc web's environment — the way to keep a password out of the saved file."),
      el("span"), el("label", { class: "check full", title: "The assistant may see up to ai_context_rows result rows " +
        "from this connection. Off by default: rows are the database's contents." }, aiRows, "Let the assistant see result rows"),
    );

    const test = el("button", { type: "button" }, "Test connection");
    const save = el("button", { type: "button", class: "primary" }, "Save");
    const cancel = el("button", { type: "button" }, "Cancel");
    cancel.addEventListener("click", () => dbc.modal.close());

    const body = () => ({ name: name.value, driver: driver.value, dsn: dsn.value, ai_rows: aiRows.checked });

    function say(level, text) {
      result.hidden = false;
      result.className = "connresult " + level;
      result.textContent = text;
    }

    // busy disables the buttons while a request is out, so a double click
    // cannot save twice or race two tests' answers into the box
    function busy(on) { test.disabled = save.disabled = on; }

    // Each test gets a number; an answer that is not the latest one's is
    // dropped, so editing the DSN and testing again never shows the old
    // DSN's verdict last.
    let seq = 0;
    async function runTest() {
      const n = ++seq;
      busy(true);
      say("", "connecting…");
      try {
        const r = await dbc.api("POST", "/api/v1/conns/test", body());
        if (n !== seq) return;
        const warns = (r.warnings || []).join("\n");
        if (!r.ok) say("err", "✗ " + r.error + (warns ? "\n" + warns : ""));
        else if (r.note) say("warn", "✓ " + r.note + (warns ? "\n" + warns : ""));
        else say(warns ? "warn" : "ok", "✓ connected in " + r.ms + " ms" + (warns ? "\n" + warns : ""));
      } catch (e) {
        if (n === seq) say("err", e.message); // the form itself was refused (400)
      } finally {
        if (n === seq) busy(false);
      }
    }

    async function runSave() {
      seq++; // a test still out must not overwrite what the save says
      busy(true);
      try {
        const r = await dbc.api("POST", "/api/v1/conns", body());
        draw(r.conns); // the "conns" event will say the same; this is sooner
        const added = name.value.trim();
        dbc.modal.close();
        dbc.log("ok", "added connection " + added);
        for (const w of r.warnings || []) dbc.log("warn", w);
        dbc.cmd.connect(added);
      } catch (e) {
        say("err", e.message);
        busy(false);
      }
    }
    test.addEventListener("click", runTest);
    save.addEventListener("click", runSave);

    dbc.modal.open({
      title: "Add a connection", focus: name,
      body: el("div", null, form, result),
      foot: el("div", "mfoot", test, el("span", "hint", ""), save, cancel),
      // Enter saves from any field, as a form would; Ctrl/⌘+Enter tests.
      // A select's Enter opens its list, so it is left alone there.
      onKey: (e) => {
        if (e.key !== "Enter" || e.target === driver || test.disabled) return false;
        if (e.ctrlKey || e.metaKey) runTest();
        else runSave();
        return true;
      },
      onClose: () => { seq++; dbc.editor.focus(); },
    });
  }

  // ── removing ───────────────────────────────────────────────────────────
  // Only a connection added here can go; the server refuses the rest, and
  // one a query tab is on (it says how many). Asked first: the DSN is
  // gone with it, and it may have taken some finding.
  function confirmRemove(name) {
    const yes = el("button", { type: "button", class: "primary" }, "Remove");
    const no = el("button", { type: "button" }, "Keep it");
    no.addEventListener("click", () => dbc.modal.close());
    yes.addEventListener("click", async () => {
      dbc.modal.close();
      try {
        const r = await dbc.api("DELETE", "/api/v1/conns/" + encodeURIComponent(name));
        draw(r.conns);
        dbc.log("ok", "removed connection " + name);
      } catch (e) {
        dbc.log(e.status === 409 ? "warn" : "err", "could not remove " + name + ": " + e.message);
      }
    });
    dbc.modal.open({
      title: "Remove " + name + "?", focus: no,
      body: el("div", "confirm", el("p", null, "This forgets the connection and its DSN. " +
        "Nothing in the database changes; add it again any time.")),
      foot: el("div", "mfoot", yes, no),
    });
  }

  // The right-click menu: connect, or remove one added here. For the
  // config file's, Remove is shown and says why it cannot, rather than
  // being absent and leaving the user to wonder where it went.
  conns.addEventListener("contextmenu", (e) => {
    const b = e.target.closest(".conn-item");
    if (!b) return;
    e.preventDefault();
    const name = b.dataset.conn;
    dbc.menu.open(e.clientX, e.clientY, [
      { head: name },
      { label: "Connect", act: () => dbc.cmd.connect(name) },
      b.dataset.saved
        ? { label: "Remove…", act: () => confirmRemove(name) }
        : { label: "Remove…", why: name + " comes from the config file (or is a built-in demo) — edit the file to remove it" },
      { head: "" },
      { label: "Add a connection…", act: openAdd },
    ]);
  });

  document.getElementById("conn-add").addEventListener("click", openAdd);

  dbc.conns = { draw, openAdd };
})();
