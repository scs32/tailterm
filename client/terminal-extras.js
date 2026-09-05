export function terminalText(term) {
  const lines = [],
    buffer = term.buffer.active;
  let pending = "";
  for (let i = 0; i < buffer.length; i++) {
    const line = buffer.getLine(i);
    if (!line) continue;
    if (!line.isWrapped && i) {
      lines.push(pending);
      pending = "";
    }
    pending += line.translateToString(!buffer.getLine(i + 1)?.isWrapped);
  }
  lines.push(pending);
  return lines.join("\n").replace(/\n+$/, "") + "\n";
}
export function downloadBlob(blob, name) {
  const url = URL.createObjectURL(blob),
    a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export function diagnosticStage(message = "", connected = false) {
  if (/fingerprint|host key|host verification/i.test(message))
    return "SSH host verification";
  if (/authenticat|sign.in|password|private key/i.test(message))
    return "SSH authentication";
  if (/tmux|session no longer exists/i.test(message)) return "Tmux session";
  if (/tailnet|tailscale|WASM/i.test(message)) return "Tailscale";
  return connected ? "Connected" : "SSH connection";
}
export function showDiagnostics({ tab, server, netState, peers, dialog }) {
  dialog(
    "Connection diagnostics",
    '<dl id="diagnostic-facts"></dl><details class="dialog-details"><summary>Troubleshooting</summary><p>Resolve host verification or sign-in errors before reconnecting. If a tmux session is missing, choose another from the launcher.</p></details>',
  );
  const facts = document.querySelector("#diagnostic-facts");
  for (const [label, value] of [
    [
      "Server",
      server
        ? `${server.name} (${server.username}@${server.host}:${server.port})`
        : "No server selected",
    ],
    ["Tailscale", `${netState}; ${peers.length} visible devices`],
    ["Browser network", navigator.onLine ? "Online" : "Offline"],
    ["Terminal", tab?.status || "No terminal open"],
    ["Stage", diagnosticStage(tab?.lastError, tab?.status === "Connected")],
    ["Last error", tab?.lastError || "None recorded"],
    [
      "Tmux",
      tab?.tmux
        ? `${tab.session}${tab.target ? ` (${tab.target.id}, created ${tab.target.created})` : " (identity not verified yet)"}`
        : "Not in use",
    ],
    ["Retry", tab?.retryMessage || "None scheduled"],
  ]) {
    const dt = document.createElement("dt"),
      dd = document.createElement("dd");
    dt.textContent = label;
    dd.textContent = value;
    facts.append(dt, dd);
  }
}
