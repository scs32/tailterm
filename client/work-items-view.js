import { createViewRefreshPresentation } from "./view-refresh-presentation.js";
import { createWorkItemsScroll } from "./work-items-scroll.js";

const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const statuses = {
  open: "Open",
  in_progress: "In progress",
  blocked: "Blocked",
  done: "Done",
  dismissed: "Dismissed",
};
const priorities = {
  low: "Low",
  normal: "Normal",
  high: "High",
  urgent: "Urgent",
};
const options = (values, selected) =>
  Object.entries(values)
    .map(
      ([id, label]) =>
        `<option value="${esc(id)}" ${id === selected ? "selected" : ""}>${esc(label)}</option>`,
    )
    .join("");

// The hub owns records. This view retains only filters and in-flight form drafts.
export function createWorkItemsView({
  kind,
  client,
  dialog,
  closeDialog,
  notice,
  configure,
  openBoard,
  draftPersistence,
}) {
  const plural = kind === "bug" ? "Bugs" : "Features",
    singular = kind === "bug" ? "Bug" : "Feature";
  let root,
    visible = false,
    generation = 0,
    subscription,
    tasks = [],
    items = [],
    scope = "",
    state = "",
    loading = false,
    again = false,
    narrativeGeneration = 0;
  const presentation = createViewRefreshPresentation({
    render: () => {
      if (visible && !loading) render();
    },
  });
  const project = (id) => tasks.find((t) => t.id === id);
  const itemScroll = createWorkItemsScroll({
    getContainer: () => root?.querySelector?.(".work-items-main"),
    render: () => render(),
    canRender: () => visible && !loading,
  });
  function mount(container) {
    root = container;
    presentation.mount(container);
  }
  function hide() {
    const wasVisible = visible;
    visible = false;
    generation++;
    loading = false;
    subscription?.stop();
    subscription = null;
    itemScroll.interrupt();
    presentation.interrupt({ capture: wasVisible });
  }
  async function show(taskId) {
    if (taskId !== undefined) scope = taskId || "";
    visible = true;
    await reload();
    if (visible && client() && !subscription)
      subscription = client().subscribe("", () => reload(), {
        onError: () => {},
      });
  }
  async function reload() {
    if (!visible || !root) return;
    if (loading) {
      again = true;
      return;
    }
    if (!client()) {
      itemScroll.interrupt();
      presentation.interrupt();
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">${plural.toUpperCase()}</span><h2>Connect a project hub.</h2><button data-items-configure>Configure project hub</button></div>`;
      root.querySelector("button").onclick = configure;
      return;
    }
    loading = true;
    again = false;
    const token = ++generation;
    const filter = { taskId: scope, status: state };
    try {
      const nextTasks = await client().listTasks(),
        next = [];
      if (!visible || token !== generation) return;
      let after = 0;
      do {
        const page = await client().listWorkItems({
          kind,
          ...filter,
          after,
          limit: 200,
        });
        next.push(...page.items);
        if (!page.items.length || !page.next || page.next <= after) break;
        after = page.next;
      } while (visible && token === generation);
      if (!visible || token !== generation) return;
      if (filter.taskId !== scope || filter.status !== state) {
        again = true;
        return;
      }
      tasks = nextTasks;
      items = next;
      render();
    } catch (error) {
      if (!visible || token !== generation) return;
      itemScroll.interrupt();
      presentation.interrupt();
      root.innerHTML = `<div class="mode-empty"><span class="eyebrow">${plural.toUpperCase()}</span><h2>Hub unavailable.</h2><p class="launcher-intro" role="alert">${esc(error.message)}</p><button data-items-retry>Retry</button></div>`;
      root.querySelector("button").onclick = reload;
    } finally {
      if (token === generation) {
        loading = false;
        if (again) void reload();
        else itemScroll.flush();
      }
    }
  }
  function render() {
    if (!visible) return;
    const filterKey = JSON.stringify([scope, state]);
    const itemScrollFrame = itemScroll.beforeRender(filterKey);
    if (!itemScrollFrame) return;
    const open = tasks.filter((task) => task.status === "open"),
      closed = tasks.filter((task) => task.status === "closed"),
      selectedProject = project(scope),
      syncLabel = client()?.cacheStatus?.().label || "",
      projectButton = (task) =>
        `<button type="button" data-board-task="${esc(task.id)}" data-items-scope="${esc(task.id)}" aria-pressed="${scope === task.id}" title="${esc(task.name)}"><span class="board-task-name">${esc(task.name)}</span><span class="fine">${esc(task.goal || (task.status === "closed" ? "Closed project" : "Open project"))}</span></button>`;
    if (!presentation.beforeRender(scope || "all")) return;
    root.innerHTML = `<div class="board mode-board work-items-view"><aside class="board-rail work-items-rail" aria-label="Projects"><div class="board-rail-head"><span class="eyebrow">PROJECTS</span><button type="button" data-items-new-rail title="New ${singular.toLowerCase()}" aria-label="New ${singular.toLowerCase()}" ${!open.length || selectedProject?.status === "closed" ? "disabled" : ""}>＋</button></div><button type="button" data-board-task="all" data-items-scope="" aria-pressed="${!scope}"><span class="board-task-name">All projects</span><span class="fine">${open.length} open project${open.length === 1 ? "" : "s"}</span></button>${open.map(projectButton).join("")}${closed.length ? `<details class="board-closed work-items-closed" data-view-disclosure="closed" ${selectedProject?.status === "closed" ? "open" : ""}><summary>Closed · ${closed.length}</summary>${closed.map(projectButton).join("")}</details>` : ""}</aside><section class="board-thread work-items-main"><div class="board-head work-items-head"><div class="view-heading"><div class="work-items-heading"><span class="eyebrow">${selectedProject ? "PROJECT" : "ALL PROJECTS"}</span><h2>${plural} <span class="count-badge">${items.length}</span></h2></div><div class="work-items-controls"><label class="work-items-project-select"><span>Project</span><select data-items-project data-view-control="project"><option value="">All projects</option>${tasks.map((t) => `<option value="${esc(t.id)}" ${scope === t.id ? "selected" : ""}>${esc(t.name)}${t.status === "closed" ? " · closed" : ""}</option>`).join("")}</select></label><label class="work-items-status-select"><span>Status</span><select data-items-status data-view-control="status"><option value="">All statuses</option>${options(statuses, state)}</select></label><span class="work-items-sync fine" data-work-items-sync role="status" aria-live="polite">${esc(syncLabel)}</span><button data-items-new class="primary" ${!open.length || selectedProject?.status === "closed" ? "disabled" : ""}>＋ New ${singular.toLowerCase()}</button></div></div><p class="fine">${esc(selectedProject?.name || "All projects")}</p></div><div class="work-items-list">${items.length ? items.map((item) => `<article class="work-item" data-work-item="${esc(item.id)}"><div class="work-item-copy"><button class="work-item-title" data-item-edit="${esc(item.id)}">${esc(item.title)}</button><span class="fine">${esc(project(item.taskId)?.name || item.taskId)} · ${esc(statuses[item.status])} · ${esc(priorities[item.priority])}${item.lastDispatch ? ` · Sent to ${esc(project(item.lastDispatch.targetTaskId)?.name || item.lastDispatch.targetTaskId)}` : ""}</span></div><div class="work-item-actions">${kind === "feature" ? `<button data-item-narrative="${esc(item.id)}">History &amp; report</button>` : ""}<button data-item-history="${esc(item.id)}">History · ${item.revision}</button><button data-item-send="${esc(item.id)}" ${project(item.taskId)?.status === "closed" ? "disabled" : ""}>Send to project</button></div></article>`).join("") : `<p class="work-items-empty fine">No ${plural.toLowerCase()} match these filters.</p>`}</div></section></div>`;
    presentation.afterRender(scope || "all");
    root.querySelectorAll("[data-items-scope]").forEach(
      (button) =>
        (button.onclick = () => {
          scope = button.dataset.itemsScope;
          void reload();
        }),
    );
    root.querySelector("[data-items-project]").onchange = (e) => {
      presentation.commit(e.target, { flush: false });
      scope = e.target.value;
      void reload();
    };
    root.querySelector("[data-items-status]").onchange = (e) => {
      presentation.commit(e.target, { flush: false });
      state = e.target.value;
      void reload();
    };
    root.querySelector("[data-items-new]").onclick = () => edit();
    root.querySelector("[data-items-new-rail]").onclick = () => edit();
    root
      .querySelectorAll("[data-item-edit]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            edit(items.find((i) => i.id === b.dataset.itemEdit))),
      );
    root
      .querySelectorAll("[data-item-history]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            history(items.find((i) => i.id === b.dataset.itemHistory))),
      );
    root
      .querySelectorAll("[data-item-narrative]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            narrative(items.find((i) => i.id === b.dataset.itemNarrative))),
      );
    root
      .querySelectorAll("[data-item-send]")
      .forEach(
        (b) =>
          (b.onclick = () =>
            dispatch(items.find((i) => i.id === b.dataset.itemSend))),
      );
    itemScroll.afterRender(itemScrollFrame);
  }
  const memoryDrafts = new Map();
  const drafts = draftPersistence || {
    load: async (_, id) => structuredClone(memoryDrafts.get(id) || null),
    save: async (draft) => memoryDrafts.set(draft.id, structuredClone(draft)),
    remove: async (_, id) => memoryDrafts.delete(id),
  };
  async function draftScope() {
    const value = JSON.stringify([client()?.base || "", client()?.token || ""]),
      bytes = await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(value),
      );
    return [...new Uint8Array(bytes)]
      .map((byte) => byte.toString(16).padStart(2, "0"))
      .join("");
  }
  async function edit(item) {
    const readonly = item && project(item.taskId)?.status === "closed";
    const credentialScope = await draftScope(),
      draftID = `${kind}:${item?.taskId || scope || "all"}:${item?.id || "new"}`,
      saved = readonly
        ? null
        : await drafts.load(credentialScope, draftID).catch(() => null),
      savedValues = saved?.values || {};
    dialog(
      item ? singular : `New ${singular.toLowerCase()}`,
      `<form id="work-item-form"><label>Project<select id="work-item-project" ${item ? "disabled" : ""}>${tasks
        .filter((t) => t.status === "open" || t.id === item?.taskId)
        .map(
          (t) =>
            `<option value="${esc(t.id)}" ${t.id === (savedValues.taskId || item?.taskId || scope) ? "selected" : ""}>${esc(t.name)}</option>`,
        )
        .join(
          "",
        )}</select></label><label>Title<input id="work-item-title" required maxlength="120" value="${esc(savedValues.title ?? item?.title ?? "")}" ${readonly ? "disabled" : ""}></label><label>Description<textarea id="work-item-description" rows="5" maxlength="8192" ${readonly ? "disabled" : ""}>${esc(savedValues.description ?? item?.description ?? "")}</textarea></label><div class="work-item-fields"><label>Status<select id="work-item-status" ${!item || readonly ? "disabled" : ""}>${options(statuses, savedValues.status || item?.status || "open")}</select></label><label>Priority<select id="work-item-priority" ${readonly ? "disabled" : ""}>${options(priorities, savedValues.priority || item?.priority || "normal")}</select></label></div><p id="work-item-error" class="fine" role="status">${readonly ? "This project is closed. Its records are read-only." : saved ? "Recovered unsent changes from this encrypted workspace." : ""}</p><div class="dialog-actions">${readonly ? "" : '<button type="button" data-item-discard>Discard draft</button><button type="submit" class="primary">Save</button>'}${item && !readonly ? '<button type="button" data-item-dispatch>Send to project</button>' : ""}</div></form>`,
    );
    const form = document.querySelector("#work-item-form"),
      error = form.querySelector("#work-item-error");
    let pending = false,
      key = saved?.requestId || crypto.randomUUID(),
      intent = saved?.intent || null;
    const values = () => ({
      taskId: item?.taskId || form.querySelector("#work-item-project").value,
      title: form.querySelector("#work-item-title").value,
      description: form.querySelector("#work-item-description").value,
      status: form.querySelector("#work-item-status").value,
      priority: form.querySelector("#work-item-priority").value,
    });
    const persist = () =>
      drafts.save({
        scope: credentialScope,
        id: draftID,
        requestId: key,
        intent,
        values: values(),
        updatedAt: new Date().toISOString(),
      });
    if (!readonly)
      form
        .querySelectorAll("input,textarea,select")
        .forEach((control) =>
          control.addEventListener(
            "input",
            () => void persist().catch(() => {}),
          ),
        );
    form
      .querySelector("[data-item-discard]")
      ?.addEventListener("click", async () => {
        await drafts.remove(credentialScope, draftID).catch(() => {});
        if (form.isConnected) closeDialog();
      });
    form
      .querySelector("[data-item-dispatch]")
      ?.addEventListener("click", () => dispatch(item));
    form.onsubmit = async (event) => {
      event.preventDefault();
      if (pending || readonly) return;
      const taskId =
        item?.taskId || form.querySelector("#work-item-project").value;
      const body = {
        title: form.querySelector("#work-item-title").value.trim(),
        description: form.querySelector("#work-item-description").value,
        priority: form.querySelector("#work-item-priority").value,
      };
      if (!taskId || !body.title) {
        error.textContent = "Choose a project and enter a title.";
        return;
      }
      const payload = JSON.stringify({
        taskId,
        ...body,
        ...(item
          ? { status: form.querySelector("#work-item-status").value }
          : {}),
      });
      const intentPayload = intent
        ? JSON.stringify({
            taskId: intent.taskId,
            title: intent.request?.title,
            description: intent.request?.description,
            priority: intent.request?.priority,
            ...(item ? { status: intent.request?.status } : {}),
          })
        : "";
      if (
        intent &&
        (intentPayload !== payload || (!item && intent.request?.kind !== kind))
      ) {
        key = crypto.randomUUID();
        intent = null;
      }
      if (
        !intent ||
        intent.taskId !== taskId ||
        intent.itemId !== (item?.id || "") ||
        intent.request?.requestId !== key
      ) {
        intent = null;
        const request = item
          ? {
              ...body,
              status: form.querySelector("#work-item-status").value,
              expectedRevision: item.revision,
              requestId: key,
            }
          : { ...body, kind, requestId: key };
        intent = { taskId, itemId: item?.id || "", request };
      }
      await persist().catch(() => {});
      pending = true;
      form.querySelectorAll("button").forEach((b) => (b.disabled = true));
      error.textContent = "Saving…";
      try {
        if (item)
          await client().createWorkItemUpdate(
            intent.taskId,
            intent.itemId,
            intent.request,
          );
        else await client().createWorkItem(intent.taskId, intent.request);
        await drafts.remove(credentialScope, draftID).catch(() => {});
        if (form.isConnected) closeDialog();
        notice(`${singular} saved.`);
        await reload();
      } catch (e) {
        if (e.status === 409) {
          key = crypto.randomUUID();
          intent = null;
          await persist().catch(() => {});
        }
        if (form.isConnected)
          error.textContent =
            e.status === 409
              ? `${e.message} Reopen this item to load its latest version; your text is retained here.`
              : e.message;
      } finally {
        pending = false;
        form.querySelectorAll("button").forEach((b) => (b.disabled = false));
      }
    };
    if (!readonly) form.querySelector("#work-item-title").focus();
  }
  async function history(item) {
    if (!item) return;
    dialog(
      `${singular} history`,
      `<section class="work-item-history" aria-live="polite"><p class="fine">Loading exact revisions…</p></section>`,
    );
    const panel = document.querySelector(".work-item-history");
    try {
      const revisions = [],
        gaps = [];
      let after = 0;
      do {
        const page = await client().listWorkItemRevisions(
          item.taskId,
          item.id,
          { after, limit: 32 },
        );
        revisions.push(...page.revisions);
        if (!page.nextAfter) {
          after = 0;
          break;
        }
        after = page.nextAfter;
      } while (panel.isConnected);
      let gapAfter = 0;
      do {
        const page = await client().listWorkItemHistoryGaps(
          item.taskId,
          item.id,
          { after: gapAfter, limit: 32 },
        );
        gaps.push(...page.gaps);
        if (!page.nextAfter) break;
        gapAfter = page.nextAfter;
      } while (panel.isConnected);
      if (!panel.isConnected) return;
      panel.innerHTML = `<div class="work-item-history-layout"><nav aria-label="Revisions">${revisions
        .slice()
        .reverse()
        .map(
          (r) =>
            `<button type="button" data-history-revision="${r.revision}" aria-pressed="false"><strong>v${r.revision}</strong><span>${esc(r.changeKind)} · ${esc(r.updatedAt)}</span></button>`,
        )
        .join(
          "",
        )}</nav><div data-history-detail></div></div>${gaps.length ? `<details class="work-item-history-gaps"><summary>History gaps · ${gaps.length}</summary>${gaps.map((gap) => `<p><strong>v${gap.firstRevision}${gap.lastRevision === gap.firstRevision ? "" : `–${gap.lastRevision}`}</strong> ${esc(gap.reasonCode)}${gap.detail ? ` · ${esc(gap.detail)}` : ""}</p>`).join("")}</details>` : ""}`;
      let selection = 0;
      const show = async (revision) => {
        const selectedAt = ++selection;
        const selected = revisions.find((entry) => entry.revision === revision);
        if (!selected || !panel.isConnected) return;
        panel
          .querySelectorAll("[data-history-revision]")
          .forEach((button) =>
            button.setAttribute(
              "aria-pressed",
              String(Number(button.dataset.historyRevision) === revision),
            ),
          );
        const detail = panel.querySelector("[data-history-detail]");
        detail.dataset.historyDetailRevision = String(revision);
        detail.innerHTML = `<span class="eyebrow">REVISION ${revision}</span><h3>${esc(selected.title)}</h3><p class="fine">${esc(statuses[selected.status] || selected.status)} · ${esc(priorities[selected.priority] || selected.priority)} · ${esc(selected.provenance)}</p><pre>${esc(selected.description)}</pre><p class="fine" data-history-messages>Loading linked messages…</p>`;
        try {
          const links = [];
          let after = 0;
          do {
            const page = await client().listWorkItemMessages(
              item.taskId,
              item.id,
              { revision, after, limit: 64 },
            );
            if (
              selectedAt !== selection ||
              !detail.isConnected ||
              detail.dataset.historyDetailRevision !== String(revision)
            )
              return;
            links.push(...page.links);
            if (!page.nextAfter) break;
            if (page.nextAfter <= after)
              throw new Error("Linked-message cursor did not advance.");
            after = page.nextAfter;
          } while (true);
          if (
            selectedAt !== selection ||
            !detail.isConnected ||
            detail.dataset.historyDetailRevision !== String(revision)
          )
            return;
          detail.querySelector("[data-history-messages]").outerHTML =
            links.length
              ? `<div class="work-item-history-messages"><span class="eyebrow">EXPLICIT MESSAGES</span>${links.map((link) => `<button type="button" data-history-message-task="${esc(link.message.taskId)}"><strong>#${link.message.seq}</strong> ${esc(link.message.text)}<span>${esc(link.revisionCoverage)}${link.source ? " · source" : ""}</span></button>`).join("")}</div>`
              : `<p class="fine">No messages were explicitly linked to this revision.</p>`;
          detail
            .querySelectorAll("[data-history-message-task]")
            .forEach(
              (button) =>
                (button.onclick = () =>
                  openBoard(button.dataset.historyMessageTask)),
            );
        } catch (error) {
          if (
            selectedAt === selection &&
            detail.isConnected &&
            detail.dataset.historyDetailRevision === String(revision)
          )
            detail.querySelector("[data-history-messages]").textContent =
              error.message;
        }
      };
      panel
        .querySelectorAll("[data-history-revision]")
        .forEach(
          (button) =>
            (button.onclick = () =>
              void show(Number(button.dataset.historyRevision))),
        );
      if (revisions.length) await show(revisions.at(-1).revision);
    } catch (error) {
      if (panel.isConnected)
        panel.innerHTML = `<p class="fine" role="alert">${esc(error.message)}</p>`;
    }
  }
  const allPages = async (load, field) => {
    const values = [],
      seen = new Set();
    let cursor = "";
    do {
      const page = await load(cursor);
      values.push(...(page[field] || []));
      cursor = page.nextCursor || page.cursor || "";
      if (cursor && seen.has(cursor))
        throw new Error("Narrative cursor did not advance.");
      if (cursor) seen.add(cursor);
    } while (cursor);
    return values;
  };
  const reportSection = (label, value) =>
    `<section><h4>${esc(label)}</h4><pre>${esc(value)}</pre></section>`;
  const actorLabel = (actor) =>
    actor?.agentId ||
    actor?.user ||
    actor?.node ||
    actor?.caller?.user ||
    actor?.caller?.node ||
    "unattributed";
  const referenceLabel = (ref) =>
    `${ref.label || ref.kind}${ref.taskId ? ` · ${ref.taskId}` : ""}${ref.messageSeq ? ` #${ref.messageSeq}` : ""}${ref.sourceId ? ` · ${ref.sourceId}` : ""}${ref.artifactId ? ` · ${ref.artifactId}` : ""}${ref.revision || ref.version ? ` v${ref.revision || ref.version}` : ""}${ref.locator ? ` · ${ref.locator}` : ""}`;
  const reportMarkup = (report) =>
    report
      ? `<p class="fine">${esc(report.reportId)} v${report.version} · scope ${report.scopeRevision} · SHA-256 ${esc(report.digest)} · submitted by ${esc(actorLabel(report.createdBy))}</p>${reportSection("Requested outcome", report.sections.requestedOutcome)}${reportSection("Delivered work or audit findings", report.sections.deliveredWork)}${reportSection("Verification and scope", report.sections.verification)}${reportSection("Limitations", report.sections.limitations)}${reportSection("Remaining work", report.sections.remainingWork)}<h4>References</h4><ul>${report.references.map((ref) => `<li>${esc(referenceLabel(ref))}</li>`).join("")}</ul>`
      : "";
  async function narrative(item, updateLocation = true) {
    if (!item || kind !== "feature") return;
    if (updateLocation && globalThis.history?.replaceState)
      globalThis.history.replaceState(
        null,
        "",
        `#feature-history/${encodeURIComponent(item.taskId)}/${encodeURIComponent(item.id)}`,
      );
    dialog(
      "Feature history & report",
      `<section class="feature-narrative" aria-live="polite"><p class="fine">Loading durable report and linked history…</p></section>`,
    );
    const panel = document.querySelector(".feature-narrative"),
      selectedAt = ++narrativeGeneration;
    try {
      const [
        overview,
        revisions,
        messages,
        artifacts,
        links,
        coverage,
        reports,
        timeline,
      ] = await Promise.all([
        client().getNarrativeOverview(item.taskId, item.id),
        (async () => {
          const out = [];
          let after = 0;
          do {
            const page = await client().listWorkItemRevisions(
              item.taskId,
              item.id,
              { after, limit: 64 },
            );
            out.push(...page.revisions);
            if (!page.nextAfter) break;
            after = page.nextAfter;
          } while (true);
          return out;
        })(),
        (async () => {
          const out = [];
          let after = 0;
          do {
            const page = await client().listWorkItemMessages(
              item.taskId,
              item.id,
              { after, limit: 64 },
            );
            out.push(...page.links);
            if (!page.nextAfter) break;
            after = page.nextAfter;
          } while (true);
          return out;
        })(),
        allPages(
          (cursor) =>
            client().listNarrativeArtifacts(item.taskId, item.id, {
              cursor,
              limit: 64,
            }),
          "artifacts",
        ),
        allPages(
          (cursor) =>
            client().listNarrativeLinks(item.taskId, item.id, {
              cursor,
              limit: 64,
            }),
          "links",
        ),
        allPages(
          (cursor) =>
            client().listNarrativeCoverage(item.taskId, item.id, {
              cursor,
              limit: 64,
            }),
          "coverage",
        ),
        allPages(
          (cursor) =>
            client().listNarrativeReports(item.taskId, item.id, {
              cursor,
              limit: 64,
            }),
          "reports",
        ),
        allPages(
          (cursor) =>
            client().listNarrativeTimeline(item.taskId, item.id, {
              cursor,
              limit: 64,
            }),
          "entries",
        ),
      ]);
      if (!panel.isConnected || selectedAt !== narrativeGeneration) return;
      const latestLinks = new Map();
      for (const link of links) latestLinks.set(link.linkId, link);
      const currentLinks = [...latestLinks.values()].filter(
        (link) => link.action !== "retract",
      );
      const seenMessages = new Set(messages.map((entry) => entry.message.seq));
      const linkedMessages = [];
      if (client().listMessages)
        for (const link of currentLinks) {
          if (
            ["message", "decision-request", "decision-answer"].includes(
              link.target?.kind,
            ) &&
            !seenMessages.has(link.target.messageSeq)
          ) {
            const found = await client().listMessages(link.target.taskId, {
              after: link.target.messageSeq - 1,
              limit: 1,
            });
            if (found[0]?.seq === link.target.messageSeq) {
              linkedMessages.push({
                message: found[0],
                revisionCoverage: "explicit narrative link",
              });
              seenMessages.add(found[0].seq);
            }
          }
        }
      if (!panel.isConnected || selectedAt !== narrativeGeneration) return;
      const reportPin = overview.latestReport;
      const report = reportPin
        ? await client().getNarrativeReportVersion(
            item.taskId,
            item.id,
            reportPin.reportId,
            reportPin.version,
          )
        : null;
      if (!panel.isConnected || selectedAt !== narrativeGeneration) return;
      const coverageRows = [...coverage, ...(overview.defaultGaps || [])];
      const chronology = [
        ...revisions.map((value) => ({
          at: value.updatedAt,
          label: `Feature revision ${value.revision}`,
          detail: `${value.changeKind} · ${value.provenance}`,
          text: value.description,
        })),
        ...[...messages, ...linkedMessages].map((value) => ({
          at: value.message.createdAt,
          label: `${value.message.decisionRequest ? "Decision request" : value.message.decisionAnswer ? "Decision answer" : "Board message"} #${value.message.seq}`,
          detail: `${value.revisionCoverage}${value.source ? " · original source" : ""}${value.message.decisionAnswer?.optionId ? ` · answer ${value.message.decisionAnswer.optionId}` : ""}`,
          text: value.message.text,
        })),
        ...timeline.map((value) => ({
          at: value.sourceTime || value.createdAt,
          label: `${value.kind} ${value.objectId} v${value.version}`,
          detail: [value.source, value.relationship, value.captureState]
            .filter(Boolean)
            .join(" · "),
          text: "",
        })),
      ].sort((a, b) => String(a.at).localeCompare(String(b.at)));
      panel.innerHTML = `<header><span class="eyebrow">DURABLE FEATURE NARRATIVE</span><h3>${esc(overview.item.title)}</h3><p class="fine">${esc(statuses[overview.item.status] || overview.item.status)} · scope ${overview.item.scopeRevision} · ${overview.history.complete ? "retained work-item history complete" : "work-item history has gaps"}</p></header>
        <section class="feature-report"><span class="eyebrow">FINAL REPORT</span>${overview.completionReport ? `<p class="feature-completion-pin"><strong>Completion pin:</strong> ${esc(overview.completionReport.reportId)} v${overview.completionReport.version} · scope ${overview.completionReport.scopeRevision} · SHA-256 ${esc(overview.completionReport.digest)}</p>` : ""}${overview.completionReport && overview.latestReport && (overview.completionReport.reportId !== overview.latestReport.reportId || overview.completionReport.version !== overview.latestReport.version) ? '<p class="feature-gap">Showing the latest corrected report below. The exact report used for completion remains pinned and available in report history.</p>' : ""}${reports.length ? `<nav class="feature-report-versions" aria-label="Report versions">${reports.map((entry) => `<button type="button" data-report-id="${esc(entry.reportId)}" data-report-version="${entry.version}" aria-pressed="${entry.reportId === report?.reportId && entry.version === report?.version}">${entry.reportId === overview.completionReport?.reportId && entry.version === overview.completionReport?.version ? "Completion · " : ""}${entry.reportId === overview.latestReport?.reportId && entry.version === overview.latestReport?.version ? "Latest · " : ""}${esc(entry.reportId)} v${entry.version}</button>`).join("")}</nav><div data-feature-report-detail>${reportMarkup(report)}</div>` : `<p class="feature-gap" role="status">${overview.legacyReportMissing ? "Legacy completed feature: its retained description is available below, but no dedicated report was imported." : "No dedicated feature report has been stored."}</p>`}</section>
        <section><span class="eyebrow">SOURCE COVERAGE</span><div class="feature-coverage">${coverageRows.length ? coverageRows.map((entry) => `<article><strong>${esc(entry.source || "unspecified source")}</strong><span>${esc(entry.captureState)} · ${esc(entry.assessment || "unverified")}${entry.unknownExtent ? " · extent unknown" : ""}${entry.asOf ? ` · as of ${esc(entry.asOf)}` : ""}</span><p>${esc(entry.scope)}</p><p class="fine">Declared by ${esc(actorLabel(entry.createdBy))}; assessment attributed to ${esc(actorLabel(entry.assessmentBy))}.</p>${entry.assessmentText ? `<p>${esc(entry.assessmentText)}</p>` : ""}${entry.capturedIds?.length ? `<p class="fine">Captured IDs: ${esc(entry.capturedIds.join("; "))}</p>` : ""}${entry.evidenceReferences?.length ? `<p class="fine">Assessment evidence: ${entry.evidenceReferences.map((ref) => esc(referenceLabel(ref))).join("; ")}</p>` : '<p class="feature-gap">No exact evidence versions support an execution assessment.</p>'}${entry.knownGaps?.length ? `<p class="feature-gap">Gaps: ${esc(entry.knownGaps.join("; "))}</p>` : ""}</article>`).join("") : '<p class="feature-gap">No coverage declarations were submitted.</p>'}</div></section>
        <section><span class="eyebrow">ARTIFACTS & LINKS</span>${artifacts.length ? `<div class="feature-artifacts">${artifacts.map((artifact) => `<button type="button" data-narrative-artifact="${esc(artifact.artifactId)}" data-version="${artifact.latest.version}"><strong>${esc(artifact.latest.title)}</strong><span>${esc(artifact.namespace)} · ${esc(artifact.latest.captureState)} · v${artifact.latest.version}</span><span>original ${esc(actorLabel(artifact.latest.originalAuthor))} · ingested ${esc(actorLabel(artifact.latest.ingestedBy))}</span></button>`).join("")}</div>` : '<p class="fine">No submitted external or supporting artifacts.</p>'}<p class="fine">${currentLinks.length} current explicit link${currentLinks.length === 1 ? "" : "s"}; ${links.length} immutable link event${links.length === 1 ? "" : "s"} retained.</p>${links.length ? `<div class="feature-link-history">${links.map((link) => `<article class="${link.action === "retract" ? "is-retracted" : ""}"><strong>${esc(link.action)} · ${esc(link.relationship)}</strong><span>${esc(referenceLabel(link.target))} · feature scope ${link.featureRevision || "unknown"}</span><span>by ${esc(actorLabel(link.createdBy))}${link.reason ? ` · ${esc(link.reason)}` : ""}</span></article>`).join("")}</div>` : ""}<div data-narrative-artifact-detail></div></section>
        <section><span class="eyebrow">CHRONOLOGY</span><div class="feature-chronology">${chronology.map((entry) => `<article><time>${esc(entry.at)}</time><strong>${esc(entry.label)}</strong><span>${esc(entry.detail)}</span>${entry.text ? `<pre>${esc(entry.text)}</pre>` : ""}</article>`).join("")}</div></section>`;
      panel.querySelectorAll("[data-report-id]").forEach((button) => {
        button.onclick = async () => {
          const reportAt = ++narrativeGeneration,
            detail = panel.querySelector("[data-feature-report-detail]");
          detail.innerHTML = '<p class="fine">Loading exact report…</p>';
          try {
            const selectedReport = await client().getNarrativeReportVersion(
              item.taskId,
              item.id,
              button.dataset.reportId,
              Number(button.dataset.reportVersion),
            );
            if (!detail.isConnected || reportAt !== narrativeGeneration) return;
            panel
              .querySelectorAll("[data-report-id]")
              .forEach((choice) =>
                choice.setAttribute(
                  "aria-pressed",
                  String(
                    choice.dataset.reportId === selectedReport.reportId &&
                      Number(choice.dataset.reportVersion) ===
                        selectedReport.version,
                  ),
                ),
              );
            detail.innerHTML = reportMarkup(selectedReport);
          } catch (error) {
            if (detail.isConnected && reportAt === narrativeGeneration)
              detail.innerHTML = `<p role="alert">${esc(error.message)}</p>`;
          }
        };
      });
      panel.querySelectorAll("[data-narrative-artifact]").forEach((button) => {
        button.onclick = async () => {
          const detail = panel.querySelector(
              "[data-narrative-artifact-detail]",
            ),
            requestAt = ++narrativeGeneration;
          detail.innerHTML = '<p class="fine">Loading artifact versions…</p>';
          try {
            const versions = await allPages(
              (cursor) =>
                client().listNarrativeArtifactVersions(
                  item.taskId,
                  item.id,
                  button.dataset.narrativeArtifact,
                  { cursor, limit: 64 },
                ),
              "versions",
            );
            if (!detail.isConnected || requestAt !== narrativeGeneration)
              return;
            detail.innerHTML = `<nav class="feature-artifact-versions" aria-label="Artifact versions">${versions.map((version) => `<button type="button" data-artifact-version="${version.version}">v${version.version}${version.supersedesVersion ? ` · corrects v${version.supersedesVersion}` : ""}</button>`).join("")}</nav><article class="feature-artifact-detail"></article>`;
            const showVersion = async (version) => {
              const versionAt = ++narrativeGeneration,
                body = detail.querySelector(".feature-artifact-detail");
              body.innerHTML =
                '<p class="fine">Loading exact artifact content…</p>';
              try {
                const artifact = await client().getNarrativeArtifactVersion(
                  item.taskId,
                  item.id,
                  button.dataset.narrativeArtifact,
                  version,
                );
                if (!body.isConnected || versionAt !== narrativeGeneration)
                  return;
                detail
                  .querySelectorAll("[data-artifact-version]")
                  .forEach((choice) =>
                    choice.setAttribute(
                      "aria-pressed",
                      String(
                        Number(choice.dataset.artifactVersion) === version,
                      ),
                    ),
                  );
                body.innerHTML = `<h4>${esc(artifact.title)}</h4><p class="fine">${esc(artifact.provenance)} · ${esc(artifact.availability)}${artifact.sourceTime ? ` · source time ${esc(artifact.sourceTime)}` : ""}${artifact.contentDigest ? ` · SHA-256 ${esc(artifact.contentDigest)}` : ""}</p><p class="fine">Original author: ${esc(actorLabel(artifact.originalAuthor))}; ingested by: ${esc(actorLabel(artifact.ingestedBy))} at ${esc(artifact.ingestedAt)}.</p>${artifact.content ? `<pre>${esc(artifact.content)}</pre>` : `<p>${artifact.locator ? `Durable reference: ${esc(artifact.locator)}` : "Content was not ingested."}</p>`}`;
              } catch (error) {
                if (body.isConnected && versionAt === narrativeGeneration)
                  body.innerHTML = `<p role="alert">${esc(error.message)}</p>`;
              }
            };
            detail
              .querySelectorAll("[data-artifact-version]")
              .forEach(
                (choice) =>
                  (choice.onclick = () =>
                    showVersion(Number(choice.dataset.artifactVersion))),
              );
            await showVersion(Number(button.dataset.version));
          } catch (error) {
            if (detail.isConnected && requestAt === narrativeGeneration)
              detail.innerHTML = `<p role="alert">${esc(error.message)}</p>`;
          }
        };
      });
    } catch (error) {
      if (panel.isConnected && selectedAt === narrativeGeneration)
        panel.innerHTML = `<p class="fine" role="alert">${esc(error.message)}</p>`;
    }
  }
  function dispatch(item) {
    if (!item || project(item.taskId)?.status === "closed") return;
    dialog(
      "Send to project",
      `<form id="work-item-dispatch"><p>${esc(item.title)}</p><label>Project<select id="work-item-target">${tasks
        .filter((t) => t.status === "open")
        .map(
          (t) =>
            `<option value="${esc(t.id)}" ${t.id === item.taskId ? "selected" : ""}>${esc(t.name)}${t.orchestrator ? " · " + esc(t.orchestrator) : " · no orchestrator"}</option>`,
        )
        .join(
          "",
        )}</select></label><p class="fine">Send this revision to the project’s orchestrator. The item stays in ${esc(project(item.taskId)?.name || item.taskId)}.</p><p id="work-item-dispatch-status" class="fine" role="status"></p><div class="dialog-actions"><button type="submit" class="primary">Send to project</button></div></form>`,
    );
    const form = document.querySelector("#work-item-dispatch"),
      button = form.querySelector("button"),
      target = form.querySelector("select"),
      status = form.querySelector("[role=status]");
    let key = crypto.randomUUID();
    target.onchange = () => {
      key = crypto.randomUUID();
    };
    form.onsubmit = async (e) => {
      e.preventDefault();
      if (button.disabled) return;
      button.disabled = true;
      target.disabled = true;
      status.textContent = "Sending…";
      try {
        const result = await client().dispatchWorkItem(item.taskId, item.id, {
          revision: item.revision,
          targetTaskId: target.value,
          requestId: key,
        });
        if (form.isConnected) {
          const targetName =
            project(result.dispatch.targetTaskId)?.name ||
            result.dispatch.targetTaskId;
          closeDialog();
          notice(
            `${singular} sent to ${targetName} · board message #${result.dispatch.messageSeq}.`,
          );
        }
        await reload();
      } catch (error) {
        if (form.isConnected) {
          status.textContent = error.message;
          target.disabled = false;
        }
      } finally {
        button.disabled = false;
      }
    };
  }
  return {
    mount,
    show,
    hide,
    reload,
    openNarrative: (id, updateLocation = true) =>
      narrative(
        items.find((item) => item.id === id),
        updateLocation,
      ),
  };
}
