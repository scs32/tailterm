import test from "node:test";
import assert from "node:assert/strict";
import {
  loadWorkItemSearchHistory,
  workItemMatchesSearch,
} from "../client/work-item-search.js";

const item = {
  id: "wi_1111111111111111",
  taskId: "tsk_1111111111111111",
  title: "Current title",
  description: "Current write-up",
};

test("search matches every term across current fields and loaded history", () => {
  assert.equal(workItemMatchesSearch(item, "CURRENT write-UP"), true);
  assert.equal(
    workItemMatchesSearch(item, "retired symptom", "Retired symptom"),
    true,
  );
  assert.equal(
    workItemMatchesSearch(item, "missing term", "Retired symptom"),
    false,
  );
  assert.equal(workItemMatchesSearch(item, "   "), true);
});

test("history loader retains paged revisions and explicit linked messages", async () => {
  const revisionCalls = [],
    messageCalls = [];
  const client = {
    async listWorkItemRevisions(_task, _id, { after }) {
      revisionCalls.push(after);
      return after === 0
        ? {
            revisions: [
              {
                revision: 1,
                title: "Original title",
                description: "Old wording",
              },
            ],
            nextAfter: 1,
          }
        : {
            revisions: [
              { revision: 2, title: "New title", description: "New wording" },
            ],
            nextAfter: 0,
          };
    },
    async listWorkItemMessages(_task, _id, { revision, after }) {
      messageCalls.push([revision, after]);
      if (revision === 1 && after === 0)
        return {
          links: [{ message: { text: "Historical board phrase" } }],
          nextAfter: 4,
        };
      return {
        links:
          revision === 1 ? [{ message: { text: "Second message page" } }] : [],
        nextAfter: 0,
      };
    },
  };
  const history = await loadWorkItemSearchHistory(client, item);
  assert.deepEqual(revisionCalls, [0, 1]);
  assert.deepEqual(messageCalls, [
    [1, 0],
    [1, 4],
    [2, 0],
  ]);
  assert.equal(workItemMatchesSearch(item, "old wording", history.text), true);
  assert.equal(
    workItemMatchesSearch(item, "historical phrase", history.text),
    true,
  );
  assert.equal(workItemMatchesSearch(item, "second page", history.text), true);
  assert.deepEqual(history.unavailable, []);
});

test("feature history includes all report sections and retained artifact versions", async () => {
  const feature = { ...item, kind: "feature" };
  const client = {
    async listWorkItemRevisions() {
      return { revisions: [], nextAfter: 0 };
    },
    async listWorkItemMessages() {
      return { links: [], nextAfter: 0 };
    },
    async listNarrativeReports() {
      return { reports: [{ reportId: "nrpt_one", version: 2 }] };
    },
    async getNarrativeReportVersion() {
      return { sections: { deliveredWork: "Durable report writeup" } };
    },
    async listNarrativeArtifacts() {
      return { artifacts: [{ artifactId: "nart_one" }] };
    },
    async listNarrativeArtifactVersions() {
      return { versions: [{ version: 1 }, { version: 2 }] };
    },
    async getNarrativeArtifactVersion(_task, _item, _artifact, version) {
      return {
        title: `Artifact ${version}`,
        content: `Captured body ${version}`,
      };
    },
  };
  const history = await loadWorkItemSearchHistory(client, feature);
  assert.equal(
    workItemMatchesSearch(feature, "report writeup", history.text),
    true,
  );
  assert.equal(
    workItemMatchesSearch(feature, "captured body 1", history.text),
    true,
  );
  assert.equal(
    workItemMatchesSearch(feature, "captured body 2", history.text),
    true,
  );
  assert.deepEqual(history.unavailable, []);
});
