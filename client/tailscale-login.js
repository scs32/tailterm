import QRCode from "qrcode";

export function safeTailscaleAuthURL(raw) {
  const url = new URL(raw);
  if (
    url.protocol !== "https:" ||
    url.hostname !== "login.tailscale.com" ||
    url.username ||
    url.password ||
    url.port
  )
    throw new Error("Unexpected Tailscale login URL");
  return url.href;
}

export const TAILSCALE_QR_OPTIONS = Object.freeze({
  errorCorrectionLevel: "M",
  margin: 4,
  width: 208,
  color: {
    dark: "#07110c",
    light: "#ffffff",
  },
});

export async function renderTailscaleQRCode(
  href,
  { documentRef = document, encoder = QRCode } = {},
) {
  const canvas = documentRef.createElement("canvas");
  await encoder.toCanvas(canvas, href, TAILSCALE_QR_OPTIONS);
  canvas.className = "tailscale-login-qr-image";
  canvas.setAttribute("aria-label", "Tailscale sign-in QR code");
  canvas.setAttribute("role", "img");
  return canvas;
}

export function tailscaleLoginStateText(state, qrFailed = false) {
  if (state === "NeedsMachineAuth")
    return "Sign-in complete. Waiting for device approval…";
  if (qrFailed) return "QR unavailable. Continue with the sign-in link below.";
  if (state === "NeedsLogin")
    return "Waiting for sign-in. This window updates automatically.";
  return "Connecting to Tailscale…";
}

export function createTailscaleLoginController({
  renderQRCode = renderTailscaleQRCode,
} = {}) {
  let sequence = 0;
  let current = null;

  const owns = (dialog) =>
    Boolean(
      current &&
      dialog === current.dialog &&
      dialog?.open &&
      dialog.dataset.tailscaleLogin === "true" &&
      dialog.dataset.tailscaleLoginId === current.id,
    );

  const updateStatus = (record) => {
    if (!owns(record.dialog)) return;
    record.status.textContent = tailscaleLoginStateText(
      record.state,
      record.qrFailed,
    );
  };

  const clear = () => {
    sequence += 1;
    const record = current;
    current = null;
    if (!record) return;
    record.host.replaceChildren();
    if (record.dialog.dataset.tailscaleLoginId === record.id) {
      delete record.dialog.dataset.tailscaleLogin;
      delete record.dialog.dataset.tailscaleLoginId;
    }
  };

  const show = ({ dialog, host, status, href, state = "NeedsLogin" }) => {
    clear();
    const record = {
      dialog,
      host,
      status,
      id: String(++sequence),
      state,
      qrFailed: false,
    };
    current = record;
    dialog.dataset.tailscaleLogin = "true";
    dialog.dataset.tailscaleLoginId = record.id;
    updateStatus(record);

    return Promise.resolve()
      .then(() => renderQRCode(href))
      .then((image) => {
        if (!owns(dialog) || current !== record) return false;
        host.replaceChildren(image);
        return true;
      })
      .catch(() => {
        if (!owns(dialog) || current !== record) return false;
        record.qrFailed = true;
        host.replaceChildren();
        updateStatus(record);
        return false;
      });
  };

  const updateState = (state) => {
    if (!current || !owns(current.dialog)) return false;
    current.state = state;
    updateStatus(current);
    return true;
  };

  const closeIfCurrent = (dialog, close) => {
    if (!owns(dialog)) return false;
    clear();
    close();
    return true;
  };

  return {
    show,
    updateState,
    clear,
    isCurrentDialog: owns,
    closeIfCurrent,
  };
}
