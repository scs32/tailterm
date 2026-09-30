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

// Steward rotation store tests (wi_5b4b94dbc9a11e8b, order #14942): an
// isolated SQLite hub per test and synthetic agents only.

type stewardRotationFixture struct {
	s      *Store
	task   api.Task
	old    api.Agent
	worker api.Agent
}

func newStewardRotationFixture(t *testing.T, summary bool) *stewardRotationFixture {
	t.Helper()
	s, task := stewardStore(t)
	f := &stewardRotationFixture{s: s, task: task, old: liveSteward(t, s, task.ID, "backlog-steward")}
	var err error
	if f.worker, err = s.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, stewardBy); err != nil {
		t.Fatal(err)
	}
	if summary {
		if _, err = s.SaveBacklogSummary(context.Background(), task.ID, api.SaveBacklogSummaryRequest{Body: "## Themes\nsynthetic", RequestID: "summary-1", AgentID: f.old.ID, RunID: f.old.RunID}, stewardBy); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *stewardRotationFixture) prepare(key, successor string) (api.StewardRotation, error) {
	return f.s.StewardRotationAction(context.Background(), f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: key, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: successor, SuccessorName: api.StewardSuccessorName(f.old.Name), Reason: api.StewardRotationReasonManual, Trigger: api.StewardRotationTriggerOwner}, stewardBy)
}

func (f *stewardRotationFixture) act(op, key, rotation string) (api.StewardRotation, error) {
	return f.s.StewardRotationAction(context.Background(), f.task.ID, api.StewardRotationRequest{Operation: op, RequestID: key, RotationID: rotation}, stewardBy)
}

// admit registers the prepared successor on the old steward's host, runtime
// and directory; online also marks it live.
func (f *stewardRotationFixture) admit(t *testing.T, id string, online bool) api.Agent {
	t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, stewardRequest(id, api.StewardSuccessorName(f.old.Name)), stewardBy)
	if err != nil {
		t.Fatalf("admit successor: %v", err)
	}
	if online {
		if _, err = f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (f *stewardRotationFixture) holders(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status<>'closed' AND steward_pending=0`, f.task.ID, api.AgentRoleBacklogSteward).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *stewardRotationFixture) activity(t *testing.T, a api.Agent, state, tool string, tokens int64) {
	t.Helper()
	now := time.Now().UTC()
	activity := api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now, PendingTool: tool, Tokens: api.TokenTotals{Total: tokens}}
	if tool != "" {
		activity.PendingSince = now
	}
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

func stewardCode(err error) string {
	var refusal *api.StewardRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func TestStewardRotationPrepareRefusals(t *testing.T) {
	bare := newStewardRotationFixture(t, false)
	if _, err := bare.prepare("no-summary", api.NewID("agt")); stewardCode(err) != api.StewardRefusedSummaryMissing {
		t.Fatalf("without a summary: %v", err)
	}
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	f.activity(t, f.old, "working", "", 0)
	if _, err := f.prepare("busy", api.NewID("agt")); stewardCode(err) != api.StewardRefusedWorking {
		t.Fatalf("busy steward: %v", err)
	}
	f.activity(t, f.old, "idle", "Bash", 0)
	if _, err := f.prepare("pending-tool", api.NewID("agt")); stewardCode(err) != api.StewardRefusedPendingTool {
		t.Fatalf("pending tool: %v", err)
	}
	// Activity reports only change on a new state.
	f.activity(t, f.old, "working", "", 0)
	f.activity(t, f.old, "idle", "", 0)
	bad := api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: "wrong-name", OldAgentID: f.old.ID, OldRunID: f.old.RunID, SuccessorAgentID: api.NewID("agt"),
		SuccessorName: "someone-else", Reason: api.StewardRotationReasonManual, Trigger: api.StewardRotationTriggerOwner}
	if _, err := f.s.StewardRotationAction(ctx, f.task.ID, bad, stewardBy); stewardCode(err) != api.StewardRefusedNameTaken {
		t.Fatalf("wrong successor name: %v", err)
	}
	bad.RequestID, bad.SuccessorName, bad.OldRunID = "stale-run", api.StewardSuccessorName(f.old.Name), api.NewID("run")
	if _, err := f.s.StewardRotationAction(ctx, f.task.ID, bad, stewardBy); stewardCode(err) != api.StewardRefusedNotSteward {
		t.Fatalf("stale old run: %v", err)
	}
	for _, op := range []string{api.StewardRotationPrepare, api.StewardRotationCommit, api.StewardRotationAbort} {
		if _, err := f.s.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: op, RequestID: "agent-" + op, ActorAgentID: f.worker.ID}, stewardBy); stewardCode(err) != api.StewardRefusedAgentCaller {
			t.Fatalf("agent caller %s: %v", op, err)
		}
	}
	r, err := f.prepare("first", api.NewID("agt"))
	if err != nil || r.State != api.StewardRotationPrepared || r.SummaryRevision != 1 || r.SuccessorName != "backlog-steward-r2" {
		t.Fatalf("prepare: %+v %v", r, err)
	}
	if replay, err := f.prepare("first", r.SuccessorAgentID); err != nil || replay.ID != r.ID {
		t.Fatalf("prepare replay: %+v %v", replay, err)
	}
	if _, err = f.prepare("second", api.NewID("agt")); stewardCode(err) != api.StewardRefusedRotationOpen {
		t.Fatalf("second prepare: %v", err)
	}
	// While prepared, any steward admission but the named successor is refused.
	if _, err = f.s.AddAgent(ctx, f.task.ID, stewardRequest(api.NewID("agt"), "intruder"), stewardBy); stewardCode(err) != api.StewardRefusedActive {
		t.Fatalf("other steward while prepared: %v", err)
	}
	if f.holders(t) != 1 {
		t.Fatal("a refused prepare changed the holder")
	}
}

func TestStewardRotationCommitMovesObligationsAndHandsOffSummary(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	byRole, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake before the rotation")
	if err != nil {
		t.Fatal(err)
	}
	env := api.Envelope{Kind: api.EnvelopeKindQuestion, To: f.old.Name, Subject: "Which batch does this belong to", Body: api.EnvelopeBody{Question: "Which batch?"}}
	byName, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: f.old.ID, AgentID: f.worker.ID, RunID: f.worker.RunID}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	ask, err := f.s.CreateDecision(ctx, f.task.ID, api.CreateDecisionRequest{AgentID: f.old.ID, RequestID: "steward-ask", DecisionRequest: api.DecisionRequest{
		Question: "Should the two scroll items ship together?", RecommendedOptionID: "yes", RecommendationReason: "Same file.",
		Options: []api.DecisionOption{{ID: "yes", Label: "Yes", Description: "One team."}, {ID: "no", Label: "No", Description: "Apart."}}}}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	// Launch failure: no successor is registered, so commit refuses and the
	// old steward keeps the role and its obligations.
	if _, err = f.act(api.StewardRotationCommit, "commit-early", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit without a successor: %v", err)
	}
	successor := f.admit(t, r.SuccessorAgentID, false)
	status, err := f.s.BacklogStewardStatus(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != f.old.ID || status.PendingSuccessorID != successor.ID || f.holders(t) != 1 {
		t.Fatalf("status while prepared: %+v %v", status, err)
	}
	during, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake while the successor is pending")
	if err != nil || during.To != f.old.ID {
		t.Fatalf("role:backlog_steward while prepared: %+v %v", during, err)
	}
	if _, err = f.s.WorkItemTriage(ctx, f.task.ID, 0, successor.ID, successor.RunID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("pending successor triage: %v", err)
	}
	if _, err = f.act(api.StewardRotationCommit, "commit-offline", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit with an offline successor: %v", err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), successor.ID); err != nil {
		t.Fatal(err)
	}
	committed, err := f.act(api.StewardRotationCommit, "commit", r.ID)
	if err != nil || committed.State != api.StewardRotationCommitted || committed.Receipt == nil || committed.Handoff == nil {
		t.Fatalf("commit: %+v %v", committed, err)
	}
	if again, err := f.act(api.StewardRotationCommit, "commit", r.ID); err != nil || again.Receipt == nil || again.Receipt.NoticeSeq != committed.Receipt.NoticeSeq {
		t.Fatalf("commit replay: %+v %v", again, err)
	}
	old, _ := f.s.GetAgent(ctx, f.old.ID)
	if old.Status != api.AgentClosed || f.holders(t) != 1 {
		t.Fatalf("after commit: old %s, holders %d", old.Status, f.holders(t))
	}
	status, err = f.s.BacklogStewardStatus(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != successor.ID || status.PendingSuccessorID != "" {
		t.Fatalf("status after commit: %+v %v", status, err)
	}
	h := committed.Handoff
	if h.SummaryRevision != 1 || h.SummaryDigest != summaryDigest("## Themes\nsynthetic") || !h.SummaryFromOldRun || len(h.OpenDecisions) != 1 || h.OpenDecisions[0].MessageSeq != ask.Seq {
		t.Fatalf("handoff: %+v", h)
	}
	moved := map[int64]bool{}
	for _, p := range h.Reissued {
		moved[p.OldMessageSeq] = true
	}
	if len(h.Reissued) != 3 || !moved[byRole.Seq] || !moved[byName.Seq] || !moved[during.Seq] {
		t.Fatalf("reissued: %+v", h.Reissued)
	}
	open, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: successor.ID, OpenOnly: true}, f.s.now())
	reassigned := 0
	for _, o := range open {
		if strings.HasPrefix(o.Subject, "Reassigned to "+successor.Name) {
			reassigned++
		}
	}
	if err != nil || reassigned != 3 || len(open) != 4 { // three moved plus the handoff notice
		t.Fatalf("successor obligations: %+v %v", open, err)
	}
	if left, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.old.ID, OpenOnly: true}, f.s.now()); len(left) != 0 {
		t.Fatalf("obligations left on the old steward: %+v", left)
	}
	msgs, err := f.s.ListMessages(ctx, f.task.ID, 0, successor.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range msgs {
		if m.Seq == committed.Receipt.NoticeSeq && m.To == successor.ID && strings.Contains(m.Text, r.ID) && strings.Contains(m.Text, "summary revision 1") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("handoff notice to the successor: %d", notices)
	}
	if after, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake after the rotation"); err != nil || after.To != successor.ID {
		t.Fatalf("role:backlog_steward after commit: %+v %v", after, err)
	}
}

func TestStewardRotationLaunchFailureAbortAndRetry(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	intake, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake before the failed launch")
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare("prepare-1", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	// The successor registered but never came online.
	successor := f.admit(t, r.SuccessorAgentID, false)
	if _, err = f.act(api.StewardRotationCommit, "commit-1", r.ID); stewardCode(err) != api.StewardRefusedSuccessor {
		t.Fatalf("commit with a successor that never came online: %v", err)
	}
	if got, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); got.State != api.StewardRotationPrepared {
		t.Fatalf("rotation after refused commit: %+v", got)
	}
	if m, err := stewardRequestMessage(f.s, f.task.ID, f.worker, "Intake after the failed launch"); err != nil || m.To != f.old.ID {
		t.Fatalf("old steward lost the role: %+v %v", m, err)
	}
	if open, _ := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.old.ID, OpenOnly: true}, f.s.now()); len(open) != 2 || open[0].MessageSeq != intake.Seq {
		t.Fatalf("old steward's obligations changed: %+v", open)
	}
	aborted, err := f.act(api.StewardRotationAbort, "abort-1", r.ID)
	if err != nil || aborted.State != api.StewardRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	if a, _ := f.s.GetAgent(ctx, successor.ID); a.Status != api.AgentClosed {
		t.Fatalf("pending successor after abort: %+v", a)
	}
	if a, _ := f.s.GetAgent(ctx, f.old.ID); a.Status != api.AgentRunning || f.holders(t) != 1 {
		t.Fatalf("old steward after abort: %+v", a)
	}
	if _, err = f.act(api.StewardRotationCommit, "commit-aborted", r.ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("commit after abort: %v", err)
	}
	// A new rotation with a new successor ID succeeds.
	r2, err := f.prepare("prepare-2", api.NewID("agt"))
	if err != nil || r2.ID == r.ID {
		t.Fatalf("second prepare: %+v %v", r2, err)
	}
	f.admit(t, r2.SuccessorAgentID, true)
	if done, err := f.act(api.StewardRotationCommit, "commit-2", r2.ID); err != nil || done.State != api.StewardRotationCommitted || f.holders(t) != 1 {
		t.Fatalf("second rotation: %+v %v", done, err)
	}
}

func TestStewardRotationBusyCommitStaysPrepared(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	f.admit(t, r.SuccessorAgentID, true)
	f.activity(t, f.old, "working", "", 0)
	if _, err = f.act(api.StewardRotationCommit, "commit-busy", r.ID); stewardCode(err) != api.StewardRefusedWorking {
		t.Fatalf("commit while working: %v", err)
	}
	if got, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); got.State != api.StewardRotationPrepared {
		t.Fatalf("rotation after busy commit: %+v", got)
	}
	f.activity(t, f.old, "idle", "", 0)
	if done, err := f.act(api.StewardRotationCommit, "commit-idle", r.ID); err != nil || done.State != api.StewardRotationCommitted {
		t.Fatalf("commit when idle: %+v %v", done, err)
	}
}

// A pause closes the old steward mid-rotation; the prepared record then holds
// nothing open, and the next steward and rotation proceed.
func TestStewardRotationStaleAfterOldStewardCloses(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	r, err := f.prepare("prepare", api.NewID("agt"))
	if err != nil {
		t.Fatal(err)
	}
	f.admit(t, r.SuccessorAgentID, false)
	setAgentStatus(t, f.s, f.old.ID, api.AgentClosed)
	setAgentStatus(t, f.s, r.SuccessorAgentID, api.AgentClosed)
	fresh := liveSteward(t, f.s, f.task.ID, "backlog-steward")
	f.old = fresh
	r2, err := f.prepare("prepare-after-close", api.NewID("agt"))
	if err != nil {
		t.Fatalf("prepare after the old steward closed: %v", err)
	}
	if stale, _ := f.s.GetStewardRotation(ctx, f.task.ID, r.ID); stale.State != api.StewardRotationAborted || r2.State != api.StewardRotationPrepared {
		t.Fatalf("stale rotation: %+v", stale)
	}
}

func TestStewardRotationPolicyDefaultsRevisionAndOwnerOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "existing project"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	// A hub that predates steward rotation: the first migration turns it off
	// for projects that already exist.
	if _, err = s.db.Exec(`DROP TABLE steward_rotation_policy`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.StewardRotationPolicy(ctx, existing.ID)
	if err != nil || p.Enabled || p.Revision != 1 || p.MaxTotalTokens != 300_000_000 || !p.OnTemplateChange {
		t.Fatalf("existing project must be opt-in: %+v %v", p, err)
	}
	fresh, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "new project"}, stewardBy)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.StewardRotationPolicy(ctx, fresh.ID)
	if err != nil || !p.Enabled || p.Revision != 0 || p.MaxTotalTokens != api.DefaultHandlerRotationMaxTotalTokens || !p.OnTemplateChange {
		t.Fatalf("new project default: %+v %v", p, err)
	}
	set := api.StewardRotationPolicyRequest{ExpectedRevision: 0, Enabled: true, MaxTotalTokens: 5000, OnTemplateChange: false}
	agentSet := set
	agentSet.ActorAgentID = api.NewID("agt")
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, agentSet, stewardBy); stewardCode(err) != api.StewardRefusedAgentCaller {
		t.Fatalf("agent caller: %v", err)
	}
	stale := set
	stale.ExpectedRevision = 4
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, stale, stewardBy); stewardCode(err) != api.StewardRefusedStalePolicy {
		t.Fatalf("stale revision: %v", err)
	}
	saved, err := s.SetStewardRotationPolicy(ctx, fresh.ID, set, stewardBy)
	if err != nil || saved.Revision != 1 || saved.MaxTotalTokens != 5000 || saved.OnTemplateChange || !saved.Enabled {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	if _, err = s.SetStewardRotationPolicy(ctx, fresh.ID, set, stewardBy); stewardCode(err) != api.StewardRefusedStalePolicy {
		t.Fatalf("reused revision: %v", err)
	}
}

func TestStewardRotationDueListReasons(t *testing.T) {
	f := newStewardRotationFixture(t, true)
	ctx := context.Background()
	current := strings.Repeat("c", 64)
	if _, err := f.s.db.Exec(`INSERT INTO steward_runs(task_id,agent_id,run_id,template_digest,created_at) VALUES(?,?,?,?,?)`, f.task.ID, f.old.ID, f.old.RunID, current, ts(f.s.now())); err != nil {
		t.Fatal(err)
	}
	due := func(host, digest string) []api.StewardRotationDue {
		t.Helper()
		list, err := f.s.StewardRotationsDue(ctx, host, digest)
		if err != nil {
			t.Fatal(err)
		}
		return list.Entries
	}
	got := due("mini", current)
	if len(got) != 1 || len(got[0].DueReasons) != 0 || !got[0].Idle || !got[0].DigestMatches || got[0].SummaryRevision != 1 || got[0].Agent.ID != f.old.ID {
		t.Fatalf("current steward: %+v", got)
	}
	if got = due("mini", strings.Repeat("d", 64)); len(got) != 1 || len(got[0].DueReasons) != 1 || got[0].DueReasons[0] != api.StewardRotationReasonTemplate {
		t.Fatalf("template change: %+v", got)
	}
	f.activity(t, f.old, "working", "", 0)
	f.activity(t, f.old, "idle", "", api.DefaultHandlerRotationMaxTotalTokens)
	if got = due("mini", current); len(got) != 1 || len(got[0].DueReasons) != 1 || got[0].DueReasons[0] != api.StewardRotationReasonTokens || got[0].TotalTokens != api.DefaultHandlerRotationMaxTotalTokens {
		t.Fatalf("token limit: %+v", got)
	}
	f.activity(t, f.old, "working", "", api.DefaultHandlerRotationMaxTotalTokens)
	if got = due("mini", current); len(got) != 1 || got[0].Idle || got[0].ActivityState != "working" {
		t.Fatalf("busy steward reported idle: %+v", got)
	}
	if got = due("other-host", current); len(got) != 0 {
		t.Fatalf("another host: %+v", got)
	}
	p, err := f.s.StewardRotationPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SetStewardRotationPolicy(ctx, f.task.ID, api.StewardRotationPolicyRequest{ExpectedRevision: p.Revision, Enabled: false, MaxTotalTokens: p.MaxTotalTokens, OnTemplateChange: true}, stewardBy); err != nil {
		t.Fatal(err)
	}
	if got = due("mini", current); len(got) != 0 {
		t.Fatalf("disabled policy: %+v", got)
	}
}
