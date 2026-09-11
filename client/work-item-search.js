const normalize = (value) => String(value ?? "").toLocaleLowerCase();

export const workItemSearchText = (item, historyText = "") =>
  normalize([item.title, item.description, historyText].join("\n"));

export const workItemMatchesSearch = (item, query, historyText = "") => {
  const terms = normalize(query).trim().split(/\s+/).filter(Boolean);
  if (!terms.length) return true;
  const text = workItemSearchText(item, historyText);
  return terms.every((term) => text.includes(term));
};

export async function loadWorkItemSearchHistory(client, item) {
  const revisions = [];
  let after = 0;
  do {
    const page = await client.listWorkItemRevisions(item.taskId, item.id, {
      after,
      limit: 64,
    });
    revisions.push(...(page.revisions || []));
    if (!page.nextAfter) break;
    if (page.nextAfter <= after)
      throw new Error("Work-item history cursor did not advance.");
    after = page.nextAfter;
  } while (true);

  const values = revisions.flatMap((revision) => [
    revision.title,
    revision.description,
  ]);
  for (const revision of revisions) {
    let messageAfter = 0;
    do {
      const page = await client.listWorkItemMessages(item.taskId, item.id, {
        revision: revision.revision,
        after: messageAfter,
        limit: 64,
      });
      values.push(...(page.links || []).map((link) => link.message?.text));
      if (!page.nextAfter) break;
      if (page.nextAfter <= messageAfter)
        throw new Error("Linked-message cursor did not advance.");
      messageAfter = page.nextAfter;
    } while (true);
  }
  return values.filter(Boolean).join("\n");
}
