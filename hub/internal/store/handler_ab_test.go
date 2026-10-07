package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler arm tests (wi_fc1396aef8a72a06, order #14869) use an isolated
// SQLite hub per test and synthetic agents only.

var (
	armS    = api.HandlerArm{ID: "S", Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}
	armO    = api.HandlerArm{ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}
	digestP = strings.Repeat("a", 64)
	digestQ = strings.Repeat("b", 64)
)

type armFixture struct {
	s      *Store
	task   api.Task
	items  []api.WorkItem
	orders []api.Message
	by     api.Caller
	clock  time.Time
	hS, hO api.Agent
}

func newArmFixture(t *testing.T, extraItems int) *armFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &armFixture{s: s, task: task, items: items, orders: orders, by: api.Caller{Node: "fixture", User: "owner"}, clock: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return f.clock }
	for i := 0; i < extraItems; i++ {
		item, err := s.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: fmt.Sprintf("Arm work %d", i), Priority: "normal", RequestID: api.NewID("req")}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		f.items = append(f.items, item)
		f.orders = append(f.orders, contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil))
	}
	f.hS = f.handler(t, "handler-s", armS, digestP)
	f.hO = f.handler(t, "handler-o", armO, digestP)
	return f
}

// handler registers an online database handler recorded with an arm's model.
func (f *armFixture) handler(t *testing.T, name string, arm api.HandlerArm, digest string) api.Agent {
	t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: name, Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: name,
		Runtime: arm.Runtime, TemplateDigest: digest, HandlerModel: arm.Model, HandlerReasoning: arm.Reasoning}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.online(t, a)
	if a, err = f.s.GetAgent(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// online uses the real clock: scanAgent compares last_seen_at with it.
func (f *armFixture) online(t *testing.T, a api.Agent) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), a.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *armFixture) offline(t *testing.T, a api.Agent) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *armFixture) policy(t *testing.T, key string, rev int64, enabled, fallback bool, seed string) api.HandlerArmPolicy {
	t.Helper()
	p, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: key, ExpectedRevision: rev, Enabled: enabled, Fallback: fallback,
		Seed: seed, TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *armFixture) entry(t *testing.T, i int) api.TeamQueueEntry {
	t.Helper()
	q, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("add"), Operation: "add", ItemID: f.items[i].ID, OrderMessageSeq: f.orders[i].Seq, Host: "mini", Cwd: "/tmp/arm"})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func (f *armFixture) claim(q api.TeamQueueEntry, key string) (api.TeamQueueEntry, error) {
	return f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
}

// release frees the serial slot after a claim, so one fixture can claim again.
func (f *armFixture) release(t *testing.T, q api.TeamQueueEntry) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished' WHERE id=?`, q.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM team_launch_reservations WHERE entry_id=?`, q.ID); err != nil {
		t.Fatal(err)
	}
}

// seedFor finds a seed whose draw over arms picks want for the entry.
func seedFor(t *testing.T, entryID, want string, arms ...api.HandlerArm) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		seed := fmt.Sprintf("seed-%d", i)
		if arm, _ := handlerArmDraw(seed, entryID, arms); arm == want {
			return seed
		}
	}
	t.Fatalf("no seed draws %s", want)
	return ""
}

func (f *armFixture) report(t *testing.T, a api.Agent, key string, activity api.AgentActivity) {
	t.Helper()
	if activity.ObservedAt.IsZero() {
		activity.ObservedAt = f.clock
	}
	activity.LastEventAt = activity.ObservedAt
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: key, RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

func usageLimitPrompt(outcome string, at time.Time) *api.RuntimePrompt {
	return &api.RuntimePrompt{Kind: api.RuntimePromptCodexUsageLimit, Runtime: "codex", Label: "Codex usage limit prompt", Fingerprint: strings.Repeat("c", 32),
		Since: at, Action: api.RuntimePromptEscalate, Outcome: outcome, At: at}
}

func (f *armFixture) notices(t *testing.T, subject string) int {
	t.Helper()
	return countRows(t, f.s, `SELECT count(*) FROM messages WHERE task_id=? AND envelope<>'' AND json_extract(envelope,'$.subject')=?`, f.task.ID, subject)
}

const (
	limitSubject     = "A handler arm is at a provider limit"
	availableSubject = "A handler arm is available again"
)

func armRefusal(err error) string {
	var refusal *api.HandlerArmRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

func TestHandlerArmPolicySetGetRefusalsAndReplay(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	view, err := f.s.HandlerArmPolicy(ctx, f.task.ID)
	if err != nil || view.Policy.Revision != 0 || view.Policy.Enabled || view.Policy.LimitHoldMinutes != 60 || len(view.Handlers) != 3 {
		t.Fatalf("default view %+v %v", view, err)
	}
	req := api.HandlerArmPolicyRequest{RequestID: "arms-1", Enabled: true, Seed: "K", TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}}
	agent := req
	agent.ActorAgentID = f.hS.ID
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, agent); armRefusal(err) != api.HandlerArmRefusedAgentCaller {
		t.Fatalf("agent session: %v", err)
	}
	stale := req
	stale.ExpectedRevision = 3
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, stale); armRefusal(err) != api.HandlerArmRefusedStalePolicy {
		t.Fatalf("stale revision: %v", err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_policy`); n != 0 {
		t.Fatalf("refusals wrote %d policies", n)
	}
	saved, err := f.s.SetHandlerArmPolicy(ctx, f.task.ID, req)
	if err != nil || saved.Revision != 1 || !saved.Enabled || saved.Seed != "K" || saved.Fallback || saved.LimitHoldMinutes != 60 || saved.TemplateDigest != digestP ||
		!reflect.DeepEqual(saved.Arms, []api.HandlerArm{armO, armS}) {
		t.Fatalf("saved %+v %v", saved, err)
	}
	replay, err := f.s.SetHandlerArmPolicy(ctx, f.task.ID, req)
	if err != nil || replay.Revision != 1 || !reflect.DeepEqual(replay, api.HandlerArmPolicy{TaskID: saved.TaskID, Enabled: true, Seed: "K", LimitHoldMinutes: 60, TemplateDigest: digestP, Arms: saved.Arms, Revision: 1, UpdatedAt: replay.UpdatedAt}) {
		t.Fatalf("replay %+v %v", replay, err)
	}
	reused := req
	reused.Seed = "other"
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, reused); armRefusal(err) != api.HandlerArmRefusedRequestReused {
		t.Fatalf("reused request ID: %v", err)
	}
	view, err = f.s.HandlerArmPolicy(ctx, f.task.ID)
	if err != nil || view.Policy.Revision != 1 || view.Policy.Seed != "K" {
		t.Fatalf("get %+v %v", view.Policy, err)
	}
	arms := map[string]string{}
	for _, h := range view.Handlers {
		arms[h.Name] = h.Arm + "/" + fmt.Sprint(h.DigestMatches) + "/" + h.NoArmReason
	}
	if arms["handler-s"] != "S/true/" || arms["handler-o"] != "O/true/" || arms["database"] != "/false/no model recorded at spawn" {
		t.Fatalf("handlers %+v", arms)
	}
}

func TestHandlerArmPolicyValidation(t *testing.T) {
	f := newArmFixture(t, 0)
	base := func() api.HandlerArmPolicyRequest {
		return api.HandlerArmPolicyRequest{RequestID: api.NewID("req"), Seed: "K", TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}}
	}
	third := api.HandlerArm{ID: "T", Runtime: "codex", Model: "gpt-6-sol", Reasoning: "high", Weight: 1}
	cases := map[string]func(*api.HandlerArmPolicyRequest){
		"bad arm ID":       func(r *api.HandlerArmPolicyRequest) { r.Arms[0].ID = "S!" },
		"long arm ID":      func(r *api.HandlerArmPolicyRequest) { r.Arms[0].ID = strings.Repeat("x", 17) },
		"repeated ID":      func(r *api.HandlerArmPolicyRequest) { r.Arms[1].ID = "S" },
		"weight zero":      func(r *api.HandlerArmPolicyRequest) { r.Arms[0].Weight = 0 },
		"weight too big":   func(r *api.HandlerArmPolicyRequest) { r.Arms[0].Weight = 1001 },
		"repeated triple":  func(r *api.HandlerArmPolicyRequest) { r.Arms[1] = armS; r.Arms[1].ID = "S2" },
		"one arm":          func(r *api.HandlerArmPolicyRequest) { r.Arms = r.Arms[:1] },
		"one arm fallback": func(r *api.HandlerArmPolicyRequest) { r.Arms, r.Fallback = r.Arms[:1], true },
		"nine arms": func(r *api.HandlerArmPolicyRequest) {
			for i := 0; i < 7; i++ {
				arm := third
				arm.ID, arm.Model = fmt.Sprintf("T%d", i), fmt.Sprintf("model-%d", i)
				r.Arms = append(r.Arms, arm)
			}
		},
		"hold too short":         func(r *api.HandlerArmPolicyRequest) { r.LimitHoldMinutes = 4 },
		"hold too long":          func(r *api.HandlerArmPolicyRequest) { r.LimitHoldMinutes = 1441 },
		"empty seed":             func(r *api.HandlerArmPolicyRequest) { r.Seed = " " },
		"missing model":          func(r *api.HandlerArmPolicyRequest) { r.Arms[0].Model = "" },
		"bad digest":             func(r *api.HandlerArmPolicyRequest) { r.TemplateDigest = "abc" },
		"enabled without digest": func(r *api.HandlerArmPolicyRequest) { r.Enabled, r.TemplateDigest = true, "" },
	}
	for name, change := range cases {
		req := base()
		change(&req)
		if _, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, req); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_policy`); n != 0 {
		t.Fatalf("invalid policies wrote %d rows", n)
	}
	// Disabling needs no template digest.
	disabled := base()
	disabled.TemplateDigest = ""
	saved, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, disabled)
	if err != nil || saved.Revision != 1 || saved.TemplateDigest != "" {
		t.Fatalf("disabled without a digest %+v %v", saved, err)
	}
	ok := base()
	ok.ExpectedRevision = 1
	ok.LimitHoldMinutes, ok.Fallback = 5, true
	ok.Arms = append(ok.Arms, third)
	ok.Arms[0].Weight, ok.Arms[1].Weight = 1000, 1
	if _, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, ok); err != nil {
		t.Fatalf("boundary policy refused: %v", err)
	}
}

// independentDraw restates the documented formula with 128-bit integer
// division instead of math/big.
func independentDraw(seed, entryID string, arms []api.HandlerArm) (string, string) {
	sorted := sortedArms(arms)
	h := sha256.Sum256([]byte(seed + "\x00" + entryID))
	u := binary.BigEndian.Uint64(h[:8])
	var total, c uint64
	for _, a := range sorted {
		total += uint64(a.Weight)
	}
	for _, a := range sorted {
		c += uint64(a.Weight)
		if c == total {
			return a.ID, hex.EncodeToString(h[:8])
		}
		if bound, _ := bits.Div64(c, 0, total); u < bound {
			return a.ID, hex.EncodeToString(h[:8])
		}
	}
	return "", ""
}

func TestHandlerArmDrawVectorsBalanceAndOrder(t *testing.T) {
	heavyO := armO
	heavyO.Weight = 3
	for _, v := range []struct {
		seed, entry string
		arms        []api.HandlerArm
		arm, draw   string
	}{
		{"trial-2026-09", "tqe_0123456789abcdef", []api.HandlerArm{armS, armO}, "S", "8a3d2756c33be22f"},
		{"trial-2026-09", "tqe_00000000000000a1", []api.HandlerArm{armS, armO}, "O", "39cdc60379263110"},
		{"seed-k", "tqe_00000000000000b2", []api.HandlerArm{armS, heavyO}, "O", "13a2db08f8899473"},
	} {
		arm, draw := handlerArmDraw(v.seed, v.entry, v.arms)
		if arm != v.arm || draw != v.draw {
			t.Fatalf("vector %s/%s: %s %s", v.seed, v.entry, arm, draw)
		}
		if again, _ := handlerArmDraw(v.seed, v.entry, v.arms); again != arm {
			t.Fatal("draw is not reproducible")
		}
		if want, wantDraw := independentDraw(v.seed, v.entry, v.arms); want != arm || wantDraw != draw {
			t.Fatalf("formula mismatch %s %s", want, wantDraw)
		}
	}
	heavyS := armS
	heavyS.Weight = 3
	three := api.HandlerArm{ID: "T", Runtime: "codex", Model: "gpt-6-sol", Reasoning: "high", Weight: 2}
	counts := map[string]map[string]int{"even": {}, "heavy": {}}
	for i := 0; i < 1000; i++ {
		entry := fmt.Sprintf("tqe_%016x", i)
		even, _ := handlerArmDraw("balance", entry, []api.HandlerArm{armS, armO})
		heavy, _ := handlerArmDraw("balance", entry, []api.HandlerArm{heavyS, armO})
		counts["even"][even]++
		counts["heavy"][heavy]++
		for _, arms := range [][]api.HandlerArm{{armS, armO, three}, {three, armO, armS}, {armO, three, armS}} {
			a, _ := handlerArmDraw("order", entry, arms)
			b, _ := handlerArmDraw("order", entry, []api.HandlerArm{armS, armO, three})
			if a != b {
				t.Fatalf("reordered arms changed the draw for %s", entry)
			}
			if want, _ := independentDraw("order", entry, arms); want != a {
				t.Fatalf("three-arm formula mismatch for %s", entry)
			}
		}
	}
	if s := counts["even"]["S"]; s < 450 || s > 550 || counts["even"]["O"] != 1000-s {
		t.Fatalf("1:1 balance %v", counts["even"])
	}
	if s := counts["heavy"]["S"]; s < 700 || s > 800 {
		t.Fatalf("3:1 balance %v", counts["heavy"])
	}
}

func TestHandlerArmLeaseDrawnArmAndStoredAssignment(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	q := f.entry(t, 0)
	seed := seedFor(t, q.ID, "S", armS, armO)
	p := f.policy(t, "enable", 0, true, false, seed)
	claimed, err := f.claim(q, "claim-s")
	if err != nil {
		t.Fatal(err)
	}
	_, draw := handlerArmDraw(seed, q.ID, []api.HandlerArm{armS, armO})
	want := api.TeamQueueHandlerArm{LeaseGeneration: claimed.HandlerLeaseGeneration, PolicyRevision: p.Revision, Draw: draw, DrawnArm: "S", Arm: "S", SkippedLimited: []string{},
		HandlerID: f.hS.ID, HandlerRunID: f.hS.RunID, HandlerDigest: digestP, PolicyDigest: digestP, LeasedAt: ts(f.clock)}
	if claimed.HandlerID != f.hS.ID || claimed.HandlerArm == nil || !reflect.DeepEqual(*claimed.HandlerArm, want) {
		t.Fatalf("claim %+v arm %+v", claimed, claimed.HandlerArm)
	}
	got, err := f.s.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || got.HandlerArm == nil || !reflect.DeepEqual(*got.HandlerArm, want) {
		t.Fatalf("get %+v %v", got.HandlerArm, err)
	}
	list, err := f.s.ListTeamQueue(ctx, f.task.ID)
	if err != nil || list.Entries[0].HandlerArm == nil || !reflect.DeepEqual(*list.Entries[0].HandlerArm, want) {
		t.Fatalf("list %+v %v", list.Entries, err)
	}
	if replay, err := f.claim(q, "claim-s"); err != nil || !reflect.DeepEqual(replay.HandlerArm, claimed.HandlerArm) {
		t.Fatalf("replayed claim %+v %v", replay.HandlerArm, err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_assignments WHERE entry_id=?`, q.ID); n != 1 {
		t.Fatalf("assignments %d", n)
	}
}

func TestHandlerArmLeaseWaitsFallsBackAndSkipsUnmatchedRuns(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	q := f.entry(t, 0)
	seed := seedFor(t, q.ID, "S", armS, armO)
	f.policy(t, "enable", 0, true, false, seed)
	// The drawn arm has no free handler; the model-less legacy handler and
	// the other arm are free but not leased.
	f.offline(t, f.hS)
	_, err := f.claim(q, "claim-busy")
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "arm S: no free handler in the drawn arm") {
		t.Fatalf("busy arm: %v", err)
	}
	after, err := f.s.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || after.State != "queued" || after.Revision != q.Revision || after.HandlerID != "" || after.HandlerArm != nil {
		t.Fatalf("busy arm wrote %+v %v", after, err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_assignments`) + countRows(t, f.s, `SELECT count(*) FROM team_launch_reservations`); n != 0 {
		t.Fatalf("busy arm left %d rows", n)
	}
	list, err := f.s.ListTeamQueue(ctx, f.task.ID)
	if err != nil || list.Entries[0].BlockReason != "Waiting for a free handler of arm S" {
		t.Fatalf("list reason %q %v", list.Entries[0].BlockReason, err)
	}
	f.policy(t, "fallback", 1, true, true, seed)
	claimed, err := f.claim(q, "claim-fallback")
	if err != nil || claimed.HandlerID != f.hO.ID || claimed.HandlerArm.Arm != "O" || claimed.HandlerArm.DrawnArm != "S" || !claimed.HandlerArm.Fallback || claimed.HandlerArm.FallbackReason != api.HandlerArmFallbackBusy {
		t.Fatalf("fallback %+v %v", claimed.HandlerArm, err)
	}
	var fallback int
	var reason string
	if err = f.s.db.QueryRow(`SELECT fallback,fallback_reason FROM handler_arm_assignments WHERE entry_id=?`, q.ID).Scan(&fallback, &reason); err != nil || fallback != 1 || reason != "busy" {
		t.Fatalf("stored fallback %d %q %v", fallback, reason, err)
	}
}

func TestHandlerArmLeaseNoModelNeverLeasedAndNewDigestLeasable(t *testing.T) {
	f := newArmFixture(t, 1)
	q := f.entry(t, 0)
	seed := seedFor(t, q.ID, "O", armS, armO)
	f.policy(t, "enable", 0, true, false, seed)
	// Only the model-less legacy handler is free: nothing is leased.
	f.offline(t, f.hS)
	f.offline(t, f.hO)
	if _, err := f.claim(q, "claim-none"); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), api.HandlerArmWaitSuffix) {
		t.Fatalf("model-less handler leased: %v", err)
	}
	// A handler of the arm started after a template change (a rotation
	// successor) stays leasable, and its assignment records its digest.
	rotated := f.handler(t, "handler-o-r2", armO, digestQ)
	claimed, err := f.claim(q, "claim-rotated")
	if err != nil || claimed.HandlerID != rotated.ID || claimed.HandlerArm.HandlerDigest != digestQ || claimed.HandlerArm.PolicyDigest != digestP {
		t.Fatalf("rotated handler %+v %v", claimed.HandlerArm, err)
	}
}

func TestHandlerRunModelRecordedOnceAndHandlerOnly(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	var model, reasoning, digest string
	if err := f.s.db.QueryRow(`SELECT model,reasoning,template_digest FROM handler_runs WHERE run_id=?`, f.hS.RunID).Scan(&model, &reasoning, &digest); err != nil ||
		model != "claude-sonnet-5-5" || reasoning != "high" || digest != digestP {
		t.Fatalf("recorded %q %q %q %v", model, reasoning, digest, err)
	}
	// Replaying the launch of the same run cannot rewrite it.
	if _, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "handler-s", Role: api.AgentRoleDatabaseHandler, AgentID: f.hS.ID, Host: "mini", Session: "handler-s",
		Runtime: "claude", TemplateDigest: digestQ, HandlerModel: "claude-opus-5-5", HandlerReasoning: "max"}, f.by); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(`SELECT model,reasoning,template_digest FROM handler_runs WHERE run_id=?`, f.hS.RunID).Scan(&model, &reasoning, &digest); err != nil ||
		model != "claude-sonnet-5-5" || reasoning != "high" || digest != digestP {
		t.Fatalf("replay rewrote %q %q %q %v", model, reasoning, digest, err)
	}
	if _, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "mini", Session: "builder", Runtime: "claude", HandlerModel: "claude-sonnet-5-5"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("handler model on a builder: %v", err)
	}
	if _, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "mini", Session: "builder", Runtime: "claude", HandlerReasoning: "high"}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("handler reasoning on a builder: %v", err)
	}
}

func TestHandlerArmTemplateMismatchBlocksEnabling(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	other := f.handler(t, "handler-o-other", armO, digestQ)
	req := api.HandlerArmPolicyRequest{RequestID: "enable", Enabled: true, Seed: "K", TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}}
	_, err := f.s.SetHandlerArmPolicy(ctx, f.task.ID, req)
	if armRefusal(err) != api.HandlerArmRefusedTemplate || !strings.Contains(err.Error(), other.ID) || strings.Contains(err.Error(), f.hO.ID) {
		t.Fatalf("mismatch: %v", err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_policy`); n != 0 {
		t.Fatal("a refused enable wrote the policy")
	}
	// A disabled policy saves; enabling it later is refused the same way.
	disabled := req
	disabled.RequestID, disabled.Enabled = "disabled", false
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, disabled); err != nil {
		t.Fatal(err)
	}
	view, err := f.s.HandlerArmPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range view.Handlers {
		if h.AgentID == other.ID && (h.DigestMatches || h.TemplateDigest != digestQ || h.Arm != "O") {
			t.Fatalf("flag %+v", h)
		}
	}
	req.RequestID, req.ExpectedRevision = "enable-2", 1
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, req); armRefusal(err) != api.HandlerArmRefusedTemplate {
		t.Fatalf("enable after save: %v", err)
	}
	if _, err = f.s.CloseAgent(ctx, other.ID, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SetHandlerArmPolicy(ctx, f.task.ID, req); err != nil {
		t.Fatalf("closed mismatched handler still blocks: %v", err)
	}
}

func TestHandlerRotationArmChangedOnlyUnderAPolicy(t *testing.T) {
	ctx := context.Background()
	// setup gives the old primary a Claude runtime and a recorded model, and
	// saves a (disabled) arm policy when armed is set.
	setup := func(t *testing.T, armed bool, oldModel string) *rotationFixture {
		f := newRotationFixture(t)
		if _, err := f.s.db.Exec(`UPDATE agents SET runtime='claude' WHERE id=?`, f.old.ID); err != nil {
			t.Fatal(err)
		}
		f.old.Runtime = "claude"
		if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`,
			f.task.ID, f.old.ID, f.old.RunID, digestP, oldModel, "high", ts(time.Now())); err != nil {
			t.Fatal(err)
		}
		if armed {
			if _, err := f.s.SetHandlerArmPolicy(ctx, f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Seed: "K", Arms: []api.HandlerArm{armS, armO}}); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}
	// prepare names the successor; it then registers with its model, as
	// tt spawn --role database_handler --model M --reasoning R records it.
	rotate := func(t *testing.T, f *rotationFixture, key, model, reasoning string) (api.HandlerRotation, api.Agent) {
		t.Helper()
		r, err := f.prepare(t, "prepare-"+key, api.Agent{}, "")
		if err != nil {
			t.Fatal(err)
		}
		a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini",
			Session: "successor-" + key, Runtime: f.old.Runtime, Cwd: f.old.Cwd, TemplateDigest: digestP, HandlerModel: model, HandlerReasoning: reasoning}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
			t.Fatal(err)
		}
		return r, a
	}
	t.Run("no policy keeps today's rotation", func(t *testing.T) {
		f := setup(t, false, "claude-sonnet-5-5")
		r, _ := rotate(t, f, "no-policy", "claude-opus-5-5", "max")
		if committed, err := f.commit(r, "commit-no-policy"); err != nil || committed.State != api.HandlerRotationCommitted {
			t.Fatalf("rotation without a policy %+v %v", committed, err)
		}
	})
	t.Run("a run of no arm rotates freely", func(t *testing.T) {
		f := setup(t, true, "claude-haiku-4-5")
		r, _ := rotate(t, f, "no-arm", "claude-opus-5-5", "max")
		if committed, err := f.commit(r, "commit-no-arm"); err != nil || committed.State != api.HandlerRotationCommitted {
			t.Fatalf("rotation of a run in no arm %+v %v", committed, err)
		}
	})
	t.Run("a saved policy keeps an arm's run in its arm", func(t *testing.T) {
		f := setup(t, true, "claude-sonnet-5-5")
		for _, change := range []struct{ key, model, reasoning string }{{"model", "claude-opus-5-5", "high"}, {"reasoning", "claude-sonnet-5-5", "max"}, {"unrecorded", "", ""}} {
			r, a := rotate(t, f, change.key, change.model, change.reasoning)
			before := openObligations(t, f.s, f.task.ID)
			if _, err := f.commit(r, "commit-"+change.key); refusalCode(err) != api.HandlerRotationRefusedArmChanged {
				t.Fatalf("%s change: %v", change.key, err)
			}
			task, _ := f.s.GetTask(ctx, f.task.ID)
			if task.PrimaryHandlerID == a.ID || task.HandlerRevision != 1 || !reflect.DeepEqual(before, openObligations(t, f.s, f.task.ID)) {
				t.Fatalf("a refused %s commit moved something", change.key)
			}
			if _, err := f.s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort-" + change.key, RotationID: r.ID}, f.by); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.CloseAgent(ctx, a.ID, f.by); err != nil {
				t.Fatal(err)
			}
		}
		r, _ := rotate(t, f, "same", "claude-sonnet-5-5", "high")
		if committed, err := f.commit(r, "commit-same"); err != nil || committed.State != api.HandlerRotationCommitted {
			t.Fatalf("same-arm rotation %+v %v", committed, err)
		}
	})
}

func TestHandlerArmLimitCodexEpisodes(t *testing.T) {
	f := newArmFixture(t, 2)
	ctx := context.Background()
	q := f.entry(t, 0)
	seed := seedFor(t, q.ID, "O", armS, armO)
	f.policy(t, "enable", 0, true, false, seed)
	// An entry already leased to the arm keeps its lease through a limit.
	held, err := f.claim(q, "claim-held")
	if err != nil || held.HandlerID != f.hO.ID {
		t.Fatalf("held %+v %v", held, err)
	}
	f.report(t, f.hO, "prompt-1", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock)})
	if n := f.notices(t, limitSubject); n != 1 {
		t.Fatalf("limit notices %d", n)
	}
	f.report(t, f.hO, "prompt-1", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock)})
	if n := f.notices(t, limitSubject); n != 1 || countRows(t, f.s, `SELECT count(*) FROM handler_arm_limit_episodes`) != 1 {
		t.Fatalf("replayed report posted again: %d", n)
	}
	if after, err := f.s.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || after.HandlerID != f.hO.ID || after.State != "launching" {
		t.Fatalf("leased entry lost its lease %+v %v", after, err)
	}
	f.release(t, held)
	next := f.entry(t, 1)
	seed2 := seedFor(t, next.ID, "O", armS, armO)
	f.policy(t, "seed-2", 1, true, false, seed2)
	claimed, err := f.claim(next, "claim-limited")
	if err != nil || claimed.HandlerID != f.hS.ID || claimed.HandlerArm.DrawnArm != "S" || !reflect.DeepEqual(claimed.HandlerArm.SkippedLimited, []string{"O"}) {
		t.Fatalf("limited arm still drawn %+v %v", claimed.HandlerArm, err)
	}
	if replay, err := f.claim(next, "claim-limited"); err != nil || replay.HandlerID != f.hS.ID || f.notices(t, limitSubject)+f.notices(t, availableSubject) != 1 {
		t.Fatalf("replayed claim %+v %v", replay, err)
	}
	// The prompt gone clears the episode once.
	f.clock = f.clock.Add(time.Minute)
	f.report(t, f.hO, "working-1", api.AgentActivity{State: "working"})
	if n := f.notices(t, availableSubject); n != 1 {
		t.Fatalf("available notices %d", n)
	}
	var reason string
	if err = f.s.db.QueryRow(`SELECT clear_reason FROM handler_arm_limit_episodes WHERE cleared_at<>''`).Scan(&reason); err != nil || reason != api.HandlerArmClearCodexPromptGone {
		t.Fatalf("clear reason %q %v", reason, err)
	}
	// An answered prompt counts as gone.
	f.clock = f.clock.Add(time.Minute)
	f.report(t, f.hO, "prompt-2", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock)})
	f.report(t, f.hO, "prompt-2-answered", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptConfirmed, f.clock)})
	if f.notices(t, limitSubject) != 2 || f.notices(t, availableSubject) != 2 {
		t.Fatalf("answered prompt: limit=%d available=%d", f.notices(t, limitSubject), f.notices(t, availableSubject))
	}
	// A closed Codex run clears its open episode at the next claim.
	f.report(t, f.hO, "prompt-3", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock.Add(time.Second))})
	if _, err = f.s.CloseAgent(ctx, f.hO.ID, f.by); err != nil {
		t.Fatal(err)
	}
	f.release(t, claimed)
	third := f.entry(t, 2)
	f.policy(t, "seed-3", 2, true, false, seedFor(t, third.ID, "S", armS, armO))
	if _, err = f.claim(third, "claim-after-close"); err != nil {
		t.Fatal(err)
	}
	if err = f.s.db.QueryRow(`SELECT clear_reason FROM handler_arm_limit_episodes WHERE run_id=? ORDER BY rowid DESC LIMIT 1`, f.hO.RunID).Scan(&reason); err != nil || reason != api.HandlerArmClearRunClosed {
		t.Fatalf("closed run %q %v", reason, err)
	}
	if f.notices(t, availableSubject) != 3 {
		t.Fatalf("available notices %d", f.notices(t, availableSubject))
	}
}

func TestHandlerArmLimitClaudeHoldRules(t *testing.T) {
	f := newArmFixture(t, 2)
	ctx := context.Background()
	f.policy(t, "enable", 0, true, false, "K")
	limited := func() bool {
		t.Helper()
		set, err := limitedArms(ctx, f.s.db, f.task.ID, f.clock)
		if err != nil {
			t.Fatal(err)
		}
		return set["S"]
	}
	start := f.clock
	f.report(t, f.hS, "idle", api.AgentActivity{State: "idle"})
	f.report(t, f.hS, "limit-1", api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason, ObservedAt: start})
	if !limited() || f.notices(t, limitSubject) != 1 {
		t.Fatal("rate limit did not open an episode")
	}
	// (i) A working report inside the hold keeps the arm limited, and on
	// its own it never clears the limit, even after the hold.
	f.clock = start.Add(5 * time.Minute)
	f.report(t, f.hS, "working-1", api.AgentActivity{State: "working"})
	if !limited() {
		t.Fatal("working report cleared the limit")
	}
	f.clock = start.Add(61 * time.Minute)
	if !limited() {
		t.Fatal("working report after the signal cleared the limit at hold expiry")
	}
	// A completed clean turn after the hold clears it.
	f.report(t, f.hS, "clean-1", api.AgentActivity{State: "idle"})
	if limited() || f.notices(t, availableSubject) != 1 {
		t.Fatalf("clean turn after the hold: available=%d", f.notices(t, availableSubject))
	}
	var reason string
	if err := f.s.db.QueryRow(`SELECT clear_reason FROM handler_arm_limit_episodes WHERE cleared_at<>''`).Scan(&reason); err != nil || reason != api.HandlerArmClearHoldExpiredClean {
		t.Fatalf("clean reason %q %v", reason, err)
	}
	// A clean turn inside the hold does not clear it; the hold does.
	second := start.Add(2 * time.Hour)
	f.clock = second
	f.report(t, f.hS, "limit-2", api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason, ObservedAt: second})
	f.clock = second.Add(10 * time.Minute)
	f.report(t, f.hS, "working-2", api.AgentActivity{State: "working"})
	f.report(t, f.hS, "clean-2", api.AgentActivity{State: "idle"})
	if !limited() {
		t.Fatal("clean turn inside the hold cleared the limit")
	}
	// A repeat signal extends the hold.
	f.clock = second.Add(30 * time.Minute)
	f.report(t, f.hS, "working-3", api.AgentActivity{State: "working"})
	f.report(t, f.hS, "limit-3", api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason})
	if n := f.notices(t, limitSubject); n != 2 {
		t.Fatalf("repeat signal posted: %d", n)
	}
	f.clock = second.Add(61 * time.Minute)
	if !limited() {
		t.Fatal("repeat signal did not extend the hold")
	}
	// (ii) Hold expiry with no report since the signal clears at the next
	// claim, with one notice; the claim then draws over both arms.
	f.clock = second.Add(91 * time.Minute)
	q := f.entry(t, 0)
	claimed, err := f.claim(q, "claim-after-hold")
	if err != nil || len(claimed.HandlerArm.SkippedLimited) != 0 || f.notices(t, availableSubject) != 2 {
		t.Fatalf("hold expiry: %+v %v available=%d", claimed.HandlerArm, err, f.notices(t, availableSubject))
	}
	if err = f.s.db.QueryRow(`SELECT clear_reason FROM handler_arm_limit_episodes ORDER BY rowid DESC LIMIT 1`).Scan(&reason); err != nil || reason != api.HandlerArmClearHoldExpiredNoReport {
		t.Fatalf("hold reason %q %v", reason, err)
	}
	// (iv) A replayed claim posts nothing.
	if _, err = f.claim(q, "claim-after-hold"); err != nil || f.notices(t, availableSubject) != 2 {
		t.Fatalf("replayed claim %v", err)
	}
	// All arms limited: 409 and the list names it.
	f.release(t, claimed)
	f.report(t, f.hS, "working-4", api.AgentActivity{State: "working"})
	f.report(t, f.hS, "limit-4", api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason})
	f.report(t, f.hO, "prompt-4", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock)})
	last := f.entry(t, 1)
	if _, err = f.claim(last, "claim-all-limited"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "every handler arm is at a provider limit") || !strings.HasSuffix(err.Error(), api.HandlerArmWaitSuffix) {
		t.Fatalf("all limited: %v", err)
	}
	list, err := f.s.ListTeamQueue(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.ID == last.ID && e.BlockReason != "Every handler arm is at a provider limit" {
			t.Fatalf("list reason %q", e.BlockReason)
		}
	}
}

func TestHandlerArmNoPolicyKeepsLegacyLease(t *testing.T) {
	f := newArmFixture(t, 0)
	q := f.entry(t, 0)
	claimed, err := f.claim(q, "claim-legacy")
	if err != nil || claimed.HandlerArm != nil || countRows(t, f.s, `SELECT count(*) FROM handler_arm_assignments`) != 0 {
		t.Fatalf("legacy lease %+v %v", claimed, err)
	}
	// A disabled policy also keeps the legacy lease and opens no episodes.
	f.release(t, claimed)
	f.policy(t, "disabled", 0, false, false, "K")
	f.report(t, f.hO, "prompt", api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.clock)})
	if countRows(t, f.s, `SELECT count(*) FROM handler_arm_limit_episodes`) != 0 {
		t.Fatal("disabled policy opened an episode")
	}
}

func TestHandlerWriteRefusalRecordsOnlyHandlersOnce(t *testing.T) {
	f := newArmFixture(t, 0)
	ctx := context.Background()
	item := f.items[0].ID
	conflict := fmt.Errorf("%w: work item revision changed; refresh it before updating", api.ErrConflict)
	if err := f.s.RecordHandlerWriteRefusal(ctx, f.task.ID, item, f.hS.ID, "", "updates", "key-1", 409, conflict); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordHandlerWriteRefusal(ctx, f.task.ID, item, f.hS.ID, "", "updates", "key-1", 409, conflict); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordHandlerWriteRefusal(ctx, f.task.ID, item, f.hS.ID, "", "update", "", 400, api.ErrInvalid); err != nil {
		t.Fatal(err)
	}
	builder, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "mini", Session: "builder"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		agent  string
		status int
	}{{builder.ID, 409}, {"", 409}, {f.hS.ID, 403}, {f.hS.ID, 429}, {f.hS.ID, 500}} {
		if err := f.s.RecordHandlerWriteRefusal(ctx, f.task.ID, item, c.agent, "", "update", "", c.status, conflict); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.s.db.Query(`SELECT run_id,code,status FROM handler_write_refusals ORDER BY at,code`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var run, code string
		var status int
		if err := rows.Scan(&run, &code, &status); err != nil {
			t.Fatal(err)
		}
		if run != f.hS.RunID {
			t.Fatalf("run %s", run)
		}
		got = append(got, fmt.Sprintf("%s/%d", code, status))
	}
	rows.Close()
	if !reflect.DeepEqual(got, []string{"invalid/400", "stale_revision/409"}) {
		t.Fatalf("refusals %v", got)
	}
	if _, err := f.s.db.Exec(`UPDATE handler_write_refusals SET code='conflict'`); err == nil {
		t.Fatal("refusal updated")
	}
	if _, err := f.s.db.Exec(`DELETE FROM handler_write_refusals`); err == nil {
		t.Fatal("refusal deleted")
	}
}

func TestHandlerABNoiseHelpers(t *testing.T) {
	if m := abMedian([]int64{4, 1, 3, 2}); m == nil || *m != 2 {
		t.Fatalf("even median %v", m)
	}
	if m := abMedian([]int64{5, 1, 3}); *m != 3 {
		t.Fatalf("odd median %d", *m)
	}
	if p := abP90([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}); *p != 10 {
		t.Fatalf("p90 %d", *p)
	}
	if abMedian(nil) != nil || abP90(nil) != nil {
		t.Fatal("empty statistics")
	}
	a := []int64{100, 110, 120, 130, 140}
	close := []int64{105, 115, 125, 135, 145}
	far := []int64{300, 310, 320, 330, 340}
	if got := abNoiseFlag(5, 5, a, close); got != "within_noise" {
		t.Fatalf("close %s", got)
	}
	if got := abNoiseFlag(5, 5, a, far); got != "difference" {
		t.Fatalf("far %s", got)
	}
	if got := abNoiseFlag(4, 5, a, far); got != "insufficient" {
		t.Fatalf("small %s", got)
	}
	if abCountFlag(5, 6, 3, 5) != "within_noise" || abCountFlag(5, 6, 1, 5) != "difference" || abCountFlag(2, 6, 1, 5) != "insufficient" {
		t.Fatal("count flags")
	}
}

// reportFixture builds finished, assigned entries with known response times,
// revisions, blocks, interventions, refusals, limits, usage and rotations.
type reportFixture struct {
	*armFixture
	t0            time.Time
	changeSeq     int64
	hO2, hS2, hS3 api.Agent
}

func (r *reportFixture) at(minutes int) time.Time {
	return r.t0.Add(time.Duration(minutes) * time.Minute)
}

func (r *reportFixture) raw(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := r.s.db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	defer r.s.db.Exec(`PRAGMA foreign_keys=ON`)
	if _, err := r.s.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (r *reportFixture) entry(t *testing.T, item api.WorkItem, handler api.Agent, state string, gen int64, leased, finished int, arm, drawn string, fallback bool, digest string, assigned bool) {
	t.Helper()
	id := api.NewID("tqe")
	finishedAt := ""
	if state == "finished" {
		finishedAt = ts(r.at(finished))
	}
	r.raw(t, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,handler_id,handler_run_id,handler_lease_generation)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, r.task.ID, item.ID, item.Revision, 1, "planned", gen+int64(leased), state, ts(r.at(leased)), ts(r.at(leased)), handler.ID, handler.RunID, gen)
	if !assigned {
		return
	}
	reason := ""
	if fallback {
		reason = "busy"
	}
	r.raw(t, `INSERT INTO handler_arm_assignments(task_id,entry_id,item_id,lease_generation,policy_revision,draw_hex,drawn_arm,arm,fallback,fallback_reason,handler_id,handler_run_id,handler_digest,policy_digest,leased_at,finished_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.task.ID, id, item.ID, gen, 1, "00", drawn, arm, fallback, reason, handler.ID, handler.RunID, digest, digestP, ts(r.at(leased)), finishedAt)
}

func (r *reportFixture) revision(t *testing.T, item api.WorkItem, rev int64, status string, by api.Agent, minutes int, fields ...string) {
	t.Helper()
	r.writtenBy(t, item, rev, status, by, "fixture", "native", minutes, fields...)
}

// writtenBy inserts a revision with an explicit caller node and provenance.
func (r *reportFixture) writtenBy(t *testing.T, item api.WorkItem, rev int64, status string, by api.Agent, node, provenance string, minutes int, fields ...string) {
	t.Helper()
	r.changeSeq++
	changed := `["` + strings.Join(fields, `","`) + `"]`
	r.raw(t, `INSERT INTO work_item_revisions(item_task_id,item_id,revision,item_seq,item_kind,title,description,status,priority,created_node,created_user,created_at,updated_agent,updated_run_id,updated_node,updated_user,updated_at,attribution_kind,change_kind,changed_fields,source_change_seq,provenance)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.task.ID, item.ID, rev, item.Seq, item.Kind, item.Title, "", status, "normal", "fixture", "owner", ts(r.t0),
		by.ID, by.RunID, node, "owner", ts(r.at(minutes)), "shared_workspace_claim", "updated", changed, 900000+r.changeSeq, provenance)
}

// message inserts a typed message linked to an item and returns its seq.
func (r *reportFixture) message(t *testing.T, from, to, kind string, item api.WorkItem) int64 {
	t.Helper()
	envelope := fmt.Sprintf(`{"kind":%q,"subject":"Synthetic fixture message"}`, kind)
	r.raw(t, `INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,?,?,?,?,?,?,?)`, r.task.ID, from, "fixture", "owner", to, "synthetic", ts(r.t0), envelope)
	var seq int64
	if err := r.s.db.QueryRow(`SELECT max(seq) FROM messages`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	r.raw(t, `INSERT INTO message_work_item_links(message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,created_at) VALUES(?,?,?,?,?,?,?)`, seq, r.task.ID, r.task.ID, item.ID, item.Revision, "primary", ts(r.t0))
	return seq
}

// request posts an owner REQUEST to a handler linked to the item, then closes
// its obligation with the given outcome after millis. It returns the message.
func (r *reportFixture) request(t *testing.T, handler api.Agent, item api.WorkItem, millis int64, outcome string) api.Message {
	t.Helper()
	m, err := r.s.PostMessage(context.Background(), r.task.ID, api.PostMessageRequest{To: handler.ID, RequestID: api.NewID("req"),
		WorkItems: []api.MessageWorkItem{{ItemTaskID: r.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
		Envelope:  &api.Envelope{Kind: "request", To: handler.ID, Subject: "Record synthetic fixture evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, r.by)
	if err != nil {
		t.Fatal(err)
	}
	state := api.ObligationClosed
	if outcome == "" {
		state = api.ObligationDelivered
	}
	r.raw(t, `UPDATE obligations SET state=?,outcome=?,created_at=?,closed_at=? WHERE message_seq=? AND agent_id=?`, state, outcome, ts(r.t0), ts(r.t0.Add(time.Duration(millis)*time.Millisecond)), m.Seq, handler.ID)
	return m
}

func (r *reportFixture) usage(t *testing.T, handler api.Agent, key string, handled ...api.Message) {
	t.Helper()
	var turns []api.UsageTurn
	for i, m := range handled {
		turn := syntheticUsageTurn(fmt.Sprintf("%s-%d", key, i))
		turn.Handled = []api.UsageEvidence{{TaskID: r.task.ID, Seq: m.Seq, Operation: "ack", At: turn.At}}
		turns = append(turns, turn)
	}
	if _, err := r.s.ReportUsage(context.Background(), r.task.ID, handler.ID, usageBatch(handler, key, turns...)); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerABReportSyntheticFixture(t *testing.T) {
	f := newArmFixture(t, 5)
	r := &reportFixture{armFixture: f, t0: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	f.policy(t, "enable", 0, true, true, "K")
	// Handlers started later: an O successor after a template change, and
	// two S rotation successors.
	r.hO2 = f.handler(t, "handler-o-r2", armO, digestQ)
	r.hS2 = f.handler(t, "handler-s-r2", armS, digestP)
	r.hS3 = f.handler(t, "handler-s-r3", armS, digestP)
	for i, pair := range [][2]api.Agent{{f.hS, r.hS2}, {r.hS2, r.hS3}} {
		r.raw(t, `INSERT INTO handler_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, api.NewID("hrot"), f.task.ID, fmt.Sprintf("rotation-%d", i), "hash", "committed", "manual", "owner", i+1, pair[0].ID, pair[0].RunID, pair[0].Name, pair[1].ID, pair[1].Name, ts(r.t0), ts(r.t0))
	}
	s1, s2, o1, o2, f1, unfinished, unassigned := f.items[0], f.items[1], f.items[2], f.items[3], f.items[4], f.items[5], f.items[6]
	r.entry(t, s1, f.hS, "finished", 1, 0, 100, "S", "S", false, digestP, true)
	r.entry(t, s2, f.hS, "finished", 2, 200, 300, "S", "S", false, digestP, true)
	r.entry(t, o1, f.hO, "finished", 1, 0, 50, "O", "O", false, digestP, true)
	r.entry(t, o2, f.hO, "finished", 2, 100, 150, "O", "O", false, digestP, true)
	r.entry(t, f1, r.hO2, "finished", 1, 400, 450, "O", "S", true, digestQ, true)
	r.entry(t, unfinished, f.hS, "running", 3, 500, 0, "S", "S", false, digestP, true)
	r.entry(t, unassigned, f.hO, "finished", 3, 600, 700, "", "", false, "", false)
	// Response times; an open and a withdrawn request are not counted.
	requests := map[string][]api.Message{}
	for _, c := range []struct {
		key     string
		handler api.Agent
		item    api.WorkItem
		millis  []int64
	}{{"s1", f.hS, s1, []int64{1000, 3000}}, {"s2", f.hS, s2, []int64{2000, 5000, 9000}}, {"o1", f.hO, o1, []int64{4000}}, {"o2", f.hO, o2, []int64{6000, 8000}}, {"f1", r.hO2, f1, []int64{7000}}} {
		for _, ms := range c.millis {
			requests[c.key] = append(requests[c.key], r.request(t, c.handler, c.item, ms, api.OutcomeResult))
		}
	}
	r.request(t, f.hS, s1, 500, "")
	r.request(t, f.hS, s1, 700, api.OutcomeWithdrawn)
	// Usage: one turn per handled request, two for s1.
	r.usage(t, f.hS, "usage-s", requests["s1"][0], requests["s1"][1], requests["s2"][0])
	r.usage(t, f.hO, "usage-o", requests["o1"][0], requests["o2"][0])
	r.usage(t, r.hO2, "usage-f", requests["f1"][0])
	// Revisions: launch-to-done ends at the first done revision after the
	// lease; owner corrections overlap a handler revision's fields.
	owner := api.Agent{}
	r.revision(t, s1, 2, "open", f.hS, 10, "description")
	r.revision(t, s1, 3, "open", owner, 20, "priority")
	r.revision(t, s1, 4, "done", f.hS, 60, "status")
	r.revision(t, s1, 5, "done", f.hS, 200, "title") // outside the s1 lease
	r.revision(t, s1, 6, "done", owner, 210, "title")
	r.revision(t, s2, 2, "open", f.hS, 210, "title")
	r.revision(t, s2, 3, "open", owner, 220, "title")
	r.revision(t, s2, 4, "done", f.hS, 230, "status")
	r.revision(t, o1, 2, "open", f.hO, 10, "priority")
	r.revision(t, o1, 3, "open", owner, 20, "priority", "title")
	r.revision(t, o1, 4, "done", f.hO, 40, "status")
	r.revision(t, o2, 2, "done", f.hO, 90, "status") // done before its lease
	// Only owner-attributed writes correct a handler: a hub system write and
	// reconstructed history do not; the owner's helper session does.
	r.revision(t, o2, 3, "open", f.hO, 110, "description")
	r.writtenBy(t, o2, 4, "open", owner, "system", "native", 120, "description")
	r.writtenBy(t, o2, 5, "open", owner, "fixture", "reconstructed_change_log", 125, "description")
	helper, err := f.s.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "helper"}, f.by)
	if err != nil || helper.Agent == nil {
		t.Fatalf("owner helper %+v %v", helper, err)
	}
	r.revision(t, s2, 5, "open", *helper.Agent, 240, "status")
	r.revision(t, f1, 2, "done", r.hO2, 420, "status")
	// Blocks addressed to and written by the handler.
	r.message(t, "", f.hS.ID, "block", s1)
	r.message(t, f.hS.ID, "", "block", s2)
	r.message(t, "", f.hS.ID, "block", o1)
	// Link corrections on handler-authored messages; a resolve is not one.
	for _, c := range []struct {
		from api.Agent
		item api.WorkItem
		op   string
	}{{f.hS, s1, "correct"}, {f.hO, o2, "correct"}, {f.hO, o1, "resolve"}} {
		seq := r.message(t, c.from.ID, "", "notice", c.item)
		r.raw(t, `INSERT INTO message_audit_events(id,message_task_id,message_seq,audit_revision,operation,provenance,after_state,reason,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			api.NewID("mae"), f.task.ID, seq, 9, c.op, "native", "corrected", "synthetic", "fixture", "owner", ts(r.t0))
	}
	// Interventions.
	for _, c := range []struct {
		kind string
		item api.WorkItem
	}{{"gate-fix", s1}, {"nudge", s1}, {"gate-fix", o2}} {
		if _, err := f.s.CreateIntervention(ctx, f.task.ID, api.CreateInterventionRequest{Kind: c.kind, ItemID: c.item.ID, Text: "Synthetic fixture intervention", RequestID: api.NewID("req")}, f.by); err != nil {
			t.Fatal(err)
		}
	}
	// Refused saves inside and outside the lease windows.
	conflict := fmt.Errorf("%w: work item revision changed", api.ErrConflict)
	for _, c := range []struct {
		handler api.Agent
		item    api.WorkItem
		minutes int
	}{{f.hS, s1, 30}, {f.hO, o1, 30}, {f.hS, s1, 150}} {
		f.clock = r.at(c.minutes)
		if err := f.s.RecordHandlerWriteRefusal(ctx, f.task.ID, c.item.ID, c.handler.ID, "", "updates", api.NewID("req"), 409, conflict); err != nil {
			t.Fatal(err)
		}
	}
	f.clock = r.at(1000)
	// Limit events: an O episode overlapping o2, an S episode outside every
	// S lease, and Codex prompt escalations in and out of O leases.
	r.raw(t, `INSERT INTO handler_arm_limit_episodes(id,task_id,arm,agent_id,run_id,source,started_at,last_signal_at,cleared_at,clear_reason) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		api.NewID("hlim"), f.task.ID, "O", f.hO.ID, f.hO.RunID, api.HandlerArmSourceCodexUsageLimit, ts(r.at(120)), ts(r.at(120)), ts(r.at(130)), "prompt_gone")
	r.raw(t, `INSERT INTO handler_arm_limit_episodes(id,task_id,arm,agent_id,run_id,source,started_at,last_signal_at) VALUES(?,?,?,?,?,?,?,?)`,
		api.NewID("hlim"), f.task.ID, "S", f.hS.ID, f.hS.RunID, api.HandlerArmSourceClaudeRateLimit, ts(r.at(500)), ts(r.at(500)))
	for i, c := range []struct {
		kind    string
		minutes int
	}{{api.RuntimePromptCodexUsageLimit, 110}, {api.RuntimePromptCodexRateLimit, 30}, {api.RuntimePromptCodexUsageLimit, 60}} {
		r.raw(t, `INSERT INTO runtime_prompt_escalations(task_id,agent_id,run_id,fingerprint,kind,created_at) VALUES(?,?,?,?,?,?)`, f.task.ID, f.hO.ID, f.hO.RunID, fmt.Sprintf("%032d", i), c.kind, ts(r.at(c.minutes)))
	}

	report, err := f.s.HandlerABReport(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ms := func(v int64) *int64 { return &v }
	type itemWant struct {
		responses                                                        []int64
		toDone                                                           *int64
		tokens                                                           int64
		blocks, authored, refused, owner, gate, incorrect, links, limits int
		interventions                                                    map[string]int
		fallback                                                         bool
		flags                                                            []string
	}
	wants := map[string]itemWant{
		s1.ID: {[]int64{1000, 3000}, ms(3_600_000), 220, 1, 0, 1, 0, 1, 1, 1, 0, map[string]int{"gate-fix": 1, "nudge": 1}, false, []string{}},
		s2.ID: {[]int64{2000, 5000, 9000}, ms(1_800_000), 110, 0, 1, 0, 2, 0, 2, 0, 0, map[string]int{}, false, []string{}},
		o1.ID: {[]int64{4000}, ms(2_400_000), 110, 0, 0, 1, 1, 0, 1, 0, 1, map[string]int{}, false, []string{}},
		o2.ID: {[]int64{6000, 8000}, nil, 110, 0, 0, 0, 0, 1, 1, 1, 2, map[string]int{"gate-fix": 1}, false, []string{}},
		f1.ID: {[]int64{7000}, ms(1_200_000), 110, 0, 0, 0, 0, 0, 0, 0, 0, map[string]int{}, true, []string{"policy", "other_arm"}},
	}
	if len(report.Items) != len(wants) {
		t.Fatalf("items %d: unfinished and unassigned entries must be excluded", len(report.Items))
	}
	for _, it := range report.Items {
		w, ok := wants[it.ItemID]
		if !ok {
			t.Fatalf("unexpected item %s", it.ItemID)
		}
		got := itemWant{it.ResponseMillis, it.LaunchToDoneMillis, it.HandlerTokens, it.HandlerBlocks, it.HandlerAuthoredBlocks, it.RefusedSaves, it.OwnerCorrections, it.GateFixes,
			it.IncorrectSaves, it.LinkCorrections, it.LimitEvents.Total, it.Interventions, it.Fallback, it.DigestFlags}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("item %s:\n got %+v\nwant %+v", it.ItemID, got, w)
		}
		if it.LaunchToDoneMillis != nil && w.toDone != nil && *it.LaunchToDoneMillis != *w.toDone {
			t.Errorf("item %s launch to done %d", it.ItemID, *it.LaunchToDoneMillis)
		}
	}
	for _, it := range report.Items {
		switch it.ItemID {
		case o2.ID:
			if it.LimitEvents != (api.HandlerABLimitIn{Episodes: 1, CodexUsageLimit: 1, Total: 2}) || *it.EntryFinishedMillis != 3_000_000 {
				t.Fatalf("o2 limits %+v finished %v", it.LimitEvents, *it.EntryFinishedMillis)
			}
		case o1.ID:
			if it.LimitEvents != (api.HandlerABLimitIn{CodexRateLimit: 1, Total: 1}) || it.HandlerRequests != 1 || it.HandlerInputTokens != 101 || it.HandlerAllocatedTurns != "1" {
				t.Fatalf("o1 limits %+v usage %d %d %s", it.LimitEvents, it.HandlerRequests, it.HandlerInputTokens, it.HandlerAllocatedTurns)
			}
		}
	}
	arms := map[string]api.HandlerABArm{}
	for _, a := range report.Arms {
		arms[a.Arm] = a
	}
	s, o := arms["S"], arms["O"]
	check := func(name string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	str := func(v string) *string { return &v }
	check("S n/fallbacks/requests", []int{s.N, s.Fallbacks, s.Requests, s.LaunchToDoneDone, s.Rotations, s.DigestFlaggedItems}, []int{2, 0, 5, 2, 2, 0})
	check("O n/fallbacks/requests", []int{o.N, o.Fallbacks, o.Requests, o.LaunchToDoneDone, o.Rotations, o.DigestFlaggedItems}, []int{3, 1, 4, 2, 0, 1})
	check("S response median/p90", []*int64{s.ResponseMedianMillis, s.ResponseP90Millis}, []*int64{ms(3000), ms(9000)})
	check("O response median/p90", []*int64{o.ResponseMedianMillis, o.ResponseP90Millis}, []*int64{ms(6500), ms(8000)})
	check("S launch to done median", s.LaunchToDoneMedian, ms(2_700_000))
	check("O launch to done median", o.LaunchToDoneMedian, ms(1_800_000))
	check("S tokens median", s.HandlerTokensMedian, ms(165))
	check("O tokens median", o.HandlerTokensMedian, ms(110))
	check("S mean input per request", s.MeanInputPerRequest, str("101"))
	check("O mean input per request", o.MeanInputPerRequest, str("101"))
	counts := func(a api.HandlerABArm) map[string]string {
		out := map[string]string{}
		for k, v := range a.Counts {
			out[k] = fmt.Sprintf("%d:%s", v.Total, v.Rate)
		}
		return out
	}
	check("S counts", counts(s), map[string]string{"handlerBlocks": "1:1/2", "handlerAuthoredBlocks": "1:1/2", "refusedSaves": "1:1/2", "incorrectSaves": "3:3/2",
		"ownerCorrections": "2:1", "gateFixes": "1:1/2", "linkCorrections": "1:1/2", "interventions": "2:1", "limitEvents": "0:0"})
	check("O counts", counts(o), map[string]string{"handlerBlocks": "0:0", "handlerAuthoredBlocks": "0:0", "refusedSaves": "1:1/3", "incorrectSaves": "2:2/3",
		"ownerCorrections": "1:1/3", "gateFixes": "1:1/3", "linkCorrections": "1:1/3", "interventions": "1:1/3", "limitEvents": "3:1"})
	if len(report.Comparisons) != 12 {
		t.Fatalf("comparisons %d", len(report.Comparisons))
	}
	for _, c := range report.Comparisons {
		if c.A != "O" || c.B != "S" || c.Flag != "insufficient" {
			t.Fatalf("small-n comparison %+v", c)
		}
	}
	if len(report.Limits) != 2 || report.Policy.Revision != 1 {
		t.Fatalf("limits %d policy %d", len(report.Limits), report.Policy.Revision)
	}
}

// Provider blocks (wi_72f41bd375032cf0): a Claude handler's provider_blocked
// usage limit is the same arm limit signal as the turn-end reason an older
// relay sends, and other block classes are not.
func TestHandlerArmLimitProviderBlockedSignal(t *testing.T) {
	block := func(class string, since time.Time) api.AgentActivity {
		return api.AgentActivity{State: "provider_blocked", Provider: &api.ProviderBlock{Provider: "anthropic", Runtime: "claude", Model: armS.Model, Class: class, Code: "rate_limit", Status: 429, Since: since}}
	}
	limited := func(f *armFixture) bool {
		t.Helper()
		set, err := limitedArms(context.Background(), f.s.db, f.task.ID, f.clock)
		if err != nil {
			t.Fatal(err)
		}
		return set["S"]
	}
	for _, class := range []string{api.ProviderBlockUsageLimit, api.ProviderBlockRateLimited} {
		f := newArmFixture(t, 0)
		f.policy(t, "enable", 0, true, false, "K")
		start := f.clock
		f.report(t, f.hS, "idle", api.AgentActivity{State: "idle"})
		f.report(t, f.hS, "blocked-1", block(class, start))
		if !limited(f) || f.notices(t, limitSubject) != 1 {
			t.Fatalf("%s did not open an episode", class)
		}
		var source string
		if err := f.s.db.QueryRow(`SELECT source FROM handler_arm_limit_episodes`).Scan(&source); err != nil || source != api.HandlerArmSourceClaudeRateLimit {
			t.Fatalf("source %q %v", source, err)
		}
		// The existing hold rules apply unchanged: a working report does not
		// clear the limit, in the hold or after it.
		f.clock = start.Add(5 * time.Minute)
		f.report(t, f.hS, "working", api.AgentActivity{State: "working"})
		f.clock = start.Add(61 * time.Minute)
		if !limited(f) {
			t.Fatalf("%s cleared by a working report", class)
		}
		// A clean completed turn after the hold clears it.
		f.report(t, f.hS, "clean", api.AgentActivity{State: "idle"})
		if limited(f) || f.notices(t, availableSubject) != 1 {
			t.Fatalf("%s did not clear after a clean turn", class)
		}
		// The legacy reason form still opens an episode.
		f.clock = start.Add(3 * time.Hour)
		f.report(t, f.hS, "legacy", api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason})
		if !limited(f) || f.notices(t, limitSubject) != 2 {
			t.Fatalf("legacy reason form did not open an episode")
		}
	}
	// An authentication or server-error block is not a provider limit.
	f := newArmFixture(t, 0)
	f.policy(t, "enable", 0, true, false, "K")
	for _, class := range []string{api.ProviderBlockAuth, api.ProviderBlockServerError} {
		f.report(t, f.hS, "blocked-"+class, block(class, f.clock))
		if limited(f) || f.notices(t, limitSubject) != 0 {
			t.Fatalf("%s opened a limit episode", class)
		}
	}
	// A Codex handler's provider block is not the Codex usage-limit prompt.
	codexBlock := api.AgentActivity{State: "provider_blocked", Provider: &api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: armO.Model, Class: api.ProviderBlockRateLimited, Status: 429, Since: f.clock}}
	f.report(t, f.hO, "codex-blocked", codexBlock)
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_arm_limit_episodes`); n != 0 {
		t.Fatalf("codex block opened %d episodes", n)
	}
}

// Automatic handler provisioning (wi_01b6d3afed81167c, order #27052). Every
// test uses an isolated SQLite hub and synthetic agents; no handler is started.

type provisionFixture struct {
	*armFixture
	entries []api.TeamQueueEntry
	sArms   []api.Agent
}

var armSHeavy = api.HandlerArm{ID: "S", Runtime: armS.Runtime, Model: armS.Model, Reasoning: armS.Reasoning, Weight: 1000}

// newProvisionFixture is the reported gap: a parallel queue with the given
// limit, sHandlers online arm-S handlers, and five scoped entries that all
// draw arm S. The first leased entries are claimed, one handler each.
func newProvisionFixture(t *testing.T, limit, sHandlers, leased int) *provisionFixture {
	t.Helper()
	f := &provisionFixture{armFixture: newArmFixture(t, 3)}
	f.sArms = []api.Agent{f.hS}
	for i := 1; i < sHandlers; i++ {
		f.sArms = append(f.sArms, f.handler(t, fmt.Sprintf("handler-s%d", i+1), armS, digestP))
	}
	observeFixtureHost(t, f.s, f.task.ID, 0, freeDiskMiB(1<<20))
	setQueueLimit(t, f.s, f.task.ID, limit)
	for i := range f.items {
		f.entries = append(f.entries, addScopedEntry(t, f.s, f.task, f.items[i], f.orders[i], fmt.Sprintf("src/%d", i)))
	}
	seed := ""
	for i := 0; i < 1000 && seed == ""; i++ {
		seed = fmt.Sprintf("seed-%d", i)
		for _, q := range f.entries {
			if arm, _ := handlerArmDraw(seed, q.ID, []api.HandlerArm{armSHeavy, armO}); arm != "S" {
				seed = ""
				break
			}
		}
	}
	if seed == "" {
		t.Fatal("no seed draws arm S for every entry")
	}
	if _, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: "provision-policy", Enabled: true, Seed: seed,
		TemplateDigest: digestP, Arms: []api.HandlerArm{armSHeavy, armO}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < leased; i++ {
		q, err := claimEntry(f.s, f.task, f.entries[i])
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		f.entries[i] = q
	}
	return f
}

// provision offers a saved spec for an entry, as the runner does.
func (f *provisionFixture) provision(q api.TeamQueueEntry, key, agentID string, spec api.HandlerArm, digest string) (api.TeamQueueEntry, error) {
	return f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "provision_handler", EntryID: q.ID, ExpectedRevision: q.Revision,
		Host: "mini", HandlerAgentID: agentID, HandlerSpecRuntime: spec.Runtime, HandlerSpecModel: spec.Model, HandlerSpecReasoning: spec.Reasoning, HandlerSpecDigest: digest})
}

func (f *provisionFixture) rows(t *testing.T, state string) int {
	t.Helper()
	return countRows(t, f.s, `SELECT count(*) FROM handler_provisions WHERE task_id=? AND state=?`, f.task.ID, state)
}

func (f *provisionFixture) allRows(t *testing.T) int {
	t.Helper()
	return countRows(t, f.s, `SELECT count(*) FROM handler_provisions WHERE task_id=?`, f.task.ID)
}

func (f *provisionFixture) setProvision(t *testing.T, value string) {
	t.Helper()
	if _, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_handler_provision", HandlerProvision: value}); err != nil {
		t.Fatal(err)
	}
}

const provisionNotice = "Automatic handler provision"

// a1, a2: limit 4, three arm-S handlers leased, the fourth entry queued.
func TestHandlerProvisionNeedListsWaitingEntry(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 3)
	list, err := f.s.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || list.HandlerProvision != api.HandlerProvisionOn {
		t.Fatalf("list %q %v", list.HandlerProvision, err)
	}
	got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID)
	n := got.HandlerNeed
	if n == nil || !n.Provision || n.Arm != "S" || n.Handlers != 3 || n.Leased != 3 || n.Attempt != 1 || !strings.Contains(n.Reason, "3 of 3 leased, limit 4") {
		t.Fatalf("handler need %+v", n)
	}
	if n.Runtime != armS.Runtime || n.Model != armS.Model || n.Reasoning != armS.Reasoning || n.TemplateDigest != digestP {
		t.Fatalf("wanted settings %+v", n)
	}
	if got.BlockReason != "Waiting for a free handler of arm S: 3 of 3 leased, limit 4; the runner is adding one" {
		t.Fatalf("reason %q", got.BlockReason)
	}
	if f.allRows(t) != 0 || f.notices(t, provisionNotice) != 0 {
		t.Fatal("listing wrote a provision")
	}
}

// a3: the handler and the waiting team do not both fit under the agent cap.
func TestHandlerProvisionStopsAtAgentCap(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 3)
	open, reserved, err := projectAgentCapUsage(context.Background(), f.s.db, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The feature team's six seats fit exactly; one more agent does not.
	f.s.MaxAgents = open + reserved + featureTeamSlots
	got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID)
	limitFix := "tt team queue limit --task " + f.task.ID + " --limit 3"
	n := got.HandlerNeed
	if n == nil || n.Provision || n.Fix != limitFix || !strings.Contains(got.BlockReason, "cannot add one: project agent cap: ") ||
		!strings.Contains(got.BlockReason, fmt.Sprintf("+ %d seats + 1 handler > %d", featureTeamSlots, f.s.MaxAgents)) || !strings.HasSuffix(got.BlockReason, "Fix: "+limitFix) {
		t.Fatalf("cap need %+v reason %q", n, got.BlockReason)
	}
	_, err = f.provision(got, "cap", api.NewID("agt"), armS, digestP)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "project agent cap: ") {
		t.Fatalf("provision at cap: %v", err)
	}
	if f.allRows(t) != 0 || f.notices(t, provisionNotice) != 0 {
		t.Fatal("cap refusal left a row or notice")
	}
	f.s.MaxAgents++
	if got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID); !got.HandlerNeed.Provision {
		t.Fatalf("one more seat: %+v", got.HandlerNeed)
	}
}

// a4: a matching spec reserves once, posts one notice that is not an owner
// intervention, and replays; the registered handler is then leased.
func TestHandlerProvisionReservesOnceAndReplays(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 3)
	ctx := context.Background()
	q := f.entries[3]
	interventions := countRows(t, f.s, `SELECT count(*) FROM owner_interventions WHERE task_id=?`, f.task.ID)
	agentID := api.NewID("agt")
	first, err := f.provision(q, "provision-1", agentID, armS, digestP)
	if err != nil || first.ID != q.ID || first.State != "queued" || first.HandlerNeed == nil || first.HandlerNeed.AgentID != agentID || !first.HandlerNeed.Provision {
		t.Fatalf("provision %+v %v", first.HandlerNeed, err)
	}
	replay, err := f.provision(q, "provision-1", agentID, armS, digestP)
	if err != nil || !reflect.DeepEqual(replay.HandlerNeed, first.HandlerNeed) {
		t.Fatalf("replay %+v %v", replay.HandlerNeed, err)
	}
	if f.rows(t, "reserved") != 1 || f.allRows(t) != 1 || f.notices(t, provisionNotice) != 1 {
		t.Fatalf("rows %d notices %d", f.allRows(t), f.notices(t, provisionNotice))
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM owner_interventions WHERE task_id=?`, f.task.ID); n != interventions {
		t.Fatalf("owner interventions %d, were %d", n, interventions)
	}
	var text, node string
	if err := f.s.db.QueryRow(`SELECT json_extract(envelope,'$.body.text'),from_node FROM messages WHERE task_id=? AND envelope<>'' AND json_extract(envelope,'$.subject')=?`, f.task.ID, provisionNotice).Scan(&text, &node); err != nil {
		t.Fatal(err)
	}
	if node != "team_queue" || !strings.Contains(text, agentID) || !strings.Contains(text, q.ID) || !strings.Contains(text, "arm S") || !strings.Contains(text, "not an owner intervention") {
		t.Fatalf("notice from %q: %s", node, text)
	}
	// While the handler starts, the entry says one is being added.
	waiting := listedEntry(t, f.s, f.task.ID, q.ID)
	if waiting.HandlerNeed.Provision || waiting.HandlerNeed.AgentID != agentID || !strings.HasSuffix(waiting.BlockReason, "the runner is adding one") {
		t.Fatalf("pending need %+v %q", waiting.HandlerNeed, waiting.BlockReason)
	}
	// The reserved handler registers online; the claim leases it.
	added, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "handler-auto", Role: api.AgentRoleDatabaseHandler, AgentID: agentID, Host: "mini", Session: "handler-auto",
		Runtime: armS.Runtime, TemplateDigest: digestP, HandlerModel: armS.Model, HandlerReasoning: armS.Reasoning}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.online(t, added)
	claimed, err := claimEntry(f.s, f.task, q)
	if err != nil || claimed.HandlerID != agentID || claimed.HandlerArm == nil || claimed.HandlerArm.Arm != "S" {
		t.Fatalf("claim after provision %+v %v", claimed, err)
	}
	if f.rows(t, "registered") != 1 || f.allRows(t) != 1 {
		t.Fatalf("registered rows %d of %d", f.rows(t, "registered"), f.allRows(t))
	}
}

// a5, a6: the saved spec must equal the wanted runtime, model, reasoning and
// template digest; any one difference, or no spec at all, is refused.
func TestHandlerProvisionRequiresExactSpec(t *testing.T) {
	differ := func(change func(*api.HandlerArm)) api.HandlerArm {
		spec := armS
		change(&spec)
		return spec
	}
	cases := []struct {
		name    string
		spec    api.HandlerArm
		digest  string
		current string
	}{
		{"runtime", differ(func(a *api.HandlerArm) { a.Runtime = "codex" }), digestP, "codex/claude-sonnet-5-5/high digest " + digestP},
		{"model", differ(func(a *api.HandlerArm) { a.Model = "claude-opus-5-5" }), digestP, "claude/claude-opus-5-5/high digest " + digestP},
		{"reasoning", differ(func(a *api.HandlerArm) { a.Reasoning = "medium" }), digestP, "claude/claude-sonnet-5-5/medium digest " + digestP},
		{"digest", armS, digestQ, "claude/claude-sonnet-5-5/high digest " + digestQ},
		{"missing", api.HandlerArm{}, "", "the saved launch spec on mini is missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvisionFixture(t, 4, 3, 3)
			q := f.entries[3]
			_, err := f.provision(q, "refused-"+tc.name, api.NewID("agt"), tc.spec, tc.digest)
			if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "cannot add one: the saved launch spec on mini") {
				t.Fatalf("mismatched spec: %v", err)
			}
			if f.rows(t, "refused") != 1 || f.allRows(t) != 1 || f.notices(t, provisionNotice) != 0 {
				t.Fatalf("rows %d refused %d notices %d", f.allRows(t), f.rows(t, "refused"), f.notices(t, provisionNotice))
			}
			got := listedEntry(t, f.s, f.task.ID, q.ID)
			specFix := "tt handler spec --task " + f.task.ID + ` -- --run claude --runtime claude --model claude-sonnet-5-5 --reasoning high --prompt "$(cat PROMPT_FILE)"`
			n := got.HandlerNeed
			if n == nil || n.Provision || !n.Refused || n.Fix != specFix {
				t.Fatalf("refused need %+v", n)
			}
			for _, want := range []string{"3 of 3 leased, limit 4; cannot add one", tc.current, "arm S needs claude/claude-sonnet-5-5/high digest " + digestP, "Fix: " + specFix,
				"or: tt team queue limit --task " + f.task.ID + " --limit 3"} {
				if !strings.Contains(got.BlockReason, want) {
					t.Fatalf("reason lacks %q: %s", want, got.BlockReason)
				}
			}
			// The same spec again keeps the one row; a corrected spec clears
			// it and reserves.
			if _, err := f.provision(q, "refused-"+tc.name, api.NewID("agt"), tc.spec, tc.digest); !errors.Is(err, api.ErrConflict) || f.allRows(t) != 1 {
				t.Fatalf("repeat refusal: %v, rows %d", err, f.allRows(t))
			}
			if _, err := f.provision(q, "refused-"+tc.name, api.NewID("agt"), armS, digestP); err != nil {
				t.Fatalf("corrected spec: %v", err)
			}
			if f.rows(t, "refused") != 0 || f.rows(t, "reserved") != 1 || f.notices(t, provisionNotice) != 1 {
				t.Fatalf("after correction: refused %d reserved %d", f.rows(t, "refused"), f.rows(t, "reserved"))
			}
		})
	}
}

// a7: one provision at a time, and never more handlers than the limit.
func TestHandlerProvisionIsBounded(t *testing.T) {
	f := newProvisionFixture(t, 0, 3, 3)
	agentID := api.NewID("agt")
	if _, err := f.provision(f.entries[3], "first", agentID, armS, digestP); err != nil {
		t.Fatal(err)
	}
	// A second waiting entry reserves nothing while one is pending.
	second := listedEntry(t, f.s, f.task.ID, f.entries[4].ID)
	if second.HandlerNeed == nil || second.HandlerNeed.Provision || second.HandlerNeed.AgentID != "" {
		t.Fatalf("second need %+v", second.HandlerNeed)
	}
	if _, err := f.provision(second, "second", api.NewID("agt"), armS, digestP); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second provision: %v", err)
	}
	if f.allRows(t) != 1 || f.notices(t, provisionNotice) != 1 {
		t.Fatalf("rows %d notices %d", f.allRows(t), f.notices(t, provisionNotice))
	}

	// With the limit's handlers all present, none is added even though one
	// is offline and the entry waits: the reason stays the plain arm wait.
	g := newProvisionFixture(t, 3, 3, 2)
	leased := map[string]bool{g.entries[0].HandlerID: true, g.entries[1].HandlerID: true}
	for _, a := range g.sArms {
		if !leased[a.ID] {
			g.offline(t, a)
		}
	}
	held := listedEntry(t, g.s, g.task.ID, g.entries[2].ID)
	if held.HandlerNeed == nil || held.HandlerNeed.Provision || held.HandlerNeed.Handlers != 3 || held.HandlerNeed.Leased != 2 || held.BlockReason != "Waiting for a free handler of arm S" {
		t.Fatalf("at-limit need %+v %q", held.HandlerNeed, held.BlockReason)
	}
	if _, err := g.provision(held, "at-limit", api.NewID("agt"), armS, digestP); !errors.Is(err, api.ErrConflict) || g.allRows(t) != 0 {
		t.Fatalf("at-limit provision: %v rows %d", err, g.allRows(t))
	}
	// Raising the limit by one admits exactly one more handler.
	setQueueLimit(t, g.s, g.task.ID, 4)
	if got := listedEntry(t, g.s, g.task.ID, g.entries[2].ID); !got.HandlerNeed.Provision || !strings.Contains(got.BlockReason, "2 of 3 leased, limit 4") {
		t.Fatalf("raised limit need %+v %q", got.HandlerNeed, got.BlockReason)
	}
}

// a8: a reservation whose handler never registers stops holding after ten
// minutes, and the next one is a new attempt.
func TestHandlerProvisionAbandonsUnregisteredReservation(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 3)
	q := f.entries[3]
	if _, err := f.provision(q, "attempt-1", api.NewID("agt"), armS, digestP); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(handlerProvisionAbandonAfter - time.Second)
	observeFixtureHost(t, f.s, f.task.ID, 0, freeDiskMiB(1<<20))
	if got := listedEntry(t, f.s, f.task.ID, q.ID); got.HandlerNeed.Provision {
		t.Fatalf("still pending: %+v", got.HandlerNeed)
	}
	if _, err := f.provision(q, "early", api.NewID("agt"), armS, digestP); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("provision while pending: %v", err)
	}
	f.clock = f.clock.Add(time.Second)
	got := listedEntry(t, f.s, f.task.ID, q.ID)
	if !got.HandlerNeed.Provision || got.HandlerNeed.Attempt != 1 {
		t.Fatalf("after ten minutes: %+v", got.HandlerNeed)
	}
	next := api.NewID("agt")
	if _, err := f.provision(q, "attempt-2", next, armS, digestP); err != nil {
		t.Fatalf("new reservation: %v", err)
	}
	if f.rows(t, "abandoned") != 1 || f.rows(t, "reserved") != 1 || f.notices(t, provisionNotice) != 2 {
		t.Fatalf("abandoned %d reserved %d", f.rows(t, "abandoned"), f.rows(t, "reserved"))
	}
	if got := listedEntry(t, f.s, f.task.ID, q.ID); got.HandlerNeed.Attempt != 2 || got.HandlerNeed.AgentID != next {
		t.Fatalf("second attempt need %+v", got.HandlerNeed)
	}
}

// a9: the switch defaults on; off stops provisioning and names the command.
func TestHandlerProvisionSwitch(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 3)
	ctx := context.Background()
	q := f.entries[3]
	if on, err := queueHandlerProvision(ctx, f.s.db, "tsk_0000000000000000"); err != nil || !on {
		t.Fatalf("project without settings reads %v %v", on, err)
	}
	f.setProvision(t, api.HandlerProvisionOff)
	list, err := f.s.ListTeamQueue(ctx, f.task.ID)
	if err != nil || list.HandlerProvision != api.HandlerProvisionOff || list.ConcurrencyLimit != 4 {
		t.Fatalf("off list %q limit %d %v", list.HandlerProvision, list.ConcurrencyLimit, err)
	}
	onFix := "tt team queue provision --task " + f.task.ID + " --auto on"
	got := listedEntry(t, f.s, f.task.ID, q.ID)
	if got.HandlerNeed == nil || got.HandlerNeed.Provision || got.HandlerNeed.Fix != onFix ||
		got.BlockReason != "Waiting for a free handler of arm S: 3 of 3 leased, limit 4; automatic provisioning is off. Fix: "+onFix {
		t.Fatalf("off need %+v %q", got.HandlerNeed, got.BlockReason)
	}
	if _, err := f.provision(q, "off", api.NewID("agt"), armS, digestP); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), onFix) || f.allRows(t) != 0 {
		t.Fatalf("provision while off: %v", err)
	}
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "bad-switch", Operation: "set_handler_provision", HandlerProvision: "maybe"}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("invalid switch value: %v", err)
	}
	f.setProvision(t, api.HandlerProvisionOn)
	got = listedEntry(t, f.s, f.task.ID, q.ID)
	if !got.HandlerNeed.Provision || got.BlockReason != "Waiting for a free handler of arm S: 3 of 3 leased, limit 4; the runner is adding one" {
		t.Fatalf("on again %+v %q", got.HandlerNeed, got.BlockReason)
	}
}

// a10: raising the limit above the available handlers warns and still saves.
func TestHandlerProvisionLimitRaiseWarns(t *testing.T) {
	f := newProvisionFixture(t, 3, 3, 0)
	ctx := context.Background()
	// Only the three arm-S handlers stay available.
	for _, name := range []string{"database", "handler-o"} {
		if _, err := f.s.db.Exec(`UPDATE agents SET status='retired' WHERE task_id=? AND name=?`, f.task.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	limit := func(n int) api.TeamQueueEntry {
		t.Helper()
		e, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_limit", Host: "mini", ConcurrencyLimit: n})
		if err != nil {
			t.Fatalf("limit %d: %v", n, err)
		}
		return e
	}
	raised := limit(4)
	if raised.Warning != "limit 4 exceeds 3 available database handlers; the runner adds one per waiting team while the agent cap allows" {
		t.Fatalf("warning %q", raised.Warning)
	}
	if list, err := f.s.ListTeamQueue(ctx, f.task.ID); err != nil || list.ConcurrencyLimit != 4 {
		t.Fatalf("limit not saved: %d %v", list.ConcurrencyLimit, err)
	}
	if lowered := limit(3); lowered.Warning != "" {
		t.Fatalf("lowering warned %q", lowered.Warning)
	}
	f.handler(t, "handler-s4", armS, digestP)
	if enough := limit(4); enough.Warning != "" {
		t.Fatalf("four handlers warned %q", enough.Warning)
	}
	f.setProvision(t, api.HandlerProvisionOff)
	if off := limit(5); off.Warning != "limit 5 exceeds 4 available database handlers; automatic provisioning is off, so teams will wait. Fix: tt team queue provision --task "+f.task.ID+" --auto on" {
		t.Fatalf("off warning %q", off.Warning)
	}
}

// Without an arm policy the wanted settings are the first handler's recorded
// ones; a handler that recorded no template keeps the legacy reason.
func TestHandlerProvisionWithoutPolicy(t *testing.T) {
	f := newProvisionFixture(t, 4, 3, 0)
	ctx := context.Background()
	if _, err := f.s.SetHandlerArmPolicy(ctx, f.task.ID, api.HandlerArmPolicyRequest{RequestID: "disable", ExpectedRevision: 1, Enabled: false, Seed: "off", Arms: []api.HandlerArm{armSHeavy, armO}}); err != nil {
		t.Fatal(err)
	}
	// Only the arm-S handlers remain, so the first open handler is recorded.
	for _, name := range []string{"database", "handler-o"} {
		if _, err := f.s.db.Exec(`UPDATE agents SET status='closed' WHERE task_id=? AND name=?`, f.task.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := claimEntry(f.s, f.task, f.entries[i]); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	q := f.entries[3]
	got := listedEntry(t, f.s, f.task.ID, q.ID)
	n := got.HandlerNeed
	if n == nil || !n.Provision || n.Arm != "" || n.Model != armS.Model || n.TemplateDigest != digestP || got.BlockReason != "No free database handler: 3 of 3 leased, limit 4; the runner is adding one" {
		t.Fatalf("no-policy need %+v %q", n, got.BlockReason)
	}
	if _, err := f.provision(q, "plain-refused", api.NewID("agt"), armO, digestP); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "the handlers in use needs claude/claude-sonnet-5-5/high") {
		t.Fatalf("mismatch without policy: %v", err)
	}
	if _, err := f.provision(q, "plain", api.NewID("agt"), armS, digestP); err != nil || f.rows(t, "reserved") != 1 || f.rows(t, "refused") != 0 {
		t.Fatalf("provision without policy: %v", err)
	}
}
