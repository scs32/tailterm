export function screenLines(term, tmux = false) {
  const buffer = term.buffer.active,
    lines = [];
  // The usual bottom tmux status line changes clocks/counters independently of output.
  for (let row = 0; row < term.rows - (tmux ? 1 : 0); row++) {
    const text = (
      buffer.getLine(buffer.baseY + row)?.translateToString(true) || ""
    ).trim();
    if (/[\p{L}\p{N}]/u.test(text)) lines.push(text);
  }
  return lines;
}

export function activitySnapshot(term, tmux = false) {
  const buffer = term.buffer.active;
  return {
    lines: screenLines(term, tmux),
    baseY: buffer.baseY,
    cursorY: buffer.cursorY,
    cursorX: buffer.cursorX,
    cursorLine: (
      buffer.getLine(buffer.baseY + buffer.cursorY)?.translateToString(true) ||
      ""
    ).trim(),
  };
}

export function hasNewText(before, after) {
  if (!before) return false;
  const beforeLines = Array.isArray(before) ? before : before.lines || [];
  const afterLines = Array.isArray(after) ? after : after.lines || [];
  const previous = new Set(beforeLines);
  if (!afterLines.some((line) => !previous.has(line))) return false;

  // Older callers only supplied rendered lines. Snapshots add enough terminal
  // position context to distinguish output from an application repainting a
  // spinner, progress value, clock, or other fixed row in place.
  if (Array.isArray(before) || Array.isArray(after)) return true;
  const beforeCursor = before.baseY + before.cursorY;
  const afterCursor = after.baseY + after.cursorY;
  if (after.baseY > before.baseY || afterCursor > beforeCursor) return true;
  if (
    afterCursor === beforeCursor &&
    after.cursorX > before.cursorX &&
    after.cursorLine !== before.cursorLine &&
    after.cursorLine.startsWith(before.cursorLine)
  )
    return true;
  if (afterLines.length > beforeLines.length) return true;

  // tmux can scroll its visible region without advancing xterm's own scrollback.
  // A retained line moving upward, followed by novel text, is still real output.
  return afterLines.some((line, index) => {
    const oldIndex = beforeLines.indexOf(line);
    return oldIndex > index;
  });
}
export function activityTitle(tabs) {
  const pending = tabs.filter((t) => !t.disposed && t.activity);
  if (!pending.length) return "Tailterm · Your servers, one workspace";
  const priority = [
    "Needs attention",
    "Bell",
    "Command finished",
    "New output",
  ];
  const first = [...pending].sort(
    (a, b) => priority.indexOf(a.activity) - priority.indexOf(b.activity),
  )[0];
  return `(${pending.length}) ${first.activity} · Tailterm`;
}

export function connectionNeedsAttention(tab, waitingForNetwork = false) {
  return (
    ["Error", "Disconnected"].includes(tab.status) &&
    !waitingForNetwork &&
    !/^Reconnecting(?:\s|\.)/.test(tab.retryMessage || "")
  );
}
