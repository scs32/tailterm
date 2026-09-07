// In-memory stand-in for tailterm-hub, mounted on the static test server at
// /fixture-hub/v1/*. It mirrors the real API closely enough for the client:
// tasks, agents, messages, lifecycle events, and long-polled feeds.
import { randomBytes } from "node:crypto";

const id = (prefix) => `${prefix}_${randomBytes(8).toString("hex")}`;
const now = () => new Date().toISOString();

export function createFixtureHub({
  node = "fixture-browser",
  user = "test@example.com",
} = {}) {
  const tasks = new Map(),
    agents = new Map(),
    messages = [],
    events = [];
  const waiters = new Set();
  const notify = () => {
    for (const w of waiters) w();
    waiters.clear();
  };
  const emit = (taskId, kind, extra = {}) => {
    const e = {
      seq: events.length + 1,
      taskId,
      kind,
      by: { node, user },
      createdAt: now(),
      ...extra,
    };
    events.push(e);
    notify();
    return e;
  };
  const withUnread = (a) => ({
    ...a,
    unread: messages.filter(
      (m) =>
        m.taskId === a.taskId &&
        m.seq > (a.readUpTo || 0) &&
        m.from.agentId !== a.id &&
        (!m.to || m.to === a.id),
    ).length,
  });
  const api = {
    createTask(name, goal = "") {
      const t = {
        id: id("tsk"),
        name,
        goal,
        status: "open",
        createdAt: now(),
        createdBy: { node, user },
        closedAt: null,
      };
      tasks.set(t.id, t);
      emit(t.id, "task_created", { text: name });
      return t;
    },
    addAgent(
      taskId,
      {
        name,
        host = "production",
        session = name,
        runtime = "claude",
        cwd = "",
        parentAgentId = "",
      },
    ) {
      const a = {
        id: id("agt"),
        taskId,
        name,
        host,
        session,
        runtime,
        cwd,
        parentAgentId,
        status: "starting",
        title: "",
        createdAt: now(),
        lastEventAt: now(),
        readUpTo: 0,
      };
      agents.set(a.id, a);
      emit(taskId, "agent_added", {
        agentId: a.id,
        text: name,
        data: { host, session, runtime },
      });
      return a;
    },
    event(taskId, kind, agentId, text = "") {
      const a = agents.get(agentId);
      const status = {
        started: "running",
        running: "running",
        done: "done",
        needs_input: "needs_input",
        closed: "closed",
      }[kind];
      if (a && status) a.status = status;
      return emit(taskId, kind, { agentId, text });
    },
    message(taskId, text, { agentId = "", to = "" } = {}) {
      const m = {
        seq: messages.length + 1,
        taskId,
        from: { agentId: agentId || undefined, node, user },
        to: to || undefined,
        text,
        createdAt: now(),
      };
      messages.push(m);
      emit(taskId, "message", {
        agentId,
        text: text.slice(0, 200),
        data: { seq: m.seq, to },
      });
      return m;
    },
    closeTask(taskId) {
      const t = tasks.get(taskId);
      for (const a of agents.values())
        if (a.taskId === taskId && a.status !== "closed")
          api.event(taskId, "closed", a.id);
      t.status = "closed";
      t.closedAt = now();
      emit(taskId, "task_closed", { text: t.name });
      return t;
    },
    agents: () => [...agents.values()],
    messages: () => messages,
    events: () => events,
    tasks: () => [...tasks.values()],
    requests: [],
  };

  async function handle(req, res, url) {
    const path = url.pathname.replace(/^\/fixture-hub/, "");
    api.requests.push(req.method + " " + path + url.search);
    const json = (status, body) => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body));
    };
    const body = await new Promise((resolve) => {
      let data = "";
      req.on("data", (c) => (data += c));
      req.on("end", () => {
        try {
          resolve(data ? JSON.parse(data) : {});
        } catch {
          resolve(null);
        }
      });
    });
    if (body === null) return json(400, { error: "malformed JSON" });
    const parts = path.split("/").filter(Boolean); // ["v1", ...]
    const [, resource, taskId, sub, agentId] = parts;
    if (resource === "whoami") return json(200, { node, user });
    if (resource === "events") return feed(res, url, null);
    if (resource !== "tasks") return json(404, { error: "not found" });
    if (!taskId) {
      if (req.method === "POST") {
        if (!/^[a-zA-Z0-9_-]{1,64}$/.test(body.name || ""))
          return json(400, { error: "invalid request" });
        return json(201, api.createTask(body.name, body.goal || ""));
      }
      return json(200, { tasks: api.tasks() });
    }
    const task = tasks.get(taskId);
    if (!task) return json(404, { error: "not found" });
    const taskAgents = () =>
      api
        .agents()
        .filter((a) => a.taskId === taskId)
        .map(withUnread);
    if (!sub) {
      if (req.method === "DELETE") return json(200, api.closeTask(taskId));
      const latestSeq =
        events.filter((e) => e.taskId === taskId).at(-1)?.seq || 0;
      return json(200, { task, agents: taskAgents(), latestSeq });
    }
    if (sub === "agents") {
      if (agentId) {
        const a = agents.get(agentId);
        if (!a || a.taskId !== taskId) return json(404, { error: "not found" });
        if (req.method === "DELETE") {
          api.event(taskId, "closed", a.id);
          return json(200, withUnread(a));
        }
        return json(200, withUnread(a));
      }
      if (req.method === "POST")
        return json(201, withUnread(api.addAgent(taskId, body)));
      return json(200, { agents: taskAgents() });
    }
    if (sub === "messages") {
      if (agentId === "read") {
        const a = agents.get(body.agentId);
        if (a) a.readUpTo = Math.max(a.readUpTo, body.upTo || 0);
        return json(200, { ok: true });
      }
      if (req.method === "POST") {
        if (!body.text) return json(400, { error: "invalid request" });
        return json(
          201,
          api.message(taskId, body.text, {
            agentId: body.agentId,
            to: body.to,
          }),
        );
      }
      const after = Number(url.searchParams.get("after") || 0);
      const to = url.searchParams.get("to") || "";
      return json(200, {
        messages: messages.filter(
          (m) =>
            m.taskId === taskId &&
            m.seq > after &&
            (!to || !m.to || m.to === to),
        ),
      });
    }
    if (sub === "events") {
      if (req.method === "POST") {
        if (
          !["started", "running", "done", "needs_input", "closed"].includes(
            body.kind,
          )
        )
          return json(400, { error: "invalid request" });
        return json(201, api.event(taskId, body.kind, body.agentId, body.text));
      }
      return feed(res, url, taskId);
    }
    return json(404, { error: "not found" });
  }

  function feed(res, url, taskId) {
    const after = Number(url.searchParams.get("after") || 0);
    const wait = url.searchParams.get("wait")
      ? Math.min(30000, parseDuration(url.searchParams.get("wait")))
      : 0;
    const limit = Number(url.searchParams.get("limit") || 100);
    const list = () =>
      events
        .filter((e) => e.seq > after && (!taskId || e.taskId === taskId))
        .slice(0, limit);
    const send = () => {
      const found = list();
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(
        JSON.stringify({ events: found, next: found.at(-1)?.seq || after }),
      );
    };
    if (list().length || !wait) return send();
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      send();
    };
    const timer = setTimeout(finish, wait);
    waiters.add(finish);
    res.on("close", () => {
      done = true;
      clearTimeout(timer);
      waiters.delete(finish);
    });
  }

  return { api, handle };
}

function parseDuration(value) {
  const m = /^(\d+)(ms|s)?$/.exec(value);
  if (!m) return 0;
  return Number(m[1]) * (m[2] === "ms" ? 1 : 1000);
}
