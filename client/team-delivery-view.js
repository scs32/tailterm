import { activityLabel, tokenSnapshot } from "./activity-format.js";
// Read-only project delivery panel. Integration is deliberately owner gated.
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

export function renderTeamDelivery(queue, agents = []) {
  if (!queue || !Array.isArray(queue.entries) || queue.entries.length === 0) return "";
  const byId = new Map(agents.map((agent) => [agent.id, agent]));
  const rows = queue.entries.map((entry) => {
    const lead = agents.find((agent) => agent.itemLead && agent.workItem?.itemId === entry.itemId)?.name || (entry.state === "launching" ? entry.launch?.members?.[0]?.fields?.name : "") || "";
    const handlerName = byId.get(entry.handlerId)?.name || entry.handlerId || "";
    const handler = handlerName ? `${handlerName}${entry.handlerLeaseGeneration ? ` · lease ${entry.handlerLeaseGeneration}` : ""}` : "";
    const owns = entry.ownership?.length ? entry.ownership.join(", ") : "Unscoped · conflicts with all work";
    const blocked = [entry.blockedBy?.length ? `Waiting for ${entry.blockedBy.join(", ")}` : "", entry.blockReason || ""].filter(Boolean).join(" · ");
    const worktree = entry.integration?.worktree ? ` · ${esc(entry.integration.worktree)}` : "";
    const ready = entry.integration && entry.state === "finished" ? `<div class="fine" data-testid="team-ready-to-integrate"><strong>Ready to integrate</strong> · ${esc(entry.integration.repository)} · base ${esc(entry.integration.baseCommit)}${worktree} · ${esc(entry.integration.branch)} @ ${esc(entry.integration.commit)} · ${esc(entry.integration.evidence)}</div>` : entry.acceptance ? `<div class="fine">Accepted ${esc(entry.acceptance.branch)} @ ${esc(entry.acceptance.commit)} · waiting for team cleanup</div>` : "";
    const activity = entry.activities?.map((member) => `${member.name}: ${activityLabel(member.activity)}`).join(" · ") || "";
    return `<div class="team-delivery-row" data-testid="team-delivery-entry"><strong>${esc(entry.itemId)}</strong><span class="fine">${esc(entry.state)}${blocked ? ` · ${esc(blocked)}` : ""}</span><span class="fine">Owns ${esc(owns)}</span>${lead || handler ? `<span class="fine">${lead ? `Lead ${esc(lead)}` : ""}${lead && handler ? " · " : ""}${handler ? `Handler ${esc(handler)}` : ""}</span>` : ""}${activity ? `<span class="fine">${esc(activity)}</span>` : ""}<span class="fine">${esc(tokenSnapshot(entry.tokens))}</span>${ready}</div>`;
  }).join("");
  return `<article class="task-card task-detail-card team-delivery-panel" data-testid="team-delivery-panel"><header><h3>Delivery</h3><span class="fine">Limit ${esc(queue.concurrencyLimit ?? 1)}</span></header><div class="team-delivery-rows">${rows}</div></article>`;
}
