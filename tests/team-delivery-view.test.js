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
  assert.match(html, /Reviews 2\/2 · Follow-ups 1 · accepted/);
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
