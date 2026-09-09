const MAX_CONTEXT_BYTES = 128 * 1024;

async function collectPages(read, field) {
  const values = [];
  let after = 0;
  for (let pageCount = 0; pageCount < 10000; pageCount++) {
    const page = await read(after);
    if (!page || !Array.isArray(page[field]))
      throw new Error("The hub returned an invalid work-item history page.");
    values.push(...page[field]);
    const next = Number(page.nextAfter || 0);
    if (!next) return { values, coverage: page.coverage };
    if (!Number.isSafeInteger(next) || next <= after)
      throw new Error("The work-item history cursor did not advance.");
    after = next;
  }
  throw new Error("The work-item history exceeded the page limit.");
}

// Build the immutable bundle used at admission from the accepted history
// interface. No prose, reply chain, recipient or timestamp inference is used.
export async function prepareWorkItemContext(
  client,
  { itemTaskId, itemId, itemRevision, workOrderMessage },
) {
  if (
    !/^tsk_[0-9a-f]{16}$/.test(itemTaskId) ||
    !/^wi_[0-9a-f]{16}$/.test(itemId) ||
    !Number.isSafeInteger(itemRevision) ||
    itemRevision < 1 ||
    workOrderMessage?.taskId !== itemTaskId ||
    !Number.isSafeInteger(workOrderMessage?.seq) ||
    workOrderMessage.seq < 1
  )
    throw new Error("Choose an exact item revision and recorded work order.");
  const [revision, revisionPages, gapPages, messagePages] = await Promise.all([
    client.getWorkItemRevision(itemTaskId, itemId, itemRevision),
    collectPages(
      (after) =>
        client.listWorkItemRevisions(itemTaskId, itemId, { after, limit: 64 }),
      "revisions",
    ),
    collectPages(
      (after) =>
        client.listWorkItemHistoryGaps(itemTaskId, itemId, {
          after,
          limit: 64,
        }),
      "gaps",
    ),
    collectPages(
      (after) =>
        client.listWorkItemMessages(itemTaskId, itemId, { after, limit: 64 }),
      "links",
    ),
  ]);
  const coverage = revisionPages.coverage || messagePages.coverage;
  if (coverage?.conversationLinks !== "explicit_only")
    throw new Error("The hub did not provide explicit-only context coverage.");
  const order = messagePages.values.find(
    (link) =>
      link?.message?.taskId === workOrderMessage.taskId &&
      link?.message?.seq === workOrderMessage.seq &&
      link?.relationship === "primary",
  );
  if (!order)
    throw new Error(
      "The recorded work-order message is not explicitly linked to this item.",
    );
  const bundle = {
    version: 1,
    itemTaskId,
    itemId,
    itemRevision,
    workOrderMessage,
    history: {
      revision,
      revisions: revisionPages.values,
      gaps: gapPages.values,
      messages: messagePages.values,
      coverage,
    },
  };
  if (
    new TextEncoder().encode(JSON.stringify(bundle)).length > MAX_CONTEXT_BYTES
  )
    throw new Error(
      "This item’s complete immutable history exceeds the 128 KiB session-context limit. Starting this team is currently unsupported; no context was truncated.",
    );
  return bundle;
}
