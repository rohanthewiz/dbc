// dbc web — menus, dialogs and the clipboard: the small pieces of UI the
// grid, the sidebar and the plan view all use.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;

  // ── context menus ──────────────────────────────────────────────────────
  // A menu is a list of items, the TUI's shape:
  //
  //   {label, key, act}     a row; key is the shortcut shown on the right
  //   {label, why}          a row that cannot run now; clicking it says why
  //                         in the log instead of silently doing nothing
  //   {head: "copy as"}     a heading ("" draws a separator)
  //
  // One menu is open at a time. It closes on a pick, Esc, a click outside,
  // a scroll or a resize. ↑↓ and Enter drive it from the keyboard.
  let open = null;

  function closeMenu() {
    if (!open) return;
    open.node.remove();
    document.removeEventListener("pointerdown", open.outside, true);
    document.removeEventListener("keydown", open.keys, true);
    window.removeEventListener("resize", closeMenu);
    const back = open.back;
    open = null;
    if (back && back.isConnected) back.focus();
  }

  function openMenu(x, y, items) {
    closeMenu();
    const node = el("div", { class: "menu", role: "menu" });
    const rows = [];
    for (const it of items) {
      if (it.head !== undefined) {
        node.append(it.head ? el("div", "mhead", it.head) : el("div", "msep"));
        continue;
      }
      const b = el("button", { class: "mitem" + (it.why ? " off" : ""), type: "button", role: "menuitem" },
        el("span", "ml", it.label), it.key ? el("span", "mk", it.key) : null);
      if (it.why) b.title = it.why;
      b.addEventListener("click", () => {
        closeMenu();
        if (it.why) dbc.log("warn", it.why);
        else if (it.act) it.act();
      });
      rows.push(b);
      node.append(b);
    }
    document.body.append(node);
    // keep it on screen: flip left/up when it would run off the edge
    const r = node.getBoundingClientRect();
    const px = x + r.width > innerWidth ? Math.max(4, x - r.width) : x;
    const py = y + r.height > innerHeight ? Math.max(4, innerHeight - r.height - 4) : y;
    node.style.left = px + "px";
    node.style.top = py + "px";

    let cur = -1;
    const focusRow = (i) => { cur = (i + rows.length) % rows.length; rows[cur].focus(); };
    open = {
      node, back: document.activeElement,
      outside: (e) => { if (!node.contains(e.target)) closeMenu(); },
      keys: (e) => {
        if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeMenu(); }
        else if (e.key === "ArrowDown") { e.preventDefault(); e.stopPropagation(); focusRow(cur + 1); }
        else if (e.key === "ArrowUp") { e.preventDefault(); e.stopPropagation(); focusRow(cur - 1); }
      },
    };
    document.addEventListener("pointerdown", open.outside, true);
    document.addEventListener("keydown", open.keys, true);
    window.addEventListener("resize", closeMenu);
    if (rows.length) focusRow(0);
  }

  // menuAt opens a menu under a button, as a dropdown.
  function menuAt(button, items) {
    const r = button.getBoundingClientRect();
    openMenu(r.left, r.bottom + 2, items);
  }

  dbc.menu = { open: openMenu, at: menuAt, close: closeMenu, isOpen: () => !!open };

  // ── dialogs ────────────────────────────────────────────────────────────
  // A modal owns the keyboard until it closes (the TUI's rule): keys go to
  // its onKey first, Esc closes it, and focus returns to where it was.
  let modal = null;

  function openModal(opts) {
    closeModal();
    const box = el("div", { class: "modal " + (opts.cls || ""), role: "dialog", "aria-modal": "true", "aria-label": opts.title });
    const close = el("button", { class: "mclose", type: "button", title: "Close (Esc)" }, "✕");
    box.append(el("div", "mtitle", el("span", null, opts.title), close), opts.body);
    if (opts.foot) box.append(opts.foot);
    const shade = el("div", "shade", box);
    document.body.append(shade);
    const m = {
      shade, back: document.activeElement, onClose: opts.onClose,
      keys: (e) => {
        if (opts.onKey && opts.onKey(e)) { e.preventDefault(); e.stopPropagation(); return; }
        if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeModal(); }
      },
    };
    close.addEventListener("click", closeModal);
    shade.addEventListener("pointerdown", (e) => { if (e.target === shade) closeModal(); });
    document.addEventListener("keydown", m.keys, true);
    modal = m;
    (opts.focus || close).focus();
    return { close: closeModal };
  }

  function closeModal() {
    if (!modal) return;
    const m = modal;
    modal = null;
    document.removeEventListener("keydown", m.keys, true);
    m.shade.remove();
    if (m.onClose) m.onClose();
    if (m.back && m.back.isConnected) m.back.focus();
  }

  dbc.modal = { open: openModal, close: closeModal, isOpen: () => !!modal };

  // ── the clipboard ──────────────────────────────────────────────────────
  // copy puts {text, html} on the clipboard; content may be a promise (a
  // copy rendered by the server). Resolves to {rich} — whether an HTML
  // flavor landed — or rejects when nothing could be written.
  //
  // WHY THE PROMISE GOES INTO THE ClipboardItem. A clipboard write needs the
  // user's click to still count, and Safari stops counting it at the first
  // await: build the item after fetching the copy and Safari refuses it.
  // A ClipboardItem whose blobs are promises is created synchronously, in
  // the click, and filled when the server answers — which Chrome and
  // Safari both accept. Which flavors it holds must be known up front, so
  // the caller says whether this copy is HTML.
  //
  // Fallbacks, in order: writeText (no ClipboardItem, or the rich write was
  // refused), then a hidden textarea and execCommand("copy") for a page
  // the browser does not treat as a secure context.
  async function copy(content, isHTML) {
    const p = Promise.resolve(content);
    if (window.ClipboardItem && navigator.clipboard && navigator.clipboard.write) {
      const flavors = { "text/plain": p.then((c) => new Blob([c.text], { type: "text/plain" })) };
      if (isHTML) flavors["text/html"] = p.then((c) => new Blob([c.html || c.text], { type: "text/html" }));
      try {
        await navigator.clipboard.write([new ClipboardItem(flavors)]);
        return { rich: !!isHTML };
      } catch (err) {
        await p; // a failed render is the caller's error, not a clipboard one
        console.warn("rich clipboard write refused, trying text:", err);
      }
    }
    const c = await p;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      try {
        await navigator.clipboard.writeText(c.text);
        return { rich: false };
      } catch (_) { /* fall through */ }
    }
    if (legacyCopy(c.text)) return { rich: false };
    throw new Error("the browser refused the clipboard");
  }

  function legacyCopy(text) {
    const ta = el("textarea", { class: "offscreen", "aria-hidden": "true" });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
    ta.remove();
    return ok;
  }

  // copyText copies plain text and logs it: "copied <what>".
  function copyText(text, what) {
    if (!text) { dbc.log("warn", "nothing to copy"); return; }
    copy({ text }, false).then(
      () => dbc.log("ok", "copied " + what),
      (e) => dbc.log("err", "copy failed: " + e.message));
  }

  dbc.clip = { copy, copyText };
})();
