export function setupVaultReset(resetVault) {
  const unlock = document.querySelector("#unlock-button");
  const button = document.createElement("button");
  button.id = "reset-vault";
  button.type = "button";
  button.className = "vault-reset-link";
  button.textContent = "Forgot passphrase? Reset this browser’s vault";
  document.querySelector("#vault-hint").after(button);
  const dialog = document.createElement("dialog");
  dialog.id = "vault-reset-dialog";
  dialog.setAttribute("aria-labelledby", "vault-reset-title");
  dialog.innerHTML = `<div class="dialog-head"><h2 id="vault-reset-title">Reset local vault?</h2></div>
    <p>The passphrase cannot be recovered. Resetting permanently deletes this browser’s saved servers, keys, passwords, bookmarks and Tailscale identity.</p>
    <p>Remote sessions, other browsers and downloaded backups stay intact. You’ll create a new vault and sign in again. Existing backups still need their original passphrase.</p>
    <form id="vault-reset-form"><label for="vault-reset-confirmation">Type RESET to delete this local vault</label>
    <input id="vault-reset-confirmation" autocomplete="off" spellcheck="false" required pattern="RESET">
    <p id="vault-reset-error" role="alert" hidden></p>
    <div class="dialog-actions"><button type="button" id="vault-reset-cancel" autofocus>Cancel</button><button class="danger" id="vault-reset-submit" disabled>Delete local vault</button></div></form>`;
  document.querySelector("#lockscreen").append(dialog);
  const input = dialog.querySelector("input");
  const submit = dialog.querySelector("#vault-reset-submit");
  const cancel = dialog.querySelector("#vault-reset-cancel");
  const error = dialog.querySelector("#vault-reset-error");
  let busy = false;
  button.onclick = () => {
    if (unlock.disabled) return;
    input.value = "";
    submit.disabled = true;
    error.hidden = true;
    unlock.disabled = true;
    dialog.showModal();
  };
  input.oninput = () => {
    submit.disabled = busy || input.value !== "RESET";
  };
  cancel.onclick = () => dialog.close();
  dialog.oncancel = (event) => {
    if (busy) event.preventDefault();
  };
  dialog.onclose = () => {
    unlock.disabled = false;
    button.focus();
  };
  dialog.querySelector("form").onsubmit = async (event) => {
    event.preventDefault();
    if (busy || input.value !== "RESET") return;
    busy = true;
    submit.disabled = cancel.disabled = input.disabled = true;
    error.hidden = true;
    try {
      await resetVault();
      location.reload();
    } catch (e) {
      error.textContent = e.message;
      error.hidden = false;
      busy = false;
      submit.disabled = cancel.disabled = input.disabled = false;
    }
  };
}
