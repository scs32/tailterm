import test from "node:test";
import assert from "node:assert/strict";
import {
  isAuthenticationFailure,
  findPeer,
  discoveryMode,
  connectionTransport,
} from "../client/connection-help.js";
test("authentication failures are distinguished from transport failures", () => {
  assert.equal(
    isAuthenticationFailure(
      "SSH Connection Error: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none], no supported methods remain",
    ),
    true,
  );
  assert.equal(
    isAuthenticationFailure("All configured authentication methods failed"),
    true,
  );
  assert.equal(isAuthenticationFailure("Connection timed out"), false);
});
test("discovery defaults to automatic login and matches tailnet addresses", () => {
  const peer = {
    name: "box.example.ts.net.",
    addresses: ["100.70.80.90/32"],
    tailscaleSSHEnabled: false,
  };
  assert.equal(discoveryMode(peer), "auto");
  assert.equal(discoveryMode({ ...peer, tailscaleSSHEnabled: true }), "auto");
  for (const host of ["box", "BOX.EXAMPLE.TS.NET", "100.70.80.90"])
    assert.equal(findPeer([peer], host), peer);
  assert.equal(findPeer([peer], "other.example.ts.net"), undefined);
});

test("automatic login selects a transport without sending keys through browser WASM", () => {
  const s = { host: "box.example.ts.net", port: 22, mode: "auto" };
  const enabled = { name: s.host, addresses: [], tailscaleSSHEnabled: true };
  assert.equal(connectionTransport(s, [], false), "login");
  assert.equal(connectionTransport(s, [enabled], true), "wasm");
  assert.equal(
    connectionTransport(s, [{ ...enabled, tailscaleSSHEnabled: false }], true),
    "ssh",
  );
  assert.equal(
    connectionTransport({ ...s, mode: "ssh" }, [enabled], true),
    "ssh",
  );
  assert.equal(
    connectionTransport({ ...s, port: 2222 }, [enabled], true),
    "ssh",
  );
  assert.equal(connectionTransport({ ...s, keyId: "saved" }, [], false), "ssh");
  assert.equal(
    connectionTransport({ ...s, hasPassword: true }, [], false),
    "ssh",
  );
  assert.equal(
    connectionTransport({ ...s, host: "192.168.1.20" }, [], false),
    "ssh",
  );
});
