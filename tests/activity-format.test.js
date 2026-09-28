import test from "node:test";
import assert from "node:assert/strict";
import { activityDetail, activityLabel, tokenSnapshot } from "../client/activity-format.js";

test("activity labels and last-transition usage remain explicit", () => {
  assert.equal(activityLabel({state:"hung_tool"}),"Hung tool");
  assert.equal(activityLabel(null),"Activity unavailable");
  assert.equal(tokenSnapshot({total:1234}),"Last transition snapshot: 1,234 tokens");
  assert.equal(activityDetail({state:"working",observedAt:"2026-09-25T20:00:00Z",tokens:{total:12}},Date.parse("2026-09-25T20:00:30Z")),"Working · observed 30s ago · Last transition snapshot: 12 tokens");
});

test("wake outcome is visible even while execution state stays idle", () => {
  const activity = {state:"idle", observedAt:"2026-09-25T20:00:00Z", tokens:{total:12}, wake:{status:"skipped", reason:"Claude input is occupied", messageSeqs:[41]}};
  assert.match(activityDetail(activity, Date.parse("2026-09-25T20:00:30Z")), /Idle · Claude wake skipped #41: Claude input is occupied/);
});
