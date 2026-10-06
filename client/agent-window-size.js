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
// before the claim and drops authority, unless a newer viewer has claimed. A
// resize in transport likewise: if it lands after its pane hid, the size this
// claim last set while visible is sent again, then the claim is released.
// A claim or resize whose reply is lost may have landed and is treated as if it
// had. A retired claim is kept until the host answers its undo and release: one
// immediate retry, then error() and another attempt when the pane's state next
// changes. Every command is conditional on the token, so none of this can
// overwrite a newer viewer.
export function createAgentWindowSizer({
  snapshot,
  execute,
  error = () => {},
  token = () => crypto.randomUUID(),
  schedule = queueMicrotask,
}) {
  let claim = null,
    owed = [],
    owedMark = null,
    pending = false,
    running = false,
    disposed = false;
  let focusVersion = 0,
    seenFocus = -1,
    wasEligible = false,
    failed = false;
  const identity = (s) => JSON.stringify([s.target, s.binding, s.connection]);
  // True once the pane a command was sent for hid, closed or changed identity.
  const stale = (key) => {
    const s = snapshot();
    return disposed || !eligibleAgentViewport(s) || identity(s) !== key;
  };
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
          (claim.undo ||
            !eligible ||
            claim.key !== key ||
            seenFocus !== focusVersion)
        ) {
          owed.push(claim);
          claim = null;
          owedMark = null;
        }
        // Unconfirmed teardowns wait for a focus or lifecycle change, so layout
        // bursts over a dead link do not repeat them.
        const mark = JSON.stringify([eligible, key, focusVersion, disposed]);
        if (owed.length && owedMark !== mark) {
          owedMark = mark;
          const waiting = owed;
          owed = [];
          for (const c of waiting) if (!(await teardown(c))) owed.push(c);
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
                prior: ready[2] && { cols: +ready[2], rows: +ready[3] },
              };
              const outcome = await command(claim, "claim");
              if (outcome === "superseded") continue;
              // A lost reply (no outcome) may be a claim that landed.
              if (!stale(key))
                claim.sized = { cols: claim.cols, rows: claim.rows };
              else if (claim.prior) claim.undo = "restore";
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
          const outcome = await command(claim, "resize");
          // A lost reply (no outcome) may be a resize that landed.
          if (outcome !== "superseded") {
            if (!stale(key))
              claim.sized = { cols: claim.cols, rows: claim.rows };
            // The next pass retires the claim and puts the visible size back.
            else if (claim.sized) claim.undo = "resize";
            else if (claim.prior) claim.undo = "restore";
          }
          pending = true;
        }
      }
    } finally {
      running = false;
    }
  }
  // Undo what landed after the pane went stale, then drop authority. False
  // while the host has not answered a step; the claim is then kept.
  async function teardown(c) {
    if (c.undo === "restore") return confirmed({ ...c, ...c.prior }, "restore");
    if (c.undo === "resize") {
      // The same token sends the size it last set while visible, so a newer
      // viewer still wins.
      if (!(await confirmed({ ...c, ...c.sized }, "resize"))) return false;
      c.undo = null;
    }
    return confirmed(c, "release");
  }
  // One immediate retry; only the last failure is reported. It does not stop
  // the visible pane from claiming again.
  async function confirmed(c, action) {
    return !!(
      (await command(c, action, () => {})) || (await command(c, action, error))
    );
  }
  async function command(c, action, report = fail) {
    try {
      const response = (
        await execute(agentWindowSizeCommand({ ...c, action }, c.path))
      ).trim();
      if (!["sized", "released", "restored", "superseded"].includes(response))
        throw new Error("Agent pane sizing was refused by the host.");
      // Keep a superseded token locally: resize must not reclaim authority.
      return response;
    } catch (e) {
      report(e);
      // Retain the token so a later hide can release an uncertain claim.
    }
  }
  function fail(e) {
    failed = true;
    error(e);
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
