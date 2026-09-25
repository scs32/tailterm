import { serializedWorkContext } from "../shared/work-context.js";

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

export async function assertCurrentWorkOrderScope(
  client,
  { itemTaskId, itemId, itemRevision, workOrderMessage },
  knownCurrent = null,
) {
  const current = knownCurrent || await client.getWorkItem(itemTaskId, itemId);
  if (current?.revision !== itemRevision)
    throw new Error("The work item changed; refresh its scope and order before launch.");
  let confirmation;
  try {
    confirmation = await client.getWorkOrderScopeConfirmation(
      itemTaskId,
      itemId,
      itemRevision,
      workOrderMessage.seq,
    );
  } catch (error) {
    throw new Error(
      `Scope is not confirmed for this item revision and order. Ask the database handler to complete intake before launch. ${error.message}`,
    );
  }
  if (
    confirmation?.taskId !== itemTaskId ||
    confirmation?.itemId !== itemId ||
    confirmation?.itemRevision !== itemRevision ||
    confirmation?.orderMessageSeq !== workOrderMessage.seq ||
    confirmation?.scopeRevision !== current.scopeRevision
  )
    throw new Error(
      "Scope confirmation is stale or belongs to another item; ask the database handler to confirm this revision and order.",
    );
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
  const [current, revision, revisionPages, gapPages, messagePages] = await Promise.all([
    client.getWorkItem(itemTaskId, itemId),
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
  if (current?.revision !== itemRevision)
    throw new Error("The work item changed; refresh its scope and order before launch.");
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
  await assertCurrentWorkOrderScope(
    client,
    { itemTaskId, itemId, itemRevision, workOrderMessage },
    current,
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
  serializedWorkContext(bundle);
  return bundle;
}
