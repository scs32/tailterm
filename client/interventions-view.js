import {
  escapeIntervention as esc,
  interventionsHtml,
} from "./interventions-format.js";

// Owner interventions per day and kind for the selected project. Like Usage,
// this closed disclosure owns its requests: it loads only when opened and never
// holds core Projects rendering. Each project keeps its own request token, so a
// Projects re-render keeps an in-flight read instead of restarting it, and a
// response only lands on the project and client that asked for it.
export function createInterventionsView({ client }) {
  let root = null,
    project = "",
    mountedClient = null;
  const states = new Map();
  const state = () => {
    if (!states.has(project))
      states.set(project, {
        open: false,
        summary: null,
        error: "",
        loading: false,
        request: 0,
      });
    return states.get(project);
  };
  const offline = () =>
    String(client()?.cacheStatus?.().label || "").includes("offline");
  const timeZone = () => {
    try {
      return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
    } catch {
      return "UTC";
    }
  };
  function mount(node, id) {
    if (mountedClient !== client()) {
      states.clear();
      mountedClient = client();
    }
    root = node;
    project = id;
    if (!node) return;
    render();
    const s = state();
    if (s.open && !s.summary && !s.error && !s.loading) void load();
  }
  function render() {
    if (!root) return;
    const s = state();
    const status =
      (s.loading ? "Loading interventions…" : s.error) ||
      (offline() ? "Saved data · offline" : s.summary ? "" : "Not loaded");
    root.innerHTML = `<details class="project-usage project-interventions" ${s.open ? "open" : ""} data-interventions-disclosure><summary>Interventions</summary><div class="usage-controls"><button data-interventions-refresh>Refresh</button></div>${status ? `<p class="fine" role="status">${esc(status)}</p>` : ""}${s.summary ? interventionsHtml(s.summary) : ""}</details>`;
    root.querySelector("[data-interventions-disclosure]").ontoggle = (e) => {
      s.open = e.target.open;
      if (s.open && !s.summary && !s.error && !s.loading) void load();
    };
    root.querySelector("[data-interventions-refresh]").onclick = () =>
      void load();
  }
  async function load() {
    const s = state(),
      id = project,
      c = client(),
      token = ++s.request;
    s.loading = true;
    render();
    try {
      if (typeof c.listInterventions !== "function")
        throw Object.assign(new Error("unsupported"), { status: 404 });
      // The summary always covers the whole project; one record is enough.
      const list = await c.listInterventions(id, { tz: timeZone(), limit: 1 });
      if (token !== s.request || c !== client()) return;
      s.summary = list.summary;
      s.error = "";
    } catch (e) {
      if (token !== s.request || c !== client()) return;
      s.error = [404, 405].includes(e.status)
        ? "Interventions unsupported by this hub"
        : e.message || "Interventions unavailable";
    }
    s.loading = false;
    if (id === project) render();
  }
  function dispose() {
    root = null;
  }
  return { mount, dispose };
}
