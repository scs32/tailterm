import { WebLinksAddon } from "@xterm/addon-web-links";

export function webURL(text) {
  try {
    const url = new URL(text);
    return ["http:", "https:"].includes(url.protocol) ? url.href : null;
  } catch {
    return null;
  }
}
function activate(event, text) {
  if (
    event.button !== 0 ||
    !(
      event.metaKey ||
      event.ctrlKey ||
      event.sourceCapabilities?.firesTouchEvents
    )
  )
    return;
  const url = webURL(text);
  if (!url) return;
  event.preventDefault();
  window.open(url, "_blank", "noopener,noreferrer");
}

export function setupTerminalLinks(term) {
  let hint;
  const leave = () => {
    hint?.remove();
    hint = null;
  };
  const handler = {
    activate,
    allowNonHttpProtocols: false,
    hover(event, text) {
      leave();
      if (!webURL(text)) return;
      hint = document.createElement("div");
      hint.className = "terminal-link-hint xterm-hover";
      hint.setAttribute("role", "tooltip");
      hint.textContent = `${/Mac|iPhone|iPad/.test(navigator.platform) ? "Command" : "Ctrl"}-click to open · ${text}`;
      hint.style.left =
        Math.max(
          8,
          Math.min(
            event.clientX,
            innerWidth - Math.min(520, innerWidth - 24) - 8,
          ),
        ) + "px";
      hint.style.top = Math.max(8, event.clientY - 64) + "px";
      term.element.append(hint);
    },
    leave,
  };
  term.options.linkHandler = handler;
  term.loadAddon(new WebLinksAddon(activate, handler));
  term.loadAddon({ activate() {}, dispose: leave });
}

// Cached history is plain text: create anchors as DOM nodes, never as HTML.
export function renderHistoryLinks(output, text) {
  const fragment = document.createDocumentFragment();
  let end = 0;
  for (const match of text.matchAll(/https?:\/\/[^\s<>"'`]+/gi)) {
    let candidate = match[0].replace(/[.,!?;:]+$/, "");
    for (const [open, close] of [
      ["(", ")"],
      ["[", "]"],
      ["{", "}"],
    ]) {
      while (
        candidate.endsWith(close) &&
        candidate.split(close).length > candidate.split(open).length
      )
        candidate = candidate.slice(0, -1);
    }
    const url = webURL(candidate);
    if (!url) continue;
    fragment.append(document.createTextNode(text.slice(end, match.index)));
    const a = document.createElement("a");
    a.href = url;
    a.textContent = candidate;
    a.target = "_blank";
    a.rel = "noopener noreferrer";
    fragment.append(a);
    end = match.index + candidate.length;
  }
  fragment.append(document.createTextNode(text.slice(end)));
  output.replaceChildren(fragment);
}
