import test from "node:test";
import assert from "node:assert/strict";
import {
  PaneGroups,
  prune,
  leaves,
  tileLayout,
} from "../client/pane-layout.js";
import {
  workspaceSnapshot,
  normalizeWorkspace,
} from "../client/workspace-state.js";

test("filtering a mixed group projects its layout without removing hidden panes", () => {
  const model = new PaneGroups();
  model.sync(["a", "b", "c"]);
  model.merge("b", "a");
  model.merge("c", "b", { axis: "y" });
  const before = structuredClone(model.groups);
  const filtered = prune(model.group("a").tree, new Set(["a", "c"]));
  assert.deepEqual(leaves(filtered), ["a", "c"]);
  assert.equal(tileLayout(filtered, 1200, 800).panes.length, 2);
  assert.deepEqual(model.groups, before);
  assert.equal(prune(model.group("a").tree, new Set()), null);
  assert.deepEqual(leaves(model.group("a").tree), ["a", "b", "c"]);
});

test("workspace remembers multiple or zero server filters with All as the legacy default", () => {
  for (const filter of [null, [], ["production", "development"]]) {
    const snapshot = workspaceSnapshot(
      [],
      [],
      null,
      filter === null ? null : new Set(filter),
    );
    assert.deepEqual(normalizeWorkspace(snapshot).serverFilter, filter);
  }
  assert.equal(normalizeWorkspace({ tabs: [] }).serverFilter, null);
  assert.deepEqual(
    normalizeWorkspace({
      tabs: [],
      serverFilter: ["production", 1, null, "production", "development"],
    }).serverFilter,
    ["production", "development"],
  );
});
