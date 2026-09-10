const NATIVE_SELECT = "select:not([multiple]):not([size])";

const isOpen = (control) => {
  try {
    return control.matches(":open");
  } catch {
    return false;
  }
};

export function keepDialogOpenForSelectCancel(event, dialog) {
  const control = dialog?.ownerDocument?.activeElement;
  if (!control || !dialog.contains(control) || !control.matches(NATIVE_SELECT))
    return false;
  if (!isOpen(control)) return false;

  event.preventDefault();
  control.blur();
  (globalThis.requestAnimationFrame || ((next) => setTimeout(next, 16)))(() => {
    if (dialog.open && control.isConnected)
      control.focus({ preventScroll: true });
  });
  return true;
}

export function openDialogSelectOnEnter(event, dialog) {
  const control = event.target?.closest?.(NATIVE_SELECT);
  if (
    event.key !== "Enter" ||
    event.isComposing ||
    !control ||
    control.disabled ||
    !dialog.contains(control) ||
    isOpen(control)
  )
    return false;

  event.preventDefault();
  let pickerRequested = false;
  try {
    if (typeof control.showPicker === "function") {
      control.showPicker();
      pickerRequested = true;
    }
  } catch {
    // A programmatic click within this user activation is the fallback.
  }
  if (!pickerRequested) {
    try {
      control.click();
    } catch {
      // Preventing the implicit submit still keeps the editor and its draft live.
    }
  }
  return true;
}

export function installDialogSelectInteraction(dialog) {
  dialog.oncancel = (event) => {
    keepDialogOpenForSelectCancel(event, dialog);
  };
  dialog.onkeydown = (event) => {
    openDialogSelectOnEnter(event, dialog);
  };
}
