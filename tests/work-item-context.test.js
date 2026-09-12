import test from "node:test";
import assert from "node:assert/strict";
import { prepareWorkItemContext } from "../client/work-item-context.js";

const source = {
  itemTaskId: "tsk_0123456789abcdef",
  itemId: "wi_abcdef0123456789",
  itemRevision: 3,
  workOrderMessage: { taskId: "tsk_0123456789abcdef", seq: 814 },
};

test("prepared model context consumes every accepted history page and only explicit links", async () => {
  const calls = [];
  const coverage = {
    complete: false,
    observedCurrentRevision: 3,
    latestMaterializedRevision: 3,
    gapCount: 1,
    conversationLinks: "explicit_only",
  };
  const client = {
    getWorkItemRevision: async (...args) => {
      calls.push(["exact", ...args]);
      return { itemId: source.itemId, revision: 3, description: "Current" };
    },
    listWorkItemRevisions: async (_task, _item, { after }) => ({
      revisions:
        after === 0
          ? [{ revision: 1, description: "Initial" }]
          : [{ revision: 3, description: "Current", provenance: "checkpoint" }],
      nextAfter: after === 0 ? 1 : 0,
      coverage,
    }),
    listWorkItemHistoryGaps: async () => ({
      gaps: [
        { firstRevision: 2, lastRevision: 2, reasonCode: "missing_revision" },
      ],
      coverage,
    }),
    listWorkItemMessages: async () => ({
      links: [
        {
          itemRevision: 3,
          relationship: "primary",
          message: { taskId: source.itemTaskId, seq: 814, text: "Order" },
        },
        {
          itemRevision: 3,
          relationship: "primary",
          message: { taskId: source.itemTaskId, seq: 820, text: "Evidence" },
        },
      ],
      coverage,
    }),
  };
  const bundle = await prepareWorkItemContext(client, source);
  assert.equal(bundle.history.revisions.length, 2);
  assert.equal(bundle.history.gaps[0].reasonCode, "missing_revision");
  assert.deepEqual(
    bundle.history.messages.map((link) => link.message.text),
    ["Order", "Evidence"],
  );
  assert.equal(JSON.stringify(bundle).includes("UNRELATED"), false);
  assert.deepEqual(calls, [
    ["exact", source.itemTaskId, source.itemId, source.itemRevision],
  ]);
});

test("prepared context rejects inferred orders, cursor loops and oversized complete history", async () => {
  const base = {
    getWorkItemRevision: async () => ({ revision: 3 }),
    listWorkItemRevisions: async () => ({
      revisions: [],
      coverage: { conversationLinks: "explicit_only" },
    }),
    listWorkItemHistoryGaps: async () => ({ gaps: [] }),
    listWorkItemMessages: async () => ({
      links: [
        {
          relationship: "primary",
          message: {
            taskId: source.itemTaskId,
            seq: 999,
            text: "Nearby prose",
          },
        },
      ],
    }),
  };
  await assert.rejects(
    () => prepareWorkItemContext(base, source),
    /not explicitly linked/,
  );
  await assert.rejects(
    () =>
      prepareWorkItemContext(
        {
          ...base,
          listWorkItemRevisions: async () => ({
            revisions: [],
            nextAfter: 1,
            coverage: { conversationLinks: "explicit_only" },
          }),
        },
        source,
      ),
    /cursor did not advance/,
  );
  await assert.rejects(
    () =>
      prepareWorkItemContext(
        {
          ...base,
          listWorkItemRevisions: async () => ({
            revisions: [{ revision: 3, description: "x".repeat(262144) }],
            coverage: { conversationLinks: "explicit_only" },
          }),
          listWorkItemMessages: async () => ({
            links: [
              {
                relationship: "primary",
                message: { taskId: source.itemTaskId, seq: 814, text: "Order" },
              },
            ],
          }),
        },
        source,
      ),
    /exceeds the 256 KiB session-context limit.*currently unsupported.*no context was truncated/,
  );
});

// wi_7e220de54deaef33@1/order3022: synthetic history only.
test("complete context accepts measured size and UTF-8 boundary without source loss", async () => {
  for (const size of [152973, 262144, 262145]) {
    const revision = { revision: 3, description: "Full source retained" };
    const links = [
      {
        relationship: "primary",
        message: { ...source.workOrderMessage, text: "" },
      },
    ];
    const client = {
      getWorkItemRevision: async () => revision,
      listWorkItemRevisions: async () => ({
        revisions: [revision],
        coverage: { conversationLinks: "explicit_only" },
      }),
      listWorkItemHistoryGaps: async () => ({ gaps: [] }),
      listWorkItemMessages: async () => ({ links }),
    };
    const empty = await prepareWorkItemContext(client, source);
    const remaining = size - Buffer.byteLength(JSON.stringify(empty));
    links[0].message.text =
      "界".repeat(Math.floor(remaining / 3)) + "x".repeat(remaining % 3);
    if (size > 262144) {
      await assert.rejects(prepareWorkItemContext(client, source), /256 KiB/);
    } else {
      const bundle = await prepareWorkItemContext(client, source);
      assert.equal(Buffer.byteLength(JSON.stringify(bundle)), size);
      assert.deepEqual(bundle.history.revisions, [revision]);
      assert.deepEqual(bundle.history.messages, links);
    }
  }
});
