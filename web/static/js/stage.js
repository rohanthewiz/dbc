// dbc web — a pan-and-zoom stage: a layer of content, moved and scaled by
// a CSS transform inside a viewport that clips it. The pipeline canvas
// (pipelines.js) draws on one.
//
//	viewport (overflow hidden; takes the wheel)
//	└─ layer (transform: translate(tx, ty) scale(k); origin 0 0)
//	     the content, in STAGE coordinates: what the page lays out in px
//
// A point on screen and a point on the stage are related by the view
// {tx, ty, k}: screen = stage·k + t, relative to the viewport's corner.
// toStage turns a pointer's client coordinates into stage ones, which is
// what a drag needs to put a card under the finger at any zoom.
//
// The wheel: with Ctrl or ⌘ held — which is also what a trackpad pinch
// sends — it zooms about the pointer, so the point under it stays put; a
// plain wheel (or a two-finger swipe) pans. Panning by dragging the
// background is the caller's, since only it knows which presses land on
// content (a card, a port) and which on the background.
//
// The canvas tabs' inspector edge lives here too (dbc.inspector, at the
// end): both tabs that draw on a stage end in the same inspector column.
//
// plan.js has the same few lines for its graph and keeps them: it is also
// inlined, alone, into the standalone plan page (explain/html.go), which a
// shared file would have to be inlined beside — and under a CSP hash.
(function () {
  "use strict";

  const dbc = window.dbc;

  // stage hooks a viewport and its layer. opts: min and max zoom, and
  // onChange(view) after every move (to keep the view, say).
  function stage(viewport, layer, opts) {
    opts = opts || {};
    const min = opts.min || 0.2, max = opts.max || 2.5;
    const v = { tx: 0, ty: 0, k: 1 };
    layer.style.transformOrigin = "0 0";

    function apply() {
      layer.style.transform = "translate(" + v.tx + "px," + v.ty + "px) scale(" + v.k + ")";
      if (opts.onChange) opts.onChange(v);
    }
    // zoomAt scales by f about (px, py), viewport coordinates: that point
    // shows the same stage point before and after
    function zoomAt(f, px, py) {
      const k = Math.min(Math.max(v.k * f, min), max);
      const r = k / v.k;
      v.tx = px - (px - v.tx) * r;
      v.ty = py - (py - v.ty) * r;
      v.k = k;
      apply();
    }
    function panBy(dx, dy) {
      v.tx += dx;
      v.ty += dy;
      apply();
    }
    // fit scales a w×h content to the viewport with pad around it, never
    // above 1 (a small pipeline stays at its natural size), from the top
    // left: lanes read top to bottom, so the first one is what to show
    function fit(w, h, pad) {
      pad = pad === undefined ? 16 : pad;
      const W = viewport.clientWidth, H = viewport.clientHeight;
      if (!W || !H || !w || !h) return;
      v.k = Math.min(Math.max(Math.min((W - 2 * pad) / w, (H - 2 * pad) / h, 1), min), max);
      v.tx = pad;
      v.ty = pad;
      apply();
    }
    function toStage(clientX, clientY) {
      const r = viewport.getBoundingClientRect();
      return { x: (clientX - r.left - v.tx) / v.k, y: (clientY - r.top - v.ty) / v.k };
    }

    viewport.addEventListener("wheel", (e) => {
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) {
        const r = viewport.getBoundingClientRect();
        zoomAt(Math.exp(-e.deltaY * 0.0025), e.clientX - r.left, e.clientY - r.top);
      } else {
        // a line-mode wheel (Firefox's mouse) moves by lines, not pixels
        const unit = e.deltaMode === 1 ? 16 : 1;
        panBy(-e.deltaX * unit, -e.deltaY * unit);
      }
    }, { passive: false });

    return {
      view: v, apply, zoomAt, panBy, fit, toStage,
      // set restores a kept view
      set(nv) {
        if (nv) Object.assign(v, nv);
        apply();
      },
    };
  }

  dbc.stage = stage;

  // ── the inspector's edge ─────────────────────────────────────────────
  // The canvas tabs (pipelines.js, jobs.js) end in an inspector (.pinsp)
  // 280px wide by default, which is tight for a Go field: a line of an
  // Apply scrolls sideways and the check's end-of-line note falls out of
  // view (N-194). A bar on its left edge sizes it, as the sidebar's edge
  // sizes the sidebar:
  //
  //	palette │ canvas             ┃ inspector
  //	        │                    ┃◄── drag left: wider
  //	                             └ .pisplit, 6px straddling the border
  //
  // ONE WIDTH for both kinds of tab, as --insp-w on the root element and
  // "inspWidth" in the layout (applied at boot): the two inspectors are
  // the same column in the same place, so a width chosen in one holds in
  // the other. Those Go fields' editors follow by themselves (editor.js
  // mini: automaticLayout).
  //
  // The bar is the inspector's sibling, not its child: the inspector's
  // content is replaced on every redraw. The stylesheet does all the
  // clamping (.pinsp's min-width and flex-shrink against .pcanvas's
  // min-width), so a width dragged past a bound simply stops there, and a
  // window made narrower later is clamped the same way; the release stores
  // the width the column actually got, so --insp-w never holds more than
  // is shown.
  const INSP_VAR = "--insp-w";
  function setInspectorWidth(px) {
    // never negative: a negative flex-basis is invalid, and the property
    // would fall back to auto, sizing the inspector to its content
    document.documentElement.style.setProperty(INSP_VAR, Math.max(0, Math.round(px)) + "px");
  }
  function saveInspectorWidth(value) {
    dbc.putLayout({ inspWidth: value }).catch((err) => dbc.log("warn", "layout not saved: " + err.message));
  }

  // inspectorSplit wires bar, the edge beside inspector insp.
  function inspectorSplit(bar, insp) {
    bar.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      const startX = e.clientX, startW = insp.getBoundingClientRect().width;
      let moved = false;
      bar.setPointerCapture(e.pointerId);
      bar.classList.add("dragging");
      document.body.classList.add("insp-dragging");
      // the inspector is right of its bar: dragging left grows it
      const move = (m) => {
        if (Math.abs(m.clientX - startX) > 2) moved = true;
        if (moved) setInspectorWidth(startW - (m.clientX - startX));
      };
      const up = () => {
        bar.removeEventListener("pointermove", move);
        bar.removeEventListener("pointerup", up);
        bar.removeEventListener("pointercancel", up);
        bar.classList.remove("dragging");
        document.body.classList.remove("insp-dragging");
        // a bare click must not turn the default into a fixed width
        if (!moved) return;
        const w = insp.getBoundingClientRect().width;
        setInspectorWidth(w);
        saveInspectorWidth(String(Math.round(w)));
      };
      bar.addEventListener("pointermove", move);
      bar.addEventListener("pointerup", up);
      bar.addEventListener("pointercancel", up);
    });
    // double-click: back to the stylesheet's width; "" because the layout
    // has no delete
    bar.addEventListener("dblclick", () => {
      document.documentElement.style.removeProperty(INSP_VAR);
      saveInspectorWidth("");
    });
  }

  dbc.inspector = {
    split: inspectorSplit,
    // boot: the saved layout, before any canvas tab is drawn
    boot(layout) {
      if (Number(layout.inspWidth) > 0) setInspectorWidth(Number(layout.inspWidth));
    },
  };
})();
