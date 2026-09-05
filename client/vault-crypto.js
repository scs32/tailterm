const encoder = new TextEncoder(),
  decoder = new TextDecoder();
export const VAULT_ITERATIONS = 600000;
const b64 = (bytes) => {
  let text = "";
  for (const b of bytes) text += String.fromCharCode(b);
  return btoa(text);
};
function unbase64(value, length) {
  if (
    typeof value !== "string" ||
    value.length > 16 * 1024 * 1024 ||
    !/^[A-Za-z0-9+/]*={0,2}$/.test(value)
  )
    throw new Error("Invalid encrypted vault.");
  const bytes = Uint8Array.from(atob(value), (c) => c.charCodeAt(0));
  if (length && bytes.length !== length)
    throw new Error("Invalid encrypted vault.");
  return bytes;
}
export async function deriveVaultKey(password, salt) {
  if (
    typeof password !== "string" ||
    password.length < 14 ||
    password.length > 1024
  )
    throw new Error("Use a passphrase of 14–1024 characters.");
  const material = await crypto.subtle.importKey(
    "raw",
    encoder.encode(password),
    "PBKDF2",
    false,
    ["deriveKey"],
  );
  return crypto.subtle.deriveKey(
    { name: "PBKDF2", hash: "SHA-256", salt, iterations: VAULT_ITERATIONS },
    material,
    { name: "AES-GCM", length: 256 },
    false,
    ["encrypt", "decrypt"],
  );
}
const aad = encoder.encode("tailserve-vault:1:PBKDF2-SHA256:600000");
export async function sealVault(data, key, salt) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const bytes = encoder.encode(JSON.stringify(data));
  try {
    const ciphertext = new Uint8Array(
      await crypto.subtle.encrypt(
        { name: "AES-GCM", iv, additionalData: aad },
        key,
        bytes,
      ),
    );
    return {
      format: "tailserve-vault",
      version: 1,
      kdf: "PBKDF2-SHA256",
      iterations: VAULT_ITERATIONS,
      salt: b64(salt),
      iv: b64(iv),
      ciphertext: b64(ciphertext),
    };
  } finally {
    bytes.fill(0);
  }
}
export async function openVault(envelope, password) {
  if (
    envelope?.format !== "tailserve-vault" ||
    envelope.version !== 1 ||
    envelope.kdf !== "PBKDF2-SHA256" ||
    envelope.iterations !== VAULT_ITERATIONS
  )
    throw new Error("Unsupported encrypted vault format.");
  const salt = unbase64(envelope.salt, 16),
    iv = unbase64(envelope.iv, 12),
    ciphertext = unbase64(envelope.ciphertext);
  const key = await deriveVaultKey(password, salt);
  let bytes;
  try {
    bytes = new Uint8Array(
      await crypto.subtle.decrypt(
        { name: "AES-GCM", iv, additionalData: aad },
        key,
        ciphertext,
      ),
    );
    return { data: JSON.parse(decoder.decode(bytes)), key, salt };
  } catch {
    throw new Error(
      "Could not unlock vault. Check the passphrase and backup file.",
    );
  } finally {
    bytes?.fill(0);
  }
}
export function portableData(data) {
  return {
    servers: structuredClone(data.servers),
    keys: structuredClone(data.keys),
    sessions: structuredClone(data.sessions),
    tailscale: {},
  };
}
