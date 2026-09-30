import { escapeHandlerAB as esc, handlerABHtml } from "./handler-ab-format.js";

// The handler arm comparison for the selected project (docs/handler-ab.md).
// Like Interventions, this closed, read-only disclosure owns its requests: it
// loads only when opened, keeps one request token per project, and a response
// only lands on the project and client that asked for it.
export function createHandlerABView({ client }) {
  let root = null,
    project = "",
    mountedClient = null;
  const states = new Map();
  const state = () => {
    if (!states.has(project))
      states.set(project, {
        open: false,
        report: null,
        error: "",
        loading: false,
        request: 0,
      });
    return states.get(project);
  };
  const offline = () =>
    String(client()?.cacheStatus?.().label || "").includes("offline");
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
    if (s.open && !s.report && !s.error && !s.loading) void load();
  }
  function render() {
    if (!root) return;
    const s = state();
    const status =
      (s.loading ? "Loading handler comparison…" : s.error) ||
      (offline() ? "Saved data · offline" : s.report ? "" : "Not loaded");
    root.innerHTML = `<details class="project-handler-ab" ${s.open ? "open" : ""} data-handler-ab-disclosure><summary>Handler A/B</summary><div class="handler-ab-controls"><button data-handler-ab-refresh>Refresh</button></div>${status ? `<p class="fine" role="status">${esc(status)}</p>` : ""}${s.report ? handlerABHtml(s.report) : ""}</details>`;
    const disclosure = root.querySelector("[data-handler-ab-disclosure]");
    if (disclosure)
      disclosure.ontoggle = (e) => {
        s.open = e.target.open;
        if (s.open && !s.report && !s.error && !s.loading) void load();
      };
    const refresh = root.querySelector("[data-handler-ab-refresh]");
    if (refresh) refresh.onclick = () => void load();
  }
  async function load() {
    const s = state(),
      id = project,
      c = client(),
      token = ++s.request;
    s.loading = true;
    render();
    try {
      if (typeof c.handlerABReport !== "function")
        throw Object.assign(new Error("unsupported"), { status: 404 });
      const report = await c.handlerABReport(id);
      if (token !== s.request || c !== client()) return;
      s.report = report;
      s.error = "";
    } catch (e) {
      if (token !== s.request || c !== client()) return;
      s.error = [404, 405].includes(e.status)
        ? "Handler A/B unsupported by this hub"
        : e.message || "Handler A/B unavailable";
    }
    s.loading = false;
    if (id === project) render();
  }
  function dispose() {
    root = null;
  }
  return { mount, dispose };
}
