// Offline migration. Does not modify the original server vault or export node identity.
import { readFile, writeFile } from "node:fs/promises";
import { createInterface } from "node:readline/promises";
import { Writable } from "node:stream";
import { unseal } from "../server/vault.js";
import {
  deriveVaultKey,
  portableData,
  sealVault,
} from "../client/vault-crypto.js";
const [input = "data/vault.enc", output = "tailterm-backup.json"] =
  process.argv.slice(2);
if (!process.stdin.isTTY)
  throw new Error(
    "Run in an interactive terminal. Do not put your passphrase in command-line arguments.",
  );
let silent = false;
const muted = new Writable({
  write(chunk, encoding, next) {
    if (!silent) process.stdout.write(chunk, encoding);
    next();
  },
});
const rl = createInterface({
  input: process.stdin,
  output: muted,
  terminal: true,
});
let opened;
try {
  process.stdout.write("Existing server vault passphrase: ");
  silent = true;
  const password = await rl.question("");
  silent = false;
  process.stdout.write("\n");
  opened = unseal(await readFile(input, "utf8"), password);
  const salt = crypto.getRandomValues(new Uint8Array(16)),
    key = await deriveVaultKey(password, salt);
  const envelope = await sealVault(portableData(opened.data), key, salt);
  await writeFile(output, JSON.stringify(envelope), {
    mode: 0o600,
    flag: "wx",
  });
  console.log(
    "Encrypted portable backup written to " +
      output +
      ". Restore it in the browser with the same passphrase.",
  );
} catch (error) {
  console.error("Export failed: " + error.message);
  process.exitCode = 1;
} finally {
  opened?.key.fill(0);
  rl.close();
}
