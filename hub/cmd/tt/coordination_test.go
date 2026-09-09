package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestWrapPreservesArguments(t *testing.T) {
	text := "two words; $(printf injected) `printf injected` 'quoted'"
	command, err := wrapCommand([]string{"--", "printf", "%s", text})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil || string(out) != text {
		t.Fatalf("argument reparsed: %q %v", out, err)
	}
	command, err = wrapCommand([]string{"--shell", "printf explicit; printf shell"})
	out, runErr := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil || runErr != nil || string(out) != "explicitshell" {
		t.Fatal("explicit shell broken")
	}
}

func TestPostHelpAndUnknownFlagsNeverContactHub(t *testing.T) {
	requests := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", 500)
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001"}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"hello", "--help"}} {
		if err := cmdPost(e, args); err != nil {
			t.Fatalf("help %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--recipient", "owner", "hello"}, {"hello", "--reply-to", "bad"}, {"hello", "--to"}} {
		if err := cmdPost(e, args); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if requests != 0 {
		t.Fatalf("help or invalid flags contacted hub %d times", requests)
	}
}

func TestPostHumanReplyAndLiteralHelp(t *testing.T) {
	var posted []api.PostMessageRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, "unexpected route", 500)
			return
		}
		var body api.PostMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		posted = append(posted, body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Message{Seq: int64(len(posted))})
	}))
	defer s.Close()
	e := env{hub: s.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001"}
	for _, args := range [][]string{{"A poem\nfor you", "--reply-to", "5"}, {"--", "--help"}} {
		if err := cmdPost(e, args); err != nil {
			t.Fatal(err)
		}
	}
	if len(posted) != 2 || posted[0].To != "" || posted[0].ReplyTo != 5 || posted[0].Text != "A poem\nfor you" || posted[1].Text != "--help" {
		t.Fatalf("incorrect posts: %+v", posted)
	}
}

func TestBoundAgentPostCarriesItsRecordedItemAndRetryIdentity(t *testing.T) {
	var posted api.PostMessageRequest
	binding := &api.AgentWorkItemBinding{
		ItemTaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", ItemRevision: 3,
		WorkOrderMessage: api.MessageReference{TaskID: "tsk_0000000000000001", Seq: 814},
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/agents/"):
			_ = json.NewEncoder(w).Encode(api.Agent{ID: "agt_0000000000000001", WorkItem: binding})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(api.Message{Seq: 1})
		default:
			http.Error(w, "unexpected route", 500)
		}
	}))
	defer s.Close()
	t.Setenv("TAILTERM_WORK_ITEM", binding.ItemID)
	e := env{hub: s.URL, task: binding.ItemTaskID, agent: "agt_0000000000000001", runID: "run_0000000000000003"}
	if err := cmdPost(e, []string{"Verified exact synthetic result"}); err != nil {
		t.Fatal(err)
	}
	if posted.RequestID == "" || len(posted.WorkItems) != 1 || posted.WorkItems[0].ItemID != binding.ItemID || posted.WorkOrderMessage == nil || *posted.WorkOrderMessage != binding.WorkOrderMessage {
		t.Fatalf("bound post lost durable context: %+v", posted)
	}
	firstKey := posted.RequestID
	if err := cmdPost(e, []string{"Verified exact synthetic result"}); err != nil || posted.RequestID != firstKey {
		t.Fatalf("exact retry identity changed: %+v %v", posted, err)
	}
}

func TestInboxDistinguishesHumanFromAgentNamedOwner(t *testing.T) {
	m := api.Message{Seq: 5, From: api.Sender{User: "owner"}, Text: "A poem please"}
	human := formatMessage(m, nil)
	if !strings.Contains(human, "owner (human)") || !strings.Contains(human, "tt post --reply-to 5") {
		t.Fatal(human)
	}
	m.From.AgentID = "agt_0000000000000001"
	agent := formatMessage(m, map[string]string{m.From.AgentID: "owner"})
	if strings.Contains(agent, "human") || strings.Contains(agent, "omit --to") {
		t.Fatal(agent)
	}
}

func TestPostTrailingFlagsAndLiteralSeparator(t *testing.T) {
	for _, args := range [][]string{{"hello", "--to", "reviewer", "--reply-to", "17"}, {"--to=reviewer", "hello", "--reply-to=17"}} {
		fs := flag.NewFlagSet("post", flag.ContinueOnError)
		to := fs.String("to", "", "")
		reply := fs.Int64("reply-to", 0, "")
		if err := fs.Parse(postArgs(args)); err != nil {
			t.Fatal(err)
		}
		if *to != "reviewer" || *reply != 17 || strings.Join(fs.Args(), " ") != "hello" {
			t.Fatal("misrouted positional message")
		}
	}
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	to := fs.String("to", "", "")
	if err := fs.Parse(postArgs([]string{"--", "literal", "--to", "reviewer"})); err != nil {
		t.Fatal(err)
	}
	if *to != "" || strings.Join(fs.Args(), " ") != "literal --to reviewer" {
		t.Fatal("literal separator broken")
	}
}

func TestOrchestratorImplementationBoundaryAcrossLaunchAndResumeRosters(t *testing.T) {
	lead := api.Agent{ID: "agt_0000000000000001", Name: "lead", Status: api.AgentRunning}
	handler := api.Agent{ID: "agt_0000000000000002", Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Status: api.AgentDone}
	worker := api.Agent{ID: "agt_0000000000000003", Name: "builder", Status: api.AgentRunning}
	cases := []struct {
		name       string
		allowSpawn bool
		maxHelpers int
		planned    int
		agents     []api.Agent
		mayBuild   bool
	}{
		{name: "fresh_launch_sole_spawn_disabled", agents: nil, mayBuild: true},
		{name: "resume_sole_spawn_disabled", agents: []api.Agent{lead}, mayBuild: true},
		{name: "database_handler_is_not_a_team_builder", agents: []api.Agent{lead, handler}, mayBuild: true},
		{name: "fresh_launch_sole_spawn_enabled", allowSpawn: true, maxHelpers: 2},
		{name: "sole_spawn_enabled_with_exhausted_quota", allowSpawn: true, maxHelpers: 0, agents: []api.Agent{lead}},
		{name: "fresh_multi_member_launch_spawn_disabled", planned: 2},
		{name: "fresh_multi_member_launch_spawn_enabled", allowSpawn: true, maxHelpers: 2, planned: 2},
		{name: "multiple_members_spawn_disabled", agents: []api.Agent{lead, worker}},
		{name: "multiple_members_spawn_enabled", allowSpawn: true, maxHelpers: 2, agents: []api.Agent{lead, worker}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := api.Task{ID: "tsk_0000000000000001", Name: "Project", Orchestrator: "lead", AllowAgentSpawn: tc.allowSpawn, MaxNewAgents: tc.maxHelpers}
			got := agentTaskBriefingForLaunch(task, "lead", "", tc.agents, tc.planned)
			for _, required := range []string{"decisions, planning, routing work, and reviewing evidence", "shared schema/types and integration code", "not a runtime sandbox or API authorization boundary"} {
				if !strings.Contains(got, required) {
					t.Fatalf("missing %q", required)
				}
			}
			if tc.mayBuild {
				if !strings.Contains(got, "both required exception conditions are present") || strings.Contains(got, "You must not implement") {
					t.Fatal(got)
				}
			} else {
				for _, required := range []string{"You must not implement", "only implementation exception requires both", "idle or retired workers", "unavailable capacity", "quota exhaustion", "cost", "consumed helper allowance"} {
					if !strings.Contains(got, required) {
						t.Fatalf("missing %q", required)
					}
				}
			}
		})
	}
}

func TestRetiredIdleAndExitedWorkersStillPreventTheSoleMemberException(t *testing.T) {
	for _, status := range []string{api.AgentDone, api.AgentRetired, api.AgentNeedsInput, api.AgentExited, api.AgentClosed} {
		t.Run(status, func(t *testing.T) {
			task := api.Task{Name: "Project", Orchestrator: "lead"}
			agents := []api.Agent{{Name: "lead", Status: api.AgentRunning}, {Name: "builder", Status: status}}
			got := agentTaskBriefing(task, "lead", "", agents)
			if !strings.Contains(got, "You must not implement") {
				t.Fatal(got)
			}
		})
	}
}

func TestWorkerBriefingPreservesAssignedBuilderAndReadOnlyRoles(t *testing.T) {
	task := api.Task{Name: "Project", Orchestrator: "lead"}
	agents := []api.Agent{{Name: "lead", Status: api.AgentRunning}, {Name: "worker", Status: api.AgentRunning}}
	got := agentTaskBriefing(task, "worker", "", agents)
	for _, required := range []string{"decides, plans, routes work and reviews evidence", "builders own assigned implementation", "shared schema/types and integration code", "Preserve any assigned read-only or other non-builder role"} {
		if !strings.Contains(got, required) {
			t.Fatalf("missing %q", required)
		}
	}
	if strings.Contains(got, "You are the MAIN ORCHESTRATOR") || strings.Contains(got, "sole non-database team member") {
		t.Fatal(got)
	}
}

func TestSpawnRejectsInvalidPlannedTeamSizeBeforeContactingHub(t *testing.T) {
	e := env{hub: "http://127.0.0.1:1", task: "tsk_0000000000000001"}
	err := cmdSpawn(e, []string{"--name", "lead", "--run", "codex", "--planned-team-members", "33"})
	if err == nil || !strings.Contains(err.Error(), "planned team members") {
		t.Fatal(err)
	}
}
