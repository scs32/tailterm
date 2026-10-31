import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { openVault, sealVault } from "../client/vault-crypto.js";
import { leaves } from "../client/pane-layout.js";
import {
  checkGroupAlone,
  checkHelperWindow,
  checkNeverNamedHelper,
  recordHelperNames,
} from "./pane-groups-browser.mjs";
import { exerciseHomeSplitOffers } from "./workspace-controls-browser.mjs";

// Drives the task hub through the UI against tests/fixture-hub.mjs: hub
// configuration, task creation with a spawned agent, mirroring of agents the
// hub adds and closes, attention labels from lifecycle events, and the board.
export async function exerciseTasks(page, hub, origin, ssh) {
  const palette = async (label) => {
    await page.locator("#commands").click();
    await page.locator("#command-query").fill(label);
    await page
      .locator("#command-results button")
      .filter({ hasText: label })
      .first()
      .click();
  };
  const panes = () => page.locator(".terminal-instance:not([hidden])").count();
  // Tooltips move title text into data-tooltip once the element renders.
  const taskTitle = async () => {
    const tab = page.locator("#tabs .tab.task-tab button[role=tab]");
    return (
      (await tab.getAttribute("data-tooltip")) ||
      (await tab.getAttribute("title")) ||
      ""
    );
  };
  const waitFor = async (fn, label) => {
    for (let i = 0; i < 150; i++) {
      if (await fn()) return;
      await new Promise((r) => setTimeout(r, 60));
    }
    throw new Error("Timed out waiting for " + label);
  };

  const originalIndex = await page.evaluate(() =>
    [...document.querySelectorAll("#tabs .tab")].findIndex((el) =>
      el.classList.contains("active"),
    ),
  );
  // Configure the hub through the dialog, including the connection probe.
  await palette("Project hub: configure");
  await page.locator("#hub-url").fill(origin + "/fixture-hub");
  await page.locator("#hub-test").click();
  await page.getByText("Hub sees this browser as fixture-browser").waitFor();
  assert.ok(
    hub.api.requests.some((request) => request.startsWith("GET /v1/whoami")),
    "configuration probes the isolated hub",
  );
  await page.locator("#hub-save").click();
  await page.getByText("Project hub saved.").waitFor();

  // Create a task with its first agent from the current tab.
  const before = await panes();
  await palette("Project: new…");
  await page.locator("#task-name").fill("demo");
  await page.locator("#task-goal").fill("prove the mirror");
  assert.equal(await page.locator("#task-with-agent").isChecked(), true);
  assert.equal(await page.locator("#task-with-agent").isDisabled(), true);
  await page.locator("#task-allow-spawn").check();
  await page.locator("#agent-name").fill("planner");
  await page.locator("#agent-cwd").fill("/tmp/fixture-project");
  await page.locator("#task-create").click();
  await waitFor(
    async () =>
      (await page.locator("#dialog").isHidden()) &&
      hub.api.tasks().some((t) => t.name === "demo") &&
      hub.api
        .agents()
        .filter((a) => a.name === "planner" || a.role === "database_handler")
        .length === 2,
    "project creation (" +
      (await page
        .locator("#task-error")
        .innerText()
        .catch(() => "")) +
      ")",
  );
  const task = hub.api.tasks().find((t) => t.name === "demo");
  assert.ok(task, "task created on the hub");
  assert.equal(task.orchestrator, "planner");
  const planner = hub.api.agents().find((a) => a.name === "planner");
  assert.ok(planner, "tt spawn registered the agent through the SSH fixture");
  assert.equal(planner.role, "", "planner is the orchestrator");
  assert.match(planner.runId, /^run_[0-9a-f]{16}$/);
  const handler = hub.api.agents().find((a) => a.role === "database_handler");
  assert.ok(handler, "a database handler launched with the orchestrator");
  assert.match(handler.runId, /^run_[0-9a-f]{16}$/);
  assert.notEqual(handler.id, planner.id);
  await page.locator(".mode-switch [data-mode=terminals]").click();
  await page.locator("#tabs .task-tab button[role=tab]").click();
  await waitFor(async () => (await panes()) === 2, "planner and handler panes");
  assert.equal(await page.locator("#tabs .tab.task-tab").count(), 1);
  assert.equal(
    await page.locator("#tabs .task-tab .tab-name").innerText(),
    "demo",
  );
  await waitFor(
    async () => /Project demo: 2 agents/.test(await taskTitle()),
    "project rollup in the tab tooltip",
  );

  // An agent the hub reports later joins the same tab.
  const tester = hub.api.addAgent(task.id, {
    name: "tester",
    session: "tester",
  });
  hub.api.event(task.id, "started", tester.id);
  await waitFor(async () => (await panes()) === 3, "tester pane");
  assert.equal(await page.locator("#tabs .tab.task-tab").count(), 1);
  assert.equal(
    await page.locator("#tabs .task-tab .tab-name").innerText(),
    "demo",
  );

  // Lifecycle events raise attention on the pane that is not focused.
  await page.locator(".terminal-instance:not([hidden])").first().click();
  hub.api.event(task.id, "started", tester.id);
  hub.api.event(task.id, "done", tester.id);
  await waitFor(
    async () => /Command finished/.test(await taskTitle()),
    "command finished label",
  );
  assert.match(await taskTitle(), /1 done/);

  // Board mode: post as the human, then receive an agent message live.
  await palette("Project demo: board");
  await waitFor(
    async () =>
      (await page
        .locator(".mode-switch [data-mode=board]")
        .getAttribute("aria-pressed")) === "true",
    "board mode",
  );
  await page.locator("#board-text").waitFor();
  await page.locator("#board-text").fill("hello team");
  await page.locator("#board-compose button[type=submit]").click();
  await waitFor(
    () => hub.api.messages().some((m) => m.text === "hello team"),
    "posted message",
  );
  await page.locator(".board-text", { hasText: "hello team" }).waitFor();
  assert.equal(
    await page.locator(".board-text", { hasText: "hello team" }).count(),
    1,
  );
  hub.api.message(task.id, "planner reporting in", { agentId: planner.id });
  await page
    .locator(".board-text", { hasText: "planner reporting in" })
    .waitFor();
  assert.equal(await page.locator(".board-agents-row .board-agent").count(), 3);
  await page.screenshot({ path: ".build/board-preview.png" });
  // Terminals survive the mode switch: their buffers still hold the prompt.
  assert.ok(
    (await page.locator(".terminal-shell").getAttribute("hidden")) !== null,
  );
  // Tasks mode lists the task with its agents and rollup.
  await page.locator(".mode-switch [data-mode=tasks]").click();
  await page.locator(".board-head h2", { hasText: "demo" }).waitFor();
  assert.equal(await page.locator(".task-card .board-agent").count(), 3);
  assert.match(
    await page.locator(".task-card header .fine").innerText(),
    /3 agents/,
  );
  await page.screenshot({ path: ".build/tasks-preview.png" });
  // Keyboard shortcut returns to Terminals with the panes intact.
  await page.keyboard.press("Control+1");
  await waitFor(
    async () =>
      (await page.locator(".terminal-shell").getAttribute("hidden")) === null,
    "terminals mode",
  );
  assert.equal(await panes(), 3);

  // An agent the mirror opens into a hidden project group (wi_b6b79229c8fec99d)
  // opens its PTY at the fixed agent size and sends no resize while hidden;
  // shown, it sends one resize equal to its tile. The attach ignores size.
  await page.locator("#tabs .tab button[role=tab]").nth(originalIndex).click();
  await waitFor(
    async () =>
      !(
        await page.locator("#tabs .tab.task-tab").getAttribute("class")
      ).includes("active"),
    "project group hidden",
  );
  const ordinaryPanes = await panes();
  const sizes = () => ssh.sizeLog.filter((e) => e.session === "sizer");
  const claims = () => ssh.sizingLog.filter((e) => e.session === "sizer");
  const sizer = hub.api.addAgent(task.id, { name: "sizer", session: "sizer" });
  ssh.sessions.add(sizer.session);
  hub.api.event(task.id, "started", sizer.id);
  await waitFor(async () => sizes().length > 0, "hidden sizer attach");
  await new Promise((r) => setTimeout(r, 1000));
  assert.deepEqual(
    sizes(),
    [{ session: "sizer", kind: "open", cols: 200, rows: 50, ignoreSize: true }],
    "hidden agent tile opens at 200x50 with ignore-size and never resizes",
  );
  assert.equal(await panes(), ordinaryPanes, "the sizer pane stays hidden");
  assert.equal(
    claims().length,
    0,
    "hidden attach never claims agent size authority",
  );
  await page.locator("#tabs .task-tab button[role=tab]").click();
  await waitFor(async () => (await panes()) === 4, "sizer pane shown");
  await waitFor(async () => sizes().length > 1, "shown sizer resize");
  await new Promise((r) => setTimeout(r, 1000));
  await page.locator(".pane-header .pane-label", { hasText: "sizer" }).click();
  const [cols, rows] = (await page.locator("#dimensions").innerText())
    .split("×")
    .map((n) => Number(n.trim()));
  assert.deepEqual(
    sizes().slice(1),
    [{ session: "sizer", kind: "resize", cols, rows }],
    "shown tile sends exactly one resize with its real size",
  );
  assert.ok(cols < 200 && rows < 50, "the tile is smaller than the default");
  await waitFor(
    () => claims().some((e) => e.command.includes("resize-window")),
    "focused sizer host sizing claim",
  );
  hub.api.event(task.id, "closed", sizer.id);
  await waitFor(async () => (await panes()) === 3, "sizer pane closed");

  // Lead clarification #24225: an adopted ordinary launcher needs an exact
  // ignore-size reattach before sizing, and must remain outside Home.
  for (const fail of [false, true]) {
    const name = fail ? "adopted-failure" : "adopted-ordinary";
    ssh.sessions.add(name);
    await page.locator("#new-tab").click();
    await page.locator("#launcher-name").fill(name);
    await page.locator("#start-session").click();
    await waitFor(async () => {
      const status = await page.locator("#terminal-status").textContent();
      return (
        status.includes(name) &&
        status.includes("Connected") &&
        !status.includes("unverified")
      );
    }, "unbound ordinary pane verified");
    const tabId = await page
      .locator("#tabs .tab.active [data-tab]")
      .getAttribute("data-tab");
    if (fail) ssh.gone.add(name);
    const adopted = hub.api.addAgent(task.id, { name, session: name });
    hub.api.event(task.id, "started", adopted.id);
    await waitFor(
      () => ssh.attachLog.includes(name),
      "adopted ordinary reattach",
    );
    assert.ok(
      ssh.sizeLog.some(
        (e) => e.session === name && e.kind === "open" && e.ignoreSize,
      ),
      "ordinary replacement uses ignore-size",
    );
    if (fail) {
      await waitFor(
        async () =>
          (await page.locator(`[data-pane="${tabId}"]`).textContent()).includes(
            "Error",
          ),
        "failed reattach is visible",
      );
      assert.equal(
        ssh.sizingLog.filter((e) => e.session === name).length,
        0,
        "failed replacement never claims size",
      );
    } else {
      await page.locator(`[data-pane="${tabId}"] .pane-label`).click();
      await waitFor(
        () =>
          ssh.sizingLog.some(
            (e) => e.session === name && e.command.includes("resize-window"),
          ),
        "adopted ordinary focus sizes",
      );
      assert.equal(
        await page.locator(`[data-pane="${tabId}"][data-home]`).count(),
        0,
        "ordinary agent retains its tab identity outside Home",
      );
      assert.ok(
        ssh.sizingLog
          .find((e) => e.session === name)
          .command.includes(adopted.runId),
        "replacement preserves the exact run binding",
      );
    }
    hub.api.event(task.id, "closed", adopted.id);
    await waitFor(async () => (await panes()) === 3, "adopted pane closed");
    ssh.gone.delete(name);
  }

  // Closing an agent removes its pane; closing the task removes the rest.
  hub.api.event(task.id, "closed", tester.id);
  await waitFor(async () => (await panes()) === 2, "tester pane closed");
  hub.api.closeTask(task.id);
  await waitFor(
    async () => (await page.locator("#tabs .tab.task-tab").count()) === 0,
    "task panes closed",
  );

  // Closing the task removes its own group, preserving ordinary terminals.
  assert.equal(await page.locator("#tabs .tab.task-tab").count(), 0);
  // Group tabs show whichever member is focused, so restore by position.
  await page.locator("#tabs .tab button[role=tab]").nth(originalIndex).click();
  await waitFor(
    async () =>
      (
        await page
          .locator("#tabs .tab")
          .nth(originalIndex)
          .getAttribute("class")
      ).includes("active"),
    "original tab reactivated",
  );

  // Only the owner helper's pane lives in Home (wi_d6327b69ba901602,
  // wi_33e3858e611cdd0b). The owner resumes a session from the launcher, as an
  // ordinary tab, before the hub reports it as the helper: adoption moves
  // that same tab into Home and replaces its plain
  // attach with an ignore-size attach-session, whether the first attach has
  // connected or is still connecting. Size-log entries are recorded only for
  // attach-session commands (never new-session); each carries its flag.
  const attaches = (name) =>
    ssh.sizeLog.filter((e) => e.session === name && e.kind === "open");
  const homePane = (id) =>
    page.locator(`.pane-header[data-home][data-pane="${id}"]`);
  const fittedHelperPTY = async (name, label) => {
    await waitFor(async () => {
      const [cols, rows] = (await page.locator("#dimensions").innerText())
        .split("×")
        .map((n) => Number(n.trim()));
      const latest = ssh.sizeLog.filter((e) => e.session === name).at(-1);
      return (
        latest?.cols === cols && latest?.rows === rows && cols > 16 && rows > 2
      );
    }, label + " real visible PTY dimensions");
  };
  const resume = async (name) => {
    ssh.sessions.add(name);
    await page.locator("#new-tab").click();
    await page.locator("#launcher-refresh").click();
    await page.locator("[data-resume]").filter({ hasText: name }).click();
    await waitFor(async () => attaches(name).length > 0, `${name} attach`);
    const id = await page
      .locator(".tab.active [data-tab]")
      .getAttribute("data-tab");
    await page.locator(`#tabs [data-tab="${id}"]`).waitFor();
    assert.equal(
      await homePane(id).count(),
      0,
      `${name} opened as a tab, not in Home`,
    );
    assert.deepEqual(
      attaches(name).map((e) => e.ignoreSize),
      [false],
      `${name}: the launcher attach does not ignore size`,
    );
    return id;
  };
  // A project with an owner helper shows only the helper until the owner
  // pulls another agent in from the Home tab's ▾ (wi_649926f70aaa762a).
  const pullIn = async (name) => {
    await page.locator(".tab-strip > .home-tab [data-tab]").click();
    await page.locator(".tab-strip > .home-tab [data-terminal-menu]").click();
    const level = (n) => page.locator(`.terminal-menu[data-level="${n}"]`);
    const named = (n, label) =>
      level(n)
        .locator("[data-menu-key]")
        .filter({
          has: page.locator(".terminal-menu-label", {
            hasText: new RegExp(`^${label}$`),
          }),
        });
    await named(0, "No work item").click();
    await named(1, name).click();
    await waitFor(
      async () =>
        (await named(1, name).getAttribute("aria-checked")) === "true",
      `${name} pulled in`,
    );
    // A newly opened pane may take focus a frame later: retry until closed.
    await waitFor(async () => {
      if (!(await level(0).count())) return true;
      await level(0).locator("[data-menu-key]").first().focus();
      await page.keyboard.press("Escape");
      return false;
    }, "terminal menu closed");
  };
  const adopter = hub.api.createTask("home-adopt");
  const anchor = hub.api.addAgent(adopter.id, { name: "anchor" });
  ssh.sessions.add("anchor");
  hub.api.event(adopter.id, "started", anchor.id);
  await palette("Project: open terminals…");
  await page.locator(`[data-open-task="${adopter.id}"]`).click();
  await page.locator("#tabs .tab.task-tab").waitFor();
  const helperTab = await resume("helper-fx");
  const helper = hub.api.addAgent(adopter.id, {
    name: "owner-helper-fx",
    session: "helper-fx",
    role: "owner_helper",
  });
  hub.api.event(adopter.id, "started", helper.id);
  await waitFor(
    async () => attaches("helper-fx").length === 2,
    "helper reattach after adoption",
  );
  assert.deepEqual(
    attaches("helper-fx").map((e) => e.ignoreSize),
    [false, true],
    "the adopted helper reattaches with ignore-size",
  );
  await waitFor(
    async () =>
      (await homePane(helperTab).count()) === 1 &&
      (await homePane(helperTab).locator(".pane-label").innerText()).endsWith(
        "Connected",
      ),
    "helper connected in Home with the same tab",
  );
  // The helper's arrival leaves only the helper; anchor comes back through
  // the menu, on the same session.
  await pullIn("anchor");
  await waitFor(
    async () =>
      (await page.locator("#tabs .tab.task-tab").count()) === 1 &&
      (
        await page
          .locator(".pane-header:not([data-home]) .pane-label")
          .allInnerTexts()
      ).some((text) => text.includes("anchor")),
    "anchor pulled into its project tab",
  );
  // The helper window: named from the hub role and project, alone at the full
  // terminal width, and in the same place after a reconnect, a roster join and
  // close, and a visit to the project group, wide and narrow.
  const viewport = page.viewportSize();
  const plainId = await page
    .locator("#tabs .tab:not(.task-tab) [data-tab]")
    .first()
    .getAttribute("data-tab");
  const helperWindow = (step, same, extra) =>
    checkHelperWindow(page, {
      id: helperTab,
      project: "home-adopt",
      session: "helper-fx",
      same,
      step,
      ...extra,
    });
  const terminals = () => page.locator(".terminal-instance").count();
  for (const width of [1440, 800]) {
    await page.setViewportSize({ width, height: viewport.height });
    const at = (step) => `${step} at ${width}px`;
    const box = await helperWindow(at("adopted"), undefined, {
      plain: plainId,
    });
    const before = attaches("helper-fx").length;
    await page.locator("#reconnect").click();
    await waitFor(
      async () =>
        attaches("helper-fx").length > before &&
        (await homePane(helperTab).count()) === 1 &&
        (await homePane(helperTab).locator(".pane-label").innerText()).endsWith(
          "Connected",
        ),
      at("helper reconnected"),
    );
    assert.equal(attaches("helper-fx").at(-1).ignoreSize, true);
    assert.equal(
      attaches("helper-fx").length,
      before + 1,
      "one attach per reconnect",
    );
    await helperWindow(at("after a reconnect"), box);
    await fittedHelperPTY("helper-fx", at("reconnected"));
    const panes = await terminals();
    const joiner = hub.api.addAgent(adopter.id, { name: `joiner-${width}` });
    ssh.sessions.add(joiner.name);
    hub.api.event(adopter.id, "started", joiner.id);
    // A roster join opens nothing in a helper project...
    await new Promise((r) => setTimeout(r, 600));
    assert.equal(await terminals(), panes, at("a joined agent stays closed"));
    await helperWindow(at("after an unshown roster join"), box);
    // ...until the owner pulls it in.
    await pullIn(joiner.name);
    await waitFor(
      async () => (await terminals()) === panes + 1,
      at("joined agent pane"),
    );
    await helperWindow(at("after a roster join"), box);
    hub.api.event(adopter.id, "closed", joiner.id);
    await waitFor(
      async () => (await terminals()) === panes,
      at("closed agent pane"),
    );
    ssh.sessions.delete(joiner.name);
    await helperWindow(at("after a roster close"), box);
    await checkGroupAlone(
      page,
      await page
        .locator("#tabs .tab.task-tab [data-tab]")
        .getAttribute("data-tab"),
    );
    await helperWindow(at("after visiting the project group"), box);
  }
  await page.setViewportSize(viewport);
  await helperWindow("original width");
  await exerciseHomeSplitOffers(page, {
    helper: helperTab,
    session: "helper-fx",
    plain: plainId,
  });
  // The same, while the launcher attach is still connecting.
  ssh.hold("helper-hold");
  ssh.sessions.add("helper-hold");
  await page.locator("#new-tab").click();
  await page.locator("#launcher-refresh").click();
  await page
    .locator("[data-resume]")
    .filter({ hasText: "helper-hold" })
    .click();
  await waitFor(() => ssh.held.has("helper-hold"), "held helper attach");
  const heldTab = await page
    .locator(".tab.active [data-tab]")
    .getAttribute("data-tab");
  const heldPane = page.locator(`.pane-header[data-pane="${heldTab}"]`);
  await heldPane.waitFor();
  assert.equal(await homePane(heldTab).count(), 0, "a tab until adopted");
  assert.match(
    await heldPane.locator(".pane-label").innerText(),
    /Connecting$/,
  );
  const held = hub.api.addAgent(adopter.id, {
    name: "owner-helper-held",
    session: "helper-hold",
    role: "owner_helper",
  });
  hub.api.event(adopter.id, "started", held.id);
  await waitFor(
    async () => attaches("helper-hold").length === 2,
    "connecting helper reattach",
  );
  ssh.release("helper-hold");
  await waitFor(
    async () =>
      (await homePane(heldTab).count()) === 1 &&
      (await homePane(heldTab).locator(".pane-label").innerText()).endsWith(
        "Connected",
      ),
    "held helper connected in Home",
  );
  assert.equal(attaches("helper-hold").at(-1).ignoreSize, true);
  assert.equal(
    attaches("helper-hold").length,
    2,
    "held adoption replaces exactly once",
  );
  // A helper first discovered through the roster starts with the policy; the
  // subsequent exact-session verification must not cause another replacement.
  const direct = hub.api.addAgent(adopter.id, {
    name: "owner-helper-direct",
    session: "helper-direct",
    role: "owner_helper",
  });
  ssh.sessions.add("helper-direct");
  hub.api.event(adopter.id, "started", direct.id);
  await waitFor(
    async () =>
      attaches("helper-direct").length === 1 &&
      (
        await page
          .locator(".pane-header[data-home] .pane-label")
          .allInnerTexts()
      ).some((text) => text.endsWith("Connected")),
    "direct helper first attach",
  );
  assert.equal(attaches("helper-direct").at(-1).ignoreSize, true);
  await new Promise((resolve) => setTimeout(resolve, 900));
  assert.equal(
    attaches("helper-direct").length,
    1,
    "verified helper does not loop on policy adoption",
  );
  // An ordinary agent adopted the same way joins its project group, never
  // Home, and its attach is unchanged.
  const plainTab = await resume("plain-fx");
  const plain = hub.api.addAgent(adopter.id, { name: "plain-fx" });
  hub.api.event(adopter.id, "started", plain.id);
  await waitFor(
    async () =>
      (await page.locator(`#tabs [data-tab="${plainTab}"]`).count()) === 0 ||
      (await page.locator("#tabs .tab.task-tab.active").count()) === 1,
    "adopted agent joined its project group",
  );
  assert.equal(await homePane(plainTab).count(), 0, "never in Home");
  await page.locator("#tabs .tab.task-tab button[role=tab]").click();
  await page
    .locator(`.pane-header:not([data-home])[data-pane="${plainTab}"]`)
    .waitFor();
  assert.ok(
    !(
      await page
        .locator(".pane-header:not([data-home]) .pane-label")
        .allInnerTexts()
    ).some((text) => text.includes("helper")),
    "helpers never join the project group",
  );
  assert.deepEqual(
    attaches("plain-fx").map((e) => e.ignoreSize),
    [false],
    "an adopted ordinary agent is not reattached",
  );
  hub.api.closeTask(adopter.id);
  await waitFor(
    async () =>
      (await page.locator("#tabs .tab.task-tab").count()) === 0 &&
      (await page.locator(".pane-header[data-home]").count()) === 0,
    "adoption project closed",
  );
  for (const name of [
    "helper-fx",
    "helper-hold",
    "helper-direct",
    "plain-fx",
    "anchor",
  ])
    ssh.sessions.delete(name);
  await page.locator("#tabs .tab button[role=tab]").nth(originalIndex).click();
  console.log(
    "Tasks passed: dedicated task-named groups, automatic agent membership, hub configuration, spawn over SSH, attention, board, close, and the helper adopted into Home with an ignore-size reattach.",
  );
}

// Login restore of project panes (wi_f378f10d36cb093b). Three lock/unlock
// cycles against the fixture hub and SSH server:
//   1. Hub changes while locked: a paused project, a replaced run, a finished
//      agent whose tmux session is gone, new live and closed projects, and a
//      project on an unknown host. Nothing may open and then close.
//   2. The hub closes an agent while its restored pane is still connecting.
//      The restore must finish, and saving must resume.
//   3. The next login restores the tab opened after cycle 2 and nothing stale.
export async function exerciseTaskRestore(page, hub, ssh) {
  const passphrase = "static browser vault passphrase";
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const waitFor = async (fn, label, timeout = 30000) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      await answerPrompts();
      if (await fn()) return;
      await sleep(100);
    }
    throw new Error("Timed out waiting for " + label);
  };
  // Restored connections ask for the fixture password and host trust.
  async function answerPrompts() {
    const method = page.locator(".login-prompt [name=method]");
    if (await method.isVisible()) await method.selectOption("password");
    const password = page.locator(".login-prompt [name=password]");
    if (await password.isVisible()) {
      await password.fill("static-ssh-password");
      await page.locator(".login-prompt button[type=submit]").click();
    }
    const trust = page.getByRole("button", { name: "Trust & continue" });
    if (await trust.isVisible()) await trust.click();
  }
  const restored = () =>
    page.locator('#workspace[data-restoring="false"]').count();
  const palette = async (label) => {
    await page.locator("#commands").click();
    await page.locator("#command-query").fill(label);
    await page
      .locator("#command-results button")
      .filter({ hasText: label })
      .first()
      .click();
  };
  // Every tab and the tmux session of each of its panes, read from the UI.
  // Home shows alone, never beside a group, so it is read once, as Home.
  async function inventory() {
    await page.locator(".mode-switch [data-mode=terminals]").click();
    const out = [];
    const sessions = (labels) =>
      labels.map((text) => {
        const parts = text.split(" · ");
        return { session: parts.at(-2), status: parts.at(-1) };
      });
    const count = await page.locator("#tabs .tab button[role=tab]").count();
    for (let i = 0; i < count; i++) {
      const tab = page.locator("#tabs .tab").nth(i);
      await tab.locator("button[role=tab]").click();
      const labels = await page
        .locator(".pane-header:not([data-home]) .pane-label")
        .allInnerTexts();
      out.push({
        name: await tab.locator(".tab-name").innerText(),
        project: (await tab.getAttribute("class")).includes("task-tab"),
        panes: sessions(labels),
      });
    }
    const home = page.locator(".tab-strip > .home-tab [data-tab]");
    if (await home.count()) {
      await home.click();
      out.push({
        name: "Home",
        project: false,
        home: true,
        panes: sessions(
          await page
            .locator(".pane-header[data-home] .pane-label")
            .allInnerTexts(),
        ),
      });
    }
    return out;
  }
  const sessionsOf = (tabs, name) =>
    tabs
      .find((t) => t.project && t.name === name)
      ?.panes.map((p) => p.session)
      .sort();
  const allPanes = (tabs) => tabs.flatMap((t) => t.panes.map((p) => p.session));
  async function readVault() {
    const record = await page.evaluate(
      () =>
        new Promise((resolve, reject) => {
          const r = indexedDB.open("tailserve", 1);
          r.onsuccess = () => {
            const q = r.result
              .transaction("vault")
              .objectStore("vault")
              .get("encrypted-v2");
            q.onsuccess = () => resolve(q.result);
            q.onerror = reject;
          };
          r.onerror = reject;
        }),
    );
    return (await openVault(record, passphrase)).data;
  }
  // Lock reloads the page, so it and the refresh below hold the hub first.
  const lock = async () => {
    await hub.navigateWithHubHeld(page, () => page.locator("#lock").click());
    await page.locator("#lockscreen").waitFor();
  };
  const unlock = async () => {
    await page.locator("#password").fill(passphrase);
    await page.locator("#unlock-button").click();
    await page.locator("#workspace").waitFor();
  };
  const paneChanges = () => page.evaluate(() => ({ ...globalThis.__panes }));
  const project = (name, agents) => {
    const task = hub.api.createTask(name);
    const byName = {};
    for (const [agentName, host = "production"] of agents) {
      const agent = hub.api.addAgent(task.id, { name: agentName, host });
      hub.api.event(task.id, "started", agent.id);
      ssh.sessions.add(agentName);
      byName[agentName] = agent;
    }
    return { task, agents: byName };
  };

  // Count terminal elements added and removed in each page load.
  await page.addInitScript(() => {
    globalThis.__panes = { added: 0, removed: 0 };
    const count = (nodes, key) => {
      for (const node of nodes)
        if (node.nodeType === 1)
          globalThis.__panes[key] +=
            (node.matches(".terminal-instance") ? 1 : 0) +
            node.querySelectorAll(".terminal-instance").length;
    };
    new MutationObserver((records) => {
      for (const r of records) {
        count(r.addedNodes, "added");
        count(r.removedNodes, "removed");
      }
    }).observe(document, { childList: true, subtree: true });
  });

  // Two bound projects with live panes.
  const P = project("restore-paused", [["p1"], ["p2"]]);
  const L = project("restore-live", [["l1"], ["l2"], ["l3"]]);
  for (const { task } of [P, L]) {
    await palette("Project: open terminals…");
    await page.locator(`[data-open-task="${task.id}"]`).click();
  }
  await waitFor(async () => {
    const tabs = await inventory();
    return (
      sessionsOf(tabs, "restore-paused")?.join() === "p1,p2" &&
      sessionsOf(tabs, "restore-live")?.join() === "l1,l2,l3" &&
      tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
    );
  }, "bound project panes");

  // The live project's owner helper: its window must come back named, alone
  // and in the same place after every restore and after a browser refresh.
  const helperAgent = hub.api.addAgent(L.task.id, {
    name: "owner-helper-rs",
    session: "helper-rs",
    role: "owner_helper",
  });
  ssh.sessions.add("helper-rs");
  hub.api.event(L.task.id, "started", helperAgent.id);
  await waitFor(async () => {
    const tabs = await inventory();
    return tabs
      .find((t) => t.home)
      ?.panes.some(
        (p) => p.session === "helper-rs" && p.status === "Connected",
      );
  }, "helper pane in Home");
  const helperId = await page
    .locator(".tab-strip > .home-tab [data-tab]")
    .getAttribute("data-tab");
  // The helper's arrival leaves only the helper (wi_649926f70aaa762a): pull
  // the three agents back in from the Home tab's ▾.
  {
    await page.locator(".tab-strip > .home-tab [data-tab]").click();
    await page.locator(".tab-strip > .home-tab [data-terminal-menu]").click();
    const level = (n) => page.locator(`.terminal-menu[data-level="${n}"]`);
    const named = (n, label) =>
      level(n)
        .locator("[data-menu-key]")
        .filter({
          has: page.locator(".terminal-menu-label", {
            hasText: new RegExp(`^${label}$`),
          }),
        });
    await named(0, "No work item").click();
    for (const name of ["l1", "l2", "l3"]) {
      if ((await named(1, name).getAttribute("aria-checked")) !== "true")
        await named(1, name).click();
      await waitFor(
        async () =>
          (await named(1, name).getAttribute("aria-checked")) === "true",
        `${name} pulled in`,
      );
    }
    // A newly opened pane may take focus a frame later: retry until closed.
    await waitFor(async () => {
      if (!(await level(0).count())) return true;
      await level(0).locator("[data-menu-key]").first().focus();
      await page.keyboard.press("Escape");
      return false;
    }, "terminal menu closed");
  }
  await waitFor(async () => {
    const tabs = await inventory();
    return (
      sessionsOf(tabs, "restore-live")?.join() === "l1,l2,l3" &&
      tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
    );
  }, "restore-live agents pulled in");
  const helperWindow = (step, same) =>
    checkHelperWindow(page, {
      id: helperId,
      project: "restore-live",
      session: "helper-rs",
      same,
      step,
    });
  const helperBox = await helperWindow("before the restores");

  // Each pane saves its project binding in its session bookmark once tmux
  // verifies the session.
  await waitFor(async () => {
    const { sessions } = await readVault();
    return ["p1", "p2", "l1", "l2", "l3"].every(
      (name) => sessions.find((s) => s.name === name)?.task,
    );
  }, "bound session bookmarks");

  // Cycle 1: the hub changes while the workspace is locked.
  const oldL2Run = L.agents.l2.runId;
  await lock();
  assert.ok((await readVault()).workspace.tasks.includes(P.task.id));
  hub.api.pauseTask(P.task.id);
  hub.api.restartAgent(L.agents.l2.id);
  assert.notEqual(L.agents.l2.runId, oldL2Run);
  hub.api.event(L.task.id, "done", L.agents.l3.id);
  ssh.gone.add("l3");
  const N = project("restore-new", [["n1"], ["n2"], ["n3"]]);
  hub.api.event(N.task.id, "exited", N.agents.n2.id);
  ssh.gone.add("n2");
  hub.api.event(N.task.id, "done", N.agents.n3.id);
  const X = project("restore-elsewhere", [["x1", "elsewhere"]]);
  const C = project("restore-closed", [["c1"]]);
  hub.api.closeTask(C.task.id);
  ssh.attachLog.length = 0;
  await unlock();
  try {
    await waitFor(restored, "cycle 1 restore", 90000);
  } catch (error) {
    error.message += `; attached ${ssh.attachLog.join(",")}; panes ${JSON.stringify(await paneChanges())}`;
    throw error;
  }
  let tabs;
  await waitFor(async () => {
    tabs = await inventory();
    return (
      sessionsOf(tabs, "restore-live")?.join() === "l1,l2" &&
      sessionsOf(tabs, "restore-new")?.join() === "n1,n3"
    );
  }, "live and discovered project panes");
  await sleep(1000);
  tabs = await inventory();
  const cycle1 = [...ssh.attachLog];
  const changes = await paneChanges();
  assert.ok(changes.added >= 4, "the pane observer saw restored panes");
  assert.equal(changes.removed, 0, "no pane opened and closed");
  for (const name of ["p1", "p2", "l3", "n2", "x1", "c1"])
    assert.ok(!cycle1.includes(name), `${name} is not attached`);
  assert.equal(cycle1.filter((n) => n === "l2").length, 1, "l2 attaches once");
  assert.equal(sessionsOf(tabs, "restore-live").join(), "l1,l2");
  assert.equal(sessionsOf(tabs, "restore-new").join(), "n1,n3");
  for (const name of ["restore-paused", "restore-closed", "restore-elsewhere"])
    assert.ok(!tabs.some((t) => t.name === name), `${name} has no tab`);
  const panes = allPanes(tabs);
  assert.equal(new Set(panes).size, panes.length, "no agent has two panes");
  await helperWindow("after the first restore", helperBox);

  // Cycle 2: the hub closes l1 while its restored pane is connecting.
  ssh.hold("l1");
  ssh.attachLog.length = 0;
  await lock();
  const afterCycle1 = await readVault();
  assert.ok(!afterCycle1.workspace.tasks.includes(P.task.id), "P unbound");
  assert.ok(afterCycle1.workspace.tasks.includes(L.task.id));
  assert.ok(afterCycle1.workspace.tasks.includes(N.task.id));
  assert.ok(!afterCycle1.workspace.tasks.includes(X.task.id));
  assert.equal(
    afterCycle1.sessions.find((s) => s.name === "l2")?.task?.runId,
    L.agents.l2.runId,
    "l2 is bound to its new run",
  );
  await unlock();
  await waitFor(() => ssh.held.has("l1"), "held l1 attach", 90000);
  assert.equal(await restored(), 0, "restore waits on l1");
  const closedAt = Date.now();
  hub.api.event(L.task.id, "closed", L.agents.l1.id);
  await waitFor(restored, "restore after l1 closed", 20000);
  const finishedIn = Date.now() - closedAt;
  ssh.release("l1");
  await waitFor(async () => {
    tabs = await inventory();
    return sessionsOf(tabs, "restore-live")?.join() === "l2";
  }, "l1 pane removed");
  await helperWindow("after the second restore", helperBox);
  // Saving resumed: a tab opened now is restored at the next login.
  ssh.sessions.add("marker");
  await page.locator("#new-tab").click();
  await page.locator("#launcher-name").fill("marker");
  await page.locator("#start-session").click();
  await waitFor(async () => {
    tabs = await inventory();
    return tabs.some((t) =>
      t.panes.some((p) => p.session === "marker" && p.status === "Connected"),
    );
  }, "marker tab");

  // Cycle 3: the marker comes back and nothing stale attaches. The hub hangs
  // on the project reads: the plain marker tab, saved last, reconnects while
  // it does, and the project panes bind once it answers.
  const stripIds = () =>
    page
      .locator("#tabs [data-tab]")
      .evaluateAll((nodes) => nodes.map((n) => n.dataset.tab));
  const homeId = () =>
    page.locator(".tab-strip > .home-tab [data-tab]").getAttribute("data-tab");
  const stripBefore = await stripIds();
  const homeBefore = await homeId();
  ssh.attachLog.length = 0;
  await lock();
  const projectReads = new RegExp(
    `/fixture-hub/v1/tasks/(${L.task.id}|${N.task.id})(\\?|$)`,
  );
  // Unrouting continues any request its handler still holds, so the held
  // reads are continued first and a read after the release falls through.
  const heldReads = new Set();
  const continued = [];
  let releaseHub,
    released = false;
  const hubAnswers = new Promise((resolve) => (releaseHub = resolve));
  const holdRead = (route) => {
    if (released) return route.fallback();
    heldReads.add(route.request().url().match(projectReads)[1]);
    const done = hubAnswers.then(() => route.continue());
    continued.push(done);
    return done;
  };
  await page.route(projectReads, holdRead);
  await unlock();
  await waitFor(
    () =>
      heldReads.has(L.task.id) &&
      heldReads.has(N.task.id) &&
      ssh.attachLog.includes("marker"),
    "held project reads and the marker attach",
  );
  const whileHeld = [...ssh.attachLog];
  assert.equal(await restored(), 0, "restore waits on the hub");
  for (const name of ["l2", "n1", "n3", "helper-rs"])
    assert.ok(
      !whileHeld.includes(name),
      `${name} waits for the hub (attached ${whileHeld.join(",")})`,
    );
  released = true;
  releaseHub();
  await Promise.all(continued);
  await page.unroute(projectReads, holdRead);
  await waitFor(restored, "cycle 3 restore", 90000);
  await waitFor(async () => {
    tabs = await inventory();
    return (
      tabs.some((t) => t.panes.some((p) => p.session === "marker")) &&
      sessionsOf(tabs, "restore-live")?.join() === "l2" &&
      sessionsOf(tabs, "restore-new")?.join() === "n1,n3"
    );
  }, "cycle 3 panes");
  const cycle3 = [...ssh.attachLog];
  assert.ok(cycle3.includes("marker"), "marker tab restored");
  for (const name of ["p1", "p2", "l1", "l3", "n2"])
    assert.ok(!cycle3.includes(name), `${name} is not attached in cycle 3`);
  for (const name of ["l2", "n1", "n3"])
    assert.equal(
      cycle3.filter((n) => n === name).length,
      1,
      `${name} attaches once in cycle 3`,
    );
  assert.deepEqual(await stripIds(), stripBefore, "cycle 3 keeps tab order");
  assert.equal(await homeId(), homeBefore, "cycle 3 keeps Home");
  await helperWindow("after the third restore", helperBox);
  console.log(
    `Plain tab first while the hub hangs: marker reconnected while ${[...heldReads].length} project reads were held; l2, n1 and n3 bound once the hub answered, in the saved tab order.`,
  );

  // A browser refresh restores the same way.
  await hub.navigateWithHubHeld(page, () => page.reload());
  await page.locator("#lockscreen").waitFor();
  await unlock();
  await waitFor(restored, "restore after a refresh", 90000);
  await waitFor(
    async () =>
      (await page
        .locator(`.tab-strip > .home-tab [data-tab="${helperId}"]`)
        .count()) === 1,
    "helper restored after a refresh",
  );
  await helperWindow("after a browser refresh", helperBox);

  // Bookmarks of closed and finished agents lose their project binding.
  await page.locator("#backup-vault").click();
  const download = page.waitForEvent("download");
  await page.locator("#export-backup").click();
  const backup = await openVault(
    JSON.parse(await readFile(await (await download).path(), "utf8")),
    passphrase,
  );
  await page.locator("#dialog-close").click();
  const bookmark = (name) => backup.data.sessions.find((s) => s.name === name);
  for (const name of ["p1", "p2", "l3", "l1"])
    assert.ok(!bookmark(name)?.task, `${name} bookmark is unbound`);
  for (const name of ["n1", "n3"])
    assert.equal(bookmark(name)?.task?.taskId, N.task.id, `${name} bound`);

  // A lower Panes per group re-splits a project, is saved and survives a refresh.
  const splitNames = ["s1", "s2", "s3", "s4", "s5", "s6"];
  const S = project(
    "restore-split",
    splitNames.map((name) => [name]),
  );
  await palette("Project: open terminals…");
  await page.locator(`[data-open-task="${S.task.id}"]`).click();
  // The project's tabs in strip order, each with its panes' sessions.
  const splitTabs = (tabs) =>
    tabs
      .filter((t) => t.project && t.name.startsWith("restore-split"))
      .map((t) => ({ name: t.name, sessions: t.panes.map((p) => p.session) }));
  await waitFor(async () => {
    tabs = await inventory();
    return (
      sessionsOf(tabs, "restore-split")?.join() === splitNames.join() &&
      tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
    );
  }, "split project panes");
  assert.equal(splitTabs(tabs).length, 1, "six panes fit the default limit");
  await page.locator("#appearance").click();
  await page.locator("#pane-group-limit").selectOption("4");
  await page.locator("#dialog-close").click();
  // The stored workspace holds the project in two groups, none over four panes.
  const storedSplit = async () => {
    const { workspace } = await readVault();
    const session = Object.fromEntries(
      workspace.tabs.map((t) => [t.id, t.session]),
    );
    return workspace.groups
      .filter((g) => g.taskId === S.task.id)
      .map((g) => leaves(g.tree).map((id) => session[id]));
  };
  let split;
  await waitFor(async () => {
    split = await storedSplit();
    return (
      split.length === 2 &&
      split.every((ids) => ids.length <= 4) &&
      split.flat().sort().join() === splitNames.join()
    );
  }, "the re-split project in the stored workspace");
  tabs = await inventory();
  const resplit = splitTabs(tabs);
  assert.deepEqual(
    resplit.map((t) => t.name),
    ["restore-split", "restore-split (continued)"],
    "the project continues in a second group",
  );
  assert.deepEqual(
    resplit.map((t) => [...t.sessions].sort()),
    split.map((ids) => [...ids].sort()),
    "the tabs show the stored groups",
  );
  await hub.navigateWithHubHeld(page, () => page.reload());
  await page.locator("#lockscreen").waitFor();
  await unlock();
  await waitFor(restored, "restore of the re-split project", 90000);
  await waitFor(async () => {
    tabs = await inventory();
    return (
      splitTabs(tabs).length === 2 &&
      tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
    );
  }, "re-split project after a refresh");
  assert.deepEqual(
    splitTabs(tabs).map((t) => ({ ...t, sessions: [...t.sessions].sort() })),
    resplit.map((t) => ({ ...t, sessions: [...t.sessions].sort() })),
    "the refresh restores the re-split groups",
  );
  // Later checks expect the default limit and no split project.
  await page.locator("#appearance").click();
  await page.locator("#pane-group-limit").selectOption("8");
  await page.locator("#dialog-close").click();
  hub.api.closeTask(S.task.id);
  console.log(
    `Panes per group 4 re-split restore-split into ${resplit.map((t) => t.sessions.length).join(" + ")} panes; the stored groups came back after a refresh.`,
  );

  // The roster arrives after the restore: the hub refuses L's project read
  // (not a 404, so the restore keeps L's panes) until rosterBack.
  const rosterURL = new RegExp(`/fixture-hub/v1/tasks/${L.task.id}(\\?|$)`);
  let refused = 0;
  const refuse = (route) => {
    refused++;
    return route.fulfill({
      status: 503,
      contentType: "application/json",
      body: '{"error":"unavailable"}',
    });
  };
  const reloadWithoutRoster = async (step) => {
    await page.route(rosterURL, refuse);
    const before = refused;
    await hub.navigateWithHubHeld(page, () => page.reload());
    await page.locator("#lockscreen").waitFor();
    await unlock();
    await waitFor(restored, `${step}: restore`, 90000);
    assert.ok(refused > before, `${step}: the hub refused the project read`);
  };
  // The feed subscribes to L's events right after its first roster render.
  const rosterBack = async (step) => {
    const from = hub.api.requests.length;
    await page.unroute(rosterURL, refuse);
    await waitFor(
      () =>
        hub.api.requests
          .slice(from)
          .some((r) => r.startsWith(`GET /v1/tasks/${L.task.id}/events`)),
      `${step}: the roster read`,
    );
    await page.evaluate(() => new Promise((r) => requestAnimationFrame(r)));
  };
  const helperNames = () =>
    page.evaluate(
      (id) => globalThis.__helperNames.filter((s) => s.id === id),
      helperId,
    );
  await recordHelperNames(page);

  // A real helper restored before the roster keeps its place in Home, unnamed
  // until the roster gives its role, then named from it.
  await reloadWithoutRoster("late roster");
  assert.equal(
    await page
      .locator(`.tab-strip > .home-tab [data-tab="${helperId}"]`)
      .count(),
    1,
    "late roster: the helper is restored in Home",
  );
  await checkNeverNamedHelper(page, {
    id: helperId,
    step: "late roster, unknown role",
  });
  assert.deepEqual(
    await page
      .locator(".pane-header[data-home]")
      .evaluateAll((els) => els.map((el) => el.dataset.pane)),
    [helperId],
    "late roster: the helper is alone in Home",
  );
  await rosterBack("late roster");
  await waitFor(
    async () =>
      (await page.locator(".tab-strip > .home-tab .tab-name").textContent()) ===
      "Owner helper - restore-live",
    "late roster: the helper named from its role",
  );
  await helperWindow("after a late roster", helperBox);
  assert.ok(
    (await helperNames()).length,
    "late roster: the recorder saw the helper's name",
  );

  // An older workspace saved an ordinary agent's pane inside Home. Restored
  // bindings carry no role, so a pane saved in Home whose agent is not the
  // helper is that case: it is never named Owner helper, before or after the
  // roster arrives.
  helperAgent.role = "";
  await reloadWithoutRoster("old workspace");
  assert.equal(
    await page
      .locator(`.tab-strip > .home-tab [data-tab="${helperId}"]`)
      .count(),
    1,
    "old workspace: the pane is restored in Home",
  );
  await checkNeverNamedHelper(page, {
    id: helperId,
    step: "old workspace, unknown role",
  });
  // The roster gives its role: the pane leaves Home for its project's tab.
  // Only the placement changes: the same terminal stays connected, nothing is
  // closed or reattached, and no other pane's tmux window is sized. Outside
  // Home the focused ordinary pane may take its own viewport claim, as any
  // focused agent pane does.
  const others = () =>
    ssh.sizingLog.filter((e) => e.session !== "helper-rs").length;
  const panesBefore = allPanes(await inventory()).sort();
  const before = {
    panes: await paneChanges(),
    opens: ssh.sizeLog.filter((e) => e.kind === "open").length,
    others: others(),
  };
  await rosterBack("old workspace");
  await waitFor(
    async () =>
      (await page.locator(".tab-strip > .home-tab [data-tab]").count()) === 0,
    "old workspace: the pane leaves Home",
  );
  assert.deepEqual(
    {
      panes: await paneChanges(),
      opens: ssh.sizeLog.filter((e) => e.kind === "open").length,
      others: others(),
    },
    before,
    "old workspace: leaving Home closes, reattaches and sizes nothing else",
  );
  await checkNeverNamedHelper(page, {
    id: helperId,
    step: "old workspace, after the roster",
  });
  const placed = await inventory();
  assert.ok(
    sessionsOf(placed, "restore-live")?.includes("helper-rs"),
    "old workspace: the pane joins its project's tab",
  );
  assert.deepEqual(
    allPanes(placed).sort(),
    panesBefore,
    "old workspace: the same panes, only placed differently",
  );
  assert.equal(
    placed.some((t) => t.home),
    false,
    "old workspace: nothing is left in Home",
  );
  console.log(
    `Late roster passed: a restored helper is unnamed until the roster (${refused} refused reads), then Owner helper - restore-live; an ordinary pane saved in Home is never named Owner helper and leaves Home for its project once the roster arrives.`,
  );

  // Leave the hub and tabs as the later checks expect them.
  for (const { task } of [L, N, X]) hub.api.closeTask(task.id);
  await waitFor(
    async () =>
      (await page.locator("#tabs .tab.task-tab").count()) === 0 &&
      (await page.locator(".tab-strip > .home-tab [data-tab]").count()) === 0,
    "restore projects and the helper closed",
  );
  ssh.sessions.delete("helper-rs");
  const marker = page.locator("#tabs .tab", {
    has: page.locator(".tab-name", { hasText: /^marker$/ }),
  });
  if (await marker.count()) await marker.locator("[data-close]").click();
  for (const name of ["l3", "n2"]) ssh.gone.delete(name);
  await exerciseHelperOnlyTerminals();
  console.log(
    `Task restore passed: stuck restore finished ${finishedIn} ms after the hub closed a connecting pane, no flash, paused/closed projects dropped, live projects bound, finished sessions checked, bookmarks unbound.`,
  );

  // Helper-only project terminals (wi_649926f70aaa762a). A project with an
  // owner helper shows only the helper, in Home; the ▾ on the Home tab, on
  // the project tab and in the tab strip pulls its other agents in by work
  // item. Removing a pane never closes, retires or cleans up the agent, and
  // the choice is saved in this origin's encrypted workspace.
  async function exerciseHelperOnlyTerminals() {
    const ITEM_A = "wi_00000000000000a1",
      ITEM_B = "wi_00000000000000b2",
      TITLE_A = "Synthetic helper-only item",
      TITLE_B = "Synthetic retried item";
    const menu = (level) =>
      page.locator(`.terminal-menu[data-level="${level}"]`);
    const labels = (level) =>
      menu(level)
        .locator("[data-menu-key] .terminal-menu-label")
        .allTextContents();
    const row = (level, label) =>
      menu(level)
        .locator("[data-menu-key]")
        .filter({
          has: page.locator(".terminal-menu-label", {
            hasText: new RegExp(`^${label}$`),
          }),
        });
    const checked = async (level, label) =>
      (await row(level, label).getAttribute("aria-checked")) === "true";
    // Escape on the first level closes every level.
    const closeMenu = async () => {
      // A newly opened pane may take focus a frame later: retry until closed.
      await waitFor(async () => {
        if (!(await menu(0).count())) return true;
        await menu(0).locator("[data-menu-key]").first().focus();
        await page.keyboard.press("Escape");
        return false;
      }, "the menu closed");
    };
    const homeMenu = page.locator(
      ".tab-strip > .home-tab [data-terminal-menu]",
    );
    const homeTab = page.locator(".tab-strip > .home-tab [data-tab]");
    const stripMenu = page.locator("#project-terminals");
    // The tab id of the pane showing a tmux session, outside Home.
    const paneId = (session) =>
      page
        .locator(".pane-header:not([data-home])")
        .evaluateAll(
          (els, name) =>
            els.find(
              (el) =>
                el
                  .querySelector(".pane-label")
                  ?.innerText.split(" · ")
                  .at(-2) === name,
            )?.dataset.pane || null,
          session,
        );
    const savedTab = async (session) =>
      (await readVault()).workspace.tabs.find((t) => t.session === session);
    const hubWrites = (from, taskId) =>
      hub.api.requests
        .slice(from)
        .filter(
          (r) => !r.startsWith("GET ") && r.includes(`/v1/tasks/${taskId}/`),
        );
    const add = (taskId, name, fields = {}) => {
      const agent = hub.api.addAgent(taskId, {
        name,
        session: fields.session || name,
        role: fields.role || "",
      });
      // The roster spreads the stored agent, so later reads carry these.
      if (fields.item)
        agent.workItem = {
          itemTaskId: taskId,
          itemId: fields.item,
          itemRevision: 1,
        };
      if (fields.lead) agent.itemLead = true;
      ssh.sessions.add(agent.session);
      hub.api.event(taskId, "started", agent.id);
      return agent;
    };

    // a1: a fresh workspace. Both projects appear while locked, so the
    // startup discovery binds them on unlock.
    await lock();
    const H = hub.api.createTask("helper-only");
    const helper = add(H.id, "owner-helper-ho", {
      session: "helper-ho",
      role: "owner_helper",
    });
    const lead = add(H.id, "ho-lead", { item: ITEM_A, lead: true });
    const builder = add(H.id, "ho-builder", { item: ITEM_A });
    const handler = add(H.id, "ho-handler");
    const G = hub.api.createTask("helperless");
    const g1 = add(G.id, "hl-one");
    const g2 = add(G.id, "hl-two");
    const runs = () =>
      [lead, builder, handler, g1, g2].map((a) => [a.name, a.status, a.runId]);
    const runsBefore = JSON.stringify(runs());
    // One item's title is served; the other's read fails (404) until serveB,
    // so its id names it until a retry reads the title.
    const titleReads = [];
    const itemBReads = [];
    let serveB = false;
    await page.route(
      new RegExp(`/fixture-hub/v1/tasks/${H.id}/work-items/${ITEM_B}(\\?|$)`),
      (route) => {
        itemBReads.push(serveB);
        return route.fulfill(
          serveB
            ? {
                status: 200,
                contentType: "application/json",
                body: JSON.stringify({
                  id: ITEM_B,
                  taskId: H.id,
                  title: TITLE_B,
                  revision: 1,
                }),
              }
            : {
                status: 404,
                contentType: "application/json",
                body: JSON.stringify({ error: "not found" }),
              },
        );
      },
    );
    await page.route(
      new RegExp(`/fixture-hub/v1/tasks/${H.id}/work-items/${ITEM_A}(\\?|$)`),
      (route) => {
        titleReads.push(ITEM_A);
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            id: ITEM_A,
            taskId: H.id,
            title: TITLE_A,
            revision: 1,
          }),
        });
      },
    );
    ssh.attachLog.length = 0;
    await unlock();
    await waitFor(restored, "helper-only restore", 90000);
    let tabs;
    await waitFor(async () => {
      tabs = await inventory();
      return (
        tabs
          .find((t) => t.home)
          ?.panes.some(
            (p) => p.session === "helper-ho" && p.status === "Connected",
          ) &&
        sessionsOf(tabs, "helperless")?.join() === "hl-one,hl-two" &&
        tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
      );
    }, "the helper in Home and the helperless project's panes");
    await sleep(1000);
    tabs = await inventory();
    assert.ok(
      !tabs.some((t) => t.project && t.name === "helper-only"),
      "a1: the helper project has no tab",
    );
    assert.deepEqual(
      tabs.find((t) => t.home).panes.map((p) => p.session),
      ["helper-ho"],
      "a1: only the helper shows",
    );
    for (const name of ["ho-lead", "ho-builder", "ho-handler"])
      assert.ok(!ssh.attachLog.includes(name), `a1: ${name} is not attached`);
    assert.equal(sessionsOf(tabs, "helperless").join(), "hl-one,hl-two");
    const hoId = await homeTab.getAttribute("data-tab");
    const hoBox = await checkHelperWindow(page, {
      id: hoId,
      project: "helper-only",
      session: "helper-ho",
      step: "a1 helper-only, fresh workspace",
    });

    // f2: the roster's work-item title is read before any menu opens.
    await waitFor(
      async () => titleReads.length >= 1,
      "f2: the title is read before the menu opens",
    );
    // a2/a3: the Home tab's ▾ lists the other agents by work item.
    let from = hub.api.requests.length;
    await homeTab.click();
    await homeMenu.click();
    await waitFor(
      async () => (await labels(0)).join("|") === `${TITLE_A}|No work item`,
      "a3: work-item sub-menus named by title, then No work item",
    );
    assert.equal(
      await homeMenu.getAttribute("aria-label"),
      "Other terminals in helper-only",
    );
    await row(0, TITLE_A).click();
    assert.deepEqual(
      await labels(1),
      ["ho-lead", "ho-builder"],
      "a3: lead first",
    );
    assert.equal(await checked(1, "ho-builder"), false);
    // Choosing an agent opens and focuses its pane in the project tab; the
    // menu stays open and ticks it.
    await row(1, "ho-builder").click();
    await waitFor(async () => {
      const id = await paneId("ho-builder");
      return (
        !!id &&
        (await page
          .locator("#tabs .tab.task-tab.active [data-tab]")
          .getAttribute("data-tab")
          .catch(() => null)) === id &&
        (await checked(1, "ho-builder"))
      );
    }, "a2: the builder's pane opens focused in the project tab");
    assert.equal(
      await page.locator("#tabs .tab.task-tab.active .tab-name").innerText(),
      "helper-only",
    );
    // Choosing it again removes the pane (its tab with it), and nothing
    // reaches the hub.
    await row(1, "ho-builder").click();
    await waitFor(
      async () =>
        !(await paneId("ho-builder")) &&
        !(await page
          .locator("#tabs .tab.task-tab", { hasText: "helper-only" })
          .count()) &&
        !(await checked(1, "ho-builder")),
      "a2: the menu removes the builder's pane",
    );
    assert.deepEqual(
      hubWrites(from, H.id),
      [],
      "a2: no close, retire or cleanup call",
    );
    assert.equal(JSON.stringify(runs()), runsBefore, "a2: no agent changed");
    assert.ok(
      ssh.sessions.has("ho-builder"),
      "a2: its tmux session is still listed",
    );
    // Choosing it again reattaches the same run.
    let attachedBefore = ssh.attachLog.filter((n) => n === "ho-builder").length;
    await row(1, "ho-builder").click();
    await waitFor(async () => {
      const saved = await savedTab("ho-builder");
      return (
        saved?.task?.runId === builder.runId &&
        saved.task.agentId === builder.id &&
        ssh.attachLog.filter((n) => n === "ho-builder").length ===
          attachedBefore + 1
      );
    }, "a2: the same run reattached from the menu");
    await closeMenu();
    // × on its pane removes it the same way.
    await waitFor(
      async () => !!(await paneId("ho-builder")),
      "the builder pane shows",
    );
    await page
      .locator(`.pane-header[data-pane="${await paneId("ho-builder")}"]`)
      .getByRole("button", { name: "Close pane", exact: true })
      .click();
    await waitFor(async () => {
      tabs = await inventory();
      return !tabs.some((t) => t.project && t.name === "helper-only");
    }, "a2: × removed the last pane and its tab");
    assert.deepEqual(hubWrites(from, H.id), [], "a2: × makes no hub call");
    assert.equal(JSON.stringify(runs()), runsBefore, "a2: × changes no agent");
    for (const name of ["ho-lead", "ho-builder"])
      assert.ok(
        ssh.sessions.has(name),
        `a2: ${name}'s tmux session is still listed`,
      );
    attachedBefore = ssh.attachLog.filter((n) => n === "ho-builder").length;
    await homeTab.click();
    await homeMenu.click();
    await row(0, TITLE_A).click();
    assert.equal(await checked(1, "ho-builder"), false, "a2: × un-ticks it");
    await row(1, "ho-builder").click();
    await waitFor(async () => {
      const saved = await savedTab("ho-builder");
      return (
        saved?.task?.runId === builder.runId &&
        ssh.attachLog.filter((n) => n === "ho-builder").length ===
          attachedBefore + 1
      );
    }, "a2: the same run reattached after ×");
    await closeMenu();
    // a3 keyboard: Enter/ArrowRight in, ArrowLeft/Escape out.
    await homeTab.click();
    // A tab-strip redraw can replace the focused ▾ before the key arrives;
    // press again only while the menu is not open.
    await waitFor(async () => {
      if (!(await menu(0).count())) {
        await homeMenu.focus();
        await page.keyboard.press("Enter");
        return false;
      }
      return await page.evaluate(
        () =>
          document.activeElement ===
          document.querySelector(
            '.terminal-menu[data-level="0"] [data-menu-key]',
          ),
      );
    }, "a3: keyboard opening focuses the first row");
    await page.keyboard.press("ArrowRight");
    await waitFor(
      async () => (await menu(1).count()) === 1,
      "a3: ArrowRight opens",
    );
    assert.equal(
      await page.evaluate(() => document.activeElement?.textContent),
      await row(1, "ho-lead").textContent(),
      "a3: focus on the sub-menu's first row",
    );
    await page.keyboard.press("ArrowLeft");
    await waitFor(
      async () => (await menu(1).count()) === 0,
      "a3: ArrowLeft returns",
    );
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Enter");
    await waitFor(
      async () => (await labels(1)).join() === "ho-handler",
      "a3: Enter opens No work item",
    );
    await page.keyboard.press("Escape");
    await waitFor(
      async () => (await menu(1).count()) === 0,
      "a3: Escape returns",
    );
    assert.equal(
      await page.evaluate(() => document.activeElement?.dataset.menuKey),
      "s:none",
    );
    await page.keyboard.press("Enter");
    await waitFor(
      async () =>
        (await page.evaluate(() => document.activeElement?.dataset.menuKey)) ===
        `a:${handler.id}`,
      "a3: Enter opens with focus on its first row",
    );
    await page.keyboard.press("Enter");
    await waitFor(
      async () =>
        (await checked(1, "ho-handler")) &&
        (await page.evaluate(() => document.activeElement?.dataset.menuKey)) ===
          `a:${handler.id}`,
      "a3: Enter on a row adds the handler and keeps the keyboard in the menu",
    );
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await waitFor(
      async () => !(await menu(0).count()),
      "a3: Escape closes the menu",
    );
    await waitFor(async () => {
      tabs = await inventory();
      return (
        sessionsOf(tabs, "helper-only")?.join() === "ho-builder,ho-handler"
      );
    }, "a3: the handler joined the project tab");
    // The project tab's own ▾ removes the builder again: one agent shown.
    const hoTab = page.locator("#tabs .tab.task-tab", {
      has: page.locator(".tab-name", { hasText: /^helper-only$/ }),
    });
    const tabMenu = hoTab.locator("[data-terminal-menu]");
    await hoTab.locator("button[role=tab]").click();
    assert.equal(
      await tabMenu.getAttribute("aria-label"),
      "Other terminals in helper-only",
    );
    await tabMenu.click();
    await row(0, TITLE_A).click();
    assert.equal(await checked(1, "ho-builder"), true);
    await row(1, "ho-builder").click();
    await waitFor(
      async () => !(await checked(1, "ho-builder")),
      "a2: removed from the project tab ▾",
    );
    await closeMenu();
    await waitFor(async () => {
      tabs = await inventory();
      return sessionsOf(tabs, "helper-only")?.join() === "ho-handler";
    }, "a2: only the handler is left in the project tab");
    assert.deepEqual(hubWrites(from, H.id), [], "a2: still no hub call");
    assert.equal(JSON.stringify(runs()), runsBefore);

    // a3/f2: an item whose title read fails is named by its id until a
    // background retry reads it, without a reload or reopening the menu.
    // Closed agents and the helper are never listed.
    const extra = add(H.id, "ho-extra", { item: ITEM_B });
    await waitFor(
      async () => itemBReads.length >= 1,
      "f2: the new item's title is read without opening the menu",
    );
    await homeTab.click();
    await homeMenu.click();
    await waitFor(
      async () =>
        (await labels(0)).join("|") === `${TITLE_A}|${ITEM_B}|No work item`,
      "a3: the item id stands in for a failed title",
    );
    serveB = true;
    await waitFor(
      async () =>
        (await labels(0)).join("|") === `${TITLE_A}|${TITLE_B}|No work item`,
      "f2: a retried read names the item in the open menu",
      60000,
    );
    assert.ok(
      itemBReads.filter((served) => !served).length >= 1 &&
        itemBReads.at(-1) === true,
      `f2: failed reads, then one served: ${JSON.stringify(itemBReads)}`,
    );
    hub.api.event(H.id, "closed", extra.id);
    await waitFor(
      async () => (await labels(0)).join("|") === `${TITLE_A}|No work item`,
      "a3: a closed agent leaves the menu",
    );
    for (const label of [TITLE_A, "No work item"]) {
      await row(0, label).click();
      assert.ok(
        !(await labels(1)).some((name) => /helper|ho-extra/.test(name)),
        `a3: ${label} lists no helper and no closed agent`,
      );
    }
    assert.ok(
      titleReads.length >= 1,
      "a3: the title came from the work-item read",
    );
    await closeMenu();

    // a4: a fresh page load restores exactly the helper and the handler.
    await waitFor(
      async () =>
        JSON.stringify((await readVault()).workspace.shownAgents) ===
        JSON.stringify([handler.id]),
      "a4: the saved workspace lists exactly the handler",
    );
    ssh.attachLog.length = 0;
    await hub.navigateWithHubHeld(page, () => page.reload());
    await page.locator("#lockscreen").waitFor();
    await unlock();
    await waitFor(restored, "a4: restore after a reload", 90000);
    await waitFor(async () => {
      tabs = await inventory();
      return (
        sessionsOf(tabs, "helper-only")?.join() === "ho-handler" &&
        tabs
          .find((t) => t.home)
          ?.panes.map((p) => p.session)
          .join() === "helper-ho" &&
        tabs.every((t) => t.panes.every((p) => p.status === "Connected"))
      );
    }, "a4: the helper and the handler after a reload");
    await sleep(1000);
    for (const name of ["ho-lead", "ho-builder"])
      assert.ok(!ssh.attachLog.includes(name), `a4: ${name} is not attached`);
    await checkHelperWindow(page, {
      id: hoId,
      project: "helper-only",
      session: "helper-ho",
      same: hoBox,
      step: "a4 after a reload",
    });
    // Across the next load, a restarted shown agent reopens on its new run
    // and a restarted agent that is not shown opens nothing.
    const oldHandlerRun = handler.runId;
    hub.api.restartAgent(handler.id);
    hub.api.restartAgent(builder.id);
    assert.notEqual(handler.runId, oldHandlerRun);
    ssh.attachLog.length = 0;
    await hub.navigateWithHubHeld(page, () => page.reload());
    await page.locator("#lockscreen").waitFor();
    await unlock();
    await waitFor(restored, "a4: restore after the restarts", 90000);
    await waitFor(
      async () => (await savedTab("ho-handler"))?.task?.runId === handler.runId,
      "a4: the restarted handler on its new run",
    );
    await sleep(1000);
    tabs = await inventory();
    assert.equal(sessionsOf(tabs, "helper-only").join(), "ho-handler");
    assert.ok(
      !ssh.attachLog.includes("ho-builder"),
      "a4: the restarted builder opens nothing",
    );
    assert.ok(!ssh.attachLog.includes("ho-lead"));

    // a2/a4 without a helper: removing its last pane removes its tab, and the
    // strip ▾ and the palette still list it, across a reload.
    from = hub.api.requests.length;
    const gRuns = JSON.stringify([g1, g2].map((a) => [a.status, a.runId]));
    const gTab = page.locator("#tabs .tab.task-tab", {
      has: page.locator(".tab-name", { hasText: /^helperless$/ }),
    });
    await gTab.locator("button[role=tab]").click();
    await gTab.locator("[data-terminal-menu]").click();
    await row(0, "No work item").click();
    assert.deepEqual(await labels(1), ["hl-one", "hl-two"]);
    assert.ok((await checked(1, "hl-one")) && (await checked(1, "hl-two")));
    await row(1, "hl-one").click();
    await waitFor(
      async () => !(await checked(1, "hl-one")),
      "helperless: menu removal",
    );
    await closeMenu();
    await gTab.locator("button[role=tab]").click();
    await waitFor(
      async () => !(await paneId("hl-one")) && !!(await paneId("hl-two")),
      "helperless: hl-two left",
    );
    await page
      .locator(`.pane-header[data-pane="${await paneId("hl-two")}"]`)
      .getByRole("button", { name: "Close pane", exact: true })
      .click();
    await waitFor(
      async () => !(await gTab.count()),
      "helperless: its tab goes with its last pane",
    );
    assert.ok(await stripMenu.isVisible(), "a2: the strip ▾ shows");
    await stripMenu.click();
    await waitFor(
      async () => (await labels(0)).includes("helperless"),
      "a2: the strip menu lists the helperless project",
    );
    await closeMenu();
    await palette("Project: pick terminals…");
    await waitFor(
      async () =>
        (await labels(0)).includes("helperless") &&
        (await page.evaluate(
          () =>
            document.activeElement ===
            document.querySelector(
              '.terminal-menu[data-level="0"] [data-menu-key]',
            ),
        )),
      "a2: the palette command opens the same menu, focused",
    );
    await closeMenu();
    // A fresh load restores none of its panes and keeps it listed.
    await waitFor(async () => {
      const { workspace } = await readVault();
      return (
        workspace.tasks.includes(G.id) &&
        !workspace.tabs.some((t) => ["hl-one", "hl-two"].includes(t.session))
      );
    }, "a4: the helperless project saved without panes");
    ssh.attachLog.length = 0;
    await hub.navigateWithHubHeld(page, () => page.reload());
    await page.locator("#lockscreen").waitFor();
    await unlock();
    await waitFor(restored, "a4: helperless restore", 90000);
    await waitFor(
      async () => await stripMenu.isVisible(),
      "a4: strip ▾ after reload",
    );
    await sleep(1000);
    for (const name of ["hl-one", "hl-two"])
      assert.ok(!ssh.attachLog.includes(name), `a4: ${name} is not restored`);
    await stripMenu.click();
    await row(0, "helperless").click();
    await row(1, "No work item").click();
    assert.equal(await checked(2, "hl-two"), false);
    await row(2, "hl-two").click();
    await waitFor(async () => {
      tabs = await inventory();
      const saved = await savedTab("hl-two");
      return (
        sessionsOf(tabs, "helperless")?.join() === "hl-two" &&
        saved?.task?.runId === g2.runId
      );
    }, "a2/a4: re-adding reopens the same run in a project tab");
    assert.deepEqual(hubWrites(from, G.id), [], "helperless: no hub call");
    assert.equal(
      JSON.stringify([g1, g2].map((a) => [a.status, a.runId])),
      gRuns,
      "helperless: no agent changed",
    );
    await closeMenu();

    // a1: a workspace saved before shownAgents restores only the helper.
    await homeTab.click();
    await homeMenu.click();
    for (const [label, name] of [
      [TITLE_A, "ho-lead"],
      [TITLE_A, "ho-builder"],
    ]) {
      await row(0, label).click();
      await row(1, name).click();
      await waitFor(async () => checked(1, name), `${name} chosen`);
    }
    await closeMenu();
    await waitFor(async () => {
      const { workspace, sessions } = await readVault();
      return ["ho-lead", "ho-builder", "ho-handler"].every(
        (name) =>
          workspace.tabs.some(
            (t) => t.session === name && t.task?.taskId === H.id,
          ) && sessions.find((s) => s.name === name)?.task,
      );
    }, "a1: three agent panes saved");
    await lock();
    const record = await page.evaluate(
      () =>
        new Promise((resolve, reject) => {
          const r = indexedDB.open("tailserve", 1);
          r.onsuccess = () => {
            const q = r.result
              .transaction("vault")
              .objectStore("vault")
              .get("encrypted-v2");
            q.onsuccess = () => resolve(q.result);
            q.onerror = reject;
          };
          r.onerror = reject;
        }),
    );
    const opened = await openVault(record, passphrase);
    delete opened.data.workspace.shownAgents;
    const envelope = await sealVault(opened.data, opened.key, opened.salt, 2);
    await page.evaluate(
      (value) =>
        new Promise((resolve, reject) => {
          const r = indexedDB.open("tailserve", 1);
          r.onsuccess = () => {
            const tx = r.result.transaction("vault", "readwrite");
            tx.objectStore("vault").put(value, "encrypted-v2");
            tx.oncomplete = resolve;
            tx.onerror = reject;
          };
          r.onerror = reject;
        }),
      envelope,
    );
    ssh.attachLog.length = 0;
    await unlock();
    await waitFor(restored, "a1: restore of a pre-change workspace", 90000);
    await waitFor(async () => {
      tabs = await inventory();
      return tabs
        .find((t) => t.home)
        ?.panes.some(
          (p) => p.session === "helper-ho" && p.status === "Connected",
        );
    }, "a1: the helper restored");
    await sleep(1000);
    tabs = await inventory();
    assert.ok(
      !tabs.some((t) => t.project && t.name === "helper-only"),
      "a1: a pre-change workspace shows no project tab",
    );
    for (const name of ["ho-lead", "ho-builder", "ho-handler"])
      assert.ok(
        !ssh.attachLog.includes(name),
        `a1: pre-change ${name} is not reconnected`,
      );
    const after = await readVault();
    assert.deepEqual(after.workspace.shownAgents, [], "a1: nothing is shown");
    for (const name of ["ho-lead", "ho-handler"])
      assert.equal(
        after.sessions.find((s) => s.name === name)?.task?.taskId,
        H.id,
        `a1: ${name}'s bookmark stays bound (the agent is alive)`,
      );
    assert.deepEqual(
      hubWrites(from, H.id),
      [],
      "a1: the restore wrote nothing to the hub",
    );

    for (const { id } of [H, G]) hub.api.closeTask(id);
    await waitFor(
      async () =>
        (await page.locator("#tabs .tab.task-tab").count()) === 0 &&
        (await page.locator(".tab-strip > .home-tab [data-tab]").count()) ===
          0 &&
        (await stripMenu.isHidden()),
      "helper-only projects closed",
    );
    for (const a of [helper, lead, builder, handler, extra, g1, g2])
      ssh.sessions.delete(a.session);
    console.log(
      `Helper-only terminals passed: a fresh workspace and a pre-change one show only the helper; the Home, tab and strip ▾ add, remove and re-add the same run with no hub call; work-item sub-menus by title or id with the lead first and keyboard navigation; the choice survives a reload; the helperless project stays reachable after its last pane.`,
    );
  }
}
