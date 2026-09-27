import { ownerWaitSummary } from "./owner-obligations.js";
import { activityLabel, tokenSnapshot } from "./activity-format.js";
// Read-only project delivery panel with immutable release evidence.
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

export function renderTeamDelivery(queue, agents = [], ownerRequests = [], taskId = "") {
  if (!queue || !Array.isArray(queue.entries) || queue.entries.length === 0) return "";
  const byId = new Map(agents.map((agent) => [agent.id, agent]));
  const rows = queue.entries.map((entry) => {
    const lead = agents.find((agent) => agent.itemLead && agent.workItem?.itemId === entry.itemId)?.name || (entry.state === "launching" ? entry.launch?.members?.[0]?.fields?.name : "") || "";
    const handlerName = byId.get(entry.handlerId)?.name || entry.handlerId || "";
    const handler = handlerName ? `${handlerName}${entry.handlerLeaseGeneration ? ` · lease ${entry.handlerLeaseGeneration}` : ""}` : "";
    const owns = entry.ownership?.length ? entry.ownership.join(", ") : "Unscoped · conflicts with all work";
    const blocked = [entry.blockedBy?.length ? `Waiting for ${entry.blockedBy.join(", ")}` : "", entry.blockReason || ""].filter(Boolean).join(" · ");
    const worktree = entry.integration?.worktree ? ` · ${esc(entry.integration.worktree)}` : "";
    const ready = entry.integration && entry.state === "finished" && !entry.release ? `<div class="fine" data-testid="team-ready-to-integrate"><strong>Ready to integrate</strong> · ${esc(entry.integration.repository)} · base ${esc(entry.integration.baseCommit)}${worktree} · ${esc(entry.integration.branch)} @ ${esc(entry.integration.commit)} · ${esc(entry.integration.evidence)}</div>` : entry.acceptance ? `<div class="fine">Accepted ${esc(entry.acceptance.branch)} @ ${esc(entry.acceptance.commit)}${entry.state === "finished" ? "" : " · waiting for team cleanup"}</div>` : "";
    const release = entry.release;
    const releaseText = release ? `<span class="fine" data-testid="release-summary" style="overflow-wrap:anywhere">Accepted → verified${["merged","released","rolled_back"].includes(release.state) || release.receipt ? " → merged" : ""}${release.state === "released" ? " → released" : ""} · ${esc(release.state)} · job ${esc(release.id)} · verification ${esc(release.verificationDigest)}${release.integratedCommit ? " · commit " + esc(release.integratedCommit) : ""}${release.receipt?.targets?.map(t => " · " + esc(t.target) + ": " + esc(t.outcome) + (t.rollback ? " · rollback " + esc(t.rollback) : "") + " · " + esc(t.release) + (t.deployment ? " · " + esc(t.deployment) : "")).join("") || ""}</span>` : "";
    const review = entry.reviews;
    const reviewText = review?.history === "recorded" ? `Reviews ${review.rounds.length}/2 · Follow-ups ${review.followUps.length}${review.disposition ? ` · ${review.disposition.kind}` : ""}` : "Reviews unknown · Follow-ups unknown";
    const followUps = review?.followUps?.length ? `<span class="fine" style="overflow-wrap:anywhere">${review.followUps.map((f) => `${esc(f.itemId)}: ${esc(f.finding.title)}`).join(" · ")}</span>` : "";
    const verification = entry.verification;
    const notableChecks = verification?.checks?.filter(c => c.status !== "pass" || c.knownFailure || c.nowPassing) || [];
    const verificationText = verification ? `<span class="fine" style="overflow-wrap:anywhere" data-testid="verification-summary">Verification ${esc(verification.state)}${notableChecks.length ? " · " + notableChecks.map(c => `${esc(c.id)}: ${esc(c.status)}${c.knownFailure ? " · known failure" : ""}${c.nowPassing ? " · now passing; remove from known failures" : ""}`).join("; ") : ""}</span>` : "";
    const activity = entry.activities?.map((member) => `${member.name}: ${activityLabel(member.activity)}`).join(" · ") || "";
    return `<div class="team-delivery-row" data-testid="team-delivery-entry"><strong>${esc(entry.itemId)}</strong><span class="fine">${esc(entry.state)}${blocked ? ` · ${esc(blocked)}` : ""}</span><span class="fine">Owns ${esc(owns)}</span>${lead || handler ? `<span class="fine">${lead ? `Lead ${esc(lead)}` : ""}${lead && handler ? " · " : ""}${handler ? `Handler ${esc(handler)}` : ""}</span>` : ""}${activity ? `<span class="fine">${esc(activity)}</span>` : ""}<span class="fine">${esc(tokenSnapshot(entry.tokens))}</span><span class="fine" data-testid="review-convergence-summary">${esc(reviewText)}</span>${ownerWaitSummary(ownerRequests, taskId || entry.taskId, entry.itemId)}${followUps}${verificationText}${ready}${releaseText}</div>`;
  }).join("");
  return `<article class="task-card task-detail-card team-delivery-panel" data-testid="team-delivery-panel"><header><h3>Delivery</h3><span class="fine">Limit ${esc(queue.concurrencyLimit ?? 1)}</span></header><div class="team-delivery-rows">${rows}</div></article>`;
}
