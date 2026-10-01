import { test } from "node:test";
import assert from "node:assert/strict";
import {
  matchServer,
  reconcileTask,
  taskRollup,
  applyEvents,
  liveProject,
  restoreVerdict,
  runChanged,
  sessionCheckNeeded,
  MAX_TASK_PANES,
  homeAgent,
  bindingOf,
  taskBinding,
  taskMemberIds,
  homePlacement,
  attachOptions,
  helperReattach,
  reattachOptions,
} from "../client/tasks.js";
import { normalizeTaskRef } from "../client/task-ref.js";
import { tmuxCommand } from "../shared/tmux-command.js";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";

const servers = [
  {
    id: "s1",
    name: "Oracle 1",
    host: "oracle1.tail1234.ts.net",
    tailnet: true,
  },
  { id: "s2", name: "NAS", host: "100.116.238.37", tailnet: false },
  { id: "s3", name: "truenas-tailarr", host: "truenas-tailarr", tailnet: true },
  { id: "s4", name: "Oracle 1 plain", host: "oracle1", tailnet: false },
];
const agent = (n, extra = {}) => ({
  id: `agt_${n.toString(16).padStart(16, "0")}`,
  name: `a${n}`,
  host: "oracle1",
  session: `a${n}`,
  status: "running",
  ...extra,
});
const TASK = "tsk_0123456789abcdef";

test("matchServer prefers tailnet hosts, then exact hosts, then names", () => {
  assert.equal(matchServer("oracle1", servers).id, "s1");
  assert.equal(matchServer("ORACLE1.tail1234.ts.net", servers).id, "s1");
  assert.equal(matchServer("truenas-tailarr", servers).id, "s3");
  assert.equal(matchServer("nas", servers).id, "s2");
  assert.equal(matchServer("nowhere", servers), null);
  assert.equal(matchServer("", servers), null);
});

test("reconcileTask opens, adopts, closes, and reports unknown hosts", () => {
  const bound = agent(1);
  const tabs = [
    {
      id: "t1",
      server: servers[0],
      tmux: true,
      session: "a1",
      task: normalizeTaskRef({ taskId: TASK, agentId: bound.id }),
    },
    { id: "t2", server: servers[0], tmux: true, session: "a2" },
    {
      id: "t3",
      server: servers[0],
      tmux: true,
      session: "gone",
      task: normalizeTaskRef({ taskId: TASK, agentId: agent(9).id }),
    },
    { id: "t4", server: servers[0], tmux: true, session: "x", disposed: true },
  ];
  const agents = [
    bound,
    agent(2),
    agent(3),
    agent(4, { host: "mystery" }),
    agent(5, { status: "closed" }),
  ];
  const r = reconcileTask({ taskId: TASK, agents, tabs, servers });
  assert.deepEqual(
    r.open.map((o) => [o.agent.name, o.server.id]),
    [["a3", "s1"]],
  );
  assert.deepEqual(
    r.adopt.map((a) => [a.tab.id, a.agent.name]),
    [["t2", "a2"]],
  );
  assert.deepEqual(
    r.unknown.map((a) => a.name),
    ["a4"],
  );
  assert.deepEqual(
    r.close.map((t) => t.id),
    ["t3"],
  );
});

test("reconcileTask caps the panes it opens", () => {
  const agents = Array.from({ length: 50 }, (_, i) => agent(i + 1));
  const r = reconcileTask({ taskId: TASK, agents, tabs: [], servers });
  assert.equal(r.open.length, MAX_TASK_PANES);
});

test("taskRollup summarizes open agents", () => {
  assert.equal(taskRollup([]), "0 agents");
  assert.equal(
    taskRollup(
      [
        agent(1),
        agent(2, { status: "needs_input" }),
        agent(3, { status: "done" }),
        agent(4, { status: "closed" }),
      ],
      1,
    ),
    "3 agents · 1 needs input · 1 running · 1 done · 1 on unknown host",
  );
});

test("applyEvents updates statuses and raises attention labels", () => {
  const agents = [agent(1), agent(2)];
  const { attention, refresh } = applyEvents(agents, [
    { kind: "done", agentId: agents[0].id },
    { kind: "needs_input", agentId: agents[1].id },
    { kind: "message", agentId: agents[0].id, data: { to: "" } },
    { kind: "message", agentId: agents[1].id, data: { to: agents[0].id } },
    { kind: "agent_added", agentId: "agt_ffffffffffffffff" },
  ]);
  assert.equal(agents[0].status, "done");
  assert.equal(agents[1].status, "needs_input");
  assert.equal(attention.get(agents[0].id), "Task message");
  assert.equal(attention.get(agents[1].id), "Needs attention");
  assert.equal(refresh, true);
  applyEvents(agents, [{ kind: "task_closed" }]);
  assert.ok(agents.every((a) => a.status === "closed"));
});

test("cached terminal task state preserves a reported blocker across a runtime stop", () => {
  const agents = [agent(1)];
  applyEvents(agents, [
    {
      kind: "needs_input",
      agentId: agents[0].id,
      text: "Denied test command",
      data: { reason: "permission" },
    },
  ]);
  const result = applyEvents(agents, [
    { kind: "done", agentId: agents[0].id, data: { runtimeStop: true } },
  ]);
  assert.equal(agents[0].status, "needs_input");
  assert.equal(agents[0].blockedReason, "permission");
  assert.equal(result.attention.size, 0);
  applyEvents(agents, [{ kind: "running", agentId: agents[0].id }]);
  assert.equal(agents[0].status, "running");
  assert.equal(agents[0].blockedReason, "");
});

test("retired agents retain panes and ignore late lifecycle hooks until explicit resume", () => {
  const agents = [
    { id: "worker", status: "retired", host: "oracle1", session: "worker" },
  ];
  const before = reconcileTask({ taskId: "task", agents, tabs: [], servers });
  assert.equal(before.open.length, 1);
  assert.equal(before.close.length, 0);
  applyEvents(
    agents,
    ["running", "done", "needs_input"].map((kind) => ({
      kind,
      agentId: "worker",
    })),
  );
  assert.equal(agents[0].status, "retired");
  applyEvents(agents, [{ kind: "resumed", agentId: "worker" }]);
  assert.equal(agents[0].status, "done");
  applyEvents(agents, [{ kind: "retired", agentId: "worker" }]);
  assert.equal(agents[0].status, "retired");
});

test("liveProject keeps active and resuming projects, drops paused and closed", () => {
  assert.equal(liveProject({ status: "open" }), true, "older hubs");
  assert.equal(liveProject({ status: "open", pauseState: "" }), true);
  assert.equal(liveProject({ status: "open", pauseState: "active" }), true);
  assert.equal(liveProject({ status: "open", pauseState: "resuming" }), true);
  assert.equal(liveProject({ status: "open", pauseState: "paused" }), false);
  assert.equal(
    liveProject({ status: "open", pauseState: "cleanup_pending" }),
    false,
  );
  assert.equal(liveProject({ status: "closed", pauseState: "active" }), false);
  assert.equal(liveProject(undefined), false);
});

test("restoreVerdict decides saved task panes from the hub state", () => {
  const RUN = "run_00000000000000a1",
    NEXT = "run_00000000000000a2";
  const live = agent(1, { runId: RUN });
  const binding = normalizeTaskRef({
    taskId: TASK,
    agentId: live.id,
    runId: RUN,
  });
  const detail = (task, agents = [live]) => ({
    task: { id: TASK, status: "open", pauseState: "active", ...task },
    agents,
  });
  assert.equal(restoreVerdict(binding, undefined), "unknown", "hub down");
  assert.equal(restoreVerdict(binding, null), "gone");
  assert.equal(restoreVerdict(binding, detail({ status: "closed" })), "closed");
  assert.equal(
    restoreVerdict(binding, detail({ pauseState: "paused" })),
    "paused",
  );
  assert.equal(
    restoreVerdict(binding, detail({ pauseState: "cleanup_pending" })),
    "paused",
  );
  assert.equal(restoreVerdict(binding, detail({}, [])), "closed", "missing");
  assert.equal(
    restoreVerdict(binding, detail({}, [{ ...live, status: "closed" }])),
    "closed",
  );
  assert.equal(
    restoreVerdict(binding, detail({}, [{ ...live, runId: NEXT }])),
    "run-changed",
  );
  assert.equal(restoreVerdict(binding, detail({})), "keep");
  assert.equal(
    restoreVerdict(binding, detail({ pauseState: "resuming" })),
    "keep",
  );
  assert.equal(
    restoreVerdict(binding, detail({}, [{ ...live, status: "exited" }])),
    "keep",
    "finished agents are kept; their session is checked separately",
  );
  // An agent without a run keeps any binding, as in reconcileTask.
  assert.equal(
    restoreVerdict(binding, detail({}, [{ ...live, runId: undefined }])),
    "keep",
  );
});

test("restoreVerdict run-changed matches the pane reconcileTask closes", () => {
  const RUN = "run_00000000000000b1";
  const cases = [
    [RUN, RUN],
    [RUN, "run_00000000000000b2"],
    [undefined, RUN],
    [RUN, undefined],
    [undefined, undefined],
  ];
  for (const [bound, current] of cases) {
    const a = agent(7, { runId: current });
    const task = normalizeTaskRef({
      taskId: TASK,
      agentId: a.id,
      runId: bound,
    });
    const tabs = [
      { id: "t", server: servers[0], tmux: true, session: "a7", task },
    ];
    const closes =
      reconcileTask({ taskId: TASK, agents: [a], tabs, servers }).close
        .length === 1;
    const verdict = restoreVerdict(task, {
      task: { status: "open", pauseState: "active" },
      agents: [a],
    });
    assert.equal(runChanged(task, a), closes, `${bound} -> ${current}`);
    assert.equal(verdict === "run-changed", closes, `${bound} -> ${current}`);
  }
});

test("sessionCheckNeeded covers finished agents only", () => {
  assert.equal(sessionCheckNeeded({ status: "done" }), true);
  assert.equal(sessionCheckNeeded({ status: "exited" }), true);
  for (const status of ["running", "starting", "needs_input", "retired"])
    assert.equal(sessionCheckNeeded({ status }), false, status);
});

// A synthetic owner helper: never the live owner session.
const helperFixture = agent(90, {
  name: "owner-helper-fx",
  session: "helper-fx",
  role: "owner_helper",
});

test("taskBinding keeps the helper role only, and homeAgent reads it", () => {
  const plain = agent(3, { runId: "run_0000000000000003" });
  assert.deepEqual(taskBinding(TASK, plain), {
    taskId: TASK,
    runId: "run_0000000000000003",
    agentId: plain.id,
    agentName: "a3",
  });
  assert.deepEqual(taskBinding(TASK, agent(4, { role: "database_handler" })), {
    taskId: TASK,
    runId: undefined,
    agentId: agent(4).id,
    agentName: "a4",
  });
  const helper = taskBinding(TASK, helperFixture);
  assert.equal(helper.role, "owner_helper");
  assert.equal(helper.agentId, helperFixture.id);
  assert.deepEqual(
    bindingOf(helper),
    helper,
    "the role survives renormalizing",
  );
  assert.equal(bindingOf(null), null);
  assert.equal(homeAgent(helperFixture), true);
  assert.equal(homeAgent(plain), false);
  assert.equal(homeAgent(undefined), false);
});

test("homePlacement: helper and new owner terminals in home, agents never", () => {
  const helper = taskBinding(TASK, helperFixture);
  const plain = taskBinding(TASK, agent(5));
  const rows = [
    // [case, input, expected]
    ["helper pane", { binding: helper }, true],
    [
      "helper pane, restored out of home",
      { binding: helper, saved: false },
      true,
    ],
    ["helper by hub role", { binding: plain, role: "owner_helper" }, true],
    ["other agent", { binding: plain }, false],
    [
      "other agent saved in home, roster known",
      { binding: plain, role: "", saved: true },
      false,
    ],
    [
      "adopted home shell becomes an agent",
      { binding: plain, role: "", explicit: true },
      false,
    ],
    [
      "bound pane, roster unknown, saved in home",
      { binding: plain, saved: true },
      true,
    ],
    ["new owner terminal, home empty", {}, true],
    ["new owner terminal", undefined, true],
    ["restored owner terminal in home", { saved: true }, true],
    ["restored owner terminal as a tab", { saved: false }, false],
    [
      "owner moves a shell out of home",
      { saved: true, explicit: false },
      false,
    ],
    ["owner moves a shell into home", { saved: false, explicit: true }, true],
    [
      "agent cannot be moved into home",
      { binding: plain, explicit: true },
      false,
    ],
    ["helper cannot be moved out", { binding: helper, explicit: false }, true],
  ];
  for (const [name, input, expected] of rows)
    assert.equal(homePlacement(input), expected, name);
});

test("reconcileTask opens and adopts the helper like any agent", () => {
  const tabs = [
    { id: "launcher", server: servers[0], tmux: true, session: "helper-fx" },
  ];
  const r = reconcileTask({
    taskId: TASK,
    agents: [helperFixture, agent(6)],
    tabs,
    servers,
  });
  assert.deepEqual(
    r.adopt.map((x) => [x.tab.id, x.agent.id]),
    [["launcher", helperFixture.id]],
  );
  assert.deepEqual(
    r.open.map((x) => x.agent.id),
    [agent(6).id],
  );
});

test("taskMemberIds excludes the helper in every state", () => {
  const eight = Array.from({ length: 8 }, (_, i) => agent(i + 1));
  const ids = eight.map((a) => a.id);
  for (const extra of [
    { host: "nowhere" }, // no pane: unknown host
    {}, // hidden by the owner: still running on the hub
    { status: "starting", runId: "run_0000000000000090" }, // opening
    { status: "exited" },
    { status: "retired" },
    { online: false },
  ]) {
    const helper = { ...helperFixture, ...extra };
    assert.deepEqual(
      taskMemberIds({ status: "open" }, [helper, ...eight]),
      ids,
      JSON.stringify(extra),
    );
    assert.deepEqual(
      taskMemberIds({ status: "open" }, [...eight, helper]),
      ids,
    );
  }
  assert.deepEqual(taskMemberIds({ status: "closed" }, eight), []);
  assert.deepEqual(
    taskMemberIds({ status: "open" }, [
      ...eight,
      agent(9, { status: "closed" }),
    ]),
    ids,
    "closed agents stay out as before",
  );
});

test("helperReattach and reattachOptions force an ignore-size attach", () => {
  const task = taskBinding(TASK, helperFixture);
  const target = { id: "$4", created: "1700000000" };
  const tab = (extra) => ({
    id: "t",
    tmux: true,
    session: "helper-fx",
    target,
    task,
    ...extra,
  });
  for (const status of ["Connecting", "Connected"])
    assert.equal(
      helperReattach(tab({ status, attachIgnoresSize: false })),
      true,
      status,
    );
  assert.equal(helperReattach(tab({ attachIgnoresSize: true })), false);
  assert.equal(helperReattach(tab({ attachIgnoresSize: undefined })), false);
  assert.equal(
    helperReattach(tab({ attachIgnoresSize: false, tmux: false })),
    false,
  );
  assert.equal(
    helperReattach(
      tab({ attachIgnoresSize: false, task: taskBinding(TASK, agent(7)) }),
    ),
    false,
    "an ordinary agent is not reattached",
  );
  assert.equal(helperReattach(null), false);
  const original = tab({ status: "Connecting", attachIgnoresSize: false });
  const options = reattachOptions(original);
  assert.equal(options.replace, original);
  assert.equal(options.resumeOnly, true);
  assert.equal(options.task, task);
  assert.equal(options.target, target);
  assert.equal(options.home, true);
  assert.deepEqual(attachOptions(task), { ignoreSize: true });
  assert.deepEqual(attachOptions(undefined), { ignoreSize: false });
  const command = tmuxCommand(
    original.session,
    "",
    options.resumeOnly,
    undefined,
    "",
    attachOptions(options.task),
  );
  assert.match(command, /tailterm_tmux_attach_flags='\\''-f ignore-size'/);
  assert.match(command, /attach-session \$tailterm_tmux_attach_flags -t/);
  assert.doesNotMatch(command, /new-session/);
  // The attach mode and the size flag are separate facts.
  const plainCommand = tmuxCommand(
    "helper-fx",
    "",
    true,
    undefined,
    "",
    attachOptions(undefined),
  );
  assert.match(plainCommand, /attach-session -t/);
  assert.doesNotMatch(plainCommand, /ignore-size/);
});

// Real tmux on private sockets: the helper's attach never resizes the owner's
// terminal. Both servers live under a private TMUX_TMPDIR.
test("the helper attach does not change the session's window size", (t) => {
  let version = "";
  try {
    version = execFileSync("tmux", ["-V"], { encoding: "utf8" }).trim();
  } catch {
    t.skip("tmux is not installed");
    return;
  }
  const [major, minor] = (version.match(/(\d+)\.(\d+)/) || [])
    .slice(1)
    .map(Number);
  if (!(major > 3 || (major === 3 && minor >= 2))) {
    t.skip(`tmux 3.2+ is required for ignore-size (found ${version})`);
    return;
  }
  // Unix socket paths are limited (104 bytes on macOS), so the private
  // sockets live in a short 0700 directory, not under a possibly long TMPDIR.
  const dir = mkdtempSync("/tmp/tt-");
  const env = { ...process.env, TMUX_TMPDIR: dir };
  delete env.TMUX;
  delete env.TMUX_PANE;
  const target = (...args) =>
    execFileSync("tmux", ["-f", "/dev/null", ...args], {
      env,
      encoding: "utf8",
    }).trim();
  const viewer = (...args) =>
    execFileSync("tmux", ["-L", "viewer", "-f", "/dev/null", ...args], {
      env,
      encoding: "utf8",
    }).trim();
  const size = () =>
    target(
      "display-message",
      "-p",
      "-t",
      "helper-fx:",
      "#{window_width}x#{window_height}",
    );
  const waitClient = (want) => {
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      if (
        target(
          "list-clients",
          "-F",
          "#{client_width}x#{client_height} #{session_name}",
        ).includes(want)
      )
        return execFileSync("sleep", ["0.2"]);
      execFileSync("sleep", ["0.05"]);
    }
    assert.fail(`no ${want} client attached`);
  };
  t.after(() => {
    for (const run of [target, viewer])
      try {
        run("kill-server");
      } catch {}
    rmSync(dir, { recursive: true, force: true });
  });
  target(
    "new-session",
    "-d",
    "-s",
    helperFixture.session,
    "-x",
    "200",
    "-y",
    "50",
    "sleep 120",
  );
  target("set-option", "-w", "-t", "helper-fx:", "window-size", "latest");
  // The owner's own terminal: a normal 120x40 client.
  viewer(
    "new-session",
    "-d",
    "-s",
    "owner",
    "-x",
    "120",
    "-y",
    "40",
    "env -u TMUX tmux attach-session -t '=helper-fx'",
  );
  waitClient("120x40 helper-fx");
  assert.equal(size(), "120x39", "the owner's client sizes the window");
  const helperAttach = tmuxCommand(
    helperFixture.session,
    "",
    true,
    undefined,
    "",
    attachOptions(taskBinding(TASK, helperFixture)),
  );
  assert.match(helperAttach, /ignore-size/);
  viewer(
    "new-session",
    "-d",
    "-s",
    "tile",
    "-x",
    "16",
    "-y",
    "2",
    "env -u TMUX " + helperAttach,
  );
  waitClient("16x2 helper-fx");
  assert.equal(size(), "120x39", "the home pane attach left the window alone");
  // Control: the same tile without the flag does resize the window.
  viewer("kill-session", "-t", "tile");
  const plainAttach = tmuxCommand(
    helperFixture.session,
    "",
    true,
    undefined,
    "",
    attachOptions(undefined),
  );
  viewer(
    "new-session",
    "-d",
    "-s",
    "plain",
    "-x",
    "16",
    "-y",
    "2",
    "env -u TMUX " + plainAttach,
  );
  waitClient("16x2 helper-fx");
  assert.notEqual(size(), "120x39", "the control attach must change the size");
});
