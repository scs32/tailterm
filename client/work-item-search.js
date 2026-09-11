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
  const unavailable = [];
  if (item.kind === "feature") {
    const pages = async (load, field) => {
      const entries = [],
        seen = new Set();
      let cursor = "";
      do {
        const page = await load(cursor);
        entries.push(...(page[field] || []));
        cursor = page.nextCursor || page.cursor || "";
        if (cursor && seen.has(cursor))
          throw new Error("Narrative search cursor did not advance.");
        if (cursor) seen.add(cursor);
      } while (cursor);
      return entries;
    };
    try {
      const reports = await pages(
        (cursor) =>
          client.listNarrativeReports(item.taskId, item.id, {
            cursor,
            limit: 64,
          }),
        "reports",
      );
      for (const report of reports) {
        const exact = await client.getNarrativeReportVersion(
          item.taskId,
          item.id,
          report.reportId,
          report.version,
        );
        values.push(...Object.values(exact.sections || {}));
      }
    } catch (error) {
      unavailable.push(`reports: ${error.message}`);
    }
    try {
      const artifacts = await pages(
        (cursor) =>
          client.listNarrativeArtifacts(item.taskId, item.id, {
            cursor,
            limit: 64,
          }),
        "artifacts",
      );
      for (const artifact of artifacts) {
        const versions = await pages(
          (cursor) =>
            client.listNarrativeArtifactVersions(
              item.taskId,
              item.id,
              artifact.artifactId,
              { cursor, limit: 64 },
            ),
          "versions",
        );
        for (const version of versions) {
          const exact = await client.getNarrativeArtifactVersion(
            item.taskId,
            item.id,
            artifact.artifactId,
            version.version,
          );
          values.push(exact.title, exact.content, exact.locator);
        }
      }
    } catch (error) {
      unavailable.push(`artifacts: ${error.message}`);
    }
  }
  return { text: values.filter(Boolean).join("\n"), unavailable };
}
