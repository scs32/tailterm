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
export function hasNewText(before, after) {
  if (!before) return false;
  const previous = new Set(before);
  return after.some((line) => !previous.has(line));
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
