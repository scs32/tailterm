import test from "node:test";
import assert from "node:assert/strict";
import {
  createCredentialCache,
  CREDENTIAL_IDLE_MS,
} from "../client/credential-cache.js";
import { createInactivityLock } from "../client/inactivity.js";
import { normalizeAppearance } from "../client/appearance.js";
test("temporary credentials expire after the final connection's reconnect window", () => {
  let time = 0;
  const cache = createCredentialCache(() => time);
  const first = cache.acquire("host");
  first.set({ password: "temporary" });
  const second = cache.acquire("host");
  first.release();
  first.release();
  time += CREDENTIAL_IDLE_MS * 2;
  cache.prune();
  assert.equal(
    second.get().password,
    "temporary",
    "active connections retain their credential",
  );
  second.release();
  time += CREDENTIAL_IDLE_MS - 1;
  const reconnect = cache.acquire("host");
  assert.equal(reconnect.get().password, "temporary");
  reconnect.release();
  time += CREDENTIAL_IDLE_MS;
  assert.equal(cache.acquire("host").get(), null);
});
test("lock invalidates active leases and late authentication callbacks", () => {
  const cache = createCredentialCache();
  const old = cache.acquire("host");
  old.set({ password: "old" });
  cache.clear();
  old.set({ password: "late" });
  assert.equal(old.get(), null);
  const fresh = cache.acquire("host");
  assert.equal(fresh.get(), null);
  fresh.set({ password: "new" });
  old.release();
  assert.equal(fresh.get().password, "new");
});
test("background discovery cannot keep temporary sign-ins alive indefinitely", () => {
  let time = 0;
  const cache = createCredentialCache(() => time);
  const terminal = cache.acquire("host");
  terminal.set({ password: "temporary" });
  terminal.release();
  time = CREDENTIAL_IDLE_MS - 1;
  const background = cache.acquire("host", { retain: false });
  assert.equal(background.get().password, "temporary");
  background.set({ password: "refresh" });
  background.release();
  time++;
  assert.equal(cache.acquire("host").get(), null);
});
test("resume locks synchronously before the first interaction can renew an expired deadline", async () => {
  let time = 0,
    calls = 0,
    finish;
  const idle = createInactivityLock({
    enabled: () => true,
    minutes: () => 5,
    now: () => time,
    lock: () => {
      calls++;
      return new Promise((resolve) => {
        finish = resolve;
      });
    },
  });
  time = 299999;
  assert.equal(idle.check(), false);
  time = 300000;
  assert.equal(idle.activity(), true);
  assert.equal(calls, 1);
  assert.equal(idle.check(), true);
  assert.equal(idle.activity(), true);
  assert.equal(calls, 1, "pending lock cannot be reentered");
  finish();
  await new Promise((resolve) => setImmediate(resolve));
  idle.reset();
  assert.equal(idle.check(), false);
});
test("interaction extends an unexpired deadline; locked screens do not trigger locking", () => {
  let time = 0,
    enabled = false,
    calls = 0;
  const idle = createInactivityLock({
    enabled: () => enabled,
    minutes: () => 5,
    now: () => time,
    lock: () => calls++,
  });
  time = 900000;
  assert.equal(idle.check(), false);
  enabled = true;
  idle.reset();
  time += 200000;
  assert.equal(idle.activity(), false);
  time += 200000;
  assert.equal(idle.check(), false);
  time += 100000;
  assert.equal(idle.check(), true);
  assert.equal(calls, 1);
  for (const value of [0, -1, 1440, "never", NaN])
    assert.equal(normalizeAppearance({ idleMinutes: value }).idleMinutes, 15);
  assert.equal(normalizeAppearance({ idleMinutes: "30" }).idleMinutes, 30);
});
