import test from "node:test";
import assert from "node:assert/strict";
import {
  readProjectAuditExport,
  readTaskHistory,
} from "../client/task-history.js";

test("history export reads all pages with separate message/event sequence spaces", async () => {
  const messages = Array.from({ length: 405 }, (_, i) => ({
    seq: i + 1000,
    text: `message ${i}`,
  }));
  const events = Array.from({ length: 203 }, (_, i) => ({ seq: i + 1 }));
  const client = {
    getTask: async () => ({
      task: { id: "task", status: "closed" },
      agents: [],
      latestSeq: 202,
    }),
    listMessages: async (_, { after, limit }) =>
      messages.filter((m) => m.seq > after).slice(0, limit),
    events: async (_, { after, limit }) => ({
      events: events.filter((e) => e.seq > after).slice(0, limit),
    }),
  };
  const history = await readTaskHistory(client, "task");
  assert.deepEqual(history.messages, messages);
  assert.deepEqual(history.events, events.slice(0, 202));
  assert.equal(history.task.status, "closed");
  assert.equal(history.snapshot, false);
  assert.equal(history.consistency, "non-snapshot-legacy");
});

test("v2 project export verifies immutable chunk identity and digest", async () => {
  const document = {
    schema: "tailterm-project-audit",
    formatVersion: 2,
    sourceProject: "task",
    streams: { task: [{ name: "Synthetic" }] },
  };
  const bytes = new TextEncoder().encode(JSON.stringify(document) + "\n");
  const digest = [
    ...new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
  ]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
  const client = {
    createAuditExport: async () => ({
      id: "aex_one",
      available: true,
      byteCount: bytes.length,
      sha256: digest,
    }),
    getAuditExportChunk: async (_, id, { offset, limit }) => {
      const end = Math.min(bytes.length, offset + Math.min(limit, 7));
      return {
        exportId: id,
        offset,
        nextOffset: end,
        total: bytes.length,
        sha256: digest,
        data: Buffer.from(bytes.slice(offset, end)).toString("base64"),
        complete: end === bytes.length,
      };
    },
  };
  const result = await readProjectAuditExport(client, "task", {
    requestId: "stable",
  });
  assert.deepEqual(result.document, document);
  assert.deepEqual(result.bytes, bytes);
});

test("v2 project export resumes recovered metadata and rejects unsafe chunks", async () => {
  const metadata = {
    id: "aex_recovered",
    available: true,
    byteCount: 8,
    sha256: "a".repeat(64),
  };
  let observed;
  const recovered = {
    createAuditExport: async () => {
      throw new Error("must not create a replacement");
    },
    getAuditExportChunk: async () => ({
      exportId: metadata.id,
      offset: 0,
      nextOffset: 0,
      total: metadata.byteCount,
      sha256: metadata.sha256,
      data: "",
      complete: false,
    }),
  };
  await assert.rejects(
    readProjectAuditExport(recovered, "task", {
      requestId: "same-key",
      metadata,
      onMetadata: async (value) => (observed = value),
    }),
    /did not advance/i,
  );
  assert.equal(observed.id, metadata.id);

  for (const [name, change] of [
    ["wrong id", (chunk) => ({ ...chunk, exportId: "aex_other" })],
    ["changing total", (chunk) => ({ ...chunk, total: 9 })],
    [
      "oversized chunk",
      (chunk) => {
        const data = new Uint8Array(256 * 1024 + 1);
        return {
          ...chunk,
          nextOffset: data.byteLength,
          total: data.byteLength,
          data: Buffer.from(data).toString("base64"),
          complete: true,
        };
      },
    ],
  ]) {
    const chunk = {
      exportId: metadata.id,
      offset: 0,
      nextOffset: 8,
      total: 8,
      sha256: metadata.sha256,
      data: Buffer.from("12345678").toString("base64"),
      complete: true,
    };
    const changed = change(chunk);
    const client = {
      createAuditExport: async () => ({
        ...metadata,
        byteCount:
          name === "oversized chunk" ? changed.total : metadata.byteCount,
      }),
      getAuditExportChunk: async () => changed,
    };
    await assert.rejects(
      readProjectAuditExport(client, "task", { requestId: name }),
      /identity|bounds/i,
      name,
    );
  }

  await assert.rejects(
    readProjectAuditExport(
      {
        createAuditExport: async () => ({
          ...metadata,
          byteCount: 32 * 1024 * 1024 + 1,
        }),
      },
      "task",
    ),
    /bounds/i,
  );
});
