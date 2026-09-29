package api

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Runtime prompt kinds (docs/runtime-prompts.md). A kind names a modal prompt
// the agent's own runtime draws in its terminal, such as Codex's rate-limit
// model menu or a Claude permission dialog. Screen text never leaves the
// host: a report carries only the kind, its fixed label and a hash.
const (
	RuntimePromptCodexRateLimit      = "codex_rate_limit_switch"
	RuntimePromptCodexModelMigration = "codex_model_migration"
	RuntimePromptCodexUsageLimit     = "codex_usage_limit"
	RuntimePromptCodexTrust          = "codex_trust"
	RuntimePromptClaudePermission    = "claude_permission"
	RuntimePromptClaudeSelection     = "claude_selection"
	RuntimePromptClaudeTrust         = "claude_trust"
	RuntimePromptUnknown             = "unknown"
)

// Policy actions. Only the keep/use actions answer a prompt, and each selects
// the option that leaves the agent's model and permissions as they are.
const (
	RuntimePromptEscalate             = "escalate"
	RuntimePromptReport               = "report"
	RuntimePromptKeepCurrentNeverShow = "keep_current_never_show"
	RuntimePromptKeepCurrent          = "keep_current"
	RuntimePromptUseExisting          = "use_existing"
)

// Outcomes of one observed prompt.
const (
	RuntimePromptConfirmed = "confirmed"
	RuntimePromptFailed    = "failed"
	RuntimePromptAmbiguous = "ambiguous"
	RuntimePromptEscalated = "escalated"
	RuntimePromptReported  = "reported"
	RuntimePromptSkipped   = "skipped"
)

// RuntimePromptKind describes one kind: its runtime, display label, the
// actions a policy may choose and the default.
type RuntimePromptKind struct {
	Kind          string   `json:"kind"`
	Runtime       string   `json:"runtime"`
	Label         string   `json:"label"`
	Actions       []string `json:"actions"`
	DefaultAction string   `json:"defaultAction"`
}

// RuntimePromptKinds is the fixed kind table. No action can approve a
// permission, trust a folder, switch models or request a limit.
var RuntimePromptKinds = []RuntimePromptKind{
	{RuntimePromptCodexRateLimit, "codex", "Codex rate-limit model menu", []string{RuntimePromptKeepCurrentNeverShow, RuntimePromptKeepCurrent, RuntimePromptEscalate}, RuntimePromptKeepCurrentNeverShow},
	{RuntimePromptCodexModelMigration, "codex", "Codex model migration menu", []string{RuntimePromptUseExisting, RuntimePromptEscalate}, RuntimePromptUseExisting},
	{RuntimePromptCodexUsageLimit, "codex", "Codex usage limit prompt", []string{RuntimePromptEscalate}, RuntimePromptEscalate},
	{RuntimePromptCodexTrust, "codex", "Codex folder trust prompt", []string{RuntimePromptEscalate, RuntimePromptReport}, RuntimePromptEscalate},
	{RuntimePromptClaudePermission, "claude", "Claude permission dialog", []string{RuntimePromptEscalate}, RuntimePromptEscalate},
	{RuntimePromptClaudeSelection, "claude", "Claude selection dialog", []string{RuntimePromptEscalate}, RuntimePromptEscalate},
	{RuntimePromptClaudeTrust, "claude", "Claude folder trust dialog", []string{RuntimePromptEscalate}, RuntimePromptEscalate},
	{RuntimePromptUnknown, "", "Unrecognized runtime prompt", []string{RuntimePromptEscalate}, RuntimePromptEscalate},
}

func LookupRuntimePromptKind(kind string) (RuntimePromptKind, bool) {
	for _, k := range RuntimePromptKinds {
		if k.Kind == kind {
			return k, true
		}
	}
	return RuntimePromptKind{}, false
}

// RuntimePromptActionAllowed reports whether a policy may set kind to action.
func RuntimePromptActionAllowed(kind, action string) bool {
	k, ok := LookupRuntimePromptKind(kind)
	if !ok {
		return false
	}
	for _, a := range k.Actions {
		if a == action {
			return true
		}
	}
	return false
}

// RuntimePromptAnswers reports whether action types into the prompt.
func RuntimePromptAnswers(action string) bool {
	switch action {
	case RuntimePromptKeepCurrentNeverShow, RuntimePromptKeepCurrent, RuntimePromptUseExisting:
		return true
	}
	return false
}

// RuntimePromptEscalates reports whether an outcome needs a person.
func RuntimePromptEscalates(outcome string) bool {
	switch outcome {
	case RuntimePromptEscalated, RuntimePromptFailed, RuntimePromptAmbiguous:
		return true
	}
	return false
}

var runtimePromptFingerprint = regexp.MustCompile(`^[0-9a-f]{32}$`)

// RuntimePrompt is the prompt part of an activity report.
type RuntimePrompt struct {
	Kind        string    `json:"kind"`
	Runtime     string    `json:"runtime"`
	Label       string    `json:"label"`
	Fingerprint string    `json:"fingerprint"`
	Since       time.Time `json:"since"`
	Action      string    `json:"action"`
	Outcome     string    `json:"outcome"`
	Reason      string    `json:"reason,omitempty"`
	At          time.Time `json:"at"`
}

// Valid checks a prompt against the kind table and field bounds.
func (p RuntimePrompt) Valid() bool {
	k, ok := LookupRuntimePromptKind(p.Kind)
	if !ok || p.Label != k.Label || !RuntimePromptActionAllowed(p.Kind, p.Action) {
		return false
	}
	if p.Runtime != "codex" && p.Runtime != "claude" || (k.Runtime != "" && k.Runtime != p.Runtime) {
		return false
	}
	switch p.Outcome {
	case RuntimePromptConfirmed, RuntimePromptFailed, RuntimePromptAmbiguous, RuntimePromptEscalated, RuntimePromptReported, RuntimePromptSkipped:
	default:
		return false
	}
	if p.Kind == RuntimePromptUnknown && p.Outcome != RuntimePromptEscalated {
		return false
	}
	return runtimePromptFingerprint.MatchString(p.Fingerprint) && !p.Since.IsZero() && !p.At.IsZero() &&
		len(p.Reason) <= 240 && !strings.ContainsAny(p.Reason, "\r\n")
}

// ErrRuntimePromptOwnerOnly refuses a policy change from an agent session.
var ErrRuntimePromptOwnerOnly = errors.New("the runtime prompt policy is the owner's; agent sessions cannot change it")

// RuntimePromptPolicy maps each configurable kind to its action. Revision 0
// means the owner has not saved one and the defaults apply.
type RuntimePromptPolicy struct {
	TaskID    string            `json:"taskId"`
	Actions   map[string]string `json:"actions"`
	Revision  int64             `json:"revision"`
	UpdatedAt *time.Time        `json:"updatedAt,omitempty"`
}

// RuntimePromptPolicyRequest replaces the listed kinds' actions; kinds it
// leaves out keep their current action.
type RuntimePromptPolicyRequest struct {
	ExpectedRevision int64             `json:"expectedRevision"`
	Actions          map[string]string `json:"actions"`
	ActorAgentID     string            `json:"actorAgentId,omitempty"`
}

// DefaultRuntimePromptActions returns a fresh map of every kind's default.
func DefaultRuntimePromptActions() map[string]string {
	out := map[string]string{}
	for _, k := range RuntimePromptKinds {
		out[k.Kind] = k.DefaultAction
	}
	return out
}

func (c *Client) RuntimePromptPolicy(ctx context.Context, task string) (RuntimePromptPolicy, error) {
	var out RuntimePromptPolicy
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/runtime-prompt/policy", nil, &out)
}

func (c *Client) SetRuntimePromptPolicy(ctx context.Context, task string, req RuntimePromptPolicyRequest) (RuntimePromptPolicy, error) {
	var out RuntimePromptPolicy
	return out, c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/runtime-prompt/policy", req, &out)
}
