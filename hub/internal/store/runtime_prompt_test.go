package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func runtimePromptFixture(kind, action, outcome, fingerprint string, since time.Time) *api.RuntimePrompt {
	k, _ := api.LookupRuntimePromptKind(kind)
	runtime := k.Runtime
	if runtime == "" {
		runtime = "codex"
	}
	return &api.RuntimePrompt{Kind: kind, Runtime: runtime, Label: k.Label, Fingerprint: fingerprint, Since: since, Action: action, Outcome: outcome, At: since}
}

func newRuntimePromptStore(t *testing.T) (*Store, api.Task) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Runtime prompt fixture"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return s, task
}

const (
	promptPrintA = "0123456789abcdef0123456789abcdef"
	promptPrintB = "fedcba9876543210fedcba9876543210"
)

func TestRuntimePromptActivity(t *testing.T) {
	s, task := newRuntimePromptStore(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "mini", Session: "fake", Runtime: "codex", Cwd: t.TempDir()}, by)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
	n := 0
	report := func(activity api.AgentActivity) error {
		n++
		_, err := s.ReportActivity(ctx, task.ID, a.ID, api.ActivityReport{RequestID: "prompt-" + strings.Repeat("x", n), RunID: a.RunID, Activity: activity})
		return err
	}
	valid := runtimePromptFixture(api.RuntimePromptCodexRateLimit, api.RuntimePromptKeepCurrentNeverShow, api.RuntimePromptConfirmed, promptPrintA, now)
	if err := report(api.AgentActivity{State: "runtime_prompt", ObservedAt: now, Prompt: valid}); err != nil {
		t.Fatalf("valid prompt refused: %v", err)
	}
	got, err := s.GetAgent(ctx, a.ID)
	if err != nil || got.Activity == nil || got.Activity.State != "runtime_prompt" || got.Activity.Prompt == nil || got.Activity.Prompt.Fingerprint != promptPrintA {
		t.Fatalf("snapshot %+v %v", got.Activity, err)
	}
	mutate := func(f func(*api.RuntimePrompt)) *api.RuntimePrompt {
		p := *valid
		f(&p)
		return &p
	}
	for name, activity := range map[string]api.AgentActivity{
		"unknown kind":          {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Kind = "codex_other" })},
		"unknown action":        {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Action = "switch" })},
		"disallowed action":     {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Action = api.RuntimePromptUseExisting })},
		"unknown outcome":       {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Outcome = "approved" })},
		"short fingerprint":     {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Fingerprint = "0123abcd" })},
		"uppercase fingerprint": {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Fingerprint = strings.ToUpper(promptPrintA) })},
		"long reason":           {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Reason = strings.Repeat("r", 241) })},
		"newline reason":        {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Reason = "one\ntwo" })},
		"free label":            {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Label = "Approaching rate limits" })},
		"wrong runtime":         {State: "runtime_prompt", ObservedAt: now, Prompt: mutate(func(p *api.RuntimePrompt) { p.Runtime = "claude" })},
		"unknown not escalated": {State: "runtime_prompt", ObservedAt: now, Prompt: runtimePromptFixture(api.RuntimePromptUnknown, api.RuntimePromptEscalate, api.RuntimePromptReported, promptPrintA, now)},
		"prompt on idle":        {State: "idle", ObservedAt: now, Prompt: valid},
		"state without prompt":  {State: "runtime_prompt", ObservedAt: now},
	} {
		if err := report(activity); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: got %v, want invalid", name, err)
		}
	}
}

func TestRuntimePromptPolicy(t *testing.T) {
	s, task := newRuntimePromptStore(t)
	ctx := context.Background()
	p, err := s.RuntimePromptPolicy(ctx, task.ID)
	if err != nil || p.Revision != 0 || p.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptKeepCurrentNeverShow ||
		p.Actions[api.RuntimePromptCodexModelMigration] != api.RuntimePromptUseExisting || p.Actions[api.RuntimePromptCodexTrust] != api.RuntimePromptEscalate ||
		p.Actions[api.RuntimePromptClaudePermission] != api.RuntimePromptEscalate || p.Actions[api.RuntimePromptUnknown] != api.RuntimePromptEscalate {
		t.Fatalf("defaults %+v %v", p, err)
	}
	set, err := s.SetRuntimePromptPolicy(ctx, task.ID, api.RuntimePromptPolicyRequest{ExpectedRevision: 0, Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptEscalate}})
	if err != nil || set.Revision != 1 || set.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptEscalate || set.Actions[api.RuntimePromptCodexModelMigration] != api.RuntimePromptUseExisting {
		t.Fatalf("set %+v %v", set, err)
	}
	if _, err := s.SetRuntimePromptPolicy(ctx, task.ID, api.RuntimePromptPolicyRequest{ExpectedRevision: 0, Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptKeepCurrent}}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := s.SetRuntimePromptPolicy(ctx, task.ID, api.RuntimePromptPolicyRequest{ExpectedRevision: 1, ActorAgentID: "agt_0123456789abcdef", Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptKeepCurrent}}); !errors.Is(err, api.ErrRuntimePromptOwnerOnly) {
		t.Fatalf("agent caller: %v", err)
	}
	for kind, action := range map[string]string{
		api.RuntimePromptClaudePermission: "approve",
		api.RuntimePromptCodexRateLimit:   "switch",
		api.RuntimePromptCodexTrust:       "trust",
		api.RuntimePromptCodexUsageLimit:  "request_increase",
		api.RuntimePromptUnknown:          api.RuntimePromptReport,
		"codex_other":                     api.RuntimePromptEscalate,
	} {
		if _, err := s.SetRuntimePromptPolicy(ctx, task.ID, api.RuntimePromptPolicyRequest{ExpectedRevision: 1, Actions: map[string]string{kind: action}}); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s=%s: got %v, want invalid", kind, action, err)
		}
	}
	got, err := s.RuntimePromptPolicy(ctx, task.ID)
	if err != nil || got.Revision != 1 || got.Actions[api.RuntimePromptCodexRateLimit] != api.RuntimePromptEscalate {
		t.Fatalf("refusals changed the policy: %+v %v", got, err)
	}
}

func TestRuntimePromptEscalation(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	add := func(name string) api.Agent {
		t.Helper()
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "mini", Session: name, Runtime: "codex", Cwd: t.TempDir()}, by)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`, a.ID, a.RunID, task.ID, items[0].ID, task.ID, orders[0].Seq, "digest", []byte("{}"), ts(s.now()))
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead := add("lead-one")
	member := add("member-one")
	if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[0].ID, lead.ID, lead.RunID); err != nil {
		t.Fatal(err)
	}
	// The lead hears about a member's prompt only while that member owes work.
	ask := func(to api.Agent, kind string, body api.EnvelopeBody) {
		t.Helper()
		if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: to.ID, RequestID: api.NewID("req"),
			Envelope: &api.Envelope{Kind: kind, To: to.Name, Subject: "Check the runtime prompt work", Body: body}}, by); err != nil {
			t.Fatal(err)
		}
	}
	ask(member, api.EnvelopeKindRequest, api.EnvelopeBody{Ask: "Please check"})
	now := time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
	count := func() (owner, toLead int) {
		t.Helper()
		for _, v := range []struct {
			to string
			n  *int
		}{{"", &owner}, {lead.ID, &toLead}} {
			if err := s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND from_node=? AND to_agent=?`, task.ID, api.BrokerNode, v.to).Scan(v.n); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	send := func(id, state string, prompt *api.RuntimePrompt) {
		t.Helper()
		if _, err := s.ReportActivity(ctx, task.ID, member.ID, api.ActivityReport{RequestID: id, RunID: member.RunID, Activity: api.AgentActivity{State: state, ObservedAt: now, Prompt: prompt}}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	expect := func(step string, owner, toLead int) {
		t.Helper()
		if o, l := count(); o != owner || l != toLead {
			t.Fatalf("%s: owner=%d lead=%d, want %d/%d", step, o, l, owner, toLead)
		}
	}
	permission := runtimePromptFixture(api.RuntimePromptClaudePermission, api.RuntimePromptEscalate, api.RuntimePromptEscalated, promptPrintA, now)
	permission.Runtime = "claude"
	send("first", "runtime_prompt", permission)
	expect("first escalation", 1, 1)
	var text string
	if err := s.db.QueryRow(`SELECT text FROM messages WHERE task_id=? AND from_node=? AND to_agent='' ORDER BY seq DESC LIMIT 1`, task.ID, api.BrokerNode).Scan(&text); err != nil || !strings.Contains(text, "Claude permission dialog") || !strings.Contains(text, "member-one") {
		t.Fatalf("notice text %q %v", text, err)
	}
	send("first", "runtime_prompt", permission) // replayed request ID
	send("repeat", "runtime_prompt", permission)
	expect("repeat and replay", 1, 1)
	send("cleared", "idle", nil)
	send("flap", "runtime_prompt", permission)
	expect("prompt, idle, same prompt", 1, 1)
	restarted := *permission
	restarted.Since, restarted.At = now.Add(time.Minute), now.Add(time.Minute)
	send("relay-restart", "runtime_prompt", &restarted)
	expect("relay restart", 1, 1)
	other := *permission
	other.Fingerprint = promptPrintB
	send("new-fingerprint", "runtime_prompt", &other)
	expect("new fingerprint", 2, 2)
	confirmed := runtimePromptFixture(api.RuntimePromptCodexRateLimit, api.RuntimePromptKeepCurrentNeverShow, api.RuntimePromptConfirmed, "11111111111111111111111111111111", now)
	send("confirmed", "runtime_prompt", confirmed)
	expect("confirmed answer", 2, 2)
	ambiguous := *confirmed
	ambiguous.Outcome, ambiguous.Reason = api.RuntimePromptAmbiguous, "prompt still shown after Enter"
	send("ambiguous", "runtime_prompt", &ambiguous)
	expect("ambiguous answer escalates once", 3, 3)
	failed := ambiguous
	failed.Outcome = api.RuntimePromptFailed
	send("failed-same", "runtime_prompt", &failed)
	expect("same fingerprint after ambiguity", 3, 3)
	unknown := runtimePromptFixture(api.RuntimePromptUnknown, api.RuntimePromptEscalate, api.RuntimePromptEscalated, "22222222222222222222222222222222", now)
	if _, err := s.SetRuntimePromptPolicy(ctx, task.ID, api.RuntimePromptPolicyRequest{Actions: map[string]string{api.RuntimePromptCodexRateLimit: api.RuntimePromptKeepCurrent}}); err != nil {
		t.Fatal(err)
	}
	send("unknown", "runtime_prompt", unknown)
	expect("unknown escalates under any policy", 4, 4)
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM runtime_prompt_escalations WHERE agent_id=?`, member.ID).Scan(&rows); err != nil || rows != 4 {
		t.Fatalf("escalation rows %d %v", rows, err)
	}
	// A done member still has its open request; only the owner is told.
	if _, err := s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: member.ID, RunID: member.RunID, Kind: api.EventDone}, by); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := s.db.QueryRow(`SELECT count(*) FROM obligations WHERE agent_id=? AND needs<>? AND state<>?`, member.ID, api.ObligationNeedsDelivery, api.ObligationClosed).Scan(&open); err != nil || open != 1 {
		t.Fatalf("done member's open obligations %d %v", open, err)
	}
	afterDone := *permission
	afterDone.Fingerprint = "33333333333333333333333333333333"
	send("done-member", "runtime_prompt", &afterDone)
	expect("done member", 5, 4)
	// A running member that owes nothing: a notice obliges only delivery.
	idle := add("member-two")
	if _, err := s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: idle.ID, RunID: idle.RunID, Kind: api.EventRunning}, by); err != nil {
		t.Fatal(err)
	}
	ask(idle, api.EnvelopeKindNotice, api.EnvelopeBody{Text: "Wait for your assignment"})
	if _, err := s.ReportActivity(ctx, task.ID, idle.ID, api.ActivityReport{RequestID: "no-obligation", RunID: idle.RunID, Activity: api.AgentActivity{State: "runtime_prompt", ObservedAt: now, Prompt: permission}}); err != nil {
		t.Fatal(err)
	}
	expect("member with no open work obligation", 6, 4)
	// The lead's own prompt reaches only the owner.
	if _, err := s.ReportActivity(ctx, task.ID, lead.ID, api.ActivityReport{RequestID: "lead-prompt", RunID: lead.RunID, Activity: api.AgentActivity{State: "runtime_prompt", ObservedAt: now, Prompt: permission}}); err != nil {
		t.Fatal(err)
	}
	expect("lead prompt", 7, 4)
}
