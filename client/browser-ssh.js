import { loginPrompt } from "./login-prompts.js";
import {
  credentials,
  privateKey,
  localData,
  rememberCredential,
} from "./local-vault.js";
import { findPeer, isAuthenticationFailure } from "./connection-help.js";
import { tmuxListCommand, tmuxRenameCommand } from "../shared/tmux-command.js";
const activeCredentials = new Map();

export function browserSSH(ipn, server, peers, options = {}) {
  const abort = new AbortController();
  let socket,
    closed = false,
    connected = false,
    size = { rows: options.rows || 24, cols: options.cols || 80 };
  const interactive = options.interactive !== false;
  const peer = findPeer(peers, server.host);
  let authentication =
    server.mode !== "ssh" && server.port === 22 && peer?.tailscaleSSHEnabled
      ? "tailscale"
      : "standard";
  let credential = {};
  const saved = credentials(server.id);
  if (saved.key) credential = { ...saved.key, keyId: saved.key.id };
  if (saved.server.password) credential.password = saved.server.password;
  const cacheKey = JSON.stringify([
    server.id,
    server.host,
    server.port,
    server.username,
  ]);
  if (activeCredentials.has(cacheKey))
    credential = activeCredentials.get(cacheKey);
  let remember = false,
    outputError,
    exitCode;
  const ask = async (retry) => {
    if (!interactive)
      throw new Error(
        "Sign in to this server once before checking sessions in the background.",
      );
    const answer = await loginPrompt(
      {
        kind: "credentials",
        host: server.host,
        username: server.username,
        methods: ["publickey", "password", "keyboard-interactive"],
        keys: localData().keys,
        retry,
      },
      abort.signal,
    );
    if (answer.cancel) throw new Error("SSH sign-in cancelled.");
    credential = {};
    remember = !!answer.remember;
    if (answer.method === "publickey") {
      const key = privateKey(answer.keyId);
      if (!key) throw new Error("The selected SSH key was removed.");
      credential = { ...key, keyId: key.id };
    } else if (answer.method === "password")
      credential.password = answer.password;
  };
  const attempt = () =>
    new Promise((resolve, reject) => {
      if (closed) {
        reject(new Error("Connection closed."));
        return;
      }
      outputError = undefined;
      exitCode = undefined;
      let finished = false;
      try {
        const handle = ipn.ssh(server.host, server.username, {
          ...size,
          port: server.port,
          authentication,
          privateKey: credential.privateKey || "",
          passphrase: credential.passphrase || "",
          password: credential.password || "",
          fingerprint: credentials(server.id).server.fingerprint || "",
          pty: options.pty !== false,
          command: options.command || "",
          upload: options.upload,
          verifyHost: async (fingerprint) => {
            if (!interactive)
              throw new Error(
                "Verify this SSH host by connecting before background discovery.",
              );
            options.onProgress?.("Verify host");
            const answer = await loginPrompt(
              {
                kind: "host-key",
                host: server.host,
                port: server.port,
                fingerprint,
              },
              abort.signal,
            );
            if (!answer.accept || closed) return false;
            await rememberCredential(server.id, { fingerprint }, server);
            return true;
          },
          ...(interactive
            ? {
                keyboardInteractive: async (challenge) => {
                  options.onProgress?.("Signing in");
                  const answer = await loginPrompt(
                    {
                      kind: "keyboard",
                      ...challenge,
                      host: server.host,
                      username: server.username,
                    },
                    abort.signal,
                  );
                  if (answer.cancel) throw new Error("SSH sign-in cancelled.");
                  return answer.answers;
                },
              }
            : {}),
          writeFn: (data) => {
            if (!closed) options.onData?.(data);
          },
          stderrFn: (data) => {
            if (!closed) (options.onStderr || options.onData)?.(data);
          },
          writeErrorFn: (error) => {
            outputError = String(error);
          },
          onBanner: (message) => {
            if (closed) return;
            // Authentication banners are untrusted text, not terminal commands.
            const text = String(message)
              .slice(0, 16384)
              .replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "")
              .replace(/\r?\n/g, "\r\n");
            if (interactive)
              options.onData?.(new TextEncoder().encode(text + "\r\n"));
            options.onWarning?.(text);
          },
          setReadFn: (fn) => {
            if (!closed) options.onInput?.(fn);
          },
          onConnectionProgress: (message) => {
            if (!closed) options.onProgress?.(message);
          },
          onConnected: () => {
            if (closed) {
              socket?.close();
              return;
            }
            connected = true;
            if (authentication === "standard")
              activeCredentials.set(cacheKey, { ...credential });
            const changes = {};
            if (remember && credential.password)
              changes.password = credential.password;
            if (credential.keyId) changes.keyId = credential.keyId;
            if (Object.keys(changes).length)
              void rememberCredential(server.id, changes, server)
                .then(() => options.onCredentialsSaved?.())
                .catch((e) => options.onWarning?.(e.message));
            options.onConnected?.();
          },
          onExit: (code) => {
            exitCode = code;
          },
          onDone: () => {
            finished = true;
            socket = undefined;
            options.onInput?.(null);
            resolve();
          },
        });
        if (!finished) socket = handle;
      } catch (e) {
        reject(e);
      }
    });
  const done = (async () => {
    try {
      if (
        !interactive &&
        authentication === "standard" &&
        (!saved.server.fingerprint ||
          (!credential.privateKey && !credential.password))
      )
        throw new Error(
          "Connect to this server and sign in before checking sessions in the background.",
        );
      if (
        interactive &&
        authentication === "standard" &&
        !credential.privateKey &&
        !credential.password
      )
        await ask(false);
      for (let retry = 0; retry < 4 && !closed; retry++) {
        await attempt();
        if (closed) return;
        if (!outputError) return { exitCode: exitCode ?? 0 };
        if (outputError.includes("SSH sign-in cancelled."))
          throw new Error(
            "SSH sign-in cancelled. Reconnect to choose an SSH key, password, or interactive login.",
          );
        if (
          connected ||
          (!isAuthenticationFailure(outputError) &&
            !outputError.includes("invalid private key"))
        )
          throw new Error(outputError);
        if (retry === 3) throw new Error(outputError);
        if (authentication === "tailscale") authentication = "standard";
        await ask(
          retry > 0 || !!credential.password || !!credential.privateKey,
        );
      }
    } finally {
      abort.abort();
      options.onDone?.();
    }
  })();
  return {
    done,
    close() {
      closed = true;
      abort.abort();
      socket?.close();
      options.onInput?.(null);
    },
    resize(rows, cols) {
      size = { rows, cols };
      socket?.resize(rows, cols);
    },
  };
}
export async function browserTmux(ipn, server, peers) {
  let output = "",
    stderr = "";
  const decoder = new TextDecoder(),
    errDecoder = new TextDecoder();
  const session = browserSSH(ipn, server, peers, {
    pty: false,
    interactive: false,
    command: tmuxListCommand(server.tmuxPath),
    onData: (chunk) => {
      output += decoder.decode(chunk, { stream: true });
      if (output.length > 65536) session.close();
    },
    onStderr: (chunk) => {
      stderr += errDecoder.decode(chunk, { stream: true });
      if (stderr.length > 65536) session.close();
    },
  });
  const timer = setTimeout(() => session.close(), 25000);
  try {
    const result = await session.done;
    output += decoder.decode();
    stderr += errDecoder.decode();
    if (!result)
      throw new Error(
        "Remote session check timed out or exceeded the output limit.",
      );
    if (result.exitCode !== 0) {
      if (
        /no server running|no sessions|error connecting to .*\(No such file or directory\)/i.test(
          stderr,
        )
      )
        return { sessions: [] };
      throw new Error(stderr.trim() || "Could not list tmux sessions.");
    }
    return {
      sessions: output
        .split(/\r?\n/)
        .filter(Boolean)
        .map((line) => {
          const [name, windows, attached, id, created] = line.split("|");
          return {
            name,
            windows: Number(windows) || 0,
            attached: Number(attached) || 0,
            ...(id && created ? { target: { id, created } } : {}),
          };
        }),
    };
  } finally {
    clearTimeout(timer);
    session.close();
  }
}

export async function browserRenameTmux(
  ipn,
  server,
  peers,
  name,
  nextName,
  target,
) {
  let stderr = "";
  const decoder = new TextDecoder();
  const session = browserSSH(ipn, server, peers, {
    pty: false,
    interactive: false,
    command: tmuxRenameCommand(name, nextName, server.tmuxPath, target),
    onData: () => {},
    onStderr: (chunk) => {
      stderr += decoder.decode(chunk, { stream: true });
      if (stderr.length > 65536) session.close();
    },
  });
  const timer = setTimeout(() => session.close(), 25000);
  try {
    const result = await session.done;
    stderr += decoder.decode();
    if (!result)
      throw new Error(
        "Rename was not confirmed. Refresh the session list before trying again.",
      );
    if (result.exitCode !== 0)
      throw new Error(stderr.trim() || "Could not rename the tmux session.");
  } finally {
    clearTimeout(timer);
    session.close();
  }
}
export async function browserCommand(
  ipn,
  server,
  peers,
  command,
  maxOutput = 65536,
) {
  let output = "",
    stderr = "",
    overflow = false,
    receivedBytes = 0;
  const decoder = new TextDecoder(),
    errors = new TextDecoder();
  const session = browserSSH(ipn, server, peers, {
    pty: false,
    interactive: false,
    command,
    onData: (d) => {
      receivedBytes += d.byteLength;
      output += decoder.decode(d, { stream: true });
      if (receivedBytes > maxOutput) {
        overflow = true;
        session.close();
      }
    },
    onStderr: (d) => {
      stderr += errors.decode(d, { stream: true });
      if (stderr.length > 65536) session.close();
    },
  });
  const timer = setTimeout(() => session.close(), 25000);
  try {
    const result = await session.done;
    if (overflow) throw new Error("Remote output exceeded the size limit.");
    output += decoder.decode();
    stderr += errors.decode();
    if (!result || result.exitCode !== 0)
      throw new Error(stderr.trim() || "Remote command failed or timed out.");
    return output;
  } finally {
    clearTimeout(timer);
    session.close();
  }
}
export function browserUpload(ipn, server, peers, file, path, progress) {
  return browserSSH(ipn, server, peers, {
    pty: false,
    interactive: false,
    upload: {
      path,
      size: file.size,
      progress,
      readChunk: async (offset, length) =>
        new Uint8Array(await file.slice(offset, offset + length).arrayBuffer()),
    },
  });
}
