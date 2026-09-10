// Review regressions for wi_a222a4d8a69c1d53 revision 2, order #1640/#1642.
// Exercises only in-browser synthetic clients and in-memory persistence.
import { chromium, webkit } from "@playwright/test";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { once } from "node:events";
import assert from "node:assert/strict";

const source = await readFile(
  new URL("../client/queue-view.js", import.meta.url),
);
const server = createServer((request, response) => {
  response.setHeader(
    "content-type",
    request.url === "/queue.js" ? "text/javascript" : "text/html",
  );
  response.end(
    request.url === "/queue.js"
      ? source
      : '<main id="root"></main><dialog id="dialog"></dialog>',
  );
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
const origin = `http://127.0.0.1:${server.address().port}`;

const setup = async (page, scenario) =>
  page.evaluate(async (name) => {
    const { createQueueView } = await import("/queue.js");
    const entry = (client, revision = 1) => ({
      id: "que_0000000000000001",
      targetTaskId: "tsk_0000000000000001",
      sourceTaskId: "tsk_0000000000000002",
      itemId: "wi_0000000000000001",
      revision,
      cycle: 1,
      state: "waiting",
      eligible: true,
      queuePriority: client === "b" ? "low" : "normal",
      orchestratorAgentId: "agt_0000000000000001",
      orchestratorRunId: "run_0000000000000001",
      item: {
        title: `Entry ${client.toUpperCase()}`,
        kind: "bug",
        status: "open",
      },
    });
    window.qa = {
      calls: [],
      notices: [],
      removed: [],
      durable: {},
      currentName: "a",
      entry,
    };
    const persistence = {
      list: async (scope) =>
        Object.values(qa.durable).filter((intent) => intent.scope === scope),
      save: async (intent) => {
        qa.durable[intent.id] = structuredClone(intent);
        if (qa.pauseSave)
          await new Promise((resolve) => (qa.releaseSave = resolve));
      },
      remove: async (scope, id, requestId) => {
        qa.removed.push([scope, id, requestId]);
        if (qa.pauseRemove) {
          qa.pauseRemove = false;
          await new Promise((resolve) => (qa.releaseRemove = resolve));
        }
        const current = qa.durable[id];
        if (
          current?.scope === scope &&
          (!requestId || current.requestId === requestId)
        )
          delete qa.durable[id];
      },
    };
    const makeClient = (clientName) => ({
      base: `https://${clientName}.invalid`,
      token: clientName,
      capabilities: async () => ({ queue: { versions: [1] } }),
      listTasks: async () => [
        {
          id: "tsk_0000000000000001",
          name: `Project ${clientName}`,
          status: "open",
        },
      ],
      listQueue: async () => {
        if (clientName === "a" && qa.pauseList)
          await new Promise((resolve) => (qa.releaseList = resolve));
        return { entries: [entry(clientName)] };
      },
      listQueueHistory: async () => {
        if (qa.pauseHistory)
          await new Promise((resolve) => (qa.releaseHistory = resolve));
        return {
          events: [
            {
              seq: 1,
              cycle: 1,
              revision: 1,
              kind: `history-${clientName}`,
              effectiveAt: "2026-09-10T00:00:00Z",
            },
          ],
          complete: true,
        };
      },
      queueAction: async (task, id, payload) => {
        qa.calls.push({ client: clientName, task, id, payload });
        if (qa.pauseAction)
          await new Promise((resolve) => (qa.releaseAction = resolve));
        if (qa.lose || qa.loseRequests?.includes(payload.requestId))
          throw new Error("Synthetic lost response");
        return {
          entry: {
            ...entry(clientName, 2),
            queuePriority: payload.priority || "normal",
          },
        };
      },
      subscribe: () => ({ stop() {} }),
    });
    qa.a = makeClient("a");
    qa.b = makeClient("b");
    const view = createQueueView({
      client: () => qa[qa.currentName],
      configure() {},
      notice: (value) => qa.notices.push(value),
      dialog: (_title, body) => {
        const modal = document.querySelector("#dialog");
        modal.innerHTML = body;
        modal.showModal();
      },
      closeDialog: () => document.querySelector("#dialog").close(),
      intentPersistence: persistence,
    });
    qa.view = view;
    view.mount(document.querySelector("#root"));
    await view.show("tsk_0000000000000001");
    qa.scenario = name;
  }, scenario);

try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch();
    const name = engine.name();
    try {
      // An action paused in persistence keeps its captured connection. A
      // replacement credential never receives the old request or its result.
      {
        const page = await browser.newPage();
        await page.goto(origin);
        await setup(page, "delayed-persistence");
        await page.evaluate(() => (qa.pauseSave = true));
        await page.selectOption("[data-queue-priority]", "urgent");
        await page.waitForFunction(() => typeof qa.releaseSave === "function");
        await page.evaluate(() => {
          qa.currentName = "b";
          qa.releaseSave();
        });
        await page.waitForFunction(() => qa.calls.length === 1);
        const result = await page.evaluate(() => ({
          call: qa.calls[0],
          removed: qa.removed,
          notices: qa.notices,
        }));
        assert.equal(result.call.client, "a");
        assert.equal(result.removed.length, 1);
        assert.equal(result.removed[0][0].length, 64);
        assert.equal(result.notices.length, 0);
        await page.close();
      }

      // A view generation change while persistence is pending leaves the exact
      // intent durable for later recovery and does not transmit it in the old
      // interaction turn.
      {
        const page = await browser.newPage();
        await page.goto(origin);
        await setup(page, "delayed-persistence-view-change");
        await page.evaluate(() => (qa.pauseSave = true));
        await page.selectOption("[data-queue-priority]", "urgent");
        await page.waitForFunction(() => typeof qa.releaseSave === "function");
        await page.evaluate(() => {
          qa.view.hide();
          void qa.view.show("tsk_0000000000000001");
          qa.releaseSave();
        });
        await page.waitForFunction(() => Object.keys(qa.durable).length === 1);
        const result = await page.evaluate(() => ({
          calls: qa.calls.length,
          durable: Object.values(qa.durable).length,
        }));
        assert.deepEqual(result, { calls: 0, durable: 1 });
        await page.close();
      }

      // A late list from an old connection cannot repaint over a queued reload
      // from the replacement connection.
      {
        const page = await browser.newPage();
        await page.goto(origin);
        await setup(page, "delayed-list");
        await page.evaluate(() => {
          qa.pauseList = true;
          void qa.view.reload();
        });
        await page.waitForFunction(() => typeof qa.releaseList === "function");
        await page.evaluate(() => {
          qa.currentName = "b";
          void qa.view.reload();
          qa.pauseList = false;
          qa.releaseList();
        });
        await page.getByText("Entry B", { exact: true }).first().waitFor();
        assert.equal(
          await page.getByText("Entry A", { exact: true }).count(),
          0,
        );
        await page.close();
      }

      // Late history and mutation responses cannot paint into a replacement
      // connection/project view. Confirmed cleanup retains the original scope.
      {
        const page = await browser.newPage();
        await page.goto(origin);
        await setup(page, "delayed-history-action");
        await page.evaluate(() => (qa.pauseHistory = true));
        await page.getByRole("button", { name: "History" }).click();
        await page.waitForFunction(
          () => typeof qa.releaseHistory === "function",
        );
        await page.evaluate(() => {
          qa.currentName = "b";
          void qa.view.reload();
          qa.releaseHistory();
        });
        await page.getByText("Entry B", { exact: true }).first().waitFor();
        assert.equal(
          await page.getByText("history-a", { exact: true }).count(),
          0,
        );

        await page.evaluate(() => {
          qa.currentName = "a";
          qa.pauseAction = true;
          void qa.view.reload();
        });
        await page.getByText("Entry A", { exact: true }).first().waitFor();
        await page.selectOption("[data-queue-priority]", "urgent");
        await page.waitForFunction(
          () => typeof qa.releaseAction === "function",
        );
        await page.evaluate(() => {
          qa.currentName = "b";
          qa.pauseAction = false;
          void qa.view.reload();
          qa.releaseAction();
        });
        await page.getByText("Entry B", { exact: true }).first().waitFor();
        await page.waitForFunction(() => qa.removed.length === 1);
        const result = await page.evaluate(() => ({
          clients: qa.calls.map((call) => call.client),
          notices: qa.notices,
          removedScope: qa.removed[0][0],
        }));
        assert.deepEqual(result.clients, ["a"]);
        assert.equal(result.notices.length, 0);
        assert.equal(result.removedScope.length, 64);
        await page.close();
      }

      // Lost same-operation attempts remain distinct through reload. An older
      // exact replay plus delayed cleanup cannot discard later unresolved work.
      {
        const page = await browser.newPage();
        await page.goto(origin);
        await setup(page, "multiple-intents");
        await page.evaluate(() => (qa.lose = true));
        await page.selectOption("[data-queue-priority]", "urgent");
        await page.waitForFunction(() => qa.calls.length === 1);
        await page.selectOption("[data-queue-priority]", "high");
        await page.waitForFunction(() => qa.calls.length === 2);
        let result = await page.evaluate(() => ({
          requests: qa.calls.map((call) => call.payload.requestId),
          durable: Object.values(qa.durable).map((intent) => intent.requestId),
        }));
        assert.equal(new Set(result.requests).size, 2);
        assert.deepEqual(new Set(result.durable), new Set(result.requests));
        await page.evaluate(async () => {
          qa.lose = false;
          await qa.view.reload();
          qa.pauseRemove = true;
        });
        const retries = page.getByRole("button", {
          name: "Retry exact request",
        });
        assert.equal(await retries.count(), 2);
        await retries.first().click();
        await page.waitForFunction(
          () => typeof qa.releaseRemove === "function",
        );
        await page.evaluate(() => (qa.lose = true));
        await page.selectOption("[data-queue-priority]", "low");
        await page.waitForFunction(() => qa.calls.length === 4);
        await page.evaluate(() => qa.releaseRemove());
        await page.waitForFunction(
          () =>
            document.querySelectorAll("[data-queue-retry-intent]").length === 2,
        );
        result = await page.evaluate(() => ({
          first: qa.calls[0].payload.requestId,
          replay: qa.calls[2].payload.requestId,
          remaining: Object.values(qa.durable).map(
            (intent) => intent.requestId,
          ),
        }));
        assert.equal(result.replay, result.first);
        assert.equal(result.remaining.length, 2);
        assert.ok(!result.remaining.includes(result.first));
        await page.close();
      }
      console.log(
        `${name}: Queue connection/view epochs and distinct uncertain attempts passed.`,
      );
    } finally {
      await browser.close();
    }
  }
} finally {
  await new Promise((resolve) => server.close(resolve));
}
