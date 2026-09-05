export function isAuthenticationFailure(error) {
  return /unable to authenticate|all configured authentication methods failed|no supported methods remain/i.test(
    String(error),
  );
}
export function findPeer(peers, host) {
  const normalize = (value) => String(value).toLowerCase().replace(/\.$/, "");
  const target = normalize(host);
  return peers.find(
    (peer) =>
      normalize(peer.name) === target ||
      normalize(peer.name).split(".")[0] === target ||
      (peer.addresses || []).some(
        (address) => address.split("/")[0] === target,
      ),
  );
}
export function discoveryMode() {
  return "auto";
}
export function isTailnetHost(server) {
  const host = server.host.toLowerCase().replace(/\.$/, "");
  const parts = host.split(".").map(Number);
  return (
    !!server.tailnet ||
    host.endsWith(".ts.net") ||
    /^fd7a:115c:a1e0:/i.test(host) ||
    (parts.length === 4 &&
      parts.every((n) => Number.isInteger(n) && n >= 0 && n <= 255) &&
      parts[0] === 100 &&
      parts[1] >= 64 &&
      parts[1] <= 127)
  );
}
export function connectionTransport(server, peers, running) {
  if (server.mode === "ssh" || server.port !== 22) return "ssh";
  const peer = findPeer(peers, server.host);
  if (peer?.tailscaleSSHEnabled === false) return "ssh";
  if (
    running &&
    (peer?.tailscaleSSHEnabled ||
      server.mode === "wasm" ||
      isTailnetHost(server))
  )
    return "wasm";
  if (
    !running &&
    (server.mode === "wasm" || isTailnetHost(server)) &&
    !server.keyId &&
    !server.hasPassword
  )
    return "login";
  return "ssh";
}
