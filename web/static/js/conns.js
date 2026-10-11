// dbc web — the connection list's own UI: adding a connection (with a test
// before saving), editing or removing one added here, and redrawing the
// sidebar list when any of these happens in any window.
//
// The list itself is rendered by the server (pages/workbench.go) and only
// redrawn here after a change — draw builds the same markup, so keep the
// two in step. Connecting is app.js's (dbc.cmd.connect); a click on a
// connection goes there directly.
//
// What the server knows about a connection and the page does not: its DSN.
// The form sends one, and nothing ever sends one back — so the edit form
// starts with the DSN field empty, and an empty DSN there means "leave it
// as it is" (the server fills in the stored one).
//
// The form takes a connection two ways, switched by "Enter as":
//
//   Fields  host, port, user, password, database, options (a file for the
//           embedded engines) — sent as parts; the server writes the DSN
//           (db.BuildDSN), so the escaping is never the user's problem
//   DSN     the whole DSN as text, as before
//
// Editing, the fields come from GET /conns/:name/parts — the stored DSN
// taken apart, password withheld. Fields left untouched send no parts at
// all, only "keep the DSN": rebuilt from the same fields the DSN would come
// out in another spelling, and the server would see a change that would
// reconnect — and be refused while a tab is on the connection. A password
// left empty over a withheld one is kept (keep_password).
//
// TLS (postgres and mysql) is its own section, sent beside either way of
// giving the DSN: the mode, and the CA / client certificate / key files.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;
  const conns = document.getElementById("conns");

  // Placeholders show each driver's DSN shape — the same examples as
  // dbc.example.toml. ${VAR} is expanded from the environment of each dbc
  // that reads connections.toml, which keeps a password out of the file.
  const DRIVERS = [
    ["postgres", "postgres://user:${PGPASS}@localhost:5432/mydb?sslmode=disable"],
    ["mysql", "user:${MYSQL_PASS}@tcp(localhost:3306)/mydb?parseTime=true"],
    ["sqlite", "file:scratch.db"],
    ["bytdb", "notes.bytdb"],
  ];

  // Defaults shown as placeholders in the fields: what the server engines
  // listen on, and an example of each one's options.
  const FIELD_HINTS = {
    postgres: { port: "5432", options: "application_name=dbc connect_timeout=10" },
    mysql: { port: "3306", options: "parseTime=true loc=Local" },
    sqlite: { file: "scratch.db", options: "mode=memory cache=shared" },
    bytdb: { file: "notes.bytdb" },
  };
  const isServer = (d) => d === "postgres" || d === "mysql";

  // TLS modes, libpq's names (config/tls.go), with what each one checks. ""
  // leaves TLS to the DSN — how every connection behaved before the setting.
  const TLS_MODES = [
    ["", "from the DSN"],
    ["disable", "disable — no TLS"],
    ["prefer", "prefer — TLS if offered, unchecked"],
    ["require", "require — TLS, certificate unchecked"],
    ["verify-ca", "verify-ca — certificate signed by the CA"],
    ["verify-full", "verify-full — CA and host name (safest)"],
  ];

  // ── the list ───────────────────────────────────────────────────────────
  // draw replaces the list with list (from GET /api/v1/conns or a "conns"
  // event). The active, connecting and in-use marks are app.js's; they are
  // carried across by name, so a redraw mid-connect does not lose "…" (the
  // in-use tooltip is not, and app.js redraws it after each draw).
  function draw(list) {
    const marks = new Map();
    for (const b of conns.querySelectorAll(".conn-item")) marks.set(b.dataset.conn, b.className);
    conns.replaceChildren();
    for (const c of list) {
      const b = el("button", { class: marks.get(c.name) || "conn-item", type: "button",
        "data-conn": c.name, "data-driver": c.driver,
        "data-saved": c.saved ? "1" : null, "data-airows": c.ai_rows ? "1" : null,
        "data-tls": c.tls || null, "data-tls-ca": c.tls_ca || null,
        "data-tls-cert": c.tls_cert || null, "data-tls-key": c.tls_key || null,
        "data-tls-key-password": c.tls_key_password || null,
        title: c.saved ? c.name + " — added here; right-click to edit or remove" : null },
        el("span", "name", c.name), el("span", "driver", c.driver));
      conns.append(el("li", null, b));
    }
  }

  // ── adding and editing ─────────────────────────────────────────────────
  // openForm shows the form: empty to add a connection, or filled from cur
  // ({name, driver, aiRows} — what the sidebar knows) to edit one added
  // here. Test runs the DSN through POST /conns/test and says what happened
  // in the result box; Save adds it (whether or not it was tested — a
  // database that is down right now may still be worth adding) and switches
  // the query tab to it, or saves the edit (PUT /conns/:name) and leaves
  // every tab where it is.
  //
  // Editing, the DSN field starts empty: the page never has the DSN. Left
  // empty it keeps the stored one — for a test too, which sends from: the
  // connection's name so the server can find it. A changed driver cannot
  // keep a DSN written for the old one, so the placeholder then asks for a
  // new DSN instead (and the server refuses one left empty).
  function openForm(cur) {
    const input = (id, attrs) => el("input", Object.assign({ type: "text", id, autocomplete: "off",
      spellcheck: "false", autocapitalize: "off" }, attrs || {}));
    const name = input("cf-name", { maxlength: "64", placeholder: "prod-reports" });
    const driver = el("select", { id: "cf-driver" });
    for (const [d] of DRIVERS) driver.append(el("option", { value: d }, d));

    // "Enter as": fields, or the DSN as text
    const asFields = el("input", { type: "radio", name: "cf-mode", id: "cf-mode-f", checked: "checked" });
    const asDSN = el("input", { type: "radio", name: "cf-mode", id: "cf-mode-d" });
    const modeRow = el("span", "check full",
      el("label", { class: "check" }, asFields, "Fields"),
      el("label", { class: "check" }, asDSN, "DSN"));

    // the fields. A password field proper (masked), unlike the DSN's text
    // one: here it holds nothing else to read and fix.
    const host = input("cf-host", { placeholder: "localhost" });
    const port = input("cf-port", { inputmode: "numeric", size: "6", class: "port" });
    const user = input("cf-user");
    const password = el("input", { type: "password", id: "cf-password", autocomplete: "new-password",
      placeholder: "or ${VAR} from the environment" });
    const database = input("cf-database");
    const file = input("cf-file", { class: "mono" });
    const options = input("cf-options", { class: "mono" });
    // a text field, not a password one: a DSN is a URL to read and fix, and
    // a password need not be in it at all — ${VAR} keeps it in the env
    const dsn = input("cf-dsn", { class: "mono" });

    // TLS
    const tls = el("select", { id: "cf-tls" });
    for (const [v, label] of TLS_MODES) tls.append(el("option", { value: v }, label));
    const tlsCA = input("cf-tls-ca", { class: "mono", placeholder: "~/certs/ca.pem — blank: the system's trusted CAs" });
    const tlsCert = input("cf-tls-cert", { class: "mono", placeholder: "only if the server asks for a client certificate" });
    const tlsKey = input("cf-tls-key", { class: "mono", placeholder: "the client certificate's private key" });
    // a text field, not a password one: it holds the NAME of an environment
    // variable (${MY_KEY_PASS}), never the passphrase — the server refuses
    // anything else (config.TLSOpts.Check), as the file it lands in is
    // plain text and the value comes back to every window
    const tlsKeyPass = input("cf-tls-key-password", { class: "mono",
      placeholder: "${VAR} holding an encrypted key's passphrase — blank if it is not encrypted" });

    const aiRows = el("input", { type: "checkbox", id: "cf-airows" });
    const result = el("div", { class: "connresult", "aria-live": "polite", hidden: "hidden" });

    // Each row is its label and its control, kept together so a group can
    // be shown or hidden as one; show() below decides which are visible.
    const row = (text, ctl) => [el("label", { for: ctl.id }, text), ctl];
    const hostPort = el("span", "hostport", host, el("label", { for: "cf-port" }, "Port"), port);
    const rows = {
      mode: [el("label", null, "Enter as"), modeRow],
      host: [el("label", { for: "cf-host" }, "Host"), hostPort],
      user: row("User", user),
      password: row("Password", password),
      database: row("Database", database),
      file: row("File", file),
      options: row("Options", options),
      dsn: row("DSN", dsn),
      dsnHint: [el("span"), el("span", "hint full", "${VAR} is read from dbc web's environment — the way to keep a password out of the saved file.")],
      tls: row("TLS", tls),
      tlsCA: row("CA file", tlsCA),
      tlsCert: row("Client cert", tlsCert),
      tlsKey: row("Client key", tlsKey),
      tlsKeyPass: row("Key password", tlsKeyPass),
      tlsHint: [el("span"), el("span", "hint full", "PEM files. ~ and ${VAR} work; a relative path is relative to ~/.config/dbc. " +
        "An encrypted key's passphrase is read from the ${VAR} under Key password, never typed here.")],
    };

    // hasPassword: editing, the stored DSN has a password the page was not
    // given; an empty password field then keeps it. dirty: a field was
    // touched since they were filled (see the top of the file).
    let hasPassword = false;
    let dirty = !cur;

    if (cur) {
      name.value = cur.name;
      driver.value = cur.driver;
      aiRows.checked = cur.aiRows;
      tls.value = cur.tls || "";
      tlsCA.value = cur.tlsCA || "";
      tlsCert.value = cur.tlsCert || "";
      tlsKey.value = cur.tlsKey || "";
      tlsKeyPass.value = cur.tlsKeyPassword || "";
    }

    const fieldMode = () => asFields.checked;

    function show() {
      const d = driver.value;
      const server = isServer(d);
      const tlsFiles = server && tls.value !== "" && tls.value !== "disable";
      const visible = {
        mode: true,
        host: fieldMode() && server, user: fieldMode() && server, password: fieldMode() && server,
        database: fieldMode() && server,
        file: fieldMode() && !server,
        options: fieldMode() && d !== "bytdb",
        dsn: !fieldMode(), dsnHint: true,
        tls: server, tlsCA: tlsFiles, tlsCert: tlsFiles, tlsKey: tlsFiles, tlsKeyPass: tlsFiles,
        tlsHint: tlsFiles,
      };
      for (const k in rows) for (const e of rows[k]) e.hidden = !visible[k];

      const h = FIELD_HINTS[d];
      port.placeholder = h.port || "";
      file.placeholder = h.file || "";
      options.placeholder = h.options || "";
      password.placeholder = hasPassword ? "unchanged — type to replace it" : "or ${VAR} from the environment";
      dsn.placeholder = cur && driver.value === cur.driver
        ? "unchanged — type a DSN to replace it"
        : DRIVERS.find(([x]) => x === d)[1];
    }
    for (const c of [driver, tls, asFields, asDSN]) c.addEventListener("change", show);
    for (const c of [driver, host, port, user, password, database, file, options]) {
      c.addEventListener("input", () => { dirty = true; });
      c.addEventListener("change", () => { dirty = true; });
    }

    // Editing: fill the fields from the stored DSN, or — when the fields
    // cannot hold it (several hosts, a unix socket) — open on the DSN text
    // and say why.
    if (cur) {
      asDSN.checked = true; // until the parts arrive, or if they cannot
      dbc.api("GET", "/api/v1/conns/" + encodeURIComponent(cur.name) + "/parts").then((r) => {
        // the TLS settings as saved (${VAR}s, relative paths), in place of
        // the resolved ones the sidebar's entry carried
        const t = r.tls || {};
        tls.value = t.tls || ""; tlsCA.value = t.tls_ca || "";
        tlsCert.value = t.tls_cert || ""; tlsKey.value = t.tls_key || "";
        tlsKeyPass.value = t.tls_key_password || "";
        if (!r.parts) {
          say("", "This DSN is edited as text: " + r.reason);
          return;
        }
        const p = r.parts;
        host.value = p.host || ""; port.value = p.port || ""; user.value = p.user || "";
        password.value = p.password || ""; database.value = p.database || "";
        file.value = p.file || ""; options.value = p.options || "";
        hasPassword = !!r.has_password;
        asFields.checked = true;
        dirty = false;
        show();
      }).catch((e) => say("err", "could not read the connection's fields: " + e.message)).finally(show);
    }
    show();

    const form = el("div", "connform",
      ...row("Name", name), ...row("Driver", driver),
      ...rows.mode, ...rows.host, ...rows.user, ...rows.password, ...rows.database,
      ...rows.file, ...rows.options, ...rows.dsn, ...rows.dsnHint,
      ...rows.tls, ...rows.tlsCA, ...rows.tlsCert, ...rows.tlsKey, ...rows.tlsKeyPass, ...rows.tlsHint,
      el("span"), el("label", { class: "check full", title: "The assistant may see up to ai_context_rows result rows " +
        "from this connection. Off by default: rows are the database's contents." }, aiRows, "Let the assistant see result rows"),
    );

    const test = el("button", { type: "button" }, "Test connection");
    const save = el("button", { type: "button", class: "primary" }, "Save");
    const cancel = el("button", { type: "button" }, "Cancel");
    cancel.addEventListener("click", () => dbc.modal.close());

    // body is the request, for a test or a save. In fields mode, untouched
    // fields of an edit send an empty DSN ("keep it") rather than parts —
    // see the top of the file. The TLS settings go only for a server
    // engine: the server refuses them for a local file.
    function body() {
      const b = { name: name.value, driver: driver.value, dsn: dsn.value, ai_rows: aiRows.checked,
        from: cur ? cur.name : undefined };
      if (fieldMode()) {
        b.dsn = "";
        if (dirty) {
          b.parts = { host: host.value, port: port.value, user: user.value, password: password.value,
            database: database.value, file: file.value, options: options.value };
          b.keep_password = hasPassword && password.value === "";
        }
      }
      if (isServer(driver.value) && tls.value) {
        b.tls = tls.value;
        if (tls.value !== "disable") {
          b.tls_ca = tlsCA.value; b.tls_cert = tlsCert.value; b.tls_key = tlsKey.value;
          b.tls_key_password = tlsKeyPass.value;
        }
      }
      return b;
    }

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
        if (cur) {
          // from rides along harmlessly: the path names the connection
          const r = await dbc.api("PUT", "/api/v1/conns/" + encodeURIComponent(cur.name), body());
          draw(r.conns);
          if (r.renamed) dbc.cmd.connRenamed(r.renamed.from, r.renamed.to);
          dbc.modal.close();
          dbc.log("ok", r.renamed ? "renamed connection " + r.renamed.from + " to " + r.renamed.to
            : "updated connection " + cur.name);
          for (const w of r.warnings || []) dbc.log("warn", w);
          return;
        }
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
      title: cur ? "Edit " + cur.name : "Add a connection", focus: name,
      body: el("div", "connbody", form, result),
      foot: el("div", "mfoot", test, el("span", "hint", ""), save, cancel),
      // Enter saves from any field, as a form would; Ctrl/⌘+Enter tests.
      // A select's Enter opens its list, so it is left alone there.
      onKey: (e) => {
        if (e.key !== "Enter" || e.target.tagName === "SELECT" || test.disabled) return false;
        if (e.ctrlKey || e.metaKey) runTest();
        else runSave();
        return true;
      },
      onClose: () => { seq++; dbc.editor.focus(); },
    });
  }

  const openAdd = () => openForm(null);

  // openEdit edits the sidebar entry b — a connection added here.
  const openEdit = (b) => openForm({ name: b.dataset.conn, driver: b.dataset.driver, aiRows: !!b.dataset.airows,
    tls: b.dataset.tls, tlsCA: b.dataset.tlsCa, tlsCert: b.dataset.tlsCert, tlsKey: b.dataset.tlsKey,
    tlsKeyPassword: b.dataset.tlsKeyPassword });

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

  // ── Postgres in Docker ─────────────────────────────────────────────────
  // openPGDocker is the Connections menu's "Postgres in Docker…": a
  // dropdown of the supported PostgreSQL majors (the server's list, which
  // drops a version at its end of life), each with the state of dbc's
  // container for it. Start brings that container up — pulled, created or
  // restarted (package pgdocker) — and adds its connection, which the tab
  // then connects to, as after a saved form.
  //
  //   open ─► GET /pgdocker ─► Docker usable? fill the dropdown : say why
  //   Start ─► POST /pgdocker {major} ─► ok: redraw, log the steps, connect
  //                                  └► not ok: docker's words in the box
  //
  // The POST answers once the server accepts connections, a minute or more
  // when the image is downloaded first, so the box says that while it waits.
  function openPGDocker() {
    const pick = el("select", { id: "pg-version", disabled: "disabled" });
    const result = el("div", { class: "connresult", "aria-live": "polite", hidden: "hidden" });
    const start = el("button", { type: "button", class: "primary", disabled: "disabled" }, "Start");
    const cancel = el("button", { type: "button" }, "Cancel");
    cancel.addEventListener("click", () => dbc.modal.close());

    function say(level, text) {
      result.hidden = false;
      result.className = "connresult " + level;
      result.textContent = text;
    }

    // ready: Docker answered and there is a version to start; open: the
    // dialog is still up (an answer after Cancel is dropped)
    let ready = false, open = true;
    const busy = (on) => { start.disabled = on || !ready; pick.disabled = on || !ready; };

    say("", "asking Docker…");
    dbc.api("GET", "/api/v1/pgdocker").then((r) => {
      if (!open) return;
      if (!r.ok) {
        say("err", r.error);
        return;
      }
      r.versions.forEach((v, i) => {
        // a version whose support ends within the year says so, as a
        // reason to prefer a newer one
        let label = "PostgreSQL " + v.major;
        if (i === 0) label += " (newest)";
        else if (new Date(v.eol) - Date.now() < 365 * 864e5) {
          label += " (support ends " + new Date(v.eol).toLocaleDateString(undefined, { month: "short", year: "numeric" }) + ")";
        }
        if (v.status) label += " — " + v.status;
        pick.append(el("option", { value: v.major }, label));
      });
      ready = r.versions.length > 0;
      result.hidden = true;
      busy(false);
      pick.focus();
    }).catch((e) => { if (open) say("err", e.message); });

    async function run() {
      if (start.disabled) return;
      const major = pick.value;
      busy(true);
      say("", "Starting PostgreSQL " + major + "… A version's first start downloads its image " +
        "(about 150 MB), which can take a minute or two.");
      try {
        const r = await dbc.api("POST", "/api/v1/pgdocker", { major });
        for (const st of r.steps || []) dbc.log("info", st);
        if (!r.ok) {
          if (open) { say("err", r.error); busy(false); }
          else dbc.log("err", r.error);
          return;
        }
        draw(r.conns); // the "conns" event will say the same; this is sooner
        if (open) dbc.modal.close();
        const what = r.added ? "added connection " + r.name
          : r.updated ? "updated connection " + r.name + " to the new container" : "connection " + r.name;
        dbc.log("ok", "PostgreSQL " + major + (r.created ? " started" : " running") + " in container " +
          r.container + " on 127.0.0.1:" + r.port + " — " + what);
        for (const w of r.warnings || []) dbc.log("warn", w);
        if (r.created) dbc.log("info", "its data is kept in the Docker volume " + r.volume +
          "; stop it with: docker stop " + r.container);
        dbc.cmd.connect(r.name);
      } catch (e) {
        if (open) { say("err", e.message); busy(false); }
        else dbc.log("err", "Postgres in Docker: " + e.message);
      }
    }
    start.addEventListener("click", run);

    dbc.modal.open({
      title: "Postgres in Docker", focus: cancel,
      body: el("div", "connbody",
        el("div", "connform",
          el("label", { for: "pg-version" }, "Version"), pick,
          el("span"), el("span", "hint full", "Runs in a container on 127.0.0.1, with its data in a Docker " +
            "volume, and adds a connection to it.")),
        result),
      foot: el("div", "mfoot", el("span", "hint", ""), start, cancel),
      // Enter starts, from anywhere but the open dropdown's own list
      onKey: (e) => {
        if (e.key !== "Enter" || e.target.tagName === "SELECT") return false;
        run(false);
        return true;
      },
      // closing does not stop a start already sent: the server finishes it,
      // and run() logs the outcome instead of showing it here
      onClose: () => { open = false; dbc.editor.focus(); },
    });
  }

  // stopPGDocker stops the container behind name, a connection Postgres in
  // Docker made (POST /pgdocker/stop). The server refuses while a query tab
  // in any window is on it; the menu row already says so for this tab's.
  async function stopPGDocker(name, container) {
    dbc.log("info", "stopping container " + container + "…");
    try {
      const r = await dbc.api("POST", "/api/v1/pgdocker/stop", { conn: name });
      if (!r.ok) dbc.log("err", "could not stop " + r.container + ": " + r.error);
      else if (!r.was_running) dbc.log("info", "container " + r.container + " was not running");
      else dbc.log("ok", "stopped container " + r.container + " — Postgres in Docker… starts it again, its data intact");
    } catch (e) {
      dbc.log(e.status === 409 ? "warn" : "err", "could not stop " + container + ": " + e.message);
    }
  }

  // ── Dump database ──────────────────────────────────────────────────────
  // openDump is the Connections menu's "Dump database…" on a Postgres
  // connection: pg_dump run the way `dbc dump` runs it (package pgdump on
  // the server — the checks, the connection, the pg_dump picked).
  //
  //   open ─► GET /dump?conn=X ─► the suggested path (Downloads, named for
  //                               the connection and the minute)
  //   Dump ─► POST /dump {conn, …fields} ─► refused: said in the box
  //                                      └► started: the dialog closes, and
  //   the dump reports on the event stream ("dump" events, onDump): each
  //   line to the log, and whether one is running, for the menu's rows.
  //
  // The POST answers once the dump has started, not when it ends: a dump
  // can take hours, and the stream outlives a reload. One runs at a time,
  // across windows; the menu offers Stop while it does.
  //
  // The path is on the machine dbc web runs on; ~ is its home directory.
  const PG_DRIVERS = new Set(["postgres", "postgresql", "pg", "pgx"]);
  const DUMP_FORMATS = [
    ["plain", "plain — one SQL file, for psql", ".sql"],
    ["custom", "custom — one archive, for pg_restore", ".dump"],
    ["directory", "directory — a file per table, for pg_restore (parallel)", "-dir"],
    ["tar", "tar — one tar archive, for pg_restore", ".tar"],
    ["split", "split — SQL, a file per table, and a restore.sql for psql", "-sql"],
  ];
  const suffixOf = (f) => DUMP_FORMATS.find(([x]) => x === f)[2];
  const isArchive = (f) => f === "custom" || f === "directory" || f === "tar";
  const toDir = (f) => f === "directory" || f === "split";

  // dumping is the dump running now ({conn, out, format, started}), or
  // null — the server's word, from GET /dump at load and "dump" events.
  let dumping = null;
  dbc.api("GET", "/api/v1/dump").then((r) => { dumping = r.running || null; }).catch(() => {});

  // sizeText is a byte count as pgdump.HumanSize says it: "512 B", "3.4 MB".
  function sizeText(n) {
    if (n < 1024) return n + " B";
    let i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < 4);
    return n.toFixed(1) + " " + "KMGTP"[i] + "B";
  }

  // downloads are the tokens of dumps this page asked to download (the
  // dialog's ⤓ Download): the "dump" event's ready names one when its file
  // is on the server, and this page — not another window — fetches it.
  const downloads = new Set();

  // onDump takes a "dump" event: a line for the log, the running state, or
  // a download ready to fetch.
  function onDump(d) {
    if ("running" in d) dumping = d.running || null;
    if (d.text) dbc.log(d.level, d.text);
    if (d.ready && downloads.delete(d.ready)) {
      // a navigation, not a fetch: the browser saves it as it comes,
      // without the page holding the whole dump
      const a = el("a", { href: "/api/v1/dump/file/" + encodeURIComponent(d.ready), download: d.name || "" });
      document.body.append(a);
      a.click();
      a.remove();
      dbc.log("ok", "⤓ downloading " + d.name + " (" + sizeText(d.size || 0) + ") — the browser saves it");
    }
  }

  function openDump(name) {
    const input = (id, attrs) => el("input", Object.assign({ type: "text", id, autocomplete: "off",
      spellcheck: "false", autocapitalize: "off" }, attrs || {}));
    const format = el("select", { id: "dp-format" });
    for (const [f, label] of DUMP_FORMATS) format.append(el("option", { value: f }, label));
    const out = input("dp-out", { class: "mono", placeholder: "~/Downloads/" + name + ".sql" });
    const jobs = input("dp-jobs", { inputmode: "numeric", placeholder: "1 — tables dumped at once" });
    const content = el("select", { id: "dp-content" },
      el("option", { value: "" }, "everything"), el("option", { value: "schema" }, "schema only"),
      el("option", { value: "data" }, "data only"));
    const schemas = input("dp-schemas", { class: "mono", placeholder: "all — or patterns: public, sales*" });
    const tables = input("dp-tables", { class: "mono", placeholder: "all — or patterns: orders, sales.*" });
    const exclTables = input("dp-excl-tables", { class: "mono", placeholder: "none" });
    const exclData = input("dp-excl-data", { class: "mono", placeholder: "none — tables kept without their rows" });
    const extra = input("dp-extra", { class: "mono", placeholder: "other pg_dump options, e.g. --no-comments" });
    const box = (id, text, title) => {
      const c = el("input", { type: "checkbox", id });
      return { c, label: el("label", { class: "check", title }, c, text) };
    };
    const noOwner = box("dp-no-owner", "no owners", "no ALTER … OWNER: objects belong to whoever restores");
    const noPriv = box("dp-no-priv", "no grants", "no GRANT / REVOKE");
    const inserts = box("dp-inserts", "INSERTs, not COPY", "slower to restore, but readable by more than Postgres");
    const create = box("dp-create", "CREATE DATABASE first", "the dump makes the database and connects to it");
    const clean = box("dp-clean", "DROP before creating", "drop each object (with -t split: the database) first");
    const result = el("div", { class: "connresult", "aria-live": "polite", hidden: "hidden" });
    const go = el("button", { type: "button", class: "primary" }, "Dump");
    // ⤓ Download: the same dump, written to a temp file of the server and
    // then saved by this browser — for a dbc web serving another machine
    // (web/dump.go DOWNLOADING IT); single-file formats only
    const dl = el("button", { type: "button", title: "Dump it, then save it here, in this browser's downloads " +
      "(plain, custom or tar; up to 512 MB)" }, "⤓ Download");
    const cancel = el("button", { type: "button" }, "Cancel");
    cancel.addEventListener("click", () => dbc.modal.close());

    function say(level, text) {
      result.hidden = false;
      result.className = "connresult " + level;
      result.textContent = text;
    }

    const rowOf = (text, ctl) => [el("label", { for: ctl.id }, text), ctl];
    const jobsRow = rowOf("Jobs", jobs);
    const archiveOnly = [noOwner.label, create.label, clean.label];
    const outLabel = el("label", { for: "dp-out" }, "To file");
    const what = el("span", "hint full", "");

    // show follows the format: the directory formats take a directory and
    // jobs; the archive formats leave owner, create and clean to
    // pg_restore, so those boxes go (and are not sent)
    let prevFormat = format.value;
    function show() {
      const f = format.value;
      for (const e of jobsRow) e.hidden = !toDir(f);
      for (const e of archiveOnly) e.hidden = isArchive(f);
      outLabel.textContent = toDir(f) ? "To directory" : "To file";
      dl.hidden = toDir(f);
      what.textContent = toDir(f) ? "a directory that does not exist yet, or is empty" : "";
      // a suggested name follows the format; one typed by hand stays
      const old = suffixOf(prevFormat);
      if (out.value.endsWith(old)) out.value = out.value.slice(0, -old.length) + suffixOf(f);
      prevFormat = f;
    }
    format.addEventListener("change", show);

    let open = true;
    dbc.api("GET", "/api/v1/dump?conn=" + encodeURIComponent(name)).then((r) => {
      if (open && !out.value) { out.value = r.out || ""; prevFormat = "plain"; show(); }
      if (r.running) say("warn", "a dump of " + r.running.conn + " is still running — one at a time");
    }).catch((e) => { if (open) say("err", e.message); });

    async function run(download) {
      if (go.disabled) return;
      const f = format.value;
      const body = {
        download: download === true,
        conn: name, format: f, out: out.value, jobs: toDir(f) ? jobs.value : "", content: content.value,
        schemas: schemas.value, tables: tables.value, exclude_tables: exclTables.value, exclude_data: exclData.value,
        no_privileges: noPriv.c.checked, inserts: inserts.c.checked, extra: extra.value,
        no_owner: !isArchive(f) && noOwner.c.checked, create: !isArchive(f) && create.c.checked,
        clean: !isArchive(f) && clean.c.checked,
      };
      go.disabled = dl.disabled = true;
      say("", "checking the server's version and finding pg_dump…");
      try {
        const r = await dbc.api("POST", "/api/v1/dump", body);
        if (!r.ok) { if (open) say("err", r.error); return; }
        if (r.token) downloads.add(r.token); // fetched when its "dump" event says ready
        if (open) dbc.modal.close(); // the dump's own lines take it from here
      } catch (e) {
        if (open) say(e.status === 409 ? "warn" : "err", e.message);
      } finally {
        go.disabled = dl.disabled = false;
      }
    }
    go.addEventListener("click", () => run(false));
    dl.addEventListener("click", () => run(true));

    show();
    dbc.modal.open({
      title: "Dump " + name, focus: out,
      body: el("div", "connbody",
        el("div", "connform",
          ...rowOf("Format", format),
          outLabel, out, el("span"), what,
          ...jobsRow,
          ...rowOf("What", content),
          ...rowOf("Schemas", schemas), ...rowOf("Tables", tables),
          ...rowOf("Skip tables", exclTables), ...rowOf("Skip data of", exclData),
          el("label", null, "Options"),
          el("span", "check full", noPriv.label, inserts.label, noOwner.label),
          el("span"), el("span", "check full", create.label, clean.label),
          ...rowOf("More options", extra),
          el("span"), el("span", "hint full", "Dump writes it on the machine dbc web runs on (~ is its home); " +
            "⤓ Download saves it in this browser instead. Runs pg_dump — found on PATH, in Homebrew's libpq, " +
            "at the config's pg_bin, or the server's own in Docker.")),
        result),
      foot: el("div", "mfoot", el("span", "hint", ""), dl, go, cancel),
      onKey: (e) => {
        if (e.key !== "Enter" || e.target.tagName === "SELECT") return false;
        run(false);
        return true;
      },
      onClose: () => { open = false; dbc.editor.focus(); },
    });
  }

  // stopDump interrupts the running dump; its end arrives as events.
  async function stopDump() {
    try {
      const r = await dbc.api("POST", "/api/v1/dump/stop", {});
      if (!r.ok) dbc.log("info", r.error);
      else dbc.log("info", "stopping the dump of " + r.conn + "…");
    } catch (e) {
      dbc.log("err", "could not stop the dump: " + e.message);
    }
  }

  // dumpItems are the menu's dump rows for the connection name: Dump
  // database…, dimmed with the reason where it cannot run, and Stop while
  // a dump is running.
  function dumpItems(b, name) {
    const label = "Dump database…";
    if (!PG_DRIVERS.has((b.dataset.driver || "").toLowerCase())) {
      return [{ label, why: "pg_dump dumps Postgres databases only" }];
    }
    if (dumping) {
      return [{ label, why: "a dump of " + dumping.conn + " is still running — one at a time" },
        { label: "Stop dump of " + dumping.conn, act: stopDump }];
    }
    return [{ label, act: () => openDump(name) }];
  }

  // PG_CONN is the names Postgres in Docker gives its connections
  // ("docker-pg18", or "docker-pg18-2" beside a config file's
  // "docker-pg18"); the major names the container. It only decides
  // whether the Stop row shows — the server checks the name for itself
  // (pgdocker.StopTarget), so keep the two patterns in step.
  const PG_CONN = /^docker-pg(\d+)(?:-\d+)?$/;

  // addItems are the two ways to add a connection, for the right-click
  // menu's foot and the + dropdown. The form leads, so + then Enter is
  // still the form.
  const addItems = () => [
    { label: "Add a connection…", act: openAdd },
    { label: "Postgres in Docker…", act: openPGDocker },
  ];

  // The right-click menu: connect (or, on the row this tab is on or is
  // connecting to, disconnect), refresh, or edit or remove one added here;
  // on a connection Postgres in Docker made, stop its container (stopItem);
  // on a Postgres one, dump it (dumpItems); then the ways to add one
  // (addItems), as under +.
  // For the config file's, Edit and Remove are shown and say why they
  // cannot, rather than being absent and leaving the user to wonder where
  // they went — and Refresh likewise on a row the tab is not on.
  //
  // Disconnect and Refresh act on the tab, not the row's name: on a
  // server's other database ("ProdDr/analytics") the base's row is the one
  // marked, and what the tab leaves, or re-reads, is the database it is on.
  //
  // Refresh is offered only on the row marked active: the server refreshes
  // the tab's own connection (Workspace.Refresh), and another row's schemas
  // are read fresh by the Connect beside it anyway. Mid-dial ("connecting")
  // there is nothing settled to re-read yet.
  conns.addEventListener("contextmenu", (e) => {
    const b = e.target.closest(".conn-item");
    if (!b) return;
    e.preventDefault();
    const name = b.dataset.conn;
    const fromFile = name + " comes from the config file (or is a built-in demo) — edit the file to ";
    const on = b.classList.contains("active"), dialing = b.classList.contains("connecting");
    dbc.menu.open(e.clientX, e.clientY, [
      { head: name },
      on || dialing
        ? { label: "Disconnect", act: () => dbc.cmd.disconnect() }
        : { label: "Connect", act: () => dbc.cmd.connect(name) },
      on
        ? { label: "Refresh", act: () => dbc.cmd.refresh() }
        : { label: "Refresh", why: dialing ? "still connecting to " + name + " — its schemas are being read now"
          : "this tab is not on " + name + " — Connect reads its schemas fresh" },
      b.dataset.saved
        ? { label: "Edit…", act: () => openEdit(b) }
        : { label: "Edit…", why: fromFile + "change it" },
      b.dataset.saved
        ? { label: "Remove…", act: () => confirmRemove(name) }
        : { label: "Remove…", why: fromFile + "remove it" },
      ...stopItem(b, name, on || dialing),
      ...dumpItems(b, name),
      { head: "" },
      ...addItems(),
    ]);
  });

  // stopItem is the Stop row for a connection Postgres in Docker made
  // (none for any other). While this tab is on it, the row says why not:
  // the stop would cut the tab's session.
  function stopItem(b, name, onIt) {
    const m = b.dataset.saved && PG_CONN.exec(name);
    if (!m) return [];
    const container = "dbc-pg" + m[1];
    return [onIt
      ? { label: "Stop container " + container, why: "this tab is on " + name + " — disconnect it first" }
      : { label: "Stop container " + container, act: () => stopPGDocker(name, container) }];
  }

  // + is a dropdown of the ways to add: the form, or Postgres in Docker
  const addBtn = document.getElementById("conn-add");
  addBtn.addEventListener("click", () => dbc.menu.at(addBtn, addItems()));

  dbc.conns = { draw, openAdd, openEdit, openPGDocker, openDump, onDump };
})();
