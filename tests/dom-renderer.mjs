// Browser suites read terminal text from xterm's DOM rows. GPU rendering
// draws to a canvas instead, so these suites opt into the DOM renderer.
// tests/renderer-browser.mjs covers the WebGL path on its own.
export function useDomRenderer(target) {
  return target.addInitScript(() => {
    globalThis.__tailserveDomRenderer = true;
  });
}
