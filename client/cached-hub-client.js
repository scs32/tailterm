import { HubError } from "./hub-client.js";

// Cache only presentation reads. The underlying client remains authoritative
// for terminal reconciliation, launches, cleanup and every mutation.
export function createCachedHubClient({
  client,
  cache,
  online = () => true,
  now = Date.now,
  refreshMs = 20000,
}) {
  let disposed = false, saved = false, failed = false, cacheFailed = false;
  let authenticationError = null, notification = null, revision = 0;
  const listeners = new Set(), pending = new Map(), checked = new Map();
  const paths = new Set(), dirty = new Set(), touched = new Map(), stops = new Set();
  const scope = crypto.subtle.digest(
    "SHA-256", new TextEncoder().encode(JSON.stringify([client.base, client.token])),
  ).then((bytes) => [...new Uint8Array(bytes)].map((b) => b.toString(16).padStart(2, "0")).join(""));
  const status = () => ({
    label: authenticationError ? "Hub sign-in required"
      : saved ? (!online() || failed ? "Saved data · offline" : pending.size ? "Saved data · refreshing" : "Saved data")
        : cacheFailed ? "Local cache unavailable" : !online() ? "Offline · no saved data" : pending.size ? "Loading…" : "",
  });
  let previousStatus = "";
  function notify(changed = false) {
    if (disposed) return;
    const label = status().label;
    if (!changed && label === previousStatus) return;
    previousStatus = label;
    if (notification !== null) return;
    notification = setTimeout(() => {
      notification = null;
      if (!disposed) for (const listener of listeners) listener();
    }, 0);
  }
  async function stored(path) {
    try { return await cache.get(await scope, path); }
    catch { cacheFailed = true; return null; }
  }
  function unwrap(entry) {
    if (entry.value.error)
      throw new HubError(entry.value.error.status, entry.value.error.message);
    return structuredClone(entry.value.data);
  }
  function refresh(path) {
    if (disposed) return Promise.reject(new Error("Workspace is locked."));
    if (pending.has(path)) return pending.get(path);
    if (!online()) {
      failed = true;
      notify();
      return Promise.reject(new HubError(0, "Offline. Connect to load data that has not been saved on this browser."));
    }
    checked.set(path, now());
    const requestedRevision = revision;
    const work = (async () => {
      try {
        const old = await stored(path);
        if (disposed) throw new Error("Workspace is locked.");
        const url = new URL(path, client.base);
        const previousMessages = old?.value.data?.messages;
        const incremental = /\/messages$/.test(url.pathname) && url.searchParams.get("latest") === "1" && previousMessages?.length;
        const limit = Math.max(1, Math.min(1000, Number(url.searchParams.get("limit")) || 50));
        let data;
        if (incremental) {
          url.searchParams.delete("latest");
          url.searchParams.set("after", previousMessages.at(-1).seq);
          data = await client.request(url.pathname + url.search, { timeoutMs: 15000 });
          // Messages are immutable. Small deltas avoid downloading the entire
          // visible conversation on every new message over a slow connection.
          if (data.messages.length < limit)
            data = { ...data, messages: [...previousMessages, ...data.messages].slice(-limit) };
          else data = await client.request(path, { timeoutMs: 15000 });
        } else data = await client.request(path, { timeoutMs: 15000 });
        if (disposed) throw new Error("Workspace is locked.");
        if (requestedRevision !== revision) {
          checked.delete(path);
          notify(true);
          return data;
        }
        let retained = false;
        try { retained = await cache.put(await scope, path, { data }); }
        catch { cacheFailed = true; }
        if (disposed) throw new Error("Workspace is locked.");
        saved ||= retained;
        failed = false;
        authenticationError = null;
        dirty.delete(path);
        if (JSON.stringify(old?.value.data) !== JSON.stringify(data)) notify(true);
        return data;
      } catch (error) {
        if (disposed) throw error;
        if (error.status === 401 || error.status === 403) {
          authenticationError = error;
          await cache.clear(await scope).catch(() => {});
          saved = false;
        } else if (error.status === 404 || error.status === 410) {
          await cache.put(await scope, path, { error: { status: error.status, message: error.message } }).catch(() => {});
          dirty.delete(path);
        } else failed = true;
        notify(true);
        throw error;
      } finally {
        pending.delete(path);
        notify();
      }
    })();
    pending.set(path, work);
    notify();
    return work;
  }
  async function read(path) {
    if (disposed) throw new Error("Workspace is locked.");
    paths.add(path);
    touched.set(path, now());
    const entry = await stored(path);
    if (disposed) throw new Error("Workspace is locked.");
    if (authenticationError) throw authenticationError;
    if (entry && (!dirty.has(path) || (failed && now() - (checked.get(path) || 0) < refreshMs))) {
      saved = true;
      if (!checked.has(path) || now() - checked.get(path) >= refreshMs)
        void refresh(path).catch(() => {});
      notify();
      return unwrap(entry);
    }
    try { return await refresh(path); }
    catch (error) {
      if (entry && !authenticationError && ![404, 410].includes(error.status)) {
        saved = true;
        notify();
        return unwrap(entry);
      }
      throw error;
    }
  }
  function invalidate() {
    revision++;
    for (const path of paths) { dirty.add(path); checked.delete(path); }
    notify(true);
  }
  function invalidateDecisions(task) {
    revision++;
    const prefix = `/v1/tasks/${task}/decisions`;
    for (const path of paths)
      if (new URL(path, client.base).pathname === prefix) {
        dirty.add(path);
        checked.delete(path);
      }
    notify(true);
  }
  async function refreshDecisions(task) {
    invalidateDecisions(task);
    const prefix = `/v1/tasks/${task}/decisions`;
    const targets = [...paths].filter(
      (path) => new URL(path, client.base).pathname === prefix,
    );
    await Promise.all(
      targets.map(async (path) => {
        await pending.get(path)?.catch(() => {});
        if (dirty.has(path)) await refresh(path);
      }),
    );
  }
  const stopMutation = client.onMutation?.(invalidate);
  const query = (params = {}) => {
    const values = Object.entries(params).filter(([, value]) => value !== undefined && value !== null && value !== "");
    const q = new URLSearchParams(values.map(([key, value]) => [key, String(value)])).toString();
    return q ? "?" + q : "";
  };
  return {
    ...client,
    cacheStatus: status,
    listTasks: async () => (await read("/v1/tasks")).tasks,
    getTask: (id) => read(`/v1/tasks/${id}`),
    listAgents: async (id) => (await read(`/v1/tasks/${id}/agents`)).agents,
    getAgent: (task, id) => read(`/v1/tasks/${task}/agents/${id}`),
    listMessages: async (task, params) => (await read(`/v1/tasks/${task}/messages` + query(params))).messages,
    listDecisions: (task, params) =>
      read(`/v1/tasks/${task}/decisions` + query(params)),
    listWorkItems: (params) => read("/v1/work-items" + query(params)),
    getWorkItem: (task, id) => read(`/v1/tasks/${task}/work-items/${id}`),
    listWorkItemRevisions: (task, id, params) => read(`/v1/tasks/${task}/work-items/${id}/revisions` + query(params)),
    getWorkItemRevision: (task, id, revision) => read(`/v1/tasks/${task}/work-items/${id}/revisions/${revision}`),
    listWorkItemHistoryGaps: (task, id, params) => read(`/v1/tasks/${task}/work-items/${id}/history-gaps` + query(params)),
    listWorkItemMessages: (task, id, params) => read(`/v1/tasks/${task}/work-items/${id}/messages` + query(params)),
    invalidate,
    invalidateDecisions,
    refreshDecisions,
    refreshConnection() {
      if (disposed) return;
      failed = !online();
      const work = [];
      if (online()) for (const path of paths)
        if (now() - touched.get(path) < refreshMs * 2) work.push(refresh(path));
      notify(true);
      return Promise.allSettled(work);
    },
    subscribe(task, onEvents, options = {}) {
      const changed = () => { Promise.resolve().then(() => onEvents([], 0)).catch(options.onError || (() => {})); };
      listeners.add(changed);
      const feed = client.subscribe(task, (events, cursor) => {
        if (events.some((event) => event.kind !== "heartbeat")) invalidate();
        return onEvents(events, cursor);
      }, { ...options, onError: (error) => { failed = true; notify(); options.onError?.(error); } });
      const timer = setInterval(() => {
        if (online()) for (const path of paths)
          if (now() - touched.get(path) < refreshMs * 2 && (!checked.has(path) || now() - checked.get(path) >= refreshMs))
            void refresh(path).catch(() => {});
      }, refreshMs);
      const stop = () => { listeners.delete(changed); feed.stop(); clearInterval(timer); stops.delete(stop); };
      stops.add(stop);
      return { stop, cursor: feed.cursor };
    },
    dispose() {
      disposed = true;
      for (const stop of stops) stop();
      listeners.clear();
      clearTimeout(notification);
      stopMutation?.();
      cache.dispose?.();
    },
  };
}
