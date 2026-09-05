import { createHash } from "node:crypto";
export const hostFingerprint = (raw) =>
  "SHA256:" +
  createHash("sha256").update(raw).digest("base64").replace(/=+$/, "");

// Credentials are selected only after SSH has verified the destination host key.
export function interactiveSSHConfig(
  server,
  { keys, ask, rememberHost, rememberCredentials },
) {
  let keyTried = false,
    passwordTried = false,
    challenges = 0,
    pendingCredential = null;
  const username = server.username;
  const keyboard = (name, instructions, language, prompts, finish) => {
    if (prompts.length > 16) {
      finish([]);
      return;
    }
    ask("keyboard", {
      name,
      instructions,
      prompts: prompts.map((p) => ({ prompt: p.prompt, echo: p.echo })),
    })
      .then((answer) => {
        if (
          !Array.isArray(answer.answers) ||
          answer.answers.length !== prompts.length ||
          answer.answers.some((x) => typeof x !== "string" || x.length > 4096)
        )
          throw new Error("Invalid authentication response");
        finish(answer.answers);
      })
      .catch(() => finish([]));
  };
  const config = {
    host: server.host,
    port: server.port,
    username,
    readyTimeout: 180000,
    keepaliveInterval: 15000,
    hostVerifier(raw, callback) {
      const fingerprint = hostFingerprint(raw);
      if (server.fingerprint) {
        callback(server.fingerprint === fingerprint);
        return;
      }
      ask("host-key", { host: server.host, port: server.port, fingerprint })
        .then((answer) => {
          if (answer.accept !== true) {
            callback(false);
            return;
          }
          rememberHost(fingerprint);
          server.fingerprint = fingerprint;
          callback(true);
        })
        .catch(() => callback(false));
    },
    authHandler(methods, partial, callback) {
      if (!partial) pendingCredential = null;
      if (methods === null) {
        callback({ type: "none", username });
        return;
      }
      const key = keys().find((k) => k.id === server.keyId);
      if (!keyTried && key && methods.includes("publickey")) {
        keyTried = true;
        callback({
          type: "publickey",
          username,
          key: key.privateKey,
          passphrase: key.passphrase,
        });
        return;
      }
      if (!passwordTried && server.password && methods.includes("password")) {
        passwordTried = true;
        callback({ type: "password", username, password: server.password });
        return;
      }
      if (++challenges > 3) {
        callback(false);
        return;
      }
      const offered = methods.filter((m) =>
        ["password", "publickey", "keyboard-interactive"].includes(m),
      );
      if (!offered.length) {
        callback(false);
        return;
      }
      if (offered.length === 1 && offered[0] === "keyboard-interactive") {
        callback({ type: "keyboard-interactive", username, prompt: keyboard });
        return;
      }
      ask("credentials", {
        username,
        host: server.host,
        methods: offered,
        partial: !!partial,
        retry: challenges > 1 || keyTried || passwordTried,
        keys: keys().map(({ id, name, fingerprint }) => ({
          id,
          name,
          fingerprint,
        })),
      })
        .then((answer) => {
          if (
            answer.method === "password" &&
            offered.includes("password") &&
            typeof answer.password === "string" &&
            answer.password.length <= 4096
          ) {
            pendingCredential = answer.remember
              ? { password: answer.password }
              : null;
            callback({ type: "password", username, password: answer.password });
          } else if (
            answer.method === "publickey" &&
            offered.includes("publickey")
          ) {
            const selected = keys().find((k) => k.id === answer.keyId);
            if (!selected) throw new Error("Select a saved SSH key");
            pendingCredential = { keyId: selected.id };
            callback({
              type: "publickey",
              username,
              key: selected.privateKey,
              passphrase: selected.passphrase,
            });
          } else if (
            answer.method === "keyboard-interactive" &&
            offered.includes("keyboard-interactive")
          ) {
            callback({
              type: "keyboard-interactive",
              username,
              prompt: keyboard,
            });
          } else callback(false);
        })
        .catch(() => callback(false));
    },
  };
  return {
    config,
    onReady() {
      if (pendingCredential) rememberCredentials(pendingCredential);
      pendingCredential = null;
    },
  };
}
