import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, statSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { Vault, tmuxCommand, unseal } from "../server/vault.js";
test("vault encrypts secrets, rejects wrong passwords and tampering, and survives restart", () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "tailterm-vault-"));
  try {
    const file = path.join(dir, "vault.enc"),
      v = new Vault(file);
    v.unlock("a sufficiently long secret");
    v.data.keys.push({ privateKey: "SECRET PRIVATE KEY" });
    v.save();
    const raw = readFileSync(file, "utf8");
    assert.ok(!raw.includes("SECRET"));
    assert.equal(statSync(file).mode & 0o777, 0o600);
    const reopened = new Vault(file);
    assert.throws(() => reopened.unlock("wrong password"));
    reopened.unlock("a sufficiently long secret");
    assert.equal(reopened.data.keys[0].privateKey, "SECRET PRIVATE KEY");
    const tampered = JSON.parse(raw);
    tampered.tag = Buffer.alloc(16).toString("base64");
    assert.throws(() =>
      unseal(JSON.stringify(tampered), "a sufficiently long secret"),
    );
    v.lock();
    assert.equal(v.data, null);
    assert.equal(v.key, null);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
test("tmux names cannot inject shell commands", () => {
  for (const name of [
    "",
    "x'; touch /tmp/pwn",
    "$(id)",
    "a.b",
    "a:b",
    "x\ny",
    "a".repeat(65),
  ])
    assert.throws(() => tmuxCommand(name));
  assert.match(tmuxCommand("work-01_test"), /new-session -A -s .*work-01_test/);
});
