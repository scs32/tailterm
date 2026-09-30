package api

import "fmt"

// Backlog steward (docs/backlog-steward.md): one persistent agent per
// project whose context is the backlog. It takes intake, researches it and
// drafts items the database handler files; it proposes batches, queue order,
// scopes and triage outcomes as owner decisions and keeps a backlog summary
// that rotation hands to a fresh session. It holds no per-item records role.
const AgentRoleBacklogSteward = "backlog_steward"

const (
	// StewardRefusedActive: the project already has a steward. Starting,
	// running, done, retired and exited stewards all hold the slot; only a
	// close or a committed rotation frees it.
	StewardRefusedActive = "steward_active"
	// StewardRefusedWrite: the steward files through the database handler.
	StewardRefusedWrite = "steward_write"
)

// StewardRefusal is a 409 with a named reason. A refused call changes nothing.
type StewardRefusal struct {
	Code   string
	Detail string
}

func (e *StewardRefusal) Error() string {
	return fmt.Sprintf("backlog steward refused (%s): %s", e.Code, e.Detail)
}

func (e *StewardRefusal) Unwrap() error { return ErrConflict }

// PersistentAgentRole reports the project roles that are admitted without a
// parent or work item under a stable agent ID, and restart by expected run.
func PersistentAgentRole(role string) bool {
	return role == AgentRoleDatabaseHandler || role == AgentRoleDeployment || role == AgentRoleBacklogSteward
}
