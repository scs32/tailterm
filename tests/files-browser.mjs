import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

// Drives Files mode against the SSH fixture's SFTP subsystem: listing,
// navigation, download, upload, folder creation, rename, delete, and opening
// a terminal in the current folder.
export async function exerciseFiles(page, files, getInput) {
  const waitFor = async (fn, label) => {
    for (let i = 0; i < 200; i++) {
      if (await fn()) return;
      await new Promise((r) => setTimeout(r, 50));
    }
    throw new Error("Timed out waiting for " + label);
  };
  const rows = () => page.locator(".files-table tbody tr[data-entry]");
  const row = (name) => page.locator(`.files-table tr[data-entry="${name}"]`);
  const status = () => page.locator(".files-status").innerText();

  files.set("/home/testuser/notes.txt", {
    data: Buffer.from("remember the milk\n"),
    mode: 0o100644,
    mtime: Date.UTC(2026, 0, 2, 3, 4),
  });
  files.set("/home/testuser/projects", { dir: true, mode: 0o755 });
  files.set("/home/testuser/projects/README.md", {
    data: Buffer.from("# hi\n"),
    mode: 0o100644,
  });

  await page.locator(".mode-switch [data-mode=files]").click();
  await waitFor(
    async () =>
      (await page.locator("#files-path").inputValue()) === "/home/testuser",
    "home listing",
  );
  await waitFor(async () => (await rows().count()) === 2, "two entries");
  await page.screenshot({ path: ".build/files-preview.png" });
  // Directories sort first and show the accent marker.
  assert.equal(await rows().first().getAttribute("data-entry"), "projects");
  assert.match(await row("notes.txt").innerText(), /18 B/);
  assert.match(await row("notes.txt").innerText(), /-rw-r--r--/);

  // Navigate into a folder by double-click and back up.
  await row("projects").dblclick();
  await waitFor(
    async () =>
      (await page.locator("#files-path").inputValue()) ===
      "/home/testuser/projects",
    "projects listing",
  );
  assert.equal(await rows().count(), 1);
  await page.locator("#files-up").click();
  await waitFor(async () => (await rows().count()) === 2, "back home");

  // Download a file: the bytes must round-trip.
  await row("notes.txt").click();
  const download = page.waitForEvent("download");
  await page.locator("#files-download").click();
  const saved = await (await download).path();
  assert.equal(await readFile(saved, "utf8"), "remember the milk\n");

  // Create a folder, then upload into it.
  await page.locator("#files-mkdir").click();
  await page.locator("#files-prompt-value").fill("reports");
  await page.locator("#files-prompt button[type=submit]").click();
  await waitFor(
    () => files.get("/home/testuser/reports")?.dir === true,
    "mkdir",
  );
  await waitFor(async () => (await rows().count()) === 3, "three entries");
  await row("reports").dblclick();
  await waitFor(
    async () =>
      (await page.locator("#files-path").inputValue()) ===
      "/home/testuser/reports",
    "reports listing",
  );
  await page.locator("#files-upload").setInputFiles({
    name: "q3.csv",
    mimeType: "text/csv",
    buffer: Buffer.from("quarter,revenue\nq3,42\n"),
  });
  await waitFor(
    () =>
      files.get("/home/testuser/reports/q3.csv")?.data.toString() ===
      "quarter,revenue\nq3,42\n",
    "upload",
  );
  await waitFor(async () => (await rows().count()) === 1, "uploaded row");

  // Rename, then delete.
  await row("q3.csv").click();
  await page.locator("#files-rename").click();
  await page.locator("#files-prompt-value").fill("q3-final.csv");
  await page.locator("#files-prompt button[type=submit]").click();
  await waitFor(
    () => files.has("/home/testuser/reports/q3-final.csv"),
    "rename",
  );
  assert.ok(!files.has("/home/testuser/reports/q3.csv"));
  await waitFor(
    async () => (await row("q3-final.csv").count()) === 1,
    "renamed row",
  );
  await row("q3-final.csv").click();
  await page.locator("#files-delete").click();
  await page.locator(".confirmation-dialog [data-confirm]").click();
  await waitFor(
    () => !files.has("/home/testuser/reports/q3-final.csv"),
    "delete",
  );
  await waitFor(async () => (await rows().count()) === 0, "empty folder");
  assert.match(await status(), /0 items/);

  // Path bar accepts an absolute path; a bad one reports an error inline.
  await page.locator("#files-path").fill("/home/testuser/projects");
  await page.locator("#files-path").press("Enter");
  await waitFor(async () => (await row("README.md").count()) === 1, "goto");
  await page.locator("#files-path").fill("/nope");
  await page.locator("#files-path").press("Enter");
  await waitFor(
    async () =>
      /does not exist|no such file|not found|failure/i.test(await status()),
    "error shown (status: " + (await status()) + ")",
  );

  // Terminal here switches to Terminals mode and opens a new pane. The tmux
  // start directory is covered by the shared tmux-command unit tests.
  const tabCount = () => page.locator("#tabs .tab").count();
  const beforeTabs = await tabCount();
  await page.locator("#files-terminal").click();
  await waitFor(
    async () =>
      (await page.locator(".terminal-shell").getAttribute("hidden")) === null,
    "terminals mode",
  );
  await waitFor(async () => (await tabCount()) > beforeTabs, "new tab opened");
  await waitFor(
    async () =>
      /Connected/.test(await page.locator("#terminal-status").innerText()),
    "new session connected",
  );
  console.log(
    "Files passed: listing, navigation, download round-trip, folder, upload, rename, delete, path bar, terminal here.",
  );
}
