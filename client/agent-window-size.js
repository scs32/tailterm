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
// a stale callback does not enqueue a new claim for an ineligible pane. A claim
// already in transport cannot be cancelled: if it sizes the window after its
// pane hid, closed or changed identity, one restore puts back the size read
// before the claim and drops authority, unless a newer viewer has claimed.
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
          const next = {
            ...s,
            key,
            token: token(),
            cols: s.cols,
            rows: s.rows,
          };
          // Read the host revision before sending a conditional claim. A newer
          // viewer advances it even if it later releases (no empty-token ABA).
          // Recheck lifecycle after the read, before any mutating command.
          const version = focusVersion;
          try {
            // A competing in-flight claim may win this compare-and-swap.
            // Retry once only while this exact focus/lifecycle remains current;
            // hidden or superseded lifecycle callbacks never retry a mutation.
            for (let attempt = 0; attempt < 2; attempt++) {
              const fresh = () => {
                const current = snapshot();
                return (
                  !disposed &&
                  eligibleAgentViewport(current) &&
                  identity(current) === key &&
                  focusVersion === version
                );
              };
              if (!fresh()) break;
              const response = (
                await execute(
                  agentWindowSizeCommand(
                    { ...next, action: "inspect" },
                    next.path,
                  ),
                )
              ).trim();
              const ready =
                /^ready:([a-zA-Z0-9_-]{16,64})?(?::(\d+)x(\d+))?$/.exec(
                  response,
                );
              if (!ready)
                throw new Error("Agent pane sizing was refused by the host.");
              if (!fresh()) break;
              const current = snapshot();
              claim = {
                ...next,
                cols: current.cols,
                rows: current.rows,
                expectedRevision: ready[1] || "",
              };
              const outcome = await command(claim, "claim");
              if (outcome === "superseded") continue;
              const after = snapshot();
              if (
                outcome === "sized" &&
                ready[2] &&
                (disposed ||
                  !eligibleAgentViewport(after) ||
                  identity(after) !== key)
              ) {
                const stale = claim;
                claim = null;
                await command(
                  { ...stale, cols: +ready[2], rows: +ready[3] },
                  "restore",
                );
              }
              break;
            }
          } catch (e) {
            failed = true;
            error(e);
          }
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
      if (!["sized", "released", "restored", "superseded"].includes(response))
        throw new Error("Agent pane sizing was refused by the host.");
      // Keep a superseded token locally: resize must not reclaim authority.
      return response;
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
