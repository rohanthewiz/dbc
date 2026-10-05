// dbc web — TAB GROUPS: a short named chip in the query-tab strip with its
// member tabs gathered behind it and underlined in the group's colour, and
// foldable to the chip alone. ced's tab groups (internal/app/tabgroups.go)
// on the browser's strip; web/groups.go has the stored shape and the merge
// across windows.
//
//   [prod][Query 1 ×][Query 3 ×] [wip +2][Query 4 ×] [Query 2 ×] [+]
//    └chip┴── underlined ───────┘ └ collapsed: 2 members hidden
//
// Design choices (ced's, unless said otherwise):
//
//   - Two kinds, one record. A CONNECTION group is a rule: every tab on its
//     connection — or on one of that connection's other databases
//     ("prod/analytics") — belongs, including tabs that switch to it later.
//     It is ced's folder group with the connection for the folder: where a
//     tab lives in dbc. An AD-HOC group is a hand-picked set. conn tells them
//     apart ("" = ad-hoc).
//   - One group per tab, resolved — never stored on the tab. groupOf answers
//     from the groups: ad-hoc membership first (the explicit gesture), then
//     the DEEPEST connection group that has not excluded the tab, so
//     "prod/analytics" beats "prod" for a tab on that database, and a
//     connection rule cannot fight a hand placement.
//   - Membership is by saved-tab KEY, not by the tab object: keys are what a
//     reload brings back, and a rename keeps the key.
//   - Groups are contiguous because the strip's array is REORDERED, not
//     because it is drawn in another order. arrange clusters each group at
//     its first member's place — stable, and a no-op with no groups — so
//     Alt+1…9, the saved order and what is drawn always agree.
//   - Collapse hides members, never the active tab: picking a member of a
//     folded group (Alt+N, the group menu) shows that one tab, so the editor
//     never shows text without its tab in the strip.
//   - Every chip gesture has another door: a click on the chip folds it and
//     a right-click opens the group menu; a tab's own right-click menu
//     carries "Add to group…", "Remove from group", and the group's menu as
//     rows, so a hand that never finds the chip reaches the same verbs.
//   - Names are letters and digits, unique case-insensitively, so a menu
//     row and a chip always mean one group. ced stops at four — the chip
//     shares one terminal row with every file name — the browser's strip has
//     room for twice that, which is still a mnemonic ("prod", "reports1").
//   - A new tab opened from a tab in an ad-hoc group joins that group, as a
//     browser's tab group does (dbc's own choice: ced opens files, not
//     siblings). A connection group needs no help: the new tab is on the
//     same connection.
//   - Where a new tab lands is never a guess (dbc's own choice): the strip's
//     + wears the colour of the group Alt+T would put the tab in, and says
//     so in its tooltip; a right-click on + — or a group's own menu ("New
//     tab in <group>") — opens the tab in any group, not only the one the
//     tab on screen belongs to. A tab opened into a group starts on that
//     group's connection (connFor), so it lands where it was asked to.
//
// The model and its menus live here; app.js owns the strip and hands this
// module what it needs (create's host). Every mutation calls host.changed,
// which saves the groups and redraws the strip — the redraw arranges, and
// saves the order when the arrangement moved a tab.
(function () {
  "use strict";

  const dbc = window.dbc;
  const el = dbc.el;

  const NAME_MAX = 8;
  // COLORS is how many hues the groups cycle through: app.css's --g0 … --g5,
  // one set per theme, each readable under var(--bg) as the chip's text.
  const COLORS = 6;

  // create builds the groups of one window's strip. host:
  //   tabs()            the query tabs, in strip order
  //   active()          the tab on screen
  //   under(conn, base) conn is base, or one of base's other databases
  //   activate(t)       show t
//   newTab(g)         open a query tab in group g (connFor's connection)
  //   close(t)          close t at once (no stateful-session question)
  //   changed()         a group changed: save the groups, redraw the strip
  //   log(level, text)
  function create(host) {
    let groups = []; // {name, conn, members: Set<key>, exclude: Set<key>, collapsed, color}

    const isConn = (g) => g.conn !== "";
    const live = (g) => groups.includes(g);

    // ── membership ──────────────────────────────────────────────────────
    function groupOf(t) {
      if (!t || !groups.length) return null;
      for (const g of groups) if (!isConn(g) && g.members.has(t.key)) return g;
      if (!t.conn) return null;
      let best = null;
      for (const g of groups) {
        if (!isConn(g) || g.exclude.has(t.key) || !host.under(t.conn, g.conn)) continue;
        // deepest wins: "prod/analytics" is the more specific claim
        if (!best || g.conn.length > best.conn.length) best = g;
      }
      return best;
    }

    const members = (g) => host.tabs().filter((t) => groupOf(t) === g);

    // landing is the group a plain new tab (Alt+T, a click on +) joins when
    // it is opened from fromTab onto conn: fromTab's ad-hoc group (joinNew),
    // else whichever connection group claims conn. The probe has no key —
    // the new tab's key is fresh, so no group's exclusion can name it, even
    // when fromTab itself was taken out of that connection group.
    function landing(fromTab, conn) {
      const g = groupOf(fromTab);
      if (g && !isConn(g)) return g;
      return conn ? groupOf({ key: "", conn }) : null;
    }

    // connFor is the connection a tab opened INTO g starts on: a connection
    // group's own; for an ad-hoc group, the tab on screen's when it is a
    // member, else its last member's (the strip's newest place the group
    // was working). fallback covers an ad-hoc group with no tab here, which
    // dropEmpty normally rules out.
    function connFor(g, fallback) {
      if (isConn(g)) return g.conn;
      const ms = members(g);
      if (ms.includes(host.active())) return host.active().conn || fallback;
      return (ms.length && ms[ms.length - 1].conn) || fallback;
    }

    // ordered is the groups in strip order — chip by chip, left to right —
    // then any with no tab here (a connection group waiting for its first),
    // so a menu listing them reads like the strip.
    function ordered() {
      const out = [];
      for (const t of host.tabs()) {
        const g = groupOf(t);
        if (g && !out.includes(g)) out.push(g);
      }
      for (const g of groups) if (!out.includes(g)) out.push(g);
      return out;
    }

    // arrange returns tabs reordered so each group's members sit together at
    // its first member's place; ungrouped tabs keep their places relative
    // to each other. The SAME array comes back when nothing moved, so the
    // caller can tell whether the order needs saving.
    function arrange(tabs) {
      if (!groups.length || tabs.length < 2) return tabs;
      const of = tabs.map(groupOf);
      const out = [];
      const placed = new Set();
      tabs.forEach((t, i) => {
        const g = of[i];
        if (!g) { out.push(t); return; }
        if (placed.has(g)) return;
        placed.add(g);
        for (let j = i; j < tabs.length; j++) if (of[j] === g) out.push(tabs[j]);
      });
      return out.every((t, i) => t === tabs[i]) ? tabs : out;
    }

    // forgetTab drops a closing tab from every group, and an ad-hoc group
    // with it when that was its last member — an empty ad-hoc group has no
    // chip to draw and nothing to manage. Connection groups stay: they are
    // rules, and the next tab on the connection joins. Reports whether
    // anything changed.
    function forgetTab(t) {
      let changed = false;
      for (const g of groups) {
        // both deletes run: a key is in at most one set per group, but
        // which one depends on the group's kind
        const m = g.members.delete(t.key), x = g.exclude.delete(t.key);
        changed = changed || m || x;
      }
      return dropEmpty() || changed;
    }

    function dropEmpty() {
      const n = groups.length;
      groups = groups.filter((g) => isConn(g) || g.members.size > 0);
      return groups.length !== n;
    }

    // ── what the strip draws ────────────────────────────────────────────
    // chip is the chip drawn in front of tabs[i], or null: the first tab of
    // each group carries it.
    function chip(tabs, i) {
      const g = groupOf(tabs[i]);
      if (!g || (i > 0 && groupOf(tabs[i - 1]) === g)) return null;
      const n = g.collapsed ? hiddenCount(g) : 0;
      return { g, text: n > 0 ? g.name + " +" + n : g.name };
    }

    // hiddenCount is how many of g's tabs a fold hides: all but the active.
    const hiddenCount = (g) => members(g).filter((t) => t !== host.active()).length;

    // hidden: t is folded away — in a collapsed group, and not on screen.
    function hidden(t) {
      const g = groupOf(t);
      return !!g && g.collapsed && t !== host.active();
    }

    // describe is a group's one-line summary for tooltips and menu heads.
    function describe(g) {
      const n = dbc.plural(members(g).length, "tab");
      return isConn(g) ? "every tab on " + g.conn + " · " + n : "ad-hoc · " + n;
    }

    // ── names ───────────────────────────────────────────────────────────
    const byName = (name, except) => groups.find((g) => g !== except && g.name.toLowerCase() === name.toLowerCase());

    // nameProblem is why name can't be used (except by the group being
    // renamed, which may keep its own), or "" when it can.
    function nameProblem(name, except) {
      const runes = [...name];
      if (!runes.length) return "a group needs a name";
      if (runes.length > NAME_MAX) return "group names are at most " + NAME_MAX + " letters (" + name + " has " + runes.length + ")";
      if (!/^[\p{L}\p{N}]+$/u.test(name)) return "group names are letters and digits only";
      if (byName(name, except)) return "there is already a group named " + byName(name, except).name;
      return "";
    }

    // nameFrom suggests a name from s: its first letters and digits, so
    // "reporting" → "reportin" and "lite-2" → "lite2".
    const nameFrom = (s) => [...s.replace(/[^\p{L}\p{N}]/gu, "")].slice(0, NAME_MAX).join("");

    // uniqueName is base, or base cut short plus a digit when base is taken
    // ("grp" → "grp2"), so the name field's seed is usable as offered.
    function uniqueName(base) {
      if (!base || !byName(base)) return base;
      const stem = [...base].slice(0, NAME_MAX - 1).join("");
      for (let d = 2; d <= 9; d++) if (!byName(stem + d)) return stem + d;
      return base;
    }

    // ── mutations ───────────────────────────────────────────────────────
    // nextColor is the first hue no live group uses, so the first six
    // groups are all distinct; after that the cycle repeats.
    function nextColor() {
      const used = new Set(groups.map((g) => g.color));
      for (let c = 0; c < COLORS; c++) if (!used.has(c)) return c;
      return groups.length % COLORS;
    }

    function newGroup(name, conn) {
      const g = { name, conn: conn || "", members: new Set(), exclude: new Set(), collapsed: false, color: nextColor() };
      groups.push(g);
      return g;
    }

    // add makes g the group t resolves to. t leaves any ad-hoc group first
    // (one group per tab). For a connection group, t is let back in if it
    // was excluded, and every DEEPER connection group that would claim it
    // excludes it, so the pick actually wins; shallower ones already lose to
    // g on depth, and an exclusion there would outlive g.
    function add(t, g) {
      if (!t || !live(g)) return;
      for (const o of groups) if (!isConn(o) && o !== g) o.members.delete(t.key);
      if (isConn(g)) {
        g.exclude.delete(t.key);
        for (const o of groups) {
          if (o !== g && isConn(o) && o.conn.length > g.conn.length && t.conn && host.under(t.conn, o.conn)) o.exclude.add(t.key);
        }
      } else {
        g.members.add(t.key);
      }
      dropEmpty();
    }

    // remove takes t out of the group it shows in: an ad-hoc membership is
    // dropped, a connection rule gets an exclusion. A shallower connection
    // group that also holds t then takes it — the gesture was about the
    // visible group.
    function remove(t) {
      const g = groupOf(t);
      if (!g) return;
      if (isConn(g)) g.exclude.add(t.key);
      else g.members.delete(t.key);
      dropEmpty();
      host.log("info", "removed " + t.title + " from group " + g.name);
      host.changed();
    }

    function toggle(g) {
      if (!live(g)) return;
      g.collapsed = !g.collapsed;
      host.changed();
    }

    // ungroup deletes g; its tabs stay open, where they are.
    function ungroup(g) {
      if (!live(g)) return;
      groups = groups.filter((x) => x !== g);
      host.log("info", "ungrouped " + g.name);
      host.changed();
    }

    // makeAdhoc turns a connection group into an ad-hoc one holding its
    // current members, so tabs on other connections can join and tabs that
    // switch to the connection later no longer do.
    function makeAdhoc(g) {
      if (!live(g) || !isConn(g)) return;
      const ms = members(g);
      g.conn = "";
      g.exclude = new Set();
      for (const t of ms) g.members.add(t.key);
      dropEmpty();
      host.log("info", g.name + " is now an ad-hoc group (" + dbc.plural(ms.length, "tab") + ")");
      host.changed();
    }

    // closeTabs closes g's tabs — but not those whose session may hold a
    // transaction (one click must not roll back work the per-tab close
    // would have asked about), and never the window's last tab.
    function closeTabs(g) {
      if (!live(g)) return;
      let kept = 0;
      // the tab on screen last: closing it activates a neighbour, which
      // should not be one about to close too
      const ms = members(g).sort((a, b) => (a === host.active()) - (b === host.active()));
      for (const t of ms) {
        if (t.stateful || host.tabs().length === 1) { kept++; continue; }
        host.close(t);
      }
      if (kept) host.log("warn", "kept " + dbc.plural(kept, "tab") + " — its session may hold a transaction, or it is the last tab");
    }

    // renameConn moves connection groups after a connection rename; move is
    // app.js's renamedConn for the rename.
    function renameConn(move) {
      let changed = false;
      for (const g of groups) {
        if (!isConn(g)) continue;
        const to = move(g.conn);
        if (to !== g.conn) { g.conn = to; changed = true; }
      }
      return changed;
    }

    // ── menus and the name dialog ───────────────────────────────────────
    // groupItems are the group menu's rows, a fixed list so the hand learns
    // the positions: rows that don't apply say why rather than vanish (the
    // menus' rule). The member tabs follow as rows of their own — the way
    // into a folded group without unfolding it (ced's "Switch to a tab in
    // group…" picker, inlined: a browser menu has the room).
    function groupItems(g) {
      const ms = members(g);
      const items = [
        { head: g.name + " — " + describe(g) },
        { label: "New tab in " + g.name, act: () => host.newTab(g) },
        { label: g.collapsed ? "Expand group" : "Collapse group", act: () => toggle(g) },
        { label: "Rename group…", act: () => promptName({ g }) },
        isConn(g) ? { label: "Make ad-hoc group", act: () => makeAdhoc(g) }
          : { label: "Make ad-hoc group", why: g.name + " is already ad-hoc" },
        ms.length ? { label: "Close group's tabs", act: () => closeTabs(g) }
          : { label: "Close group's tabs", why: g.name + " has no open tabs" },
        { label: "Ungroup", act: () => ungroup(g) },
      ];
      if (ms.length) {
        items.push({ head: "tabs" });
        for (const t of ms) {
          items.push({ label: (t === host.active() ? "● " : "") + t.title, act: () => { if (t !== host.active()) host.activate(t); } });
        }
      }
      return items;
    }

    function openGroupMenu(g, x, y) {
      if (live(g)) dbc.menu.open(x, y, groupItems(g));
    }

    // tabItems are a tab's right-click rows: add / remove, and the group's
    // own verbs when it is in one.
    function tabItems(t, x, y) {
      const g = groupOf(t);
      const items = [{ head: "group" }, { label: "Add to group…", act: () => openAddMenu(t, x, y) }];
      if (g) {
        items.push({ label: "Remove from group " + g.name, act: () => remove(t) });
        items.push({ label: "Group " + g.name + "…", act: () => openGroupMenu(g, x, y) });
      }
      return items;
    }

    // openAddMenu lists the groups t could join, then the two ways to start
    // one: an ad-hoc group, or a connection group for t's connection
    // (offered when the tab is on one and no group claims it already).
    function openAddMenu(t, x, y) {
      if (!host.tabs().includes(t)) return;
      const cur = groupOf(t);
      const items = [{ head: "add " + t.title + " to" + (cur ? " (now in " + cur.name + ")" : "") }];
      for (const g of groups) {
        if (g === cur) continue;
        // a connection group can only take a tab on its connection
        if (isConn(g) && !(t.conn && host.under(t.conn, g.conn))) continue;
        items.push({ label: g.name + "  · " + describe(g), act: () => {
          add(t, g);
          host.log("info", "added " + t.title + " to " + g.name);
          host.changed();
        } });
      }
      items.push({ label: "New ad-hoc group…", act: () => promptName({ t, conn: "", seed: uniqueName("grp") }) });
      if (t.conn && !groups.some((g) => g.conn === t.conn)) {
        const last = t.conn.slice(t.conn.lastIndexOf("/") + 1);
        items.push({ label: "New connection group: " + t.conn, act: () => promptName({ t, conn: t.conn, seed: uniqueName(nameFrom(last)) }) });
      }
      dbc.menu.open(x, y, items);
    }

    // promptName asks for a name: a new group's ({t, conn, seed} — the
    // group starts with t in it) or a rename ({g}). A refused name says why
    // under the field and keeps what was typed, so a nine-letter attempt
    // costs one Backspace rather than starting over.
    function promptName(o) {
      const renaming = !!o.g;
      const input = el("input", { class: "gname", value: renaming ? o.g.name : o.seed || "", maxlength: String(NAME_MAX * 2),
        "aria-label": "Group name", spellcheck: "false", autocomplete: "off" });
      const why = el("p", "gwhy");
      const kind = renaming ? describe(o.g)
        : o.conn ? "connection group — every tab on " + o.conn + " joins it" : "ad-hoc group — " + o.t.title + " starts it";
      const ok = el("button", { type: "button", class: "primary" }, renaming ? "Rename" : "Create");
      const cancel = el("button", { type: "button" }, "Cancel");
      const submit = () => {
        const name = input.value.trim();
        const problem = nameProblem(name, renaming ? o.g : undefined);
        if (problem) { why.textContent = problem; input.focus(); return; }
        dbc.modal.close();
        if (renaming) {
          if (!live(o.g)) return;
          o.g.name = name;
        } else {
          if (!host.tabs().includes(o.t)) return; // closed while the dialog was up
          const g = newGroup(name, o.conn);
          add(o.t, g);
          host.log("info", o.conn ? "group " + name + ": " + dbc.plural(members(g).length, "tab") + " on " + o.conn
            : "group " + name + ": added " + o.t.title);
        }
        host.changed();
      };
      ok.addEventListener("click", submit);
      cancel.addEventListener("click", () => dbc.modal.close());
      input.addEventListener("input", () => { why.textContent = ""; });
      dbc.modal.open({
        title: renaming ? "Rename group " + o.g.name : o.conn ? "New connection group" : "New tab group", focus: input,
        body: el("div", "gprompt", el("p", "hint", kind + " · up to " + NAME_MAX + " letters or digits"), input, why),
        foot: el("div", "mfoot", ok, cancel),
        onKey: (e) => {
          if (e.key !== "Enter") return false;
          submit();
          return true;
        },
      });
      input.select();
    }

    // ── the layout's form (web/groups.go's tabGroupRec) ─────────────────
    // encode keeps only the keys of tabs this window shows: another
    // window's are put back by the server's merge, and a closed tab's key
    // must not linger.
    function encode() {
      const shown = new Set(host.tabs().map((t) => t.key));
      return JSON.stringify(groups.map((g) => {
        const r = { name: g.name, color: g.color };
        if (isConn(g)) r.conn = g.conn;
        const m = [...g.members].filter((k) => shown.has(k));
        const x = [...g.exclude].filter((k) => shown.has(k));
        if (m.length) r.members = m;
        if (x.length) r.exclude = x;
        if (g.collapsed) r.collapsed = true;
        return r;
      }));
    }

    // restore rebuilds the groups from the layout against the tabs this
    // window claimed: keys with no tab here are dropped, and an ad-hoc group
    // none of whose tabs are here with them (the server's merge keeps them
    // stored for the window that holds them). A value that does not parse,
    // or a name the dialog would refuse, is skipped — groups are a
    // convenience, never a reason for the page not to boot.
    function restore(value) {
      groups = [];
      let recs = [];
      try { recs = JSON.parse(value || "[]"); } catch (_) { recs = []; }
      if (!Array.isArray(recs)) return;
      const shown = new Set(host.tabs().map((t) => t.key));
      for (const r of recs) {
        if (!r || typeof r.name !== "string" || nameProblem(r.name)) continue;
        const keys = (xs) => new Set((Array.isArray(xs) ? xs : []).filter((k) => shown.has(k)));
        const g = { name: r.name, conn: typeof r.conn === "string" ? r.conn : "", members: keys(r.members),
          exclude: keys(r.exclude), collapsed: !!r.collapsed, color: Number.isInteger(r.color) ? r.color : nextColor() };
        if (!isConn(g) && !g.members.size) continue;
        groups.push(g);
      }
    }

    return {
      groupOf, arrange, forgetTab, chip, hidden, describe, renameConn, encode, restore,
      tabItems, openGroupMenu, toggle, landing, connFor, ordered,
      // place puts a tab opened into g there by add's rules: out of any
      // ad-hoc group, and let past a deeper connection group that would
      // otherwise claim it
      place: (t, g) => add(t, g),
      // the group a new tab opened from t should join: t's ad-hoc group
      joinNew(fromTab, t) {
        const g = groupOf(fromTab);
        if (g && !isConn(g)) g.members.add(t.key);
        return !!g && !isConn(g);
      },
      byName: (name) => byName(name),
      color: (g) => ((g.color % COLORS) + COLORS) % COLORS,
      count: () => groups.length,
    };
  }

  dbc.groups = { create };
})();
