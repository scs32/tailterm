// Read every retained page; a board preview must never truncate an export.
export async function readTaskHistory(client, taskId) {
  const detail = await client.getTask(taskId);
  async function pages(fetchPage, through) {
    const rows = [];
    let after = 0;
    for (;;) {
      const batch = await fetchPage(after);
      if (!batch.length) break;
      const next = batch.at(-1).seq;
      if (next <= after) throw new Error("History pagination did not advance.");
      rows.push(...batch.filter((row) => !through || row.seq <= through));
      if (batch.length < 200 || (through && next >= through)) break;
      after = next;
    }
    return rows;
  }
  const [messages, events] = await Promise.all([
    pages((after) => client.listMessages(taskId, { after, limit: 200 })),
    pages(
      async (after) =>
        (await client.events(taskId, { after, limit: 200 })).events,
      detail.latestSeq,
    ),
  ]);
  return {
    format: "tailterm-task-history",
    version: 1,
    snapshot: false,
    consistency: "non-snapshot-legacy",
    exportedAt: new Date().toISOString(),
    ...detail,
    messages,
    events,
  };
}

const MAX_AUDIT_CHUNK = 256 * 1024;
const MAX_AUDIT_EXPORT = 32 * 1024 * 1024;

function decodeBase64(value) {
  const raw = atob(value || "");
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

function hex(bytes) {
  return [...new Uint8Array(bytes)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

// Create and download one immutable v2 server snapshot. Callers deliberately
// choose the legacy helper above when an old hub has no export capability.
export async function readProjectAuditExport(
  client,
  taskId,
  {
    requestId = crypto.randomUUID(),
    metadata: recoveredMetadata,
    onMetadata = async () => {},
  } = {},
) {
  const metadata =
    recoveredMetadata ||
    (await client.createAuditExport(taskId, {
      requestId,
      formatVersion: 2,
    }));
  await onMetadata(structuredClone(metadata));
  if (!metadata.available)
    throw new Error(
      "The immutable audit export has expired or is unavailable.",
    );
  if (
    !Number.isSafeInteger(metadata.byteCount) ||
    metadata.byteCount < 1 ||
    metadata.byteCount > MAX_AUDIT_EXPORT ||
    !/^[a-f0-9]{64}$/.test(metadata.sha256 || "")
  )
    throw new Error("Audit export metadata exceeds the supported bounds.");
  const parts = [];
  let offset = 0;
  for (;;) {
    const chunk = await client.getAuditExportChunk(taskId, metadata.id, {
      offset,
      limit: MAX_AUDIT_CHUNK,
    });
    const data = decodeBase64(chunk.data);
    if (
      chunk.exportId !== metadata.id ||
      chunk.offset !== offset ||
      chunk.nextOffset !== offset + data.byteLength ||
      chunk.total !== metadata.byteCount ||
      chunk.sha256 !== metadata.sha256 ||
      data.byteLength > MAX_AUDIT_CHUNK ||
      chunk.nextOffset > metadata.byteCount
    )
      throw new Error("Audit export chunk identity changed during download.");
    parts.push(data);
    offset = chunk.nextOffset;
    if (chunk.complete) break;
    if (!data.byteLength || offset === metadata.byteCount)
      throw new Error("Audit export download did not advance.");
  }
  if (offset !== metadata.byteCount)
    throw new Error("Audit export download is incomplete.");
  const bytes = new Uint8Array(offset);
  let position = 0;
  for (const part of parts) {
    bytes.set(part, position);
    position += part.byteLength;
  }
  const digest = hex(await crypto.subtle.digest("SHA-256", bytes));
  if (digest !== metadata.sha256)
    throw new Error("Audit export digest verification failed.");
  const document = JSON.parse(new TextDecoder().decode(bytes));
  if (
    document.schema !== "tailterm-project-audit" ||
    document.formatVersion !== 2 ||
    document.sourceProject !== taskId
  )
    throw new Error("Audit export schema or project identity is invalid.");
  return { metadata, document, bytes };
}
