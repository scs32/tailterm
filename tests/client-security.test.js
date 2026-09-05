import test from "node:test";
import assert from "node:assert/strict";
import {
  deriveVaultKey,
  sealVault,
  openVault,
  portableData,
} from "../client/vault-crypto.js";
import {
  decodeClipboard,
  fontShortcut,
  sanitizePaste,
} from "../client/terminal-input.js";
import { normalizeAppearance } from "../client/appearance.js";
test("browser vault encrypts, authenticates metadata, rejects tampering and excludes node identity from portable backups", async () => {
  const password = "local vault test passphrase",
    salt = crypto.getRandomValues(new Uint8Array(16));
  const data = {
    servers: [{ password: "secret" }],
    keys: [{ privateKey: "private ssh key" }],
    sessions: [],
    tailscale: { node: "private node identity" },
  };
  const key = await deriveVaultKey(password, salt);
  assert.equal(key.extractable, false);
  const envelope = await sealVault(data, key, salt),
    second = await sealVault(data, key, salt);
  assert.notEqual(envelope.iv, second.iv);
  assert.doesNotMatch(
    JSON.stringify(envelope),
    /secret|private ssh|private node/,
  );
  assert.deepEqual((await openVault(envelope, password)).data, data);
  await assert.rejects(openVault(envelope, "incorrect passphrase"));
  await assert.rejects(openVault({ ...envelope, iterations: 1 }, password));
  await assert.rejects(
    openVault(
      { ...envelope, ciphertext: "AAAA" + envelope.ciphertext.slice(4) },
      password,
    ),
  );
  assert.deepEqual(portableData(data).tailscale, {});
  assert.equal(data.tailscale.node, "private node identity");
});
test("OSC 52 preserves UTF-8, rejects malformed/oversized payloads and never supplies clipboard reads", () => {
  assert.equal(
    decodeClipboard("c;" + Buffer.from("tmux → café 🦊").toString("base64")),
    "tmux → café 🦊",
  );
  for (const value of [
    "c;?",
    "c;@@@@",
    "c;/w==",
    "?;YQ==",
    "c;" + "A".repeat(1400004),
  ])
    assert.equal(decodeClipboard(value), null);
});
test("font shortcuts preserve plain shell plus/minus and preference validation constrains stored values", () => {
  assert.equal(fontShortcut({ key: "+" }), null);
  assert.equal(fontShortcut({ key: "-" }), null);
  assert.equal(fontShortcut({ key: "=", ctrlKey: true }), 1);
  assert.equal(fontShortcut({ key: "-", metaKey: true }), -1);
  assert.equal(fontShortcut({ key: "0", ctrlKey: true }), 0);
  const p = normalizeAppearance({
    theme: "injected",
    font: "unknown",
    fontSize: 100,
    cursorStyle: "bad",
    lineHeight: -2,
    padding: "bad",
    remoteClipboard: "false",
  });
  assert.equal(p.fontSize, 32);
  assert.equal(p.theme, "tailserve");
  assert.equal(p.lineHeight, 1);
  assert.equal(p.padding, 16);
  assert.equal(p.cursorStyle, "block");
  assert.equal(p.remoteClipboard, true);
});

test("paste preserves Unicode and line structure but cannot inject an early bracketed-paste terminator", () => {
  assert.equal(sanitizePaste("café\tline\nnext\r"), "café\tline\nnext\r");
  assert.equal(sanitizePaste("\x1b[201~\x03\x00command"), "[201~command");
});
