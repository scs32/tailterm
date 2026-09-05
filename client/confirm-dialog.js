// Confirmation stays separate from the underlying form so Cancel preserves edits.
export function confirmDialog({
  title,
  message,
  action = "Confirm",
  destructive = false,
}) {
  return new Promise((resolve) => {
    const modal = document.createElement("dialog");
    modal.className = "confirmation-dialog";
    modal.setAttribute("aria-labelledby", "confirmation-title");
    modal.innerHTML =
      '<div class="dialog-head"><h2 id="confirmation-title"></h2><button type="button" aria-label="Cancel" data-close>×</button></div><p class="confirmation-message"></p><div class="dialog-actions"><button type="button" data-cancel>Cancel</button><button type="button" data-confirm></button></div>';
    modal.querySelector("h2").textContent = title;
    modal.querySelector("p").textContent = message;
    const confirm = modal.querySelector("[data-confirm]");
    confirm.textContent = action;
    confirm.className = destructive ? "danger" : "primary";
    let settled = false;
    const finish = (accepted) => {
      if (settled) return;
      settled = true;
      modal.close();
      modal.remove();
      resolve(accepted);
    };
    modal.querySelector("[data-close]").onclick = () => finish(false);
    modal.querySelector("[data-cancel]").onclick = () => finish(false);
    confirm.onclick = () => finish(true);
    modal.oncancel = (event) => {
      event.preventDefault();
      finish(false);
    };
    modal.onclose = () => finish(false);
    (document.fullscreenElement || document.body).append(modal);
    modal.showModal();
    modal.querySelector("[data-cancel]").focus();
  });
}
