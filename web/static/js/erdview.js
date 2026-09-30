// dbc web — the entity-relationship diagram dialog.
//
//   dbc.erd.open("")        the whole connection   (the Tables heading's ERD)
//   dbc.erd.open("orders")  orders and its neighbours  (a table's menu, or e)
//
//   ┌ ERD · orders ─────────────────────────────────────────── ✕ ┐
//   │ around orders: [1 hop ▾]  ☐ views     dbc · shop · 4 tables │  the bar
//   ├─────────────────────────────────────────────────────────────┤
//   │                  the picture (erd.png)                      │  f: fit ⇄ 100%
//   ├─────────────────────────────────────────────────────────────┤
//   │ ⤓ PNG  ⤓ JPEG  ⤓ Mermaid  ⧉ Copy as Mermaid (m)             │  the foot
//   └─────────────────────────────────────────────────────────────┘
//
// The server draws everything (web/erd.go → erd.Picture): the page only
// asks. It asks for …/erd (JSON) first and the picture second, because an
// <img> that fails says nothing — a refusal (no connection, a table that
// is gone) must arrive as words in the log, not as a broken image.
//
// The picture is drawn at 2× for sharpness; "100%" shows it at its CSS
// size (half its pixels), "fit" shrinks it to the dialog. It follows the
// page's light or dark, and so do the files saved from here, so what is
// sent is what was on screen.
(function () {
  "use strict";

  const dbc = window.dbc;
  const { api, log, el } = dbc;

  // query is the selection as the server reads it (erdSelection).
  function query(sel, extra) {
    const p = new URLSearchParams();
    if (sel.table) {
      p.set("table", sel.table);
      p.set("depth", String(sel.depth));
    }
    if (sel.views) p.set("views", "1");
    for (const k in extra || {}) p.set(k, extra[k]);
    const s = p.toString();
    return s ? "?" + s : "";
  }

  const pageTheme = () => document.documentElement.dataset.theme === "light" ? "light" : "dark";

  async function open(table) {
    const sel = { table: table || "", depth: 1, views: false };
    let info;
    try {
      info = await api("GET", dbc.wsPath("/erd" + query(sel)));
    } catch (e) {
      log(e.status === 400 ? "warn" : "err", "diagram: " + e.message);
      return;
    }

    const img = el("img", { class: "erd-img fit", alt: "entity-relationship diagram" });
    const pane = el("div", { class: "erd-pane", tabindex: "0" }, img);
    const summary = el("span", "hint");
    const bar = el("div", "erd-bar");

    // the scope: how far around the table, and whether views come too
    if (sel.table) {
      const depth = el("select", { "aria-label": "How far around " + sel.table });
      for (const [v, label] of [["0", "just it"], ["1", "1 hop"], ["2", "2 hops"], ["3", "3 hops"], ["-1", "all it connects to"]]) {
        depth.append(el("option", { value: v }, label));
      }
      depth.value = "1";
      depth.addEventListener("change", () => { sel.depth = Number(depth.value); refresh(); });
      bar.append(el("label", "erd-ctl", "around " + sel.table + ": ", depth));
    }
    const views = el("input", { type: "checkbox" });
    views.addEventListener("change", () => { sel.views = views.checked; refresh(); });
    bar.append(el("label", { class: "erd-ctl", title: "Views have no keys, so they are left out unless asked for" }, views, " views"));
    const zoom = el("button", { type: "button", title: "Fit the dialog, or show at actual size (f)" }, "100%");
    zoom.addEventListener("click", toggleZoom);
    bar.append(zoom, summary);

    const btn = (label, title, act) => {
      const b = el("button", { type: "button", title }, label);
      b.addEventListener("click", act);
      return b;
    };
    const foot = el("div", "mfoot",
      btn("⤓ PNG", "Download the picture, lossless", () => download("png")),
      btn("⤓ JPEG", "Download the picture as a JPEG", () => download("jpg")),
      btn("⤓ Mermaid", "Download the erDiagram source (.mmd), for a pull request or a wiki", () => download("mmd")),
      btn("⧉ Copy as Mermaid", "Copy the erDiagram source (m)", copyMermaid),
      el("span", "hint", "f fit ⇄ 100% · m copy · Esc close"));

    function show(i) {
      info = i;
      summary.textContent = i.title.replace(/^dbc · /, "");
      img.src = dbc.wsPath("/erd.png" + query(sel, { theme: pageTheme(), v: String(Date.now()) }));
    }

    // refresh re-asks after a scope change; a refusal keeps the last
    // picture and says why
    let seq = 0;
    async function refresh() {
      const my = ++seq;
      try {
        const i = await api("GET", dbc.wsPath("/erd" + query(sel)));
        if (my === seq) show(i);
      } catch (e) {
        log("warn", "diagram: " + e.message);
      }
    }

    // 100% is the picture's CSS size: it is drawn at 2 device pixels per
    // CSS pixel (erd.Options' default)
    function toggleZoom() {
      const actual = !img.classList.toggle("fit");
      img.style.width = actual ? img.naturalWidth / 2 + "px" : "";
      zoom.textContent = actual ? "Fit" : "100%";
    }

    function download(ext) {
      const a = el("a", { href: dbc.wsPath("/erd." + ext + query(sel, { download: "1", theme: pageTheme() })), download: "" });
      document.body.append(a);
      a.click();
      a.remove();
      log("ok", "downloading the diagram as ." + ext);
    }

    // The promise goes into the clipboard write, not its result: Safari
    // stops counting the click at the first await (see dbc.clip.copy).
    function copyMermaid() {
      const p = api("GET", dbc.wsPath("/erd" + query(sel)));
      dbc.clip.copy(p, false).then(() => log("ok", "copied the diagram as Mermaid"),
        (e) => log("err", "copy failed: " + e.message));
    }

    img.addEventListener("error", () => log("err", "the diagram's picture did not load"));
    dbc.modal.open({
      // the connection is named in the bar's summary
      title: sel.table ? "ERD · around " + sel.table : "ERD",
      cls: "erd",
      body: el("div", "erd-body", bar, pane),
      foot,
      focus: pane,
      onKey: (e) => {
        if (e.ctrlKey || e.metaKey || e.altKey || e.target.tagName === "SELECT") return false;
        if (e.key === "f") { toggleZoom(); return true; }
        if (e.key === "m") { copyMermaid(); return true; }
        return false;
      },
    });
    show(info);
  }

  dbc.erd = { open };
})();
