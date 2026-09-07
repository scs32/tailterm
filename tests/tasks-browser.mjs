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
  await palette("Task hub: configure");
  await page.locator("#hub-url").fill(origin + "/fixture-hub");
  await page.locator("#hub-test").click();
  await page.getByText("Hub sees this browser as fixture-browser").waitFor();
  await page.locator("#hub-save").click();
  await page.getByText("Task hub saved.").waitFor();

  // Create a task with its first agent from the current tab.
  const before = await panes();
  await palette("Task: new…");
  await page.locator("#task-name").fill("demo");
  await page.locator("#task-goal").fill("prove the mirror");
  await page.locator("#agent-name").fill("planner");
  await page.locator("#task-create").click();
  await waitFor(
    async () =>
      /planner is starting/.test(await page.locator("#notice").innerText()) ||
      (await page.locator("#task-error").count()) === 0,
    "task creation (" +
      (await page
        .locator("#task-error")
        .innerText()
        .catch(() => "")) +
      ")",
  );
  const task = hub.api.tasks().find((t) => t.name === "demo");
  assert.ok(task, "task created on the hub");
  const planner = hub.api.agents().find((a) => a.name === "planner");
  assert.ok(planner, "tt spawn registered the agent through the SSH fixture");
  await waitFor(async () => (await panes()) === before + 1, "planner pane");
  assert.equal(await page.locator("#tabs .tab.task-tab").count(), 1);
  await waitFor(
    async () => /Task demo: 1 agent/.test(await taskTitle()),
    "task rollup in the tab tooltip",
  );

  // An agent the hub reports later joins the same tab.
  const tester = hub.api.addAgent(task.id, {
    name: "tester",
    session: "tester",
  });
  await waitFor(async () => (await panes()) === before + 2, "tester pane");
  assert.equal(await page.locator("#tabs .tab.task-tab").count(), 1);

  // Lifecycle events raise attention on the pane that is not focused.
  await page.locator(".terminal-instance:not([hidden])").first().click();
  hub.api.event(task.id, "started", tester.id);
  hub.api.event(task.id, "done", tester.id);
  await waitFor(
    async () => /Command finished/.test(await taskTitle()),
    "command finished label",
  );
  assert.match(await taskTitle(), /1 done/);

  // Board: post as the human, then receive an agent message live.
  await palette("Task demo: board");
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
  assert.equal(await page.locator(".board-agent").count(), 2);
  await page.locator("#dialog-close").click();

  // Closing an agent removes its pane; closing the task removes the rest.
  hub.api.event(task.id, "closed", tester.id);
  await waitFor(
    async () => (await panes()) === before + 1,
    "tester pane closed",
  );
  hub.api.closeTask(task.id);
  await waitFor(async () => (await panes()) === before, "task panes closed");

  // Detach through the palette leaves the tab without a task marker.
  await palette("Task demo: detach from this tab");
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
    "Tasks passed: hub configuration, spawn over SSH, mirrored panes, attention labels, board, close and detach.",
  );
}
