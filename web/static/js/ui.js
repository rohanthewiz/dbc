// dbc web — menus, dialogs, hover cards and the clipboard: the small
// pieces of UI the grid, the sidebar and the plan view all use.
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

  // ── hover cards ────────────────────────────────────────────────────────
  // A hover card is a tooltip built from real markup: a heading,
  // highlighted SQL, key hints. The browser's `title` can only draw one
  // unstyled run of text, wrapped wherever the OS likes, after a delay the
  // page cannot set.
  //
  //   dbc.hovercard.bind(root, selector, render, keyOf)
  //
  // listens on root for the pointer resting on an element matching
  // selector (the anchor), calls render(anchor) → a Node, or null for no
  // card, and floats the result under the anchor, or above it when there
  // is no room below. The listeners are delegated, so root's children can
  // be redrawn freely. A redraw that removes the anchor should call
  // dbc.hovercard.hide(): a removed node gets no pointerout, so nothing
  // else would put the card away.
  //
  // keyOf(anchor) names what the anchor stands for (a result tab's id).
  // It is what a press mutes (below), because a click usually redraws the
  // anchor: the button under the pointer afterwards is a new element for
  // the same tab, and muting the old element would let the card straight
  // back. Omitted, the anchor element is its own key.
  //
  // The mute ends when the pointer is over anything but an anchor with
  // that key. The check runs on every pointerover in the document rather
  // than on the anchor's pointerout, because a redraw under a resting
  // pointer removes the element that would have had the pointerout.
  //
  // The card is look-only (pointer-events: none). It never takes a click
  // or the focus, so it cannot get between the user and what is under it,
  // and leaving the anchor is all it takes to put it away. Everything it
  // says can be reached another way (a menu row, a key): it explains, it
  // offers nothing.
  //
  // Timing follows native tooltips. The first card waits SHOW_MS of
  // resting, so a pointer passing over a strip does not flash cards. For
  // WARM_MS after a card goes, the next anchor's card shows at once, so
  // scanning along a row of tabs reads each without waiting again:
  //
  //   rest ─ SHOW_MS ─▶ card A ──move──▶ card B (at once) ──leave──▶ gone
  //                                       └── within WARM_MS of A going
  //
  // It goes away when the pointer leaves the anchor, on any press or key
  // (the user is acting, not reading), on a wheel or on a scroll of a box
  // that holds the anchor (the anchor moves), on a resize, and when the
  // window loses focus. After a press or
  // a key it stays away until the pointer leaves that anchor. A click on a
  // tab should not bring the card straight back over what the click
  // showed.
  //
  // A child of the anchor with a title of its own (the ↻ or × in a tab)
  // puts the card away while the pointer is on it, so the child's own
  // short tip is the only one showing.
  //
  // Mouse only: a touch or a pen has no hover, and a card left up by a tap
  // would have nothing to take it down.
  const SHOW_MS = 450, WARM_MS = 600;
  const GAP = 6, EDGE = 8; // px: from the anchor, and kept from the window's edges
  // `on` below is a binding: { root, selector, key }, one per bind call.
  let hc = null;        // the card up: { node, anchor, on, key }
  let hcWait = null;    // a card about to show: { anchor, on, key, timer }
  let hcWarmUntil = 0;  // performance.now() before which the next card shows at once
  let hcMuted = null;   // { on, key }: what a press or key put the card away on, until the pointer leaves it

  function hideCard() {
    if (hcWait) { clearTimeout(hcWait.timer); hcWait = null; }
    if (!hc) return;
    hc.node.remove();
    hc = null;
    hcWarmUntil = performance.now() + WARM_MS;
  }

  function showCard(at, render) {
    hideCard();
    if (!at.anchor.isConnected) return; // redrawn away during the wait
    const body = render(at.anchor);
    if (!body) return;
    const node = el("div", { class: "hcard", role: "tooltip" }, body);
    document.body.append(node);
    placeCard(node, at.anchor);
    hc = { node, anchor: at.anchor, on: at.on, key: at.key };
  }

  // placeCard puts the card under the anchor, left edges aligned, slid
  // left to stay in the window. With no room below it goes above. When
  // neither side has room it takes the roomier one and is cut to fit
  // (.hcard hides its overflow); the SQL inside is already cut to a few
  // lines, so that only happens in a very short window.
  function placeCard(node, anchor) {
    const a = anchor.getBoundingClientRect();
    const r = node.getBoundingClientRect();
    const x = Math.max(EDGE, Math.min(a.left, innerWidth - r.width - EDGE));
    const below = innerHeight - EDGE - (a.bottom + GAP);
    const above = a.top - GAP - EDGE;
    let y;
    if (r.height <= below || below >= above) {
      y = a.bottom + GAP;
      if (r.height > below) node.style.maxHeight = Math.max(0, below) + "px";
    } else {
      const h = Math.min(r.height, above);
      if (r.height > above) node.style.maxHeight = above + "px";
      y = a.top - GAP - h;
    }
    node.style.left = x + "px";
    node.style.top = y + "px";
  }

  function bindCard(root, selector, render, keyOf) {
    const on = { root, selector, key: keyOf || ((a) => a) };
    root.addEventListener("pointerover", (e) => {
      if (e.pointerType !== "mouse") return;
      const a = e.target.closest(selector);
      if (!a || !root.contains(a)) return;
      // over a titled child: its own tip speaks, the card steps aside
      const own = e.target.closest("[title]");
      if (own && own !== a && a.contains(own)) { hideCard(); return; }
      if (hcMuted) return; // still on the muted anchor: the document's check below cleared any other
      if ((hc && hc.anchor === a) || (hcWait && hcWait.anchor === a)) return;
      const warm = !!hc || performance.now() < hcWarmUntil;
      hideCard();
      const at = { anchor: a, on, key: on.key(a) };
      if (warm) { showCard(at, render); return; }
      hcWait = Object.assign(at, { timer: setTimeout(() => { hcWait = null; showCard(at, render); }, SHOW_MS) });
    });
    root.addEventListener("pointerout", (e) => {
      const a = e.target.closest(selector);
      // a move between the anchor's own children is not leaving it
      if (!a || (e.relatedTarget && a.contains(e.relatedTarget))) return;
      if ((hc && hc.anchor === a) || (hcWait && hcWait.anchor === a)) hideCard();
    });
  }

  // A press or a key mutes the anchor under the pointer. Both listen in
  // the capture phase, so a handler that stops the event (the grid's keys,
  // a menu's) cannot leave a card up.
  const muteCard = () => {
    const cur = hc || hcWait;
    if (cur) hcMuted = { on: cur.on, key: cur.key };
    hideCard();
  };
  // The mute's end: the pointer over anything that is not an anchor of
  // the muted binding with the muted key. Capture phase, so it runs before
  // the binding's own pointerover and that one sees the mute already gone.
  const unmuteCard = (e) => {
    if (!hcMuted) return;
    const { on, key } = hcMuted;
    const a = e.target instanceof Element ? e.target.closest(on.selector) : null;
    if (!a || !on.root.contains(a) || on.key(a) !== key) hcMuted = null;
  };
  document.addEventListener("pointerdown", muteCard, true);
  document.addEventListener("keydown", muteCard, true);
  document.addEventListener("pointerover", unmuteCard, true);
  document.addEventListener("wheel", hideCard, { capture: true, passive: true });
  // a scroll moves the anchor only when the box scrolled holds it; the
  // log scrolling itself to a new line must not take a card away
  document.addEventListener("scroll", (e) => {
    if (hc && (e.target === document || (e.target instanceof Node && e.target.contains(hc.anchor)))) hideCard();
  }, true);
  window.addEventListener("resize", hideCard);
  window.addEventListener("blur", hideCard);

  dbc.hovercard = { bind: bindCard, hide: hideCard };

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
  // refused), then execCommand("copy") for a page the browser does not
  // treat as a secure context — dbc web reached over plain http by a LAN
  // address, where navigator.clipboard does not exist at all. That last
  // one still carries the HTML flavor (see legacyCopy), so a table copy
  // pastes as a table there too.
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
    const legacy = legacyCopy(c.text, isHTML ? c.html : "");
    if (legacy.ok) return { rich: legacy.rich };
    throw new Error("the browser refused the clipboard");
  }

  // legacyCopy is execCommand("copy") on a hidden textarea. The textarea
  // gives the command a selection to act on (no selection, no copy event in
  // some browsers); the one-shot copy listener then REPLACES what the
  // browser would have copied with our flavors via clipboardData.setData —
  // the only way this old API sets text/html. Without html it leaves the
  // event alone and the textarea's text is what lands, as before. Returns
  // {ok, rich}: whether anything was copied, and whether the HTML went too.
  function legacyCopy(text, html) {
    const ta = el("textarea", { class: "offscreen", "aria-hidden": "true" });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    let rich = false;
    const onCopy = (e) => {
      if (!html || !e.clipboardData) return;
      rich = true;
      e.clipboardData.setData("text/plain", text);
      e.clipboardData.setData("text/html", html);
      e.preventDefault(); // keep the browser from overwriting our flavors
    };
    document.addEventListener("copy", onCopy, true);
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
    document.removeEventListener("copy", onCopy, true);
    ta.remove();
    return { ok, rich: ok && rich };
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
