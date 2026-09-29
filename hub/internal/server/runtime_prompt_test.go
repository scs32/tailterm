package server

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestRuntimePromptPolicy(t *testing.T) {
	c := newClient(t)
	task := c.task("Runtime prompt policy")
	path := "/v1/tasks/" + task.ID + "/runtime-prompt/policy"
	var p api.RuntimePromptPolicy
	if code := c.do("GET", path, nil, &p); code != 200 || p.Revision != 0 || p.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptKeepCurrentNeverShow {
		t.Fatalf("defaults %d %+v", code, p)
	}
	set := api.RuntimePromptPolicyRequest{ExpectedRevision: 0, Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptKeepCurrent}}
	if code := c.do("PUT", path, set, &p); code != 200 || p.Revision != 1 || p.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptKeepCurrent {
		t.Fatalf("owner set %d %+v", code, p)
	}
	var refusal api.ErrorResponse
	if code := c.do("PUT", path, set, &refusal); code != 409 {
		t.Fatalf("stale revision %d %+v", code, refusal)
	}
	agent := api.RuntimePromptPolicyRequest{ExpectedRevision: 1, ActorAgentID: "agt_0123456789abcdef", Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptEscalate}}
	if code := c.do("PUT", path, agent, &refusal); code != 403 || refusal.Code != "owner-only" {
		t.Fatalf("agent caller %d %+v", code, refusal)
	}
	for kind, action := range map[string]string{api.RuntimePromptClaudePermission: "approve", api.RuntimePromptCodexRateLimit: "switch", api.RuntimePromptUnknown: api.RuntimePromptReport} {
		bad := api.RuntimePromptPolicyRequest{ExpectedRevision: 1, Actions: map[string]string{kind: action}}
		if code := c.do("PUT", path, bad, &refusal); code != 400 || refusal.Code != "invalid-action" {
			t.Fatalf("%s=%s: %d %+v", kind, action, code, refusal)
		}
	}
	if code := c.do("GET", path, nil, &p); code != 200 || p.Revision != 1 || p.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptKeepCurrent {
		t.Fatalf("after refusals %d %+v", code, p)
	}
	if code := c.do("GET", "/v1/tasks/tsk_0123456789abcdef/runtime-prompt/policy", nil, nil); code != 404 {
		t.Fatalf("missing project %d", code)
	}
}
