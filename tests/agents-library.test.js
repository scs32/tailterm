import test from "node:test";
import assert from "node:assert/strict";
import {
  migrateAgentData,
  normalizeAgentDefinition,
  normalizeReferencedTeam,
  resolveTeamMember,
} from "../client/agents.js";
import { teamLaunches } from "../client/teams.js";
import {
  launchPlanForStorage,
  MAX_TEAM_LAUNCH_PLAN_BYTES,
  migrateUnsupportedUnstartedLaunchReasoning,
  normalizeTeamLaunchPlans,
  restoreLaunchMembers,
  serverLaunchScope,
} from "../client/launch-journal.js";
import {
  guardedLaunchEffect,
  reconciledAgentProblem,
} from "../client/launch-reconciliation.js";
import { agentSpawnCommand } from "../shared/tmux-command.js";
import { MAX_WORK_CONTEXT_BYTES } from "../shared/work-context.js";

const legacyMember = (name, overrides = {}) => ({
  name,
  role: "Builder",
  serverId: "machine-demo-01",
  runtime: "codex",
  model: "gpt-5.3-codex",
  reasoning: "high",
  permissionMode: "",
  approvalMode: "on-request",
  sandboxMode: "workspace-write",
  allowedTools: [],
  run: "codex",
  cwd: "/synthetic/project",
  prompt: `Long reusable prompt for ${name}\n${"context ".repeat(80)}`,
  ...overrides,
});

test("raw catalog migration is lossless, separate per legacy member, atomic and idempotent", () => {
  const source = {
    teams: [
      {
        id: "team_review",
        name: "Review",
        orchestrator: "lead",
        members: [
          legacyMember("lead", { prompt: "\n  preserve exactly  \n" }),
          legacyMember("worker", {
            model: "gpt-5.6-terra",
            reasoning: "",
            run: "codex --search",
          }),
        ],
      },
    ],
  };
  const first = migrateAgentData(source);
  assert.equal(first.agentCatalog.definitions.length, 2);
  assert.equal(
    first.agentCatalog.definitions[0].prompt,
    "\n  preserve exactly  \n",
  );
  assert.notEqual(
    first.agentCatalog.definitions[0].id,
    first.agentCatalog.definitions[1].id,
  );
  const resolved = first.teams[0].members.map((member) =>
    resolveTeamMember(member, first.agentCatalog.definitions),
  );
  assert.equal(resolved[1].run, "codex --search");
  assert.match(resolved[1].prompt, /context/);
  assert.deepEqual(migrateAgentData(first), first);
  const unsupportedReasoning = structuredClone(first);
  unsupportedReasoning.agentCatalog.definitions[0] = {
    ...unsupportedReasoning.agentCatalog.definitions[0],
    runtime: "claude",
    run: "claude",
    reasoning: "high",
    approvalMode: "",
    sandboxMode: "",
  };
  const repairedReasoning = migrateAgentData(unsupportedReasoning);
  assert.equal(repairedReasoning.agentCatalog.definitions[0].reasoning, "");
  assert.equal(
    repairedReasoning.agentCatalog.definitions[0].runtime,
    "claude",
  );
  assert.equal(repairedReasoning.agentCatalog.definitions[0].run, "claude");
  assert.equal(
    unsupportedReasoning.agentCatalog.definitions[0].reasoning,
    "high",
  );
  assert.deepEqual(
    migrateAgentData({
      teams: [],
      launchProfiles: [{ name: "do-not-resurrect", runtime: "codex" }],
    }).teams,
    [],
  );
  assert.throws(
    () =>
      migrateAgentData({
        teams: Array.from({ length: 31 }, (_, index) => ({
          name: `T${index}`,
          members: [legacyMember(`a${index}`)],
        })),
      }),
    /30/,
  );
  assert.throws(
    () =>
      migrateAgentData({
        teamsVersion: 99,
        teams: [],
        agentCatalog: { version: 99, definitions: [] },
      }),
    /Unsupported/,
  );
  const missingDefinitionId = structuredClone(first);
  delete missingDefinitionId.agentCatalog.definitions[0].id;
  assert.throws(
    () => migrateAgentData(missingDefinitionId),
    /agent definition ID/,
  );
  const missingDefinitionRevision = structuredClone(first);
  delete missingDefinitionRevision.agentCatalog.definitions[0].revision;
  assert.throws(
    () => migrateAgentData(missingDefinitionRevision),
    /definition revision/,
  );
  const duplicateDefinitionId = structuredClone(first);
  duplicateDefinitionId.agentCatalog.definitions.push({
    ...duplicateDefinitionId.agentCatalog.definitions[0],
  });
  assert.throws(
    () => migrateAgentData(duplicateDefinitionId),
    /Duplicate agent definition ID/,
  );
  const invalidDuplicateTeamIds = structuredClone(first);
  invalidDuplicateTeamIds.teams = [
    { ...invalidDuplicateTeamIds.teams[0], id: "invalid/id" },
    {
      ...invalidDuplicateTeamIds.teams[0],
      id: "invalid/id",
      name: "Second invalid persisted team",
    },
  ];
  assert.throws(
    () => migrateAgentData(invalidDuplicateTeamIds),
    /Invalid team ID/,
  );
});

test("stable references share future edits while copied launches keep their revision", () => {
  const definition = normalizeAgentDefinition({
    ...legacyMember("builder"),
    id: "agent_builder",
    revision: 1,
    name: "Implementation",
    launchName: "builder",
  });
  const teamA = normalizeReferencedTeam(
    { name: "A", members: [{ agentDefinitionId: definition.id }] },
    [definition],
  );
  const teamB = normalizeReferencedTeam(
    { name: "B", members: [{ agentDefinitionId: definition.id }] },
    [definition],
  );
  const server = [{ id: "machine-demo-01" }];
  const frozen = teamLaunches(teamA, server, undefined, undefined, null, [
    definition,
  ]);
  const edited = normalizeAgentDefinition({
    ...definition,
    revision: 2,
    prompt: "Changed centrally",
  });
  assert.equal(
    teamLaunches(teamA, server, undefined, undefined, null, [edited])[0].fields
      .prompt,
    "Role: Builder\n\nChanged centrally",
  );
  assert.equal(
    teamLaunches(teamB, server, undefined, undefined, null, [edited])[0].fields
      .agentDefinitionRevision,
    2,
  );
  assert.equal(frozen[0].fields.agentDefinitionRevision, 1);
  assert.notEqual(
    frozen[0].fields.prompt,
    "Role: Builder\n\nChanged centrally",
  );
  assert.throws(
    () =>
      normalizeReferencedTeam(
        { name: "Dangling", members: [{ agentDefinitionId: "agent_missing" }] },
        [definition],
      ),
    /definition is missing/,
  );
});

test("reasoning and independent Codex permission intent reach actual tt argv", () => {
  const command = agentSpawnCommand({
    hub: "http://127.0.0.1:18765",
    task: "tsk_0123456789abcdef",
    name: "builder",
    runtime: "codex",
    run: "codex",
    model: "gpt-5.3-codex",
    reasoning: "high",
    approvalMode: "on-request",
    sandboxMode: "workspace-write",
    cwd: "/synthetic/project",
  });
  assert.match(command, /--reasoning/);
  assert.match(command, /--approval-mode/);
  assert.match(command, /--sandbox-mode/);
  assert.doesNotMatch(
    agentSpawnCommand({
      hub: "http://127.0.0.1:18765",
      task: "tsk_0123456789abcdef",
      name: "builder",
      runtime: "codex",
      run: "codex",
      model: "gpt-5.3-codex",
      cwd: "/synthetic/project",
    }),
    /--reasoning/,
  );
  assert.throws(
    () =>
      agentSpawnCommand({
        hub: "http://127.0.0.1:18765",
        task: "tsk_0123456789abcdef",
        name: "builder",
        runtime: "codex",
        run: "codex",
        model: "custom/model",
        reasoning: "high",
        cwd: "/synthetic/project",
      }),
    /verified Codex/,
  );
  assert.throws(
    () =>
      agentSpawnCommand({
        hub: "http://127.0.0.1:18765",
        task: "tsk_0123456789abcdef",
        name: "builder",
        runtime: "claude",
        run: "claude",
        reasoning: "high",
        cwd: "/synthetic/project",
      }),
    /not supported/,
  );
  assert.throws(
    () =>
      agentSpawnCommand({
        hub: "http://127.0.0.1:18765",
        task: "tsk_0123456789abcdef",
        name: "builder",
        runtime: "codex",
        run: "node unknown-wrapper.js",
        model: "gpt-5.6-sol",
        reasoning: "high",
        cwd: "/synthetic/project",
      }),
    /native codex command/,
  );
});

test("encrypted retry journal is bounded, stores exact machine scope and preserves uncertain identity", async () => {
  const contextOverhead = Buffer.byteLength(
    JSON.stringify({
      version: 1,
      itemId: "wi_abcdef0123456789",
      payload: "",
    }),
  );
  const payloadBytes = MAX_WORK_CONTEXT_BYTES - contextOverhead;
  const context = JSON.stringify({
    version: 1,
    itemId: "wi_abcdef0123456789",
    payload:
      "\\".repeat(Math.floor(payloadBytes / 2)) + "x".repeat(payloadBytes % 2),
  });
  const server = {
    id: "machine-demo-01",
    host: "example.invalid",
    port: 22,
    username: "synthetic",
    mode: "ssh",
    credentialRevision: 3,
    password: "never-copy",
    hasPassword: true,
  };
  const scope = await serverLaunchScope(server);
  const stored = launchPlanForStorage({
    id: "launch_test",
    kind: "add-team",
    scope: "a".repeat(64),
    taskId: "tsk_0123456789abcdef",
    createdAt: new Date(0).toISOString(),
    updatedAt: new Date(0).toISOString(),
    members: [
      {
        server,
        serverScope: scope,
        state: "uncertain",
        fields: {
          name: "builder",
          agentId: "agt_1111111111111111",
          run: "codex",
          workContextBundle: context,
          cwd: "/synthetic/project",
        },
      },
      {
        server,
        serverScope: scope,
        state: "unstarted",
        fields: {
          name: "reviewer",
          agentId: "agt_2222222222222222",
          run: "codex",
          workContextBundle: context,
          cwd: "/synthetic/project",
        },
      },
    ],
  });
  assert.equal(stored.workContextBundle, context);
  assert.equal(Buffer.byteLength(context), MAX_WORK_CONTEXT_BYTES);
  assert.ok(Buffer.byteLength(JSON.stringify(stored)) > 1024 * 1024);
  assert.ok(
    Buffer.byteLength(JSON.stringify(stored)) < MAX_TEAM_LAUNCH_PLAN_BYTES,
  );
  assert.throws(
    () =>
      normalizeTeamLaunchPlans(
        Array.from({ length: 2 }, (_, i) => ({
          ...stored,
          id: `aggregate_${i}`,
        })),
      ),
    /2 MiB/,
  );
  assert.equal(JSON.stringify(stored).match(/wi_abcdef0123456789/g).length, 1);
  assert.doesNotMatch(JSON.stringify(stored), /never-copy/);
  const restored = await restoreLaunchMembers(stored, [server]);
  assert.equal(restored[0].state, "uncertain");
  assert.equal(restored[0].fields.workContextBundle, context);
  await assert.rejects(
    restoreLaunchMembers(stored, [
      { ...server, credentialRevision: server.credentialRevision + 1 },
    ]),
    /machine profile.*changed/,
  );
  assert.throws(
    () =>
      normalizeTeamLaunchPlans(
        Array.from({ length: 9 }, (_, index) => ({
          ...stored,
          id: `launch_${index}`,
        })),
      ),
    /At most 8/,
  );
  assert.throws(
    () =>
      launchPlanForStorage({
        ...stored,
        id: "launch_huge",
        members: [
          {
            ...stored.members[0],
            fields: {
              name: "builder",
              agentId: "agt_1111111111111111",
              run: "codex",
              cwd: "/synthetic/project",
              prompt: "x".repeat(MAX_TEAM_LAUNCH_PLAN_BYTES),
            },
          },
        ],
      }),
    /1\.25 MiB/,
  );
});

test("launch guard rejects profile or endpoint changes across delayed effects", async () => {
  let scope = "scope-a";
  let release;
  const delayed = new Promise((resolve) => (release = resolve));
  const result = guardedLaunchEffect(
    async () => {
      if (scope !== "scope-a") throw new Error("launch scope changed");
    },
    () => delayed,
  );
  await Promise.resolve();
  scope = "scope-b";
  release("late response");
  await assert.rejects(result, /launch scope changed/);
});

test("resume launch journals preserve the exact lifecycle and fresh orchestrator", async () => {
  const server = {
    id: "resume-machine",
    host: "resume.invalid",
    port: 22,
    username: "synthetic",
    mode: "ssh",
    credentialRevision: 1,
  };
  const stored = launchPlanForStorage({
    id: "resume_launch",
    kind: "resume-project",
    scope: "b".repeat(64),
    taskId: "tsk_0123456789abcdef",
    teamId: "previous-team",
    createdAt: new Date(0).toISOString(),
    updatedAt: new Date(0).toISOString(),
    resume: {
      state: "prepared",
      requestId: "resume_request_1",
      expectedPauseGeneration: 3,
      expectedLifecycleGeneration: 8,
      retainedHandoffDigest: "c".repeat(64),
      selectedTeamId: "previous-team",
      orchestratorAgentId: "agt_1111111111111111",
      orchestratorRunId: "run_1111111111111111",
      orchestratorName: "fresh-lead",
    },
    members: [
      {
        server,
        serverScope: await serverLaunchScope(server),
        state: "unstarted",
        fields: {
          name: "fresh-lead",
          agentId: "agt_1111111111111111",
          expectedRunId: "run_1111111111111111",
          run: "codex",
          cwd: "/synthetic/resume",
          expectedLifecycleGeneration: 9,
        },
      },
      {
        server,
        serverScope: await serverLaunchScope(server),
        state: "unstarted",
        fields: {
          name: "db-handler",
          agentRole: "database_handler",
          agentId: "agt_2222222222222222",
          run: "codex",
          cwd: "/synthetic/resume",
          expectedLifecycleGeneration: 9,
        },
      },
    ],
  });
  assert.equal(stored.kind, "resume-project");
  assert.equal(stored.resume.expectedLifecycleGeneration, 8);
  assert.equal(stored.resume.orchestratorAgentId, "agt_1111111111111111");
  assert.equal(stored.resume.orchestratorRunId, "run_1111111111111111");
  assert.equal(stored.members[0].fields.expectedLifecycleGeneration, 9);
  const legacyReasoning = structuredClone(stored);
  legacyReasoning.members.push({
    ...legacyReasoning.members[1],
    fields: {
      ...legacyReasoning.members[1].fields,
      name: "unstarted-claude",
      agentId: "agt_3333333333333333",
      agentRole: undefined,
      runtime: "claude",
      run: "claude",
      reasoning: "high",
    },
  });
  legacyReasoning.members[1].fields = {
    ...legacyReasoning.members[1].fields,
    runtime: "claude",
    run: "claude",
    reasoning: "high",
  };
  legacyReasoning.members[1].state = "uncertain";
  const repairedLaunchReasoning =
    migrateUnsupportedUnstartedLaunchReasoning([legacyReasoning])[0];
  assert.equal(repairedLaunchReasoning.members[2].fields.reasoning, "");
  assert.equal(repairedLaunchReasoning.members[1].fields.reasoning, "high");
  assert.equal(
    repairedLaunchReasoning.members[1].fields.agentId,
    legacyReasoning.members[1].fields.agentId,
  );
  assert.throws(
    () =>
      normalizeTeamLaunchPlans([
        {
          ...stored,
          resume: { ...stored.resume, retainedHandoffDigest: "missing" },
        },
      ]),
    /Invalid project resume retry state/,
  );
  assert.throws(
    () =>
      normalizeTeamLaunchPlans([
        {
          ...stored,
          members: stored.members.map((member, index) =>
            index
              ? member
              : {
                  ...member,
                  fields: {
                    ...member.fields,
                    expectedRunId: "run_3333333333333333",
                  },
                },
          ),
        },
      ]),
    /bind one exact fresh orchestrator/,
  );
});

test("project creation journal and agent reconciliation preserve exact identities", async () => {
  const creation = {
    state: "uncertain",
    request: {
      name: "Synthetic frozen project",
      goal: "Exercise a lost response.",
      allowAgentSpawn: true,
      maxNewAgents: 2,
      swarm: false,
      orchestrator: "builder",
    },
    knownTaskIds: ["tsk_0000000000000001"],
    preparedAt: new Date(0).toISOString(),
    attemptedAt: new Date(1).toISOString(),
  };
  const context = JSON.stringify({ version: 1, exact: "context bytes" });
  const digest = Buffer.from(
    await crypto.subtle.digest("SHA-256", new TextEncoder().encode(context)),
  ).toString("hex");
  const fields = {
    name: "builder",
    agentId: "agt_1111111111111111",
    run: "codex",
    runtime: "codex",
    cwd: "/synthetic/project",
    workItemTaskId: "tsk_0123456789abcdef",
    workItemId: "wi_abcdef0123456789",
    workItemRevision: 2,
    workOrderTaskId: "tsk_0123456789abcdef",
    workOrderMessageSeq: 1720,
    workContextBundle: context,
  };
  const member = {
    state: "started",
    fields,
    agent: {
      id: fields.agentId,
      runId: "run_2222222222222222",
      name: fields.name,
    },
  };
  const agent = {
    taskId: fields.workItemTaskId,
    id: fields.agentId,
    runId: member.agent.runId,
    name: fields.name,
    status: "running",
    role: "",
    parentAgentId: "",
    runtime: fields.runtime,
    cwd: fields.cwd,
    workItem: {
      agentId: fields.agentId,
      runId: member.agent.runId,
      itemTaskId: fields.workItemTaskId,
      itemId: fields.workItemId,
      itemRevision: fields.workItemRevision,
      workOrderMessage: {
        taskId: fields.workOrderTaskId,
        seq: fields.workOrderMessageSeq,
      },
      contextDigest: digest,
    },
  };
  const entry = { fields };
  // Current and historical Go encoders are exact, distinct retry codecs.
  const original = fields.workContextBundle;
  fields.workContextBundle =
    '{ "version":1, "exact":"<>&界\\u003c", "n":9007199254740993 }';
  for (const encoded of [
    '{"version":1,"exact":"<>&界\\u003c","n":9007199254740993}',
    '{"version":1,"exact":"\\u003c\\u003e\\u0026界\\u003c","n":9007199254740993}',
  ]) {
    const expected = Buffer.from(
      await crypto.subtle.digest("SHA-256", new TextEncoder().encode(encoded)),
    ).toString("hex");
    const frozen = structuredClone(fields);
    assert.equal(
      await reconciledAgentProblem(fields.workItemTaskId, entry, member, {
        ...agent,
        workItem: { ...agent.workItem, contextDigest: expected },
      }),
      "",
    );
    assert.deepEqual(fields, frozen);
  }
  assert.equal(
    await reconciledAgentProblem(fields.workItemTaskId, entry, member, agent),
    "work-item/order/context binding",
  );
  fields.workContextBundle = original;

  assert.equal(
    await reconciledAgentProblem(fields.workItemTaskId, entry, member, agent),
    "",
  );
  assert.equal(
    await reconciledAgentProblem(fields.workItemTaskId, entry, member, {
      ...agent,
      runId: "run_3333333333333333",
    }),
    "agent run",
  );
  assert.equal(
    await reconciledAgentProblem(fields.workItemTaskId, entry, member, {
      ...agent,
      status: "closed",
    }),
    "lifecycle",
  );
  assert.equal(
    await reconciledAgentProblem(fields.workItemTaskId, entry, member, {
      ...agent,
      workItem: { ...agent.workItem, itemRevision: 3 },
    }),
    "work-item/order/context binding",
  );
});
