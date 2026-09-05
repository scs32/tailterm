export function showForgetDevice({ dialog, forget, backup }) {
  dialog(
    "Forget this device",
    `<p>Delete this browser’s vault, credentials, bookmarks, layout and Tailscale identity, and reset its appearance. Remote sessions and downloaded backups stay intact.</p><p class="fine">This does not revoke tailnet access. Remove this browser’s device separately from <a href="https://login.tailscale.com/admin/machines" target="_blank" rel="noopener noreferrer">Tailscale Machines</a>.</p><button id="forget-backup">Download a backup first</button><label for="forget-confirm">Type FORGET to delete this browser’s data</label><input id="forget-confirm" autocomplete="off" spellcheck="false"><p id="forget-error" role="alert"></p><div class="dialog-actions"><button id="forget-submit" class="danger" disabled>Forget this device</button></div>`,
  );
  const input = document.querySelector("#forget-confirm"),
    submit = document.querySelector("#forget-submit");
  input.oninput = () => (submit.disabled = input.value !== "FORGET");
  document.querySelector("#forget-backup").onclick = backup;
  submit.onclick = async () => {
    if (input.value !== "FORGET") return;
    submit.disabled = true;
    try {
      await forget();
    } catch (e) {
      document.querySelector("#forget-error").textContent = e.message;
      submit.disabled = false;
    }
  };
}
