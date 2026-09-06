export const IDLE_MINUTES = [5, 15, 30, 60];
export function createInactivityLock({
  enabled,
  minutes,
  lock,
  now = Date.now,
}) {
  let last = now();
  let pending = false;
  const check = () => {
    if (!enabled()) return false;
    if (pending) return true;
    if (now() - last < minutes() * 60000) return false;
    pending = true;
    // Start synchronously so resume/input handlers cannot run before locking.
    try {
      Promise.resolve(lock())
        .catch(() => {})
        .finally(() => {
          pending = false;
        });
    } catch {
      pending = false;
    }
    return true;
  };
  return {
    check,
    // The first interaction after sleep cannot renew an expired deadline.
    activity() {
      const expired = check();
      if (!expired) last = now();
      return expired;
    },
    reset() {
      last = now();
    },
  };
}
