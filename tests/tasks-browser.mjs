import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { openVault } from "../client/vault-crypto.js";
import { checkGroupAlone, checkHelperWindow } from "./pane-groups-browser.mjs";

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
    await helperWindow(at("after a reconnect"), box);
    const panes = await terminals();
    const joiner = hub.api.addAgent(adopter.id, { name: `joiner-${width}` });
    ssh.sessions.add(joiner.name);
    hub.api.event(adopter.id, "started", joiner.id);
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
  for (const name of ["helper-fx", "helper-hold", "plain-fx", "anchor"])
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
  const lock = async () => {
    await page.locator("#lock").click();
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

  // Cycle 3: the marker comes back and nothing stale attaches.
  ssh.attachLog.length = 0;
  await lock();
  await unlock();
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
  await helperWindow("after the third restore", helperBox);

  // A browser refresh restores the same way.
  await page.reload();
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
  console.log(
    `Task restore passed: stuck restore finished ${finishedIn} ms after the hub closed a connecting pane, no flash, paused/closed projects dropped, live projects bound, finished sessions checked, bookmarks unbound.`,
  );
}
