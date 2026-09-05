export function paneDirection(event) {
  if (
    event.metaKey ||
    !event.altKey ||
    !(Boolean(event.ctrlKey) !== Boolean(event.shiftKey)) ||
    event.isComposing
  )
    return null;
  return (
    {
      ArrowLeft: "left",
      ArrowRight: "right",
      ArrowUp: "up",
      ArrowDown: "down",
    }[event.key] || null
  );
}
export function setupPaneShortcuts({ enabled, navigate }) {
  window.addEventListener(
    "keydown",
    (event) => {
      const direction = paneDirection(event);
      if (!direction || !enabled() || document.querySelector("dialog[open]"))
        return;
      const target = event.target;
      if (
        target?.closest?.('input,select,textarea,[contenteditable="true"]') &&
        !target.classList?.contains("xterm-helper-textarea")
      )
        return;
      event.preventDefault();
      event.stopImmediatePropagation();
      navigate(direction);
    },
    true,
  );
}
