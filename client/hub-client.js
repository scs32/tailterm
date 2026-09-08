// JSON client for the task hub, carried over the browser's Tailscale node.
// fetchImpl(url, {method, headers, body, timeoutMs}) resolves to
// {status, statusText, headers, text()} and is normally ipn.fetch.
export const HUB_URL_RE =
  /^https?:\/\/[A-Za-z0-9][A-Za-z0-9.-]*(:\d{1,5})?(\/[A-Za-z0-9._~\/-]*)?$/;
export function normalizeHubURL(value) {
  const url = String(value || "")
    .trim()
    .replace(/\/+$/, "");
  return url.length <= 200 && HUB_URL_RE.test(url) ? url : null;
}
export class HubError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}
export function createHubClient({ fetchImpl, baseURL, token = "" }) {
  const base = normalizeHubURL(baseURL);
  if (!base) throw new Error("Hub URL must be http(s)://host[:port]");
  async function request(path, { method = "GET", body, timeoutMs } = {}) {
    const init = { method, headers: {}, timeoutMs };
    if (token) init.headers.Authorization = "Bearer " + token;
    if (body !== undefined) {
      init.body = JSON.stringify(body);
      init.headers["Content-Type"] = "application/json";
    }
    const res = await fetchImpl(base + path, init);
    const text = await res.text();
    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        data = null;
      }
    }
    if (res.status >= 400)
      throw new HubError(
        res.status,
        data?.error || res.statusText || "hub error",
      );
    return data;
  }
  const q = (params) => {
    const s = new URLSearchParams();
    for (const [k, v] of Object.entries(params))
      if (v !== undefined && v !== null && v !== "") s.set(k, String(v));
    const out = s.toString();
    return out ? "?" + out : "";
  };
  const client = {
    base,
    token,
    request,
    whoami: () => request("/v1/whoami"),
    listTasks: async () => (await request("/v1/tasks")).tasks,
    createTask: (body) => request("/v1/tasks", { method: "POST", body }),
    getTask: (id) => request(`/v1/tasks/${id}`),
    updateTask: (id, body) =>
      request(`/v1/tasks/${id}`, { method: "PATCH", body }),
    closeTask: (id) => request(`/v1/tasks/${id}`, { method: "DELETE" }),
    listWorkItems: (params = {}) => request("/v1/work-items" + q(params)),
    getWorkItem: (task, id) => request(`/v1/tasks/${task}/work-items/${id}`),
    createWorkItem: (task, body) =>
      request(`/v1/tasks/${task}/work-items`, { method: "POST", body }),
    updateWorkItem: (task, id, body) =>
      request(`/v1/tasks/${task}/work-items/${id}`, { method: "PATCH", body }),
    dispatchWorkItem: (task, id, body) =>
      request(`/v1/tasks/${task}/work-items/${id}/dispatch`, {
        method: "POST",
        body,
      }),
    addAgent: (task, body) =>
      request(`/v1/tasks/${task}/agents`, { method: "POST", body }),
    listAgents: async (task) =>
      (await request(`/v1/tasks/${task}/agents`)).agents,
    getAgent: (task, agent) => request(`/v1/tasks/${task}/agents/${agent}`),
    updateAgent: (task, agent, body) =>
      request(`/v1/tasks/${task}/agents/${agent}`, { method: "PATCH", body }),
    closeAgent: (task, agent) =>
      request(`/v1/tasks/${task}/agents/${agent}`, { method: "DELETE" }),
    postMessage: (task, body) =>
      request(`/v1/tasks/${task}/messages`, { method: "POST", body }),
    listMessages: async (task, params = {}) =>
      (await request(`/v1/tasks/${task}/messages` + q(params))).messages,
    markRead: (task, body) =>
      request(`/v1/tasks/${task}/messages/read`, { method: "POST", body }),
    postEvent: (task, body) =>
      request(`/v1/tasks/${task}/events`, { method: "POST", body }),
    events: (task, { after = 0, wait, limit } = {}) =>
      request(
        (task ? `/v1/tasks/${task}/events` : "/v1/events") +
          q({ after, wait, limit }),
        {
          timeoutMs: wait ? 40000 : undefined,
        },
      ),
    // subscribe long-polls a feed until stop() is called. onEvents receives
    // each non-empty batch; onError receives failures (the loop backs off).
    subscribe(task, onEvents, { after = 0, onError = () => {} } = {}) {
      let stopped = false,
        cursor = after,
        backoff = 1000,
        timer = null;
      const sleep = (ms) =>
        new Promise((resolve) => {
          timer = setTimeout(resolve, ms);
        });
      (async () => {
        while (!stopped) {
          try {
            const list = await client.events(task, {
              after: cursor,
              wait: "25s",
              limit: 100,
            });
            if (stopped) return;
            backoff = 1000;
            if (list.events.length) {
              cursor = list.next;
              try {
                await onEvents(list.events, cursor);
              } catch (error) {
                onError(error);
              }
            }
          } catch (error) {
            if (stopped) return;
            onError(error);
            await sleep(backoff);
            backoff = Math.min(backoff * 2, 30000);
          }
        }
      })();
      return {
        stop() {
          stopped = true;
          clearTimeout(timer);
        },
        cursor: () => cursor,
      };
    },
  };
  return client;
}
