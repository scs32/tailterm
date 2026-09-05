import {
  randomBytes,
  scryptSync,
  createCipheriv,
  createDecipheriv,
} from "node:crypto";
import fs from "node:fs";
import path from "node:path";
export function seal(data, key, salt) {
  const iv = randomBytes(12),
    cipher = createCipheriv("aes-256-gcm", key, iv);
  const ciphertext = Buffer.concat([
    cipher.update(JSON.stringify(data)),
    cipher.final(),
  ]);
  return JSON.stringify({
    version: 1,
    salt: salt.toString("base64"),
    iv: iv.toString("base64"),
    tag: cipher.getAuthTag().toString("base64"),
    ciphertext: ciphertext.toString("base64"),
  });
}
export function unseal(raw, password) {
  const doc = JSON.parse(raw),
    salt = Buffer.from(doc.salt, "base64");
  const key = derive(password, salt),
    decipher = createDecipheriv(
      "aes-256-gcm",
      key,
      Buffer.from(doc.iv, "base64"),
    );
  decipher.setAuthTag(Buffer.from(doc.tag, "base64"));
  return {
    key,
    salt,
    data: JSON.parse(
      Buffer.concat([
        decipher.update(Buffer.from(doc.ciphertext, "base64")),
        decipher.final(),
      ]).toString(),
    ),
  };
}
export const derive = (password, salt) =>
  scryptSync(password, salt, 32, {
    N: 32768,
    r: 8,
    p: 1,
    maxmem: 64 * 1024 * 1024,
  });
export class Vault {
  constructor(file) {
    this.file = file;
    this.data = null;
  }
  get exists() {
    return fs.existsSync(this.file);
  }
  unlock(password) {
    if (this.exists)
      Object.assign(this, unseal(fs.readFileSync(this.file, "utf8"), password));
    else {
      this.salt = randomBytes(16);
      this.key = derive(password, this.salt);
      this.data = { servers: [], keys: [], tailscale: {}, sessions: [] };
      this.save();
    }
  }
  save() {
    fs.mkdirSync(path.dirname(this.file), { recursive: true, mode: 0o700 });
    fs.writeFileSync(this.file + ".tmp", seal(this.data, this.key, this.salt), {
      mode: 0o600,
    });
    fs.renameSync(this.file + ".tmp", this.file);
  }
  lock() {
    this.key?.fill(0);
    this.key = null;
    this.data = null;
  }
}
export { tmuxCommand } from "../shared/tmux-command.js";
