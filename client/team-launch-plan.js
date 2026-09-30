import { itemScopedAgentName, resolveTeam, teamLaunches } from "./teams.js";

// Both Add team and the local CLI resolve members here. The project owns one
// database handler; a template's database seat is never another agent launch.
export function teamLaunchPlan({
  team,
  servers,
  mainServerId,
  projectFolders,
  itemRouting,
  catalog = null,
  handler,
}) {
  if (
    !handler ||
    handler.role !== "database_handler" ||
    ["closed", "exited"].includes(handler.status)
  )
    throw new Error(
      "This project needs an available database handler. Add one in Projects → Set up database handler before launching the team.",
    );
  // Features get a plan review; bugs get a plan only (wi_ade4aa60c5d9b55e).
  // The kind comes from the exact item revision in the prepared context.
  let kind = "";
  if (itemRouting) {
    kind = itemRouting.workContextBundle?.history?.revision?.kind;
    if (kind !== "bug" && kind !== "feature")
      throw new Error(
        "The work item kind is missing; refresh its context before launch.",
      );
  }
  const resolved = catalog ? resolveTeam(team, catalog) : team;
  const isTemplateHandler = (member) =>
    member.name === "database" && member.role === "Database handler";
  const isPlanReviewer = (member) =>
    member.name === "plan-reviewer" && member.role === "Plan review";
  const dropped = (member) =>
    isTemplateHandler(member) || (kind === "bug" && isPlanReviewer(member));
  const members = team.members.filter(
    (_, index) => !dropped(resolved.members[index]),
  );
  const resolvedMembers = resolved.members.filter(
    (member) => !dropped(member),
  );
  if (
    !members.length ||
    !resolvedMembers.some((member) => member.name === team.orchestrator)
  )
    throw new Error("The team has no non-database orchestrator.");
  const plan = teamLaunches(
    { ...team, members },
    servers,
    mainServerId,
    projectFolders,
    itemRouting,
    catalog,
  );
  // Legacy example teams carry reasoning on each member; normalizeTeam's
  // legacy shape predates that field. Keep the template's exact choice here.
  for (const entry of plan) {
    const original = resolvedMembers.find(
      (member) =>
        entry.fields.name ===
        (itemRouting
          ? itemScopedAgentName(member.name, itemRouting.workItemId)
          : member.name),
    );
    if (original?.reasoning) entry.fields.reasoning = original.reasoning;
  }
  return plan;
}
