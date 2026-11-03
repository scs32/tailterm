import test from "node:test";
import assert from "node:assert/strict";
import { renderTeamDelivery } from "../client/team-delivery-view.js";

const entry = (release) => ({ itemId: "wi_fixture", state: "finished", acceptance: { branch: "feature/fixture", commit: "a".repeat(40) }, release: { id: "rel_fixture", verificationDigest: "d".repeat(64), ...release } });
const summary = (release) => renderTeamDelivery({ entries: [entry(release)] });

test("f7 a job blocked after publication still shows merged", () => {
  assert.match(summary({ state: "blocked", published: true }), /Accepted → verified → merged · blocked/);
  assert.ok(!summary({ state: "blocked" }).includes("→ merged"));
  assert.ok(!summary({ state: "claimed", integratedCommit: "b".repeat(40) }).includes("→ merged"));
});

test("a superseded job reads as released by hand, never merged by the deployer", () => {
  const html = summary({ state: "superseded", supersession: { releasedCommit: "c".repeat(40), release: "20260929-owner-helper-c6a8ec1" } });
  assert.match(html, /Accepted → verified → superseded \(released by hand\) · 20260929-owner-helper-c6a8ec1 @ c{40} · superseded/);
  assert.ok(!html.includes("→ merged"));
  assert.ok(summary({ state: "superseded", supersession: { releasedCommit: "c".repeat(40), release: "<script>" } }).includes("&lt;script&gt;"));
});

test("the receipt's push and tasks-hub revert are shown", () => {
  const commit = "e".repeat(40);
  assert.match(summary({ state: "released", receipt: { commit, outcome: "released", targets: [], push: { remote: "origin", commit, outcome: "pushed" } } }), /→ merged → released · released .* · push pushed/);
  assert.match(summary({ state: "rolled_back", published: true, receipt: { commit, outcome: "rolled_back", targets: [], revert: { commit: "f".repeat(40), outcome: "committed" } } }), /tasks-hub revert committed f{40}/);
});

// wi_2ecd30082f47de50: a failed push's diagnostics tell its outcomes apart;
// an older receipt with only remote, commit and outcome reads as before.
test("the push text tells a landed, timed-out and failed push apart", () => {
  const commit = "e".repeat(40);
  const row = (fields) => summary({ state: "released", receipt: { commit, outcome: "released", targets: [], push: { remote: "origin", commit, ...fields } } });
  const push = (fields) => row(fields).match(/ · (push [^<]*)<\/span>/)[1];
  const landed = { outcome: "failed", timedOut: true, exitCode: 128, elapsedMs: 300000, reason: "push-timed-out", remoteCheck: "landed", stderrSHA256: "a".repeat(64) };
  assert.equal(push(landed), "push landed after a failed attempt");
  assert.ok(!row(landed).includes("push failed") && !row(landed).includes("push timed out"));
  assert.equal(push({ outcome: "failed", remoteCheck: "landed", reason: "push-failed" }), "push landed after a failed attempt");
  assert.equal(push({ outcome: "failed", timedOut: true, elapsedMs: 300000, reason: "push-timed-out", remoteCheck: "absent" }), "push timed out · remote absent");
  assert.equal(push({ outcome: "failed", timedOut: true, reason: "push-timed-out" }), "push timed out");
  assert.equal(push({ outcome: "failed", exitCode: 1, reason: "push-failed", remoteCheck: "absent" }), "push failed · remote absent");
  assert.equal(push({ outcome: "failed", signal: "sigterm", reason: "push-killed", remoteCheck: "unknown" }), "push failed · remote unknown");
  assert.equal(push({ outcome: "failed" }), "push failed");
  assert.equal(push({ outcome: "pushed" }), "push pushed");
  const odd = push({ outcome: "failed", remoteCheck: "<img src=x>" });
  assert.equal(odd, "push failed · remote &lt;img src=x&gt;");
  assert.equal(push({ outcome: "<b>" }), "push &lt;b&gt;");
});

// wi_ade4aa60c5d9b55e: the Delivery panel shows whether a team has a plan review.
test("the frozen launch shows a plan review team or a plan-only team", () => {
  const member = (name, role) => ({ fields: { name, role } });
  const row = (entry) => renderTeamDelivery({ entries: [{ itemId: "wi_fixture", ...entry }] });
  const feature = row({ state: "running", launch: { members: [member("lead-1", "Delivery lead and orchestrator"), member("plan-reviewer-1", "Plan review"), member("builder-1", "Implementation")] } });
  assert.match(feature, /running · Plan review team/);
  assert.ok(!feature.includes("Plan-only team"));
  const bug = row({ state: "running", launch: { members: [member("lead-1", "Delivery lead and orchestrator"), member("builder-1", "Implementation")] } });
  assert.match(bug, /running · Plan-only team/);
  assert.ok(!bug.includes("Plan review team"));
  const queued = row({ state: "queued" });
  assert.ok(!queued.includes("Plan review team") && !queued.includes("Plan-only team"));
});

// wi_f6c458bfb60667c6: finished entries arrive as hub summaries without their
// launch; the row reads the team shape and summaries from the summary fields.
test("a finished summary entry renders its shape, reviews, verification and release", () => {
  const html = renderTeamDelivery({ entries: [{
    itemId: "wi_summary", state: "finished", summary: true, teamShape: "plan-review", tokens: { total: 7 },
    reviews: { history: "recorded", rounds: [{ number: 1 }, { number: 2 }], followUps: [{ itemId: "wi_follow", finding: { id: "F1", title: "Short title" } }], scopes: [], focused: [], disposition: { kind: "accepted" } },
    verification: { state: "passing", commit: "c".repeat(40), checks: [{ id: "a3", status: "fail", knownFailure: true }] },
    acceptance: { branch: "fix/summary", commit: "a".repeat(40) },
    release: { id: "rel_summary", state: "released", verificationDigest: "d".repeat(64), plan: {}, receipt: { outcome: "released", targets: [{ target: "hub", outcome: "deployed", release: "r1" }] } },
  }] });
  assert.match(html, /finished · Plan review team/);
  assert.match(html, /Reviews 2\/2 · Follow-ups 1 new, 0 folded · accepted/);
  assert.match(html, /wi_follow: Short title/);
  assert.match(html, /Verification passing · a3: fail · known failure/);
  assert.match(html, /Accepted → verified → merged → released · released · job rel_summary/);
  assert.ok(!html.includes("activity"));
  const planOnly = renderTeamDelivery({ entries: [{ itemId: "wi_bug", state: "finished", summary: true, teamShape: "plan-only" }] });
  assert.match(planOnly, /finished · Plan-only team/);
});

test("the panel says when older finished entries are not shown", () => {
  const entries = [{ itemId: "wi_running", state: "running" }, ...Array.from({ length: 50 }, (_, i) => ({ itemId: `wi_done${i}`, state: "finished", summary: true }))];
  const paged = renderTeamDelivery({ entries, history: { total: 120, limit: 50, nextAfter: 70 } });
  assert.match(paged, /Showing 50 of 120 finished/);
  const whole = renderTeamDelivery({ entries, history: { total: 50, limit: 50 } });
  assert.ok(!whole.includes("Showing"));
});

// wi_44b17e10c6450225 v4: only the current review stage counts against the
// limit. The numbers match TestSummarizeReviewsCountsCurrentStage in
// hub/cmd/tt/message_checks_test.go.
test("the review count is the current stage's, with earlier stages named", () => {
  const rounds = [{ number: 1, requestSeq: 10 }, { number: 2, requestSeq: 20 }, { number: 1, requestSeq: 40 }];
  const followUps = [1, 2, 3].map((n) => ({ itemId: `wi_follow${n}`, finding: { id: `F${n}`, title: `Title ${n}` } }));
  const stages = [{ number: 1, orderSeq: 3, scopeRevision: 1, assignmentSeq: 5 }, { number: 2, orderSeq: 25, scopeRevision: 2, assignmentSeq: 30 }];
  const text = (reviews) => renderTeamDelivery({ entries: [{ itemId: "wi_stage", state: "running", reviews: { history: "recorded", scopes: [], focused: [], ...reviews } }] }).match(/data-testid="review-convergence-summary">([^<]*)</)[1];
  assert.equal(text({ stages, rounds, followUps, disposition: { kind: "accept" } }), "Stage 2 · Reviews 1/2 · Earlier stages 2 · Follow-ups 3 new, 0 folded · accept");
  assert.equal(text({ stages: stages.slice(0, 1), rounds: rounds.slice(0, 2), followUps: followUps.slice(0, 1) }), "Reviews 2/2 · Follow-ups 1 new, 0 folded");
  assert.equal(text({ rounds: rounds.slice(0, 2), followUps: followUps.slice(0, 1) }), "Reviews 2/2 · Follow-ups 1 new, 0 folded");
  assert.equal(text({ stages, rounds: rounds.slice(0, 2), followUps: [] }), "Stage 2 · Reviews 0/2 · Earlier stages 2 · Follow-ups 0 new, 0 folded");
  assert.equal(renderTeamDelivery({ entries: [{ itemId: "wi_legacy", state: "running", reviews: { history: "unknown", stages, rounds, followUps } }] }).match(/data-testid="review-convergence-summary">([^<]*)</)[1], "Reviews unknown · Follow-ups unknown");
});

// wi_ad2010dd554a5d2e: a folded finding was saved on an existing item, so the
// row counts it apart from new follow-ups and names the item it went into,
// both for a finished summary entry and a running entry with full reviews.
test("folded follow-ups are counted apart and named as folded into their item", () => {
  const followUps = [{ itemId: "wi_new", finding: { id: "F1", title: "New one" } }, { itemId: "wi_target", folded: true, finding: { id: "F2", title: "Folded one" } }];
  const reviews = { history: "recorded", rounds: [{ number: 1 }], followUps, scopes: [], focused: [] };
  for (const entry of [{ itemId: "wi_done", state: "finished", summary: true, teamShape: "plan-only", reviews }, { itemId: "wi_running", state: "running", reviews: { ...reviews, stages: [] } }]) {
    const html = renderTeamDelivery({ entries: [entry] });
    assert.equal(html.match(/data-testid="review-convergence-summary">([^<]*)</)[1], "Reviews 1/2 · Follow-ups 1 new, 1 folded");
    assert.equal(html.match(/data-testid="review-follow-ups">([^<]*)</)[1], "wi_new: New one · folded into wi_target: Folded one");
  }
  const escaped = renderTeamDelivery({ entries: [{ itemId: "wi_esc", state: "running", reviews: { ...reviews, followUps: [{ itemId: "<b>", folded: true, finding: { id: "F3", title: "<i>x</i>" } }] } }] });
  assert.match(escaped, /folded into &lt;b&gt;: &lt;i&gt;x&lt;\/i&gt;/);
  assert.ok(!escaped.includes("<b>") && !escaped.includes("<i>"));
  assert.match(escaped, /Follow-ups 0 new, 1 folded/);
});

// wi_35e9f7f50b620957 a3: queued entries read as the up-next list, in the
// listing's order, and the header names the lanes only with a lane policy.
test("queued rows are numbered up next and the header shows the lanes", () => {
  const entries = [
    { itemId: "wi_running", state: "running" },
    { itemId: "wi_second", state: "queued", upNext: 1 },
    { itemId: "wi_third", state: "queued", upNext: 2 },
  ];
  const html = renderTeamDelivery({ concurrencyLimit: 4, entries });
  assert.deepEqual([...html.matchAll(/<strong>(?:<span data-testid="team-up-next">([^<]*)<\/span> · )?([^<]*)<\/strong>/g)].map((m) => [m[1] ?? "", m[2]]), [["", "wi_running"], ["Up next 1", "wi_second"], ["Up next 2", "wi_third"]]);
  assert.ok(!html.includes("team-delivery-lanes") && !html.includes("Lanes "));
  const lanes = (fields) => renderTeamDelivery({ concurrencyLimit: 4, entries, lanes: { policy: { day: 4, night: 3 }, window: "night", lanes: 3, active: 1, ...fields } }).match(/data-testid="team-delivery-lanes"[^>]*>([^<]*)</)[1];
  assert.equal(lanes({}), "Lanes 3 · night");
  assert.equal(lanes({ window: "day", lanes: 4 }), "Lanes 4 · day");
  assert.equal(lanes({ lanes: 2, strained: true, memoryPressure: 85 }), "Lanes 2 · night · strained");
  assert.equal(lanes({ hold: "a matrix run waits for the verification host (1 waiting)", matrixWaiters: 1 }), "Lanes 3 · night · held: matrix waits");
  assert.equal(lanes({ hold: "the verification host's matrix lock is not observed" }), "Lanes 3 · night · held: matrix lock not observed");
  const escaped = renderTeamDelivery({ entries, lanes: { window: "<b>", lanes: "<i>", hold: '"><img src=x>' } });
  assert.ok(!escaped.includes("<b>") && !escaped.includes("<i>") && !escaped.includes("<img"));
});

// wi_57c903a832b38b70 a4: the header shows the project's host schedule as it
// applies now, with who set or cleared it in the title; nothing without one.
test("the header shows the host schedule while active, expired or cleared", () => {
  const entries = [{ itemId: "wi_running", state: "running" }];
  const schedule = { reason: "release turns: team matrices start only on a handler grant", setBy: "agt_handler", setAt: "2026-11-03T08:00:00Z", expiresAt: "2026-11-03T12:00:00Z" };
  const render = (state, fields = {}) => renderTeamDelivery({ entries, lanes: { policy: { day: 3, night: 2, hostSchedule: { ...schedule, ...fields } }, window: "day", lanes: 3, active: 1, scheduleState: state } });
  const span = (html) => html.match(/<span class="fine" data-testid="team-delivery-host-schedule" title="([^"]*)">([^<]*)</);
  const active = span(render("active", { grants: [{ itemId: "wi_a", by: "agt_handler", at: "2026-11-03T08:05:00Z" }] }));
  assert.equal(active[2], "Host schedule · release turns: team matrices start only on a handler grant · until 2026-11-03T12:00:00Z · grants 1");
  assert.equal(active[1], "set by agt_handler at 2026-11-03T08:00:00Z; grants wi_a");
  assert.equal(span(render("active"))[2].endsWith("grants 0"), true);
  assert.equal(span(render("expired"))[2], "Host schedule expired");
  const cleared = span(render("cleared", { clearedBy: "owner", clearedAt: "2026-11-03T09:00:00Z", clearReason: "releases done" }));
  assert.equal(cleared[2], "Host schedule cleared by owner");
  assert.equal(cleared[1], "set by agt_handler at 2026-11-03T08:00:00Z; cleared at 2026-11-03T09:00:00Z: releases done");
  assert.ok(!renderTeamDelivery({ entries, lanes: { policy: { day: 3, night: 2 }, window: "day", lanes: 3 } }).includes("team-delivery-host-schedule"));
  const escaped = render("active", { reason: "<img src=x>", setBy: '"><b>' });
  assert.ok(!escaped.includes("<img") && !escaped.includes("<b>"));
});
