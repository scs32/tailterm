package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// providerFixture is a project with two items, each with a lead, on a
// controllable clock. Reports and sweeps use synthetic agents only.
type providerFixture struct {
	t     *testing.T
	s     *Store
	task  api.Task
	items []api.WorkItem
	order []api.Message
	clock time.Time
	leads []api.Agent
	n     int
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &providerFixture{t: t, s: s, task: task, items: items, order: orders, clock: time.Date(2026, 9, 26, 19, 0, 0, 0, time.UTC)}
	s.now = func() time.Time { return f.clock }
	for i := range items {
		lead := f.agent(fmt.Sprintf("lead-%d", i), "claude", i)
		if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, items[i].ID, lead.ID, lead.RunID); err != nil {
			t.Fatal(err)
		}
		f.leads = append(f.leads, lead)
	}
	return f
}

// agent adds a running agent of runtime, bound to item (or unbound when < 0).
func (f *providerFixture) agent(name, runtime string, item int) api.Agent {
	f.t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "mini", Session: name, Runtime: runtime, Cwd: f.t.TempDir()}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, a.ID); err != nil {
		f.t.Fatal(err)
	}
	if item >= 0 {
		_, err = f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`,
			a.ID, a.RunID, f.task.ID, f.items[item].ID, f.task.ID, f.order[item].Seq, "digest", []byte("{}"), ts(f.clock))
		if err != nil {
			f.t.Fatal(err)
		}
	}
	return a
}

func providerBlock(runtime, class string, since time.Time) *api.ProviderBlock {
	p := &api.ProviderBlock{Provider: api.ProviderForRuntime(runtime), Runtime: runtime, Model: "claude-fable-5-1", Class: class, Code: "rate_limit", Status: 429, Since: since}
	if runtime == "codex" {
		p.Model, p.Code, p.Status = "gpt-6-astra", "other", 401
	}
	return p
}

func (f *providerFixture) report(a api.Agent, key string, activity api.AgentActivity) error {
	activity.ObservedAt = f.clock
	_, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: key, RunID: a.RunID, Activity: activity})
	return err
}

// block reports a provider block that began now; state reports another state.
func (f *providerFixture) block(a api.Agent, runtime, class string) {
	f.t.Helper()
	f.n++
	if err := f.report(a, fmt.Sprintf("block-%d", f.n), api.AgentActivity{State: "provider_blocked", Provider: providerBlock(runtime, class, f.clock)}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *providerFixture) state(a api.Agent, state string) {
	f.t.Helper()
	f.n++
	if err := f.report(a, fmt.Sprintf("state-%d", f.n), api.AgentActivity{State: state}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *providerFixture) sweep() int {
	f.t.Helper()
	n, err := f.s.ProviderBlockSweep(context.Background(), f.clock)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *providerFixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

// notices counts broker notices with subject sent to agent ("" is the owner).
func (f *providerFixture) notices(subject, to string) int {
	f.t.Helper()
	return countRows(f.t, f.s, `SELECT count(*) FROM messages WHERE task_id=? AND from_node=? AND to_agent=? AND envelope<>'' AND json_extract(envelope,'$.subject')=?`, f.task.ID, api.BrokerNode, to, subject)
}

func (f *providerFixture) lastText(subject string) string {
	f.t.Helper()
	var text string
	if err := f.s.db.QueryRow(`SELECT json_extract(envelope,'$.body.text') FROM messages WHERE task_id=? AND envelope<>'' AND json_extract(envelope,'$.subject')=? ORDER BY seq DESC LIMIT 1`, f.task.ID, subject).Scan(&text); err != nil {
		f.t.Fatal(err)
	}
	return text
}

func TestProviderBlockLeadNotice(t *testing.T) {
	f := newProviderFixture(t)
	member := f.agent("reviewer", "claude", 0)
	lead, other := f.leads[0], f.leads[1]
	since := f.clock
	blocked := api.AgentActivity{State: "provider_blocked", Provider: providerBlock("claude", "usage_limit", since)}
	if err := f.report(member, "first", blocked); err != nil {
		t.Fatal(err)
	}
	if n := f.notices(providerBlockLeadSubject, lead.ID); n != 1 {
		t.Fatalf("lead notices %d", n)
	}
	text := f.lastText(providerBlockLeadSubject)
	for _, part := range []string{"reviewer", "anthropic usage_limit", "code rate_limit", "status 429", "model claude-fable-5-1", "since 2026-09-26T19:00:00Z", "/model"} {
		if !strings.Contains(text, part) {
			t.Fatalf("lead notice lacks %q: %s", part, text)
		}
	}
	got, err := f.s.GetAgent(context.Background(), member.ID)
	if err != nil || got.Activity == nil || got.Activity.State != "provider_blocked" || !api.SameProviderBlock(got.Activity.Provider, blocked.Provider) {
		t.Fatalf("snapshot %+v %v", got.Activity, err)
	}
	// A replayed request, and the same block reported under a new request.
	if err := f.report(member, "first", blocked); err != nil {
		t.Fatal(err)
	}
	f.advance(10 * time.Second)
	if err := f.report(member, "steady", blocked); err != nil {
		t.Fatal(err)
	}
	// A flap that returns with the same class and since.
	f.state(member, "idle")
	if n := countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE agent_id=? AND cleared_at<>''`, member.ID); n != 1 {
		t.Fatalf("idle did not clear the episode: %d", n)
	}
	f.advance(10 * time.Second)
	if err := f.report(member, "flap-back", blocked); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE agent_id=? AND cleared_at=''`, member.ID); n != 1 {
		t.Fatalf("flap did not reopen the episode: %d", n)
	}
	if lead, others, owner := f.notices(providerBlockLeadSubject, lead.ID), f.notices(providerBlockLeadSubject, other.ID), f.notices(providerBlockLeadSubject, ""); lead != 1 || others != 0 || owner != 0 {
		t.Fatalf("after replay and flap: lead=%d other=%d owner=%d", lead, others, owner)
	}
	// A cleared block followed by a new one is a new episode.
	f.state(member, "working")
	f.advance(time.Hour)
	f.block(member, "claude", "usage_limit")
	if n := f.notices(providerBlockLeadSubject, lead.ID); n != 2 {
		t.Fatalf("new episode notices %d", n)
	}
	// A class change is a new episode too, and ends the earlier one.
	f.advance(time.Minute)
	f.block(member, "claude", "auth")
	if n, open := f.notices(providerBlockLeadSubject, lead.ID), countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE agent_id=? AND cleared_at=''`, member.ID); n != 3 || open != 1 {
		t.Fatalf("class change notices=%d open=%d", n, open)
	}
	// A blocked lead is not told about itself; an agent without an item has
	// no lead. Both still get an episode for the owner notice.
	f.block(lead, "claude", "usage_limit")
	loose := f.agent("loose", "claude", -1)
	f.block(loose, "claude", "usage_limit")
	if n, total := f.notices(providerBlockLeadSubject, lead.ID), countRows(t, f.s, `SELECT count(*) FROM messages WHERE task_id=? AND envelope<>'' AND json_extract(envelope,'$.subject')=?`, f.task.ID, providerBlockLeadSubject); n != 3 || total != 3 {
		t.Fatalf("lead and unbound agent: lead=%d total=%d", n, total)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE agent_id IN (?,?)`, lead.ID, loose.ID); n != 2 {
		t.Fatalf("episodes for lead and unbound agent: %d", n)
	}

	// Payload validation: every field is an enum, pattern or range.
	good := func() api.AgentActivity {
		return api.AgentActivity{State: "provider_blocked", Provider: providerBlock("claude", "usage_limit", since)}
	}
	bad := map[string]func(*api.AgentActivity){
		"class":            func(a *api.AgentActivity) { a.Provider.Class = "quota" },
		"model":            func(a *api.AgentActivity) { a.Provider.Model = "sk-fake key" },
		"empty model":      func(a *api.AgentActivity) { a.Provider.Model = "" },
		"status":           func(a *api.AgentActivity) { a.Provider.Status = 200 },
		"code":             func(a *api.AgentActivity) { a.Provider.Code = "Incorrect API key provided" },
		"provider":         func(a *api.AgentActivity) { a.Provider.Provider = "openai" },
		"runtime":          func(a *api.AgentActivity) { a.Provider.Runtime = "other" },
		"since":            func(a *api.AgentActivity) { a.Provider.Since = time.Time{} },
		"missing provider": func(a *api.AgentActivity) { a.Provider = nil },
		"other state":      func(a *api.AgentActivity) { a.State = "idle" },
	}
	for name, mutate := range bad {
		a := good()
		mutate(&a)
		if err := f.report(member, "bad-"+strings.ReplaceAll(name, " ", "-"), a); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestProviderBlockOwnerNotice(t *testing.T) {
	f := newProviderFixture(t)
	first := f.agent("reviewer", "claude", 0)
	second := f.agent("verifier", "claude", 0)
	// A peer of the same runtime is working, so this is not an outage.
	f.state(f.leads[0], "working")
	f.block(first, "claude", "usage_limit")
	f.advance(10 * time.Second)
	f.block(second, "claude", "auth")
	f.advance(20 * time.Second)
	if n := f.sweep(); n != 0 || f.notices(providerBlockOwnerSubject, "") != 0 {
		t.Fatalf("owner notice inside the grace period: %d", n)
	}
	f.advance(61 * time.Second) // 91 s after the first block, 81 s after the second
	if n := f.sweep(); n != 1 || f.notices(providerBlockOwnerSubject, "") != 1 {
		t.Fatalf("first owner notice: swept %d", n)
	}
	text := f.lastText(providerBlockOwnerSubject)
	for _, part := range []string{"reviewer", "anthropic usage_limit", "model claude-fable-5-1", "Recovery:", "Nothing is switched automatically"} {
		if !strings.Contains(text, part) {
			t.Fatalf("owner notice lacks %q: %s", part, text)
		}
	}
	if n := f.sweep(); n != 0 {
		t.Fatalf("second sweep posted %d", n)
	}
	f.advance(10 * time.Second)
	if n := f.sweep(); n != 1 || f.notices(providerBlockOwnerSubject, "") != 2 || !strings.Contains(f.lastText(providerBlockOwnerSubject), "verifier") {
		t.Fatalf("second agent's owner notice: swept %d", n)
	}
	f.advance(time.Hour)
	if n := f.sweep(); n != 0 || f.notices(providerBlockOwnerSubject, "") != 2 || f.notices(providerBlockOutageSubject, "") != 0 {
		t.Fatalf("later sweep posted %d", n)
	}
	// A block that clears inside the grace period still gets its notice,
	// which says it has cleared.
	third := f.agent("planner", "claude", 1)
	f.block(third, "claude", "server_error")
	f.advance(30 * time.Second)
	f.state(third, "idle")
	f.advance(61 * time.Second)
	if n := f.sweep(); n != 1 || !strings.Contains(f.lastText(providerBlockOwnerSubject), "planner was blocked") || !strings.Contains(f.lastText(providerBlockOwnerSubject), "already cleared") {
		t.Fatalf("cleared episode notice: swept %d: %s", n, f.lastText(providerBlockOwnerSubject))
	}
	// The grace period is configurable and bounded.
	t.Setenv("TAILTERM_PROVIDER_BLOCK_GRACE", "300")
	if providerBlockGrace() != 300*time.Second {
		t.Fatal("grace override ignored")
	}
	t.Setenv("TAILTERM_PROVIDER_BLOCK_GRACE", "0")
	if providerBlockGrace() != 90*time.Second {
		t.Fatal("grace override not bounded")
	}
	// The owner helper is the owner's own session and opens no episode.
	helper := f.agent("owner-helper", "claude", -1)
	if _, err := f.s.db.Exec(`UPDATE agents SET role=? WHERE id=?`, api.AgentRoleOwnerHelper, helper.ID); err != nil {
		t.Fatal(err)
	}
	f.block(helper, "claude", "usage_limit")
	if n := countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE agent_id=?`, helper.ID); n != 0 {
		t.Fatalf("owner helper episodes %d", n)
	}
}

func TestProviderBlockOutage(t *testing.T) {
	f := newProviderFixture(t)
	var codex []api.Agent
	for i := 0; i < 4; i++ {
		codex = append(codex, f.agent(fmt.Sprintf("codex-%d", i), "codex", i%2))
	}
	idle := f.agent("codex-idle", "codex", 0)
	f.state(idle, "idle") // an idle agent makes no provider calls and counts neither way
	claude := f.agent("reviewer", "claude", 0)
	f.state(f.leads[0], "working") // a working Claude peer: Claude is not out

	f.block(codex[0], "codex", "auth")
	f.advance(5 * time.Second)
	f.block(claude, "claude", "usage_limit")
	f.advance(5 * time.Second)
	f.block(codex[1], "codex", "auth")
	f.advance(10 * time.Second)
	f.block(codex[2], "codex", "auth")
	owner := func() (outage, agent int) {
		return f.notices(providerBlockOutageSubject, ""), f.notices(providerBlockOwnerSubject, "")
	}
	f.advance(40 * time.Second) // 60 s after the first block
	if n := f.sweep(); n != 0 {
		t.Fatalf("notice inside the grace period: %d", n)
	}
	f.advance(31 * time.Second) // 91 s: the first Codex episode is due, the others are not
	if n := f.sweep(); n != 1 {
		t.Fatalf("outage sweep posted %d", n)
	}
	if outage, agent := owner(); outage != 1 || agent != 0 {
		t.Fatalf("outage=%d per-agent=%d", outage, agent)
	}
	text := f.lastText(providerBlockOutageSubject)
	for _, part := range []string{"runtime codex", "openai", "3 agents", "auth", "codex-0, codex-1, codex-2", "2026-09-26T19:00:00Z"} {
		if !strings.Contains(text, part) {
			t.Fatalf("outage notice lacks %q: %s", part, text)
		}
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM provider_block_episodes WHERE runtime='codex' AND owner_state='outage' AND outage_id<>''`); n != 3 {
		t.Fatalf("episodes attached to the outage: %d", n)
	}
	// The blocked Claude agent is handled on its own.
	f.advance(10 * time.Second)
	if n := f.sweep(); n != 1 {
		t.Fatalf("claude sweep posted %d", n)
	}
	if outage, agent := owner(); outage != 1 || agent != 1 || !strings.Contains(f.lastText(providerBlockOwnerSubject), "reviewer") {
		t.Fatalf("claude handled with the outage: outage=%d per-agent=%d", outage, agent)
	}
	// A fourth agent blocking later joins the outage without a notice.
	f.advance(time.Minute)
	f.block(codex[3], "codex", "auth")
	f.advance(10 * time.Second)
	f.sweep()
	f.advance(5 * time.Minute)
	if n := f.sweep(); n != 0 {
		t.Fatalf("late sweep posted %d", n)
	}
	if outage, agent := owner(); outage != 1 || agent != 1 {
		t.Fatalf("fourth agent: outage=%d per-agent=%d", outage, agent)
	}
	if n := countRows(t, f.s, `SELECT agent_count FROM provider_outages WHERE closed_at=''`); n != 4 {
		t.Fatalf("outage agent count %d", n)
	}
	// All clear: the outage closes. Blocking again is a new outage with one
	// new notice.
	for _, a := range codex {
		f.state(a, "idle")
	}
	f.sweep()
	if open := countRows(t, f.s, `SELECT count(*) FROM provider_outages WHERE closed_at=''`); open != 0 {
		t.Fatalf("outage still open: %d", open)
	}
	f.advance(time.Hour)
	f.block(codex[0], "codex", "auth")
	f.block(codex[1], "codex", "server_error")
	f.advance(30 * time.Second)
	if n := f.sweep(); n != 0 {
		t.Fatalf("second outage inside the grace period: %d", n)
	}
	f.advance(61 * time.Second)
	if n := f.sweep(); n != 1 {
		t.Fatalf("second outage sweep posted %d", n)
	}
	if outage, agent := owner(); outage != 2 || agent != 1 || !strings.Contains(f.lastText(providerBlockOutageSubject), "auth, server_error") {
		t.Fatalf("second outage: outage=%d per-agent=%d: %s", outage, agent, f.lastText(providerBlockOutageSubject))
	}
	if n := f.sweep(); n != 0 || countRows(t, f.s, `SELECT count(*) FROM provider_outages`) != 2 {
		t.Fatalf("repeat sweep posted %d", n)
	}
	// One blocked agent of a runtime while a peer of it works is not an
	// outage; neither is a single blocked agent.
	f.state(codex[1], "working")
	f.state(codex[0], "idle")
	f.sweep()
	f.advance(time.Hour)
	f.block(codex[0], "codex", "auth")
	f.block(codex[2], "codex", "auth")
	f.advance(91 * time.Second)
	if n := f.sweep(); n != 2 {
		t.Fatalf("blocked beside a working peer posted %d", n)
	}
	if outage, agent := owner(); outage != 2 || agent != 3 {
		t.Fatalf("working peer: outage=%d per-agent=%d", outage, agent)
	}
}
