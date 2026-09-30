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
