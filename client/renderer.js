import { WebglAddon } from "@xterm/addon-webgl";

// GPU rendering draws the grid from a glyph atlas instead of styled DOM rows.
// Contexts are scarce (Chrome allows roughly sixteen per page), so each
// terminal holds one only while it is visible and releases it when hidden.
const MAX_CONTEXT_LOSSES = 3;
let supported;
export function webglSupported() {
  // Browser suites that scrape DOM rows opt out through this test hook.
  if (globalThis.__tailserveDomRenderer) return false;
  if (supported === undefined) {
    try {
      const canvas = document.createElement("canvas");
      supported = !!canvas.getContext("webgl2");
    } catch {
      supported = false;
    }
  }
  return supported;
}
export function createRenderer(term, element, { enabled }) {
  let addon = null,
    disposed = false,
    losses = 0,
    retry = null,
    fontsPending = null;
  const visible = () => element.clientWidth > 0 && element.clientHeight > 0;
  function attach() {
    if (addon || disposed || fontsPending) return;
    if (!enabled() || !webglSupported() || losses >= MAX_CONTEXT_LOSSES) return;
    if (!visible()) return;
    // The atlas is rasterized from whichever font is loaded at first render.
    const pending = (fontsPending = document.fonts.ready);
    pending.then(() => {
      if (fontsPending !== pending) return;
      fontsPending = null;
      if (addon || disposed || !enabled() || !visible()) return;
      const next = new WebglAddon();
      try {
        next.onContextLoss(() => {
          losses++;
          detach();
          // Lost contexts are usually background eviction; try again shortly.
          retry = setTimeout(attach, 1000);
        });
        term.loadAddon(next);
        addon = next;
      } catch {
        // Context creation failed; the DOM renderer stays in place.
        losses = MAX_CONTEXT_LOSSES;
        next.dispose();
      }
    });
  }
  function detach() {
    clearTimeout(retry);
    retry = null;
    fontsPending = null;
    const current = addon;
    addon = null;
    // Disposing the addon restores xterm's DOM renderer.
    current?.dispose();
  }
  const observer = new ResizeObserver(() => {
    if (visible()) attach();
    else detach();
  });
  observer.observe(element);
  attach();
  return {
    // Call after font or theme option changes; the atlas must not keep glyphs
    // rasterized before a newly selected font finished loading.
    update() {
      if (!enabled()) return detach();
      attach();
      document.fonts.ready.then(() => {
        if (addon && !disposed) term.clearTextureAtlas();
      });
    },
    active: () => !!addon,
    dispose() {
      disposed = true;
      observer.disconnect();
      detach();
    },
  };
}
