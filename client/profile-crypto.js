// Profile keys are independent of local vault encryption and task-agent tokens.
// Password material is retained only as a non-exportable HKDF key while unlocked.
import { VAULT_ITERATIONS } from "./vault-crypto.js";
const encoder = new TextEncoder();
export const normalizeUsername = (value) =>
  String(value || "")
    .trim()
    .toLowerCase();
export const validUsername = (value) =>
  /^[a-z0-9][a-z0-9._-]{0,63}$/.test(value);
const b64 = (bytes) =>
  btoa(Array.from(bytes, (b) => String.fromCharCode(b)).join(""));
const decode = (value) => {
  if (
    typeof value !== "string" ||
    value.length > 2 * 1024 * 1024 ||
    !/^[A-Za-z0-9+/]*={0,2}$/.test(value)
  )
    throw new Error("Invalid encrypted profile.");
  return Uint8Array.from(atob(value), (c) => c.charCodeAt(0));
};
export async function profileMaster(password, username) {
  if (!validUsername(username))
    throw new Error(
      "Username: 1–64 letters, numbers, dots, dashes or underscores.",
    );
  const raw = encoder.encode(password);
  try {
    const material = await crypto.subtle.importKey(
      "raw",
      raw,
      "PBKDF2",
      false,
      ["deriveBits"],
    );
    const bits = new Uint8Array(
      await crypto.subtle.deriveBits(
        {
          name: "PBKDF2",
          hash: "SHA-256",
          salt: encoder.encode("tailterm-profile:v1:" + username),
          iterations: VAULT_ITERATIONS,
        },
        material,
        256,
      ),
    );
    try {
      return await crypto.subtle.importKey("raw", bits, "HKDF", false, [
        "deriveKey",
        "deriveBits",
      ]);
    } finally {
      bits.fill(0);
    }
  } finally {
    raw.fill(0);
  }
}
export async function profileKeys(master, username, instanceId) {
  if (!master || !/^profilehub_[0-9a-f]{16}$/.test(instanceId))
    throw new Error("Unlock this username with your vault passphrase first.");
  const params = (purpose) => ({
    name: "HKDF",
    hash: "SHA-256",
    salt: encoder.encode(instanceId),
    info: encoder.encode("tailterm-profile:v1:" + purpose),
  });
  const encryption = await crypto.subtle.deriveKey(
    params("encryption"),
    master,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
  const credential = new Uint8Array(
    await crypto.subtle.deriveBits(params("authentication"), master, 256),
  );
  const token = Array.from(credential, (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
  credential.fill(0);
  const aad = encoder.encode(
    JSON.stringify(["tailterm-profile", 1, username, instanceId]),
  );
  return {
    token,
    async seal(data) {
      const iv = crypto.getRandomValues(new Uint8Array(12)),
        raw = encoder.encode(JSON.stringify(data));
      try {
        return {
          format: "tailterm-profile",
          version: 1,
          iv: b64(iv),
          ciphertext: b64(
            new Uint8Array(
              await crypto.subtle.encrypt(
                { name: "AES-GCM", iv, additionalData: aad },
                encryption,
                raw,
              ),
            ),
          ),
        };
      } finally {
        raw.fill(0);
      }
    },
    async open(envelope) {
      if (envelope?.format !== "tailterm-profile" || envelope.version !== 1)
        throw new Error("Unsupported encrypted profile.");
      let raw;
      try {
        const iv = decode(envelope.iv);
        if (iv.length !== 12) throw new Error("Invalid IV");
        raw = new Uint8Array(
          await crypto.subtle.decrypt(
            { name: "AES-GCM", iv, additionalData: aad },
            encryption,
            decode(envelope.ciphertext),
          ),
        );
        return JSON.parse(new TextDecoder().decode(raw));
      } catch {
        throw new Error(
          "Could not unlock the saved profile. Check your username and passphrase.",
        );
      } finally {
        raw?.fill(0);
      }
    },
  };
}
