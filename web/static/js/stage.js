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
})();
