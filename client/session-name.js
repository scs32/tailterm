import { validateSession } from "../shared/tmux-command.js";
export function resolveSessionName(value = "") {
  const name =
    value.trim() ||
    "tt-" +
      Array.from(crypto.getRandomValues(new Uint8Array(8)), (byte) =>
        byte.toString(16).padStart(2, "0"),
      ).join("");
  validateSession(name);
  return name;
}
