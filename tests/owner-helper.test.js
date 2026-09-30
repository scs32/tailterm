import test from "node:test";
import assert from "node:assert/strict";
import { rosterActivity, rosterRole } from "../client/tasks-view.js";

test("the owner helper is labelled and shown offline, not by activity, when its session is gone", () => {
  const helper = {
    role: "owner_helper",
    online: false,
    activity: { state: "unknown", reason: "owner session offline" },
  };
  assert.equal(rosterRole(helper), " · Owner helper");
  assert.equal(rosterActivity(helper), "Offline");
  assert.equal(
    rosterActivity({ ...helper, online: true, activity: { state: "idle" } }),
    "Idle",
  );
});

test("other roster rows keep their labels", () => {
  assert.equal(rosterRole({ role: "database_handler" }), " · Database handler");
  assert.equal(rosterRole({ role: "" }), "");
  assert.equal(
    rosterActivity({ role: "", online: false, activity: { state: "working" } }),
    "Working",
  );
  assert.equal(
    rosterActivity({ role: "database_handler", online: false, activity: null }),
    "Activity unavailable",
  );
});
