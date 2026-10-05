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
  helperLabel,
  attachOptions,
  helperReattach,
  helperAttachKey,
  reattachOptions,
} from "../client/tasks.js";
import { normalizeTaskRef } from "../client/task-ref.js";
import { tmuxCommand, shellQuote } from "../shared/tmux-command.js";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";

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
  runId: "run_0000000000000090",
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

test("homePlacement: only the owner helper is in home", () => {
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
      "bound pane, roster unknown, saved in home",
      { binding: plain, saved: true },
      true,
    ],
    [
      "bound pane, roster unknown, saved as a tab",
      { binding: plain, saved: false },
      false,
    ],
    ["new owner terminal", {}, false],
    ["new owner terminal, no input", undefined, false],
    ["owner terminal an older workspace saved in home", { saved: true }, false],
    ["restored owner terminal as a tab", { saved: false }, false],
    [
      "an unbound terminal cannot claim the role",
      { role: "owner_helper", saved: true },
      false,
    ],
  ];
  for (const [name, input, expected] of rows)
    assert.equal(homePlacement(input), expected, name);
});

test("helperLabel names the helper window from the hub role", () => {
  const helper = taskBinding(TASK, helperFixture);
  assert.equal(
    helperLabel(helper, "Fixture project"),
    "Owner helper - Fixture project",
  );
  assert.equal(helperLabel(helper), "Owner helper", "project name unknown");
  assert.equal(helperLabel(helper, ""), "Owner helper");
  assert.equal(helperLabel(taskBinding(TASK, agent(5)), "Fixture project"), "");
  assert.equal(helperLabel(undefined, "Fixture project"), "");
  assert.ok(
    !helperLabel(
      { ...helper, agentName: "Generic" },
      "Fixture project",
    ).includes("Generic"),
    "never the session or agent name",
  );
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
  assert.equal(
    helperReattach(
      tab({ attachIgnoresSize: true, attachHelperKey: helperAttachKey(task) }),
    ),
    false,
  );
  assert.equal(helperReattach(tab({ attachIgnoresSize: undefined })), true);
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
  assert.equal(
    helperReattach(tab({ attachIgnoresSize: true })),
    true,
    "legacy ignore-size helper adoption still needs the latest policy",
  );
  assert.equal(
    helperReattach(
      tab({
        attachIgnoresSize: true,
        attachHelperKey: helperAttachKey({
          ...task,
          runId: "run_0000000000000091",
        }),
      }),
    ),
    true,
    "a different run's emitted command cannot suppress the current policy",
  );
  const original = tab({ status: "Connecting", attachIgnoresSize: false });
  const options = reattachOptions(original);
  assert.equal(options.replace, original);
  assert.equal(options.resumeOnly, true);
  assert.equal(options.task, task);
  assert.equal(options.target, target);
  assert.equal(options.home, true);
  assert.deepEqual(attachOptions(task), {
    ignoreSize: true,
    helperBinding: {
      taskId: task.taskId,
      agentId: task.agentId,
      runId: task.runId,
      role: "owner_helper",
    },
  });
  assert.deepEqual(attachOptions({ ...task, runId: undefined }), {
    ignoreSize: true,
  });
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

// Real PTYs and generated attach commands on TWO private sockets. No inherited
// session/configuration/credential state participates in the sizing policy.
test("helper latest policy follows lone TailOS and defers to the owner across transitions", (t) => {
  const version = execFileSync("tmux", ["-V"], { encoding: "utf8" }).trim();
  assert.match(
    version,
    /^tmux (?:3\.[2-9]|[4-9]\.)/,
    "tmux 3.2+ required; this check must execute",
  );
  const dir = mkdtempSync("/tmp/tt-help-");
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([key]) =>
        !key.startsWith("TAILTERM_") && !["TMUX", "TMUX_PANE"].includes(key),
    ),
  );
  const binary = dir + "/tmux";
  const tmuxPath = execFileSync("/bin/sh", ["-c", "command -v tmux"], {
    env,
    encoding: "utf8",
  }).trim();
  writeFileSync(
    binary,
    `#!/bin/sh\nexec '${tmuxPath}' -S '${dir}/target' -f /dev/null "$@"\n`,
    { mode: 0o700 },
  );
  const run = (socket, ...args) =>
    execFileSync(
      tmuxPath,
      ["-S", dir + "/" + socket, "-f", "/dev/null", ...args],
      { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] },
    ).trim();
  const target = (...args) => run("target", ...args);
  const viewer = (...args) => run("viewer", ...args);
  t.after(() => {
    for (const socket of ["target", "viewer"])
      try {
        run(socket, "kill-server");
      } catch {}
    rmSync(dir, { recursive: true, force: true });
  });
  const binding = attachOptions(taskBinding(TASK, helperFixture));
  const tag = binding.helperBinding;
  target(
    "new-session",
    "-d",
    "-s",
    "helper-fx",
    "-n",
    "control",
    "-x",
    "200",
    "-y",
    "50",
    ...Object.entries({
      TAILTERM_TASK: tag.taskId,
      TAILTERM_AGENT: tag.agentId,
      TAILTERM_RUN: tag.runId,
      TAILTERM_ROLE: tag.role,
    }).flatMap(([k, v]) => ["-e", `${k}=${v}`]),
    "sleep 120",
  );
  const [id, created, windowId] = target(
    "display-message",
    "-p",
    "-t",
    "helper-fx:",
    "#{session_id}|#{session_created}|#{window_id}",
  ).split("|");
  const identity = { id, created };
  const window = id + ":" + windowId;
  const dimensions = () =>
    target(
      "display-message",
      "-p",
      "-t",
      window,
      "#{window_width}x#{window_height}",
    );
  const wait = (predicate, label) => {
    const deadline = Date.now() + 5000;
    while (Date.now() < deadline) {
      if (predicate()) return;
      execFileSync("sleep", ["0.05"]);
    }
    assert.fail(
      `${label}: window ${dimensions()}, clients ${target("list-clients", "-F", "#{client_width}x#{client_height}|#{client_flags}")}`,
    );
  };
  const expectClient = (cols, rows) =>
    wait(
      () =>
        target("list-clients", "-F", "#{client_width}x#{client_height}")
          .split("\n")
          .includes(`${cols}x${rows}`),
      "PTY resize delivered",
    );
  const record = (label) =>
    t.diagnostic(
      `${label}: ${target("display-message", "-p", "-t", window, "#{session_id}|#{session_created}|#{window_id}|#{window-size}|#{window_width}x#{window_height}|pane=#{pane_width}x#{pane_height}|status=#{status}")}; clients ${target("list-clients", "-F", "#{client_width}x#{client_height}|#{client_flags}")}`,
    );
  const expect = (size, label) => {
    wait(() => dimensions() === size, label);
    record(label);
  };
  const manual = () => {
    target("set-option", "-w", "-t", window, "window-size", "manual");
    target("resize-window", "-t", window, "-x", "200", "-y", "50");
  };
  const attach = (name, cols, rows, options = binding, exact = identity) => {
    const command = tmuxCommand("helper-fx", binary, true, exact, "", options);
    viewer(
      "new-session",
      "-d",
      "-s",
      name,
      "-x",
      String(cols),
      "-y",
      String(rows),
      "env -u TMUX " + command,
    );
    wait(
      () =>
        target(
          "list-clients",
          "-F",
          "#{client_width}x#{client_height}",
        ).includes(`${cols}x${rows}`),
      `${name} attached`,
    );
  };
  // Sentinels include a non-target helper window, unrelated session and global policy.
  target("new-window", "-d", "-t", id, "-n", "sentinel", "sleep 120");
  target(
    "new-session",
    "-d",
    "-s",
    "unrelated",
    "-x",
    "180",
    "-y",
    "45",
    "sleep 120",
  );
  target("set-option", "-w", "-t", id + ":sentinel", "window-size", "manual");
  target("set-option", "-w", "-t", "unrelated:", "window-size", "manual");
  const sentinels = () => [
    target(
      "list-windows",
      "-t",
      id,
      "-F",
      "#{window_id}|#{window-size}|#{window_width}x#{window_height}",
    )
      .split("\n")
      .filter((line) => !line.startsWith(windowId + "|")),
    target(
      "display-message",
      "-p",
      "-t",
      "unrelated:",
      "#{window-size}|#{window_width}x#{window_height}",
    ),
    target("show-option", "-gw", "window-size"),
  ];
  const before = sentinels();
  manual();
  attach("baseline", 137, 24, { ignoreSize: true });
  expect("200x50", "baseline: manual size clips lone ignore-size client");
  viewer("kill-session", "-t", "baseline");
  attach("tile", 137, 24);
  expect("137x23", "M1 first helper attach");
  assert.equal(
    target("show-option", "-wv", "-t", window, "window-size"),
    "latest",
  );
  viewer("resize-window", "-t", "tile:", "-x", "145", "-y", "30");
  expectClient(145, 30);
  expect("145x29", "M1 visible PTY resize");
  viewer(
    "new-session",
    "-d",
    "-s",
    "owner",
    "-x",
    "120",
    "-y",
    "40",
    "env -u TMUX " + binary + " attach-session -t " + shellQuote(id),
  );
  expect("120x39", "M5 TailOS first then owner");
  viewer("resize-window", "-t", "tile:", "-x", "137", "-y", "24");
  expectClient(137, 24);
  expect("120x39", "M4 TailOS resize cannot impose owner dimensions");
  viewer("kill-session", "-t", "tile");
  manual();
  attach("tile", 137, 24);
  expect("120x39", "M4 owner first plus historical manual size on reconnect");
  viewer("kill-session", "-t", "owner");
  expect("137x23", "M5 owner detach without a browser command");
  viewer(
    "new-session",
    "-d",
    "-s",
    "owner",
    "-x",
    "120",
    "-y",
    "40",
    "env -u TMUX " + binary + " attach-session -t " + shellQuote(id),
  );
  expect("120x39", "M5 owner reattach takes precedence again");
  viewer("kill-session", "-t", "tile");
  attach("tile", 137, 24);
  expect("120x39", "M5 TailOS reconnect while owner remains");
  viewer("kill-session", "-t", "owner");
  expect("137x23", "M5 owner detach after reconnect");
  viewer("kill-session", "-t", "tile");
  manual();
  attach("tile", 110, 21);
  expect("110x20", "M3 smaller reconnect clears historical manual size");
  target("set-option", "-t", id, "status", "off");
  expect("110x21", "M6 no status row");
  target("set-option", "-t", id, "status", "2");
  expect("110x19", "M6 two status rows");
  target("set-option", "-t", id, "status", "on");
  expect("110x20", "M6 one status row");
  // Adoption: ordinary launcher first, followed by generated helper reattach.
  viewer("kill-session", "-t", "tile");
  manual();
  attach("launcher", 137, 24, { ignoreSize: false });
  expect("200x50", "M2 plain launcher preserves historical manual size");
  viewer("kill-session", "-t", "launcher");
  attach("adopted", 137, 24, binding, undefined);
  expect("137x23", "M2 adopted exact-name helper policy");
  viewer("kill-session", "-t", "adopted");
  target("rename-session", "-t", id, "renamed-helper");
  attach("renamed", 130, 28);
  expect("130x27", "renamed exact session attach");
  viewer("kill-session", "-t", "renamed");
  // Negative commands must exit before attach AND before the latest mutation.
  const refuse = (options, exact, label) => {
    manual();
    const command = tmuxCommand("helper-fx", binary, true, exact, "", options);
    const result = spawnSync("/bin/sh", ["-c", command], {
      env,
      encoding: "utf8",
      timeout: 5000,
    });
    assert.equal(result.status, 1, `${label}: ${result.stderr}`);
    assert.equal(dimensions(), "200x50", label);
    assert.equal(
      target("show-option", "-wv", "-t", window, "window-size"),
      "manual",
      label,
    );
    assert.deepEqual(sentinels(), before, label);
  };
  for (const key of ["taskId", "agentId", "runId"]) {
    refuse(
      {
        ...binding,
        helperBinding: { ...tag, [key]: tag[key].slice(0, -1) + "1" },
      },
      identity,
      `wrong ${key}`,
    );
  }
  target("set-environment", "-t", id, "TAILTERM_ROLE", "ordinary");
  refuse(binding, identity, "remote role mismatch");
  target("set-environment", "-t", id, "TAILTERM_ROLE", "owner_helper");
  refuse(
    binding,
    { ...identity, created: String(Number(created) - 1) },
    "stale creation time",
  );
  refuse(binding, { ...identity, id: "$99999" }, "absent session");
  target("link-window", "-d", "-s", window, "-t", "unrelated:");
  // Linked-window guard: record the linked manual state, not the new unrelated link.
  manual();
  const linkedBefore = target(
    "list-windows",
    "-t",
    "unrelated",
    "-F",
    "#{window_id}|#{window-size}|#{window_width}x#{window_height}",
  );
  const result = spawnSync(
    "/bin/sh",
    ["-c", tmuxCommand("helper-fx", binary, true, identity, "", binding)],
    { env, encoding: "utf8", timeout: 5000 },
  );
  assert.equal(result.status, 1, result.stderr);
  assert.match(result.stderr, /linked/);
  assert.equal(dimensions(), "200x50");
  assert.equal(
    target(
      "list-windows",
      "-t",
      "unrelated",
      "-F",
      "#{window_id}|#{window-size}|#{window_width}x#{window_height}",
    ),
    linkedBefore,
  );
  t.diagnostic(
    `${version}; isolated sockets ${dir}/target and ${dir}/viewer; all identity and linked-window negatives refused`,
  );
});
