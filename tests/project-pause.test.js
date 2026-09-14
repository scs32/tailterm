import test from "node:test";
import assert from "node:assert/strict";
import {
  assertPauseEvidence,
  assertPauseSnapshot,
  assertProjectLifecycle,
  assertResumeEvidence,
  assertResumeSnapshot,
  pauseStateLabel,
  projectPauseState,
  supportsProjectPauseV1,
} from "../client/project-pause.js";
import { createHubClient } from "../client/hub-client.js";

const capability = { projectPause: { supported: true, versions: [1] } };
const digest = "a".repeat(64);
const target = {
  agentId: "agt_0123456789abcdef",
  runId: "run_0123456789abcdef",
};

test("project Pause capability and lifecycle states fail closed", () => {
  assert.equal(supportsProjectPauseV1(capability), true);
  assert.equal(
    supportsProjectPauseV1({ projectPause: { versions: [1] } }),
    false,
  );
  assert.equal(projectPauseState({ pauseState: "paused" }, {}), "legacy");
  assert.equal(
    projectPauseState({ pauseState: "mystery" }, capability),
    "unknown",
  );
  assert.equal(
    projectPauseState({ pauseState: "cleanup_pending" }, capability),
    "cleanup_pending",
  );
  assert.equal(
    projectPauseState({ pauseState: "resuming" }, capability),
    "resuming",
  );
  assert.equal(
    pauseStateLabel(
      { pauseState: "cleanup_pending", pauseCleanupPending: 2 },
      capability,
      "3 agents",
    ),
    "Pause cleanup pending · 2",
  );
});

test("resume evidence binds the exact fresh orchestrator and receipt", () => {
  const resume = {
    state: "uncertain",
    requestId: "resume_request_1",
    expectedPauseGeneration: 2,
    expectedLifecycleGeneration: 5,
    retainedHandoffDigest: digest,
    selectedTeamId: "team-synthetic",
    orchestratorAgentId: "agt_1111111111111111",
    orchestratorRunId: "run_1111111111111111",
    orchestratorName: "fresh-lead",
  };
  const result = {
    version: 1,
    taskId: "tsk_aaaaaaaaaaaaaaaa",
    state: "resuming",
    pauseGeneration: 2,
    lifecycleGeneration: 6,
    retainedHandoffDigest: digest,
    receipt: {
      id: "ppr_1111111111111111",
      operation: "resume",
      requestId: resume.requestId,
      taskId: "tsk_aaaaaaaaaaaaaaaa",
      pauseGeneration: 2,
    },
    resumeAdmission: {
      receiptId: "ppr_1111111111111111",
      selectedTeamId: "team-synthetic",
      orchestrator: {
        agentId: resume.orchestratorAgentId,
        runId: resume.orchestratorRunId,
        name: resume.orchestratorName,
      },
      pending: true,
    },
  };
  assert.equal(assertResumeEvidence(result, resume), result);
  assert.throws(
    () =>
      assertResumeEvidence(
        {
          ...result,
          resumeAdmission: {
            ...result.resumeAdmission,
            orchestrator: {
              ...result.resumeAdmission.orchestrator,
              runId: "run_2222222222222222",
            },
          },
        },
        resume,
      ),
    /incomplete resume admission evidence/,
  );
  assert.equal(
    assertResumeSnapshot(result, resume, {
      id: result.taskId,
      lifecycleGeneration: 6,
    }),
    result,
  );
});

test("project lifecycle assertions require open authoritative generations", () => {
  assert.doesNotThrow(() =>
    assertProjectLifecycle(
      { status: "open", pauseState: "active", lifecycleGeneration: 0 },
      "active",
    ),
  );
  assert.throws(
    () =>
      assertProjectLifecycle(
        { status: "open", pauseState: "active" },
        "active",
      ),
    /generation is missing/,
  );
  assert.throws(
    () =>
      assertProjectLifecycle(
        { status: "closed", pauseState: "paused", lifecycleGeneration: 2 },
        "paused",
      ),
    /not fully paused/,
  );
});

test("pause mutations require complete receipt, generation and exact targets", () => {
  const result = {
    version: 1,
    state: "cleanup_pending",
    pauseGeneration: 1,
    lifecycleGeneration: 4,
    cleanupPending: 1,
    retainedHandoffDigest: digest,
    targets: [target],
    receipt: { id: "pause-receipt" },
  };
  assert.equal(assertPauseEvidence(result, 3), result);
  assert.throws(
    () =>
      assertPauseEvidence(
        { ...result, targets: [{ ...target, runId: "other" }] },
        3,
      ),
    /incomplete pause evidence/,
  );
  assert.throws(
    () => assertPauseEvidence({ ...result, receipt: null }, 3),
    /incomplete pause evidence/,
  );
});

test("pause snapshot must match task lifecycle and carry retained evidence", () => {
  const task = { pauseGeneration: 2, lifecycleGeneration: 5 };
  const snapshot = {
    version: 1,
    state: "paused",
    pauseGeneration: 2,
    lifecycleGeneration: 5,
    retainedHandoffDigest: digest,
    targets: [target],
    receipt: { id: "pause-receipt" },
  };
  assert.equal(assertPauseSnapshot(snapshot, task), snapshot);
  assert.throws(
    () => assertPauseSnapshot({ ...snapshot, lifecycleGeneration: 4 }, task),
    /snapshot or receipt is unavailable/,
  );
});

test("project pause client uses the additive GET and mutation routes exactly", async () => {
  const requests = [];
  const client = createHubClient({
    baseURL: "http://synthetic.invalid",
    fetchImpl: async (url, init) => {
      requests.push({
        path: new URL(url).pathname,
        method: init.method,
        body: init.body && JSON.parse(init.body),
      });
      return { status: 200, statusText: "", text: async () => "{}" };
    },
  });
  const pause = { requestId: "pause-request" };
  const handoff = { requestId: "handoff-request" };
  const resume = { requestId: "resume-request" };
  await client.getTaskPause("tsk_aaaaaaaaaaaaaaaa");
  await client.pauseTask("tsk_aaaaaaaaaaaaaaaa", pause);
  await client.resolvePauseHandoff("tsk_aaaaaaaaaaaaaaaa", handoff);
  await client.resumeTask("tsk_aaaaaaaaaaaaaaaa", resume);
  assert.deepEqual(requests, [
    {
      path: "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/pause",
      method: "GET",
      body: undefined,
    },
    {
      path: "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/pause",
      method: "POST",
      body: pause,
    },
    {
      path: "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/pause/handoff",
      method: "POST",
      body: handoff,
    },
    {
      path: "/v1/tasks/tsk_aaaaaaaaaaaaaaaa/resume",
      method: "POST",
      body: resume,
    },
  ]);
});
