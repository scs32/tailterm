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
  let disposed = false,
    saved = false,
    failed = false,
    cacheFailed = false;
  let authenticationError = null,
    notification = null,
    revision = 0;
  const listeners = new Set(),
    pending = new Map(),
    checked = new Map();
  const auditJobs = new Map(),
    auditEpochs = new Map(),
    auditMemory = new Map(),
    auditSaveQueues = new Map();
  const paths = new Set(),
    dirty = new Set(),
    touched = new Map(),
    stops = new Set();
  const scope = crypto.subtle
    .digest(
      "SHA-256",
      new TextEncoder().encode(JSON.stringify([client.base, client.token])),
    )
    .then((bytes) =>
      [...new Uint8Array(bytes)]
        .map((b) => b.toString(16).padStart(2, "0"))
        .join(""),
    );
  const status = () => ({
    label: authenticationError
      ? "Hub sign-in required"
      : saved
        ? !online() || failed
          ? "Saved data · offline"
          : pending.size
            ? "Saved data · refreshing"
            : "Saved data"
        : cacheFailed
          ? "Local cache unavailable"
          : !online()
            ? "Offline · no saved data"
            : pending.size
              ? "Loading…"
              : "",
  });
  let previousStatus = "";
  function notify(changed = false) {
    if (disposed) return;
    // These two routine cache states are intentionally absent from every
    // view. Their transitions must not make subscribers reload visible DOM.
    const current = status().label;
    const label =
      current === "Saved data" || current === "Saved data · refreshing"
        ? ""
        : current;
    if (!changed && label === previousStatus) return;
    previousStatus = label;
    if (notification !== null) return;
    notification = setTimeout(() => {
      notification = null;
      if (!disposed) for (const listener of listeners) listener();
    }, 0);
  }
  async function stored(path) {
    try {
      return await cache.get(await scope, path);
    } catch {
      cacheFailed = true;
      return null;
    }
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
      return Promise.reject(
        new HubError(
          0,
          "Offline. Connect to load data that has not been saved on this browser.",
        ),
      );
    }
    checked.set(path, now());
    const requestedRevision = revision;
    const work = (async () => {
      try {
        const old = await stored(path);
        if (disposed) throw new Error("Workspace is locked.");
        const url = new URL(path, client.base);
        const previousMessages = old?.value.data?.messages;
        const incremental =
          /\/messages$/.test(url.pathname) &&
          url.searchParams.get("latest") === "1" &&
          previousMessages?.length;
        const limit = Math.max(
          1,
          Math.min(1000, Number(url.searchParams.get("limit")) || 50),
        );
        let data;
        if (incremental) {
          url.searchParams.delete("latest");
          url.searchParams.set("after", previousMessages.at(-1).seq);
          data = await client.request(url.pathname + url.search, {
            timeoutMs: 15000,
          });
          // Messages are immutable. Small deltas avoid downloading the entire
          // visible conversation on every new message over a slow connection.
          if (data.messages.length < limit)
            data = {
              ...data,
              messages: [...previousMessages, ...data.messages].slice(-limit),
            };
          else data = await client.request(path, { timeoutMs: 15000 });
        } else data = await client.request(path, { timeoutMs: 15000 });
        if (disposed) throw new Error("Workspace is locked.");
        if (requestedRevision !== revision) {
          checked.delete(path);
          notify(true);
          return data;
        }
        let retained = false;
        try {
          retained = await cache.put(await scope, path, { data });
        } catch {
          cacheFailed = true;
        }
        if (disposed) throw new Error("Workspace is locked.");
        saved ||= retained;
        failed = false;
        authenticationError = null;
        dirty.delete(path);
        if (JSON.stringify(old?.value.data) !== JSON.stringify(data))
          notify(true);
        return data;
      } catch (error) {
        if (disposed) throw error;
        if (error.status === 401 || error.status === 403) {
          authenticationError = error;
          await cache.clear(await scope).catch(() => {});
          saved = false;
        } else if (error.status === 404 || error.status === 410) {
          await cache
            .put(await scope, path, {
              error: { status: error.status, message: error.message },
            })
            .catch(() => {});
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
    if (
      entry &&
      (!dirty.has(path) ||
        (failed && now() - (checked.get(path) || 0) < refreshMs))
    ) {
      saved = true;
      if (!checked.has(path) || now() - checked.get(path) >= refreshMs)
        void refresh(path).catch(() => {});
      notify();
      return unwrap(entry);
    }
    try {
      return await refresh(path);
    } catch (error) {
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
    for (const task of new Set([...auditEpochs.keys(), ...auditMemory.keys()]))
      auditEpochs.set(task, (auditEpochs.get(task) || 0) + 1);
    for (const path of paths) {
      dirty.add(path);
      checked.delete(path);
    }
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
    const values = Object.entries(params).filter(
      ([, value]) => value !== undefined && value !== null && value !== "",
    );
    const q = new URLSearchParams(
      values.map(([key, value]) => [key, String(value)]),
    ).toString();
    return q ? "?" + q : "";
  };
  const auditKey = (task) => `/v1/tasks/${task}/message-audit/overlay`;
  async function auditOverlay(task) {
    if (auditMemory.has(task)) return structuredClone(auditMemory.get(task));
    const entry = await stored(auditKey(task));
    const value = entry?.value?.data;
    if (
      value?.version === 1 &&
      value.taskId === task &&
      value.records &&
      typeof value.records === "object"
    ) {
      auditMemory.set(task, value);
      saved = true;
      return structuredClone(value);
    }
    return { version: 1, taskId: task, checkpoint: "", records: {} };
  }
  async function saveAuditOverlay(task, value, epoch) {
    const previous = auditSaveQueues.get(task) || Promise.resolve();
    const save = previous
      .catch(() => {})
      .then(async () => {
        if (disposed) throw new Error("Workspace is locked.");
        if (auditEpochs.get(task) !== epoch) return false;
        const before = await auditOverlay(task);
        if (disposed) throw new Error("Workspace is locked.");
        if (auditEpochs.get(task) !== epoch) return false;
        const retained = await cache.put(await scope, auditKey(task), {
          data: value,
        });
        if (!retained)
          throw new Error(
            "The current audit overlay exceeds the encrypted 4 MiB cache bound.",
          );
        if (disposed) throw new Error("Workspace is locked.");
        if (auditEpochs.get(task) !== epoch) {
          // The complete write is serialized with all other audit saves. Restore
          // the last accepted projection before allowing a newer epoch to save.
          await cache.put(await scope, auditKey(task), { data: before });
          return false;
        }
        auditMemory.set(task, structuredClone(value));
        saved = true;
        return true;
      });
    auditSaveQueues.set(task, save);
    try {
      return await save;
    } finally {
      if (auditSaveQueues.get(task) === save) auditSaveQueues.delete(task);
    }
  }
  async function refreshAuditOverlay(task) {
    if (disposed) throw new Error("Workspace is locked.");
    const current = auditJobs.get(task);
    if (current && current.epoch === auditEpochs.get(task))
      return current.promise;
    const epoch = (auditEpochs.get(task) || 0) + 1;
    auditEpochs.set(task, epoch);
    let job;
    job = (async () => {
      let next = await auditOverlay(task);
      if (!online()) {
        failed = true;
        return next;
      }
      const records = structuredClone(next.records);
      let cursor = "";
      for (;;) {
        const page = await client.listMessageAuditChanges(task, {
          ...(cursor ? { cursor } : { checkpoint: next.checkpoint }),
          limit: 64,
        });
        for (const event of page.events || []) {
          const key = String(event.message.seq);
          records[key] = {
            ...records[key],
            message: event.message,
            current: event.after,
          };
        }
        if (page.nextCursor) {
          cursor = page.nextCursor;
          continue;
        }
        if (!page.checkpoint)
          throw new Error(
            "Audit feed ended without a completely-applied checkpoint.",
          );
        next = {
          version: 1,
          taskId: task,
          checkpoint: page.checkpoint,
          records,
          savedAt: now(),
        };
        if (!(await saveAuditOverlay(task, next, epoch)))
          return auditOverlay(task);
        failed = false;
        return next;
      }
    })()
      .catch((error) => {
        failed = true;
        notify(true);
        throw error;
      })
      .finally(() => {
        if (auditJobs.get(task)?.promise === job) auditJobs.delete(task);
      });
    auditJobs.set(task, { epoch, promise: job });
    return job;
  }
  async function loadMessageAudits(task, messages = []) {
    let overlay = await auditOverlay(task);
    if (online()) {
      try {
        overlay = await refreshAuditOverlay(task);
      } catch {
        // A complete prior checkpoint remains usable and truthfully stale.
      }
    }
    // Feed events carry the changing projection, not immutable creation
    // context. Bootstrap every visible record without an original so the UI
    // never invents an unclassified origin or loses an exact order reference.
    const missing = messages.filter(
      (message) => !overlay.records[String(message.seq)]?.original,
    );
    if (missing.length && online()) {
      const epoch = (auditEpochs.get(task) || 0) + 1;
      auditEpochs.set(task, epoch);
      const records = structuredClone(overlay.records);
      try {
        for (let start = 0; start < missing.length; start += 16) {
          const batch = await Promise.all(
            missing
              .slice(start, start + 16)
              .map((message) => client.getMessageAudit(task, message.seq)),
          );
          for (const record of batch)
            records[String(record.message.seq)] = record;
        }
        overlay = { ...overlay, records, savedAt: now() };
        if (!(await saveAuditOverlay(task, overlay, epoch)))
          overlay = await auditOverlay(task);
      } catch (error) {
        if (disposed) throw error;
        overlay = await auditOverlay(task);
        if (disposed) throw new Error("Workspace is locked.");
        failed = true;
        notify(true);
      }
    }
    return structuredClone(overlay.records);
  }
  return {
    ...client,
    cacheStatus: status,
    // Capability negotiation must reflect the connected hub. Persisting a 404
    // would strand this client on the legacy path after a hub upgrade, while
    // serving stale success offline could expose controls the hub cannot honor.
    capabilities: () => client.capabilities(),
    listTasks: async () => (await read("/v1/tasks")).tasks,
    getTask: (id) => read(`/v1/tasks/${id}`),
    listAgents: async (id) => (await read(`/v1/tasks/${id}/agents`)).agents,
    getAgent: (task, id) => read(`/v1/tasks/${task}/agents/${id}`),
    listMessages: async (task, params) =>
      (await read(`/v1/tasks/${task}/messages` + query(params))).messages,
    getMessageAudit: (task, seq) =>
      read(`/v1/tasks/${task}/message-audit/messages/${seq}`),
    listMessageAuditHistory: (task, seq, params) =>
      read(
        `/v1/tasks/${task}/message-audit/messages/${seq}/history` +
          query(params),
      ),
    loadMessageAudits,
    refreshMessageAuditOverlay: refreshAuditOverlay,
    listDecisions: (task, params) =>
      read(`/v1/tasks/${task}/decisions` + query(params)),
    listWorkItems: (params) => read("/v1/work-items" + query(params)),
    getWorkItem: (task, id) => read(`/v1/tasks/${task}/work-items/${id}`),
    listWorkItemRevisions: (task, id, params) =>
      read(`/v1/tasks/${task}/work-items/${id}/revisions` + query(params)),
    getWorkItemRevision: (task, id, revision) =>
      read(`/v1/tasks/${task}/work-items/${id}/revisions/${revision}`),
    listWorkItemHistoryGaps: (task, id, params) =>
      read(`/v1/tasks/${task}/work-items/${id}/history-gaps` + query(params)),
    listWorkItemMessages: (task, id, params) =>
      read(`/v1/tasks/${task}/work-items/${id}/messages` + query(params)),
    getNarrativeOverview: (task, id, params = {}) =>
      read(`/v1/tasks/${task}/work-items/${id}/narrative` + query(params)),
    listNarrativeTimeline: (task, id, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/timeline` + query(params),
      ),
    listNarrativeArtifacts: (task, id, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/artifacts` +
          query(params),
      ),
    listNarrativeArtifactVersions: (task, id, artifact, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/artifacts/${encodeURIComponent(artifact)}/versions` +
          query(params),
      ),
    getNarrativeArtifactVersion: (task, id, artifact, version) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/artifacts/${encodeURIComponent(artifact)}/versions/${version}`,
      ),
    listNarrativeLinks: (task, id, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/links` + query(params),
      ),
    listNarrativeCoverage: (task, id, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/coverage` + query(params),
      ),
    listNarrativeReports: (task, id, params) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/reports` + query(params),
      ),
    getNarrativeReportVersion: (task, id, report, version) =>
      read(
        `/v1/tasks/${task}/work-items/${id}/narrative/reports/${encodeURIComponent(report)}/versions/${version}`,
      ),
    listQueue: (task, params = {}) =>
      read(`/v1/tasks/${task}/queue` + query(params)),
    getQueueEntry: (task, entry) =>
      read(`/v1/tasks/${task}/queue/${encodeURIComponent(entry)}`),
    listQueueHistory: (task, entry, params = {}) =>
      read(
        `/v1/tasks/${task}/queue/${encodeURIComponent(entry)}/history` +
          query(params),
      ),
    listQueueChanges: (task, params = {}) =>
      read(`/v1/tasks/${task}/queue/changes` + query(params)),
    invalidate,
    invalidateDecisions,
    refreshDecisions,
    refreshConnection() {
      if (disposed) return;
      failed = !online();
      const work = [];
      if (online())
        for (const path of paths)
          if (now() - touched.get(path) < refreshMs * 2)
            work.push(refresh(path));
      notify();
      return Promise.allSettled(work);
    },
    subscribe(task, onEvents, options = {}) {
      const changed = () => {
        Promise.resolve()
          .then(() => onEvents([], 0))
          .catch(options.onError || (() => {}));
      };
      listeners.add(changed);
      const feed = client.subscribe(
        task,
        (events, cursor) => {
          if (events.some((event) => event.kind !== "heartbeat")) invalidate();
          return onEvents(events, cursor);
        },
        {
          ...options,
          onError: (error) => {
            failed = true;
            notify();
            options.onError?.(error);
          },
        },
      );
      const timer = setInterval(() => {
        if (online())
          for (const path of paths)
            if (
              now() - touched.get(path) < refreshMs * 2 &&
              (!checked.has(path) || now() - checked.get(path) >= refreshMs)
            )
              void refresh(path).catch(() => {});
      }, refreshMs);
      const stop = () => {
        listeners.delete(changed);
        feed.stop();
        clearInterval(timer);
        stops.delete(stop);
      };
      stops.add(stop);
      return { stop, cursor: feed.cursor };
    },
    dispose() {
      disposed = true;
      for (const task of auditEpochs.keys())
        auditEpochs.set(task, (auditEpochs.get(task) || 0) + 1);
      for (const stop of stops) stop();
      listeners.clear();
      clearTimeout(notification);
      stopMutation?.();
      cache.dispose?.();
    },
  };
}
