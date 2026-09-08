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
    exportedAt: new Date().toISOString(),
    ...detail,
    messages,
    events,
  };
}
