import test from "node:test";
import assert from "node:assert/strict";
import { createHubClient } from "../client/hub-client.js";

const response = (body, status = 200) => ({
  status,
  statusText: status >= 400 ? "error" : "OK",
  headers: {},
  text: async () => JSON.stringify(body),
});

// A fake fetchImpl that records every call and lets the test settle each one.
// A held call rejects with an AbortError when its init.signal aborts, as
// window.fetch and the ipn.fetch bridge do.
function fakeHub() {
  const calls = [];
  const fetchImpl = (url, init) =>
    new Promise((resolve, reject) => {
      const call = { url, init, resolve, reject, aborted: false };
      calls.push(call);
      init.signal?.addEventListener("abort", () => {
        call.aborted = true;
        reject(new DOMException("The operation was aborted.", "AbortError"));
      });
    });
  const client = createHubClient({
    fetchImpl,
    baseURL: "http://hub.test:8765",
    token: "t",
  });
  return { calls, client };
}

const settle = () => new Promise((resolve) => setImmediate(resolve));
const eventsCalls = (calls) =>
  calls.filter((call) => new URL(call.url).pathname.endsWith("/events"));

test("stop aborts the in-flight long poll and starts no further request", async () => {
  const { calls, client } = fakeHub();
  const errors = [];
  const sub = client.subscribe("tsk_one", () => {}, {
    onError: (error) => errors.push(error),
  });
  await settle();
  assert.equal(eventsCalls(calls).length, 1);
  const [held] = calls;
  assert.equal(new URL(held.url).searchParams.get("wait"), "25s");
  assert.ok(held.init.signal, "the long poll carries a signal");
  assert.equal(held.init.signal.aborted, false);
  let abortEvents = 0;
  held.init.signal.addEventListener("abort", () => abortEvents++);

  sub.stop();
  assert.equal(held.init.signal.aborted, true);
  assert.equal(abortEvents, 1, "the abort event fired once");
  assert.equal(held.aborted, true);
  sub.stop();
  assert.equal(abortEvents, 1, "a second stop changes nothing");
  await settle();
  await settle();
  assert.deepEqual(errors, [], "an abort after stop reports no error");
  assert.equal(calls.length, 1, "no further request after stop");
});

test("a normal batch reaches onEvents with the next cursor and polls again", async () => {
  const { calls, client } = fakeHub();
  const batches = [];
  const sub = client.subscribe(
    "",
    (events, cursor) => batches.push([events, cursor]),
    { after: 4 },
  );
  await settle();
  assert.equal(new URL(calls[0].url).pathname, "/v1/events");
  assert.equal(new URL(calls[0].url).searchParams.get("after"), "4");
  calls[0].resolve(response({ events: [{ seq: 5, kind: "post" }], next: 5 }));
  await settle();
  assert.deepEqual(batches, [[[{ seq: 5, kind: "post" }], 5]]);
  assert.equal(sub.cursor(), 5);
  assert.equal(calls.length, 2);
  assert.equal(new URL(calls[1].url).searchParams.get("after"), "5");
  assert.notEqual(
    calls[1].init.signal,
    calls[0].init.signal,
    "each poll has its own signal",
  );
  sub.stop();
  assert.equal(calls[1].init.signal.aborted, true);
  assert.equal(
    calls[0].init.signal.aborted,
    false,
    "a finished poll is not aborted",
  );
});

test("stop during backoff makes no further request", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const { calls, client } = fakeHub();
  const errors = [];
  const sub = client.subscribe("tsk_one", () => {}, {
    onError: (error) => errors.push(error),
  });
  await settle();
  calls[0].resolve(response({ error: "down" }, 503));
  await settle();
  assert.equal(errors.length, 1, "the failure is reported before backoff");
  assert.equal(calls.length, 1, "backing off, not polling");

  sub.stop();
  await settle();
  t.mock.timers.tick(120000);
  await settle();
  await settle();
  assert.equal(calls.length, 1, "no request after stop during backoff");
  assert.equal(errors.length, 1);
});

test("backoff without stop polls again after the delay", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const { calls, client } = fakeHub();
  const sub = client.subscribe("tsk_one", () => {}, { onError: () => {} });
  await settle();
  calls[0].resolve(response({ error: "down" }, 503));
  await settle();
  assert.equal(calls.length, 1);
  t.mock.timers.tick(1000);
  await settle();
  assert.equal(calls.length, 2, "the loop resumes after its backoff");
  sub.stop();
  assert.equal(calls[1].init.signal.aborted, true);
});

test("requests made without a signal carry none", async () => {
  const { calls, client } = fakeHub();
  const listed = client.listTasks();
  await settle();
  assert.equal(calls.length, 1);
  assert.equal("signal" in calls[0].init, false);
  calls[0].resolve(response({ tasks: [] }));
  await listed;

  const events = client.events("tsk_one", { after: 0, wait: "25s" });
  await settle();
  assert.equal("signal" in calls[1].init, false);
  assert.equal(calls[1].init.timeoutMs, 40000);
  calls[1].resolve(response({ events: [], next: 0 }));
  await events;

  const controller = new AbortController();
  const passed = client.events("tsk_one", { signal: controller.signal });
  await settle();
  assert.equal(calls[2].init.signal, controller.signal);
  controller.abort();
  await assert.rejects(passed, { name: "AbortError" });
});
