import assert from "node:assert/strict";

// Drives the task hub through the UI against tests/fixture-hub.mjs: hub
// configuration, task creation with a spawned agent, mirroring of agents the
// hub adds and closes, attention labels from lifecycle events, and the board.
export async function exerciseTasks(page, hub, origin) {
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
  console.log(
    "Tasks passed: dedicated task-named groups, automatic agent membership, hub configuration, spawn over SSH, attention, board, and close.",
  );
}
