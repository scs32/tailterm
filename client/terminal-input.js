export function decodeClipboard(sequence) {
  const separator = sequence.indexOf(";");
  if (separator < 0) return null;
  const selection = sequence.slice(0, separator),
    encoded = sequence.slice(separator + 1);
  // Never answer remote clipboard read queries. Bound allocations and reject malformed data.
  if (
    !/^[cps0-7]*$/.test(selection) ||
    encoded === "?" ||
    encoded.length > 1398104 ||
    encoded.length % 4 !== 0 ||
    /[^A-Za-z0-9+/=]/.test(encoded)
  )
    return null;
  try {
    const bytes = Uint8Array.from(atob(encoded), (c) => c.charCodeAt(0));
    if (bytes.length > 1024 * 1024) return null;
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    return null;
  }
}
export function sanitizePaste(text) {
  // Prevent embedded ESC from ending bracketed paste early. Preserve tabs and newlines.
  return text.replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, "");
}
export function fontShortcut(e) {
  if (!(e.metaKey || e.ctrlKey) || e.altKey) return null;
  if (["+", "="].includes(e.key) || e.code === "NumpadAdd") return 1;
  if (e.key === "-" || e.code === "NumpadSubtract") return -1;
  if (e.key === "0" || e.code === "Numpad0") return 0;
  return null;
}
export function setupTerminalInput(
  t,
  { preferences, isActive, notice, setFont, changed, pasteImages },
) {
  const canFocus = () =>
    !t.disposed &&
    isActive(t) &&
    !t.el.querySelector(".local-history") &&
    !document.querySelector("dialog[open]");
  // Real movement, rather than a layout-generated pointerenter after closing a dialog.
  t.el.addEventListener(
    "pointermove",
    (e) => {
      if (
        e.pointerType === "mouse" &&
        preferences().focusFollowsMouse &&
        canFocus() &&
        !t.el.contains(document.activeElement)
      )
        t.term.focus();
    },
    { passive: true },
  );
  const paste = async (text) => {
    if (!text || t.disposed || !t.send) return;
    if (new TextEncoder().encode(text).byteLength > 1024 * 1024) {
      notice("Paste is limited to 1 MiB. Transfer a file for larger content.");
      return;
    }
    const cleaned = sanitizePaste(text);
    if (cleaned !== text)
      notice("Control characters were removed from the pasted text.");
    text = cleaned;
    if (/[\r\n]/.test(text)) {
      const approved = await pastePreview(text, t);
      if (!approved) return;
    }
    if (t.disposed || !isActive(t) || !t.send) {
      notice(
        "The destination terminal changed. Paste again in the intended session.",
      );
      return;
    }
    t.term.paste(text);
    t.term.focus();
  };
  t.copy = async () => {
    const text = t.term.getSelection() || t.clipboardText;
    if (!text) {
      notice("Select text with Shift + drag, or copy in tmux copy mode first.");
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      t.clipboardPending = false;
      changed();
      notice("Copied to clipboard.");
    } catch {
      clipboardFallback(text, "Copy terminal text");
    }
  };
  t.paste = async () => {
    t.history?.close();
    if (pasteImages && navigator.clipboard.read) {
      try {
        const items = await navigator.clipboard.read(),
          images = [];
        for (const item of items) {
          const type = item.types.find((type) => type.startsWith("image/"));
          if (type)
            images.push(
              new File(
                [await item.getType(type)],
                `clipboard-${Date.now()}-${images.length}.${type.split("/")[1].replace(/[^a-z0-9]/gi, "")}`,
                { type },
              ),
            );
        }
        if (images.length) {
          await pasteImages(images);
          return;
        }
      } catch {
        /* Text paste and the manual fallback remain available. */
      }
    }
    try {
      await paste(await navigator.clipboard.readText());
    } catch {
      const value = await clipboardFallback("", "Paste into terminal", true);
      if (value) await paste(value);
    }
  };
  // Capture the native paste event so keyboard, context menu and toolbar share one path.
  t.el.addEventListener(
    "paste",
    (e) => {
      if (e.target.closest?.(".local-history")) return;
      e.preventDefault();
      e.stopImmediatePropagation();
      void paste(e.clipboardData?.getData("text/plain") || "");
    },
    true,
  );
  t.el.addEventListener(
    "copy",
    (e) => {
      if (e.target.closest?.(".local-history")) return;
      const text = t.term.getSelection() || t.clipboardText;
      if (text) {
        e.preventDefault();
        e.stopImmediatePropagation();
        e.clipboardData.setData("text/plain", text);
      }
    },
    true,
  );
  // Terminal output cannot be cut; Cmd+X copies a selection without sending a shell command.
  t.term.attachCustomKeyEventHandler((e) => {
    if (e.type !== "keydown") return true;
    const font = fontShortcut(e);
    if (font !== null) {
      e.preventDefault();
      e.stopPropagation();
      setFont(font);
      return false;
    }
    const key = e.key.toLowerCase();
    if ((e.metaKey || (e.ctrlKey && e.shiftKey)) && ["c", "x"].includes(key)) {
      e.preventDefault();
      e.stopPropagation();
      void t.copy();
      return false;
    }
    if ((e.metaKey || (e.ctrlKey && e.shiftKey)) && key === "v") {
      // Let the browser emit its trusted paste event (works without readText permission).
      return false;
    }
    if (
      (e.metaKey || e.ctrlKey) &&
      e.shiftKey &&
      ["k", "l", "f", "p"].includes(key)
    )
      return false;
    return true;
  });
  t.el.addEventListener("pointerup", () => {
    if (preferences().copyOnSelect && t.term.hasSelection()) void t.copy();
  });
  t.term.parser.registerOscHandler(52, (sequence) => {
    const text = decodeClipboard(sequence);
    if (text === null || !preferences().remoteClipboard) return true;
    t.clipboardText = text;
    t.clipboardPending = true;
    changed();
    if (canFocus() && document.hasFocus())
      navigator.clipboard
        .writeText(text)
        .then(() => {
          if (t.clipboardText === text) t.clipboardPending = false;
          changed();
        })
        .catch(() => {
          notice(
            "tmux copied text. Click Copy to put it on your system clipboard.",
          );
        });
    return true;
  });
}
function clipboardFallback(text, title, editable = false) {
  return new Promise((resolve) => {
    const d = document.createElement("dialog");
    d.className = "clipboard-dialog";
    const h = document.createElement("h2");
    h.textContent = title;
    const p = document.createElement("p");
    p.textContent = editable
      ? "Paste here using your browser shortcut, then continue."
      : "Your browser blocked clipboard access. Copy the selected text using your browser shortcut.";
    const input = document.createElement("textarea");
    input.value = text;
    input.readOnly = !editable;
    input.rows = 8;
    input.setAttribute("aria-label", title);
    const button = document.createElement("button");
    button.textContent = editable ? "Continue" : "Done";
    button.className = "primary";
    const finish = (value) => {
      d.close();
      d.remove();
      resolve(value);
    };
    button.onclick = () => finish(editable ? input.value : null);
    d.oncancel = () => finish(null);
    const head = document.createElement("div");
    head.className = "dialog-head";
    h.id = "clipboard-title";
    d.setAttribute("aria-labelledby", h.id);
    const close = document.createElement("button");
    close.textContent = "×";
    close.setAttribute("aria-label", "Cancel clipboard action");
    close.onclick = () => finish(null);
    head.append(h, close);
    const actions = document.createElement("div");
    actions.className = "dialog-actions";
    actions.append(button);
    d.append(head, p, input, actions);
    (document.fullscreenElement || document.body).append(d);
    d.showModal();
    input.focus();
    if (!editable) input.select();
  });
}
function pastePreview(text, t) {
  return new Promise((resolve) => {
    const d = document.createElement("dialog");
    d.className = "clipboard-dialog";
    const title = document.createElement("h2");
    title.textContent = "Paste multiple lines?";
    const p = document.createElement("p");
    p.textContent = `${text.split(/\r\n|\r|\n/).length} lines → ${t.server.name} / ${t.tmux ? t.session : "shell"}. Newlines can execute commands; review before pasting.`;
    const preview = document.createElement("pre");
    preview.textContent =
      text.slice(0, 12000) +
      (text.length > 12000 ? "\n… preview truncated" : "");
    const actions = document.createElement("div");
    actions.className = "dialog-actions";
    const cancel = document.createElement("button");
    cancel.textContent = "Cancel";
    const approve = document.createElement("button");
    approve.className = "primary";
    approve.textContent = "Paste into this session";
    const finish = (value) => {
      d.close();
      d.remove();
      resolve(value);
    };
    cancel.onclick = () => finish(false);
    approve.onclick = () => finish(true);
    d.oncancel = () => finish(false);
    actions.append(cancel, approve);
    const head = document.createElement("div");
    head.className = "dialog-head";
    title.id = "paste-preview-title";
    d.setAttribute("aria-labelledby", title.id);
    const close = document.createElement("button");
    close.textContent = "×";
    close.setAttribute("aria-label", "Cancel paste");
    close.onclick = () => finish(false);
    head.append(title, close);
    d.append(head, p, preview, actions);
    (document.fullscreenElement || document.body).append(d);
    d.showModal();
    cancel.focus();
  });
}
