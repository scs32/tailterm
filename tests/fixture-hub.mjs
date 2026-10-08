// In-memory stand-in for tailterm-hub, mounted on the static test server at
// /fixture-hub/v1/*. It mirrors the real API closely enough for the client:
// tasks, agents, messages, lifecycle events, and long-polled feeds. It also
// offers navigateWithHubHeld, which a test wraps around every Lock or reload
// of a page that talks to it.
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
    createTask(name, goal = "", allowAgentSpawn = false, orchestrator = "") {
      const t = {
        id: id("tsk"),
        name,
        goal,
        allowAgentSpawn,
        orchestrator,
        status: "open",
        pauseState: "active",
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
        agentId = "",
        runId = "",
        role = "",
      },
    ) {
      const a = {
        id: agentId || id("agt"),
        taskId,
        name,
        host,
        session,
        runtime,
        cwd,
        parentAgentId,
        runId: runId || id("run"),
        role,
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
        exited: "exited",
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
    // Pausing closes every run and keeps the project open, as the hub does.
    pauseTask(taskId) {
      const t = tasks.get(taskId);
      for (const a of agents.values())
        if (a.taskId === taskId && a.status !== "closed")
          api.event(taskId, "closed", a.id);
      t.pauseState = "paused";
      emit(taskId, "task_updated");
      return t;
    },
    // A fresh run of the same agent in the same tmux session.
    restartAgent(agentId) {
      const a = agents.get(agentId);
      a.runId = id("run");
      return api.event(a.taskId, "started", a.id);
    },
    agents: () => [...agents.values()],
    messages: () => messages,
    events: () => events,
    tasks: () => [...tasks.values()],
    requests: [],
  };

  // Requests not answered yet, and the answers kept back while a page
  // navigates (see navigateWithHubHeld below).
  const unanswered = new Set();
  let held = null;

  async function handle(req, res, url) {
    const entry = { key: req.method + " " + req.url };
    const end = res.end.bind(res);
    unanswered.add(entry);
    res.on("close", () => unanswered.delete(entry));
    res.end = (...args) => {
      const send = () => {
        unanswered.delete(entry);
        end(...args);
      };
      if (held) held.push(send);
      else send();
      return res;
    };
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
        return json(
          201,
          api.createTask(
            body.name,
            body.goal || "",
            !!body.allowAgentSpawn,
            body.orchestrator || "",
          ),
        );
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
      if (req.method === "PATCH") {
        Object.assign(task, body);
        emit(taskId, "task_updated");
        return json(200, task);
      }
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

  // A navigation must not cut off a hub response. The Go WASM HTTP client reads
  // a response body as a stream and, when the read fails, cancels the stream
  // without handling the promise that returns. A reload (Lock included) that
  // lands between a response's headers and the end of its body therefore
  // leaves an unhandled "Load failed" rejection, which WebKit reports as a page
  // error. The fixture hub answers every long-poll whenever any event is posted,
  // so such a response can be on the wire at any reload. WebKit cancels the old
  // document's requests as the navigation starts, so the wait comes first: stop
  // the fixture answering, wait until the browser has received every answer
  // already sent, then navigate. Requests the fixture has not answered are
  // cancelled before their headers, which the client handles.
  const hubKey = (request) => {
    const url = new URL(request.url());
    return url.pathname.startsWith("/fixture-hub/")
      ? request.method() + " " + url.pathname + url.search
      : "";
  };
  const watched = new WeakSet();
  const inFlight = new Set();
  // The hold has to see every hub request a page makes, so a test hands over
  // its browser context before opening the page.
  function watchRequests(context) {
    watched.add(context);
    // Chromium reports neither an end nor a failure for a request that a
    // navigation discards. So a frame's open requests are forgotten when it
    // commits the document it asked for (framenavigated alone also fires for
    // same-document navigations, which discard nothing), and a page's when it
    // closes.
    const documentRequests = new Map();
    const forget = (gone) => {
      for (const request of inFlight)
        if (gone(request.frame())) inFlight.delete(request);
    };
    context.on("request", (request) => {
      if (hubKey(request)) inFlight.add(request);
      else if (request.isNavigationRequest())
        documentRequests.set(request.frame(), request);
    });
    context.on("requestfinished", (request) => inFlight.delete(request));
    context.on("requestfailed", (request) => {
      inFlight.delete(request);
      if (documentRequests.get(request.frame()) === request)
        documentRequests.delete(request.frame());
    });
    context.on("page", (opened) => {
      opened.on("framenavigated", (frame) => {
        if (documentRequests.delete(frame)) forget((owner) => owner === frame);
      });
      opened.on("close", () => forget((owner) => owner.page() === opened));
    });
  }
  // A request the browser still waits on that the fixture is not keeping has
  // its answer, or the request itself, on the wire.
  const onTheWire = () => {
    const kept = [...unanswered].map((entry) => entry.key);
    return [...inFlight].map(hubKey).filter((key) => {
      const at = kept.indexOf(key);
      if (at < 0) return true;
      kept.splice(at, 1);
      return false;
    });
  };
  let holds = 0;
  async function navigateWithHubHeld(page, navigate) {
    if (!watched.has(page.context()))
      throw new Error(
        "navigateWithHubHeld needs watchRequests(context) before the page opens",
      );
    holds++;
    held ||= [];
    try {
      const deadline = Date.now() + 15000;
      while (onTheWire().length) {
        if (Date.now() > deadline)
          throw new Error(
            "Hub responses still on the wire before navigation: " +
              onTheWire().join(", "),
          );
        await new Promise((resolve) => setTimeout(resolve, 5));
      }
      const navigated = page.waitForEvent("framenavigated", {
        predicate: (frame) => frame === page.mainFrame(),
      });
      navigated.catch(() => {}); // reported by the await below, or by navigate()
      const result = await navigate();
      await navigated;
      return result;
    } finally {
      // The old document's requests are cancelled by now; an answer still
      // kept for another page is sent.
      if (!--holds) {
        const kept = held;
        held = null;
        for (const send of kept) send();
      }
    }
  }

  return { api, handle, watchRequests, navigateWithHubHeld };
}

function parseDuration(value) {
  const m = /^(\d+)(ms|s)?$/.exec(value);
  if (!m) return 0;
  return Number(m[1]) * (m[2] === "ms" ? 1 : 1000);
}
