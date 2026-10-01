import test from "node:test";
import assert from "node:assert/strict";
import { activityDetail, activityLabel, providerBlockDetail, runtimePromptDetail, tokenSnapshot } from "../client/activity-format.js";

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

test("runtime prompt state names the prompt, the policy action and the outcome", () => {
  const escalated = {state:"runtime_prompt", observedAt:"2026-09-28T20:00:00Z", tokens:{total:5}, prompt:{kind:"claude_permission", action:"escalate", outcome:"escalated"}};
  assert.equal(activityLabel(escalated), "Runtime prompt");
  assert.equal(activityDetail(escalated, Date.parse("2026-09-28T20:00:10Z")), "Runtime prompt · Claude permission dialog: ask the owner · escalated · observed 10s ago · Last transition snapshot: 5 tokens");
  assert.equal(runtimePromptDetail({kind:"codex_rate_limit_switch", action:"keep_current_never_show", outcome:"confirmed", reason:"selected the policy option"}), "Codex rate-limit menu: keep current model, never show again · confirmed (selected the policy option)");
  assert.equal(runtimePromptDetail({kind:"codex_new_thing", action:"escalate", outcome:"escalated"}), "Unrecognized runtime prompt: ask the owner · escalated");
  assert.equal(runtimePromptDetail(null), "");
});

test("stuck state says why the agent cannot make progress", () => {
  const stuck = {state:"stuck", reason:"Claude wake unconfirmed for 4m (retry 2/4): retry 2/4: Enter on own unsubmitted text", observedAt:"2026-09-29T21:00:00Z", tokens:{total:7}};
  assert.equal(activityLabel(stuck), "Stuck");
  assert.equal(activityDetail(stuck, Date.parse("2026-09-29T21:00:05Z")), "Stuck: Claude wake unconfirmed for 4m (retry 2/4): retry 2/4: Enter on own unsubmitted text · observed 5s ago · Last transition snapshot: 7 tokens");
  // Other states keep their reason out of the summary, as before.
  assert.equal(activityDetail({state:"unknown", reason:"turn boundary unavailable", observedAt:"2026-09-29T21:00:00Z", tokens:{total:7}}, Date.parse("2026-09-29T21:00:05Z")), "Unknown · observed 5s ago · Last transition snapshot: 7 tokens");
});

test("provider blocked names the provider, class, model and since, apart from stalls", () => {
  const since = "2026-09-26T18:53:10Z";
  const local = new Date(since);
  const hhmm = `${String(local.getHours()).padStart(2, "0")}:${String(local.getMinutes()).padStart(2, "0")}`;
  const blocked = {state:"provider_blocked", observedAt:"2026-09-26T19:00:00Z", tokens:{total:9}, provider:{provider:"anthropic", runtime:"claude", model:"claude-fable-5-1", class:"usage_limit", code:"rate_limit", status:429, since}};
  assert.equal(activityLabel(blocked), "Provider blocked");
  assert.equal(activityDetail(blocked, Date.parse("2026-09-26T19:00:05Z")), `Provider blocked: Anthropic usage limit · claude-fable-5-1 · since ${hhmm} · observed 5s ago · Last transition snapshot: 9 tokens`);
  assert.match(providerBlockDetail(blocked.provider), /^Anthropic usage limit · claude-fable-5-1 · since \d\d:\d\d$/);
  assert.equal(providerBlockDetail({provider:"openai", model:"gpt-6-astra", class:"auth", since}), `OpenAI authentication failure · gpt-6-astra · since ${hhmm}`);
  assert.match(providerBlockDetail({provider:"openai", model:"gpt-6-astra", class:"rate_limited", since}), /^OpenAI rate limit · /);
  assert.match(providerBlockDetail({provider:"anthropic", model:"claude-opus-5-5", class:"server_error", since}), /^Anthropic server error · /);
  // Unknown values never echo raw strings, and a missing time is left out.
  assert.equal(providerBlockDetail({provider:"x<y>", class:"new_class"}), "Provider failure · unknown");
  assert.equal(providerBlockDetail(null), "");
  // It reads differently from the stall states.
  const now = Date.parse("2026-09-26T19:00:05Z");
  const stuck = activityDetail({state:"stuck", reason:"pane 16x1 below minimum 80x24", observedAt:"2026-09-26T19:00:00Z", tokens:{total:9}}, now);
  const hung = activityDetail({state:"hung_tool", pendingTool:"exec_command", observedAt:"2026-09-26T19:00:00Z", tokens:{total:9}}, now);
  for (const other of [stuck, hung]) assert.doesNotMatch(other, /Provider blocked|Anthropic/);
  assert.doesNotMatch(activityDetail(blocked, now), /Stuck|Hung tool/);
});
