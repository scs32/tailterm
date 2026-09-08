import test from "node:test";
import assert from "node:assert/strict";
import {
  profileMaster,
  profileKeys,
  normalizeUsername,
} from "../client/profile-crypto.js";
test("profile encryption is portable, authenticated, and separate from authentication credentials", async () => {
  const password = "A long test passphrase",
    instance = "profilehub_0123456789abcdef";
  const master = await profileMaster(password, "alice");
  assert.equal(master.extractable, false);
  const a = await profileKeys(master, "alice", instance),
    b = await profileKeys(
      await profileMaster(password, "alice"),
      "alice",
      instance,
    );
  assert.equal(a.token, b.token);
  const payload = {
    servers: [{ password: "private secret" }],
    keys: [],
    sessions: [],
  };
  const envelope = await a.seal(payload);
  assert.ok(!JSON.stringify(envelope).includes("private secret"));
  assert.deepEqual(await b.open(envelope), payload);
  const otherHub = await profileKeys(
    master,
    "alice",
    "profilehub_1111111111111111",
  );
  assert.notEqual(otherHub.token, a.token);
  await assert.rejects(otherHub.open(envelope));
  const wrong = await profileKeys(
    await profileMaster("A different passphrase", "alice"),
    "alice",
    instance,
  );
  assert.notEqual(wrong.token, a.token);
  await assert.rejects(wrong.open(envelope));
  const otherUser = await profileKeys(
    await profileMaster(password, "bob"),
    "bob",
    instance,
  );
  assert.notEqual(otherUser.token, a.token);
  await assert.rejects(otherUser.open(envelope));
  const tampered = {
    ...envelope,
    ciphertext:
      (envelope.ciphertext[0] === "A" ? "B" : "A") +
      envelope.ciphertext.slice(1),
  };
  await assert.rejects(b.open(tampered));
  assert.equal(normalizeUsername(" Alice "), "alice");
});
