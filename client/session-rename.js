import { validateSession } from "../shared/tmux-command.js";

export function showSessionRename({ name, serverName, dialog, close, rename }) {
  dialog(
    "Rename session",
    `<p id="rename-destination"></p><form id="rename-session-form"><label for="rename-session-name">Session name</label><input id="rename-session-name" required maxlength="64" autocomplete="off" spellcheck="false"><p class="fine">1–64 letters, numbers, underscores or dashes.</p><p id="rename-session-error" role="alert" hidden></p><div class="dialog-actions"><button id="rename-session-cancel" type="button">Cancel</button><button id="rename-session-submit" class="primary">Rename session</button></div></form>`,
  );
  const modal = document.querySelector("#dialog");
  modal.querySelector("#rename-destination").textContent =
    `${serverName} · Updates every device. Running programs stay connected.`;
  const input = modal.querySelector("#rename-session-name");
  input.value = name;
  input.focus();
  input.select();
  const error = modal.querySelector("#rename-session-error");
  input.oninput = () => {
    error.hidden = true;
  };
  const controls = [...modal.querySelectorAll("button, input")];
  let busy = false;
  modal.oncancel = (e) => {
    if (busy) e.preventDefault();
  };
  modal.querySelector("#rename-session-cancel").onclick = close;
  modal.querySelector("form").onsubmit = async (e) => {
    e.preventDefault();
    if (busy) return;
    error.hidden = true;
    try {
      validateSession(input.value);
      if (input.value === name) return close();
      busy = true;
      controls.forEach((control) => (control.disabled = true));
      await rename(input.value);
      close();
    } catch (e) {
      error.textContent = e.message;
      error.hidden = false;
    } finally {
      busy = false;
      controls.forEach((control) => (control.disabled = false));
    }
  };
}
