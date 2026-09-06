export const CREDENTIAL_IDLE_MS = 5 * 60 * 1000;
// Keep temporary credentials only while connections use them, plus a short
// reconnect window. A generation prevents late callbacks from undoing a clear.
export function createCredentialCache(now = Date.now) {
  const entries = new Map();
  let generation = 0;
  const prune = () => {
    for (const [id, entry] of entries)
      if (!entry.users && now() - entry.idleSince >= CREDENTIAL_IDLE_MS) {
        entry.value = null;
        entries.delete(id);
      }
  };
  return {
    prune,
    clear() {
      generation++;
      for (const entry of entries.values()) entry.value = null;
      entries.clear();
    },
    acquire(id, { retain = true } = {}) {
      prune();
      const epoch = generation;
      let entry = entries.get(id);
      if (!entry)
        entries.set(id, (entry = { users: 0, value: null, idleSince: now() }));
      if (retain) entry.users++;
      let released = false;
      return {
        get() {
          return epoch === generation && !released && entry.value
            ? { ...entry.value }
            : null;
        },
        set(value) {
          if (retain && epoch === generation && !released)
            entry.value = { ...value };
        },
        release() {
          if (released) return;
          released = true;
          if (retain) {
            entry.users--;
            if (!entry.users) entry.idleSince = now();
          }
        },
      };
    },
  };
}
export const credentialCache = createCredentialCache();
