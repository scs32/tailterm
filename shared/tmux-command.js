export function validateSession(name) {
  if (!/^[a-zA-Z0-9_-]{1,64}$/.test(name))
    throw new Error(
      "Use 1–64 letters, numbers, underscores or dashes for a session name.",
    );
}
export function validateTmuxPath(path = "") {
  if (
    typeof path !== "string" ||
    path.length > 512 ||
    /[\x00-\x1f\x7f]/.test(path) ||
    (path && !path.startsWith("/"))
  )
    throw new Error("The tmux executable must be an absolute path.");
}
export const shellQuote = (value) => "'" + value.replace(/'/g, "'\\''") + "'";
function resolver(path) {
  validateTmuxPath(path);
  if (path)
    return `tailterm_tmux_bin=${shellQuote(path)}; if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then printf 'Configured tmux executable is not executable: %s\\n' "$tailterm_tmux_bin" >&2; exit 127; fi; `;
  return `tailterm_tmux_bin=$(command -v tmux 2>/dev/null || :); if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then for tailterm_tmux_candidate in /opt/homebrew/bin/tmux /usr/local/bin/tmux /usr/bin/tmux /bin/tmux /opt/local/bin/tmux /run/current-system/sw/bin/tmux /home/linuxbrew/.linuxbrew/bin/tmux "$HOME/.nix-profile/bin/tmux" "$HOME/.local/bin/tmux"; do if [ -f "$tailterm_tmux_candidate" ] && [ -x "$tailterm_tmux_candidate" ]; then tailterm_tmux_bin=$tailterm_tmux_candidate; break; fi; done; fi; if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then printf 'Tailterm could not locate tmux in this SSH environment. It may be installed outside PATH. Set its absolute executable path in Edit server.\\nSSH PATH: %s\\n' "$PATH" >&2; exit 127; fi; `;
}
export function tmuxCommand(name, path = "", resumeOnly = false) {
  validateSession(name);
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        clipboardFeatures() +
        (resumeOnly ? exactSession(name) : "") +
        `"$tailterm_tmux_bin" $tailterm_tmux_features ${resumeOnly ? 'attach-session -t "$tailterm_tmux_target"' : "new-session -A -s " + shellQuote(name)} \\; if-shell -F '#{==:#{set-clipboard},off}' 'set-option -s set-clipboard external' \\; set-option mouse on; tailterm_tmux_status=$?; if [ "$tailterm_tmux_status" -ne 0 ]; then printf 'tmux failed with exit status %s; see its error above.\\n' "$tailterm_tmux_status" >&2; fi; exit "$tailterm_tmux_status"`,
    )
  );
}
function clipboardFeatures() {
  // -T is available in tmux 3.2+. Older tmux uses the terminal's Ms capability.
  // Do not overwrite an existing 'on' clipboard policy or modify .tmux.conf.
  return `tailterm_tmux_features='-T clipboard'; case "$("$tailterm_tmux_bin" -V 2>/dev/null)" in 'tmux 0.'*|'tmux 1.'*|'tmux 2.'*|'tmux 3.0'*|'tmux 3.1'|'tmux 3.1a'|'tmux 3.1b'|'tmux 3.1c') tailterm_tmux_features='' ;; esac; `;
}
export function tmuxListCommand(path = "") {
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        `exec "$tailterm_tmux_bin" list-sessions -F '#{session_name}|#{session_windows}|#{session_attached}'`,
    )
  );
}

function exactSession(name) {
  return `tailterm_tmux_target=$("$tailterm_tmux_bin" list-sessions -F '#{session_name}|#{session_id}' | while IFS='|' read -r tailterm_tmux_name tailterm_tmux_id; do if [ "$tailterm_tmux_name" = ${shellQuote(name)} ]; then printf '%s' "$tailterm_tmux_id"; break; fi; done); if [ -z "$tailterm_tmux_target" ]; then printf 'That tmux session no longer exists. Start a new session from the launcher.\\n' >&2; exit 1; fi; `;
}
