import { agentWindowSizeCommand } from "../shared/tmux-command.js";

export function visibleAgentViewport(el) {
  if (!el?.isConnected) return false;
  for (let node = el; node; node = node.parentElement) {
    const style = getComputedStyle(node);
    if (
      node.hidden ||
      style.display === "none" ||
      ["hidden", "collapse"].includes(style.visibility)
    )
      return false;
  }
  const rect = el.getBoundingClientRect();
  return rect.width > 0 && rect.height > 0;
}

export function eligibleAgentViewport(s) {
  return !!(
    s.staticMode &&
    s.connected &&
    s.tmux &&
    s.verified &&
    s.ignoreSize &&
    s.binding?.runId &&
    s.binding?.role !== "owner_helper" &&
    !s.home &&
    !s.disposed &&
    !s.locked &&
    s.mode === "terminals" &&
    s.active &&
    s.foreground &&
    s.visible &&
    s.cols > 0 &&
    s.rows > 0 &&
    s.target
  );
}

// A focus transition creates a fresh token even at identical dimensions. Resize
// only updates an existing claim. One command in flight coalesces layout bursts;
// a stale callback never revives a disconnected/hidden/rebound pane.
export function createAgentWindowSizer({
  snapshot,
  execute,
  error = () => {},
  token = () => crypto.randomUUID(),
  schedule = queueMicrotask,
}) {
  let claim = null,
    pending = false,
    running = false,
    disposed = false;
  let focusVersion = 0,
    seenFocus = -1,
    wasEligible = false,
    failed = false;
  const identity = (s) => JSON.stringify([s.target, s.binding, s.connection]);
  function refresh({ focus = false } = {}) {
    if (disposed) return;
    if (focus) {
      focusVersion++;
      failed = false;
    }
    pending = true;
    schedule(drain);
  }
  async function drain() {
    if (running || !pending) return;
    running = true;
    try {
      while (pending) {
        pending = false;
        const s = snapshot(),
          eligible = !disposed && eligibleAgentViewport(s);
        const key = identity(s);
        if (
          claim &&
          (!eligible || claim.key !== key || seenFocus !== focusVersion)
        ) {
          const previous = claim;
          claim = null;
          await command(previous, "release");
          pending = true; // recompute after the await; never reuse this snapshot
          continue;
        }
        if (!eligible) {
          wasEligible = false;
          failed = false;
          continue;
        }
        if (!wasEligible) {
          focusVersion++;
          failed = false;
        }
        wasEligible = true;
        if (!claim && !failed) {
          seenFocus = focusVersion;
          claim = { ...s, key, token: token(), cols: s.cols, rows: s.rows };
          await command(claim, "claim");
          pending = true;
        } else if (claim && (claim.cols !== s.cols || claim.rows !== s.rows)) {
          claim.cols = s.cols;
          claim.rows = s.rows;
          await command(claim, "resize");
          pending = true;
        }
      }
    } finally {
      running = false;
    }
  }
  async function command(c, action) {
    try {
      const response = (
        await execute(agentWindowSizeCommand({ ...c, action }, c.path))
      ).trim();
      if (!["sized", "released", "superseded"].includes(response))
        throw new Error("Agent pane sizing was refused by the host.");
      // Keep a superseded token locally: resize must not reclaim authority.
    } catch (e) {
      failed = true;
      error(e);
      // Retain the token so a later hide can release an uncertain claim.
    }
  }
  return {
    refresh,
    dispose() {
      disposed = true;
      pending = true;
      schedule(drain);
    },
    async settled() {
      while (pending || running) await new Promise((r) => setTimeout(r, 0));
    },
  };
}
