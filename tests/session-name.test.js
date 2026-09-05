import test from "node:test";
import assert from "node:assert/strict";
import { resolveSessionName } from "../client/session-name.js";
test("blank tmux names get unique random identifiers and supplied names are preserved", () => {
  const names = Array.from({ length: 100 }, () => resolveSessionName(""));
  assert.equal(new Set(names).size, 100);
  for (const name of names) assert.match(name, /^tt-[a-f0-9]{16}$/);
  assert.match(resolveSessionName("   "), /^tt-[a-f0-9]{16}$/);
  assert.equal(resolveSessionName("my-session"), "my-session");
  assert.throws(() => resolveSessionName("bad'; id"));
});
