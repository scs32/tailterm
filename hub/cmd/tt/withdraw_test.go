package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestWithdrawCLIThroughLoopbackHub(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Withdraw CLI", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "h", Session: "lead", Runtime: "codex"}, by)
	worker, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	e := env{hub: srv.URL, task: task.ID, agent: lead.ID, runID: lead.RunID}
	post := func() (api.Message, api.Obligation) {
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: worker.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Please inspect the candidate", Body: api.EnvelopeBody{Ask: "Look it over"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		list, err := st.ListObligations(ctx, task.ID, store.ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now())
		if err != nil || len(list) != 1 {
			t.Fatalf("obligation: %+v %v", list, err)
		}
		return m, list[0]
	}
	m, o := post()
	if err := cmdWithdraw(e, []string{strconv.FormatInt(m.Seq, 10), "--reason", "new assignment"}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ListObligations(ctx, task.ID, store.ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now()); err != nil || got[0].Outcome != api.OutcomeWithdrawn {
		t.Fatalf("sequence selector: %+v %v", got, err)
	}
	if err := cmdWithdraw(e, []string{strconv.FormatInt(m.Seq, 10), "--reason", "new assignment"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	m2, o2 := post()
	if err := cmdWithdraw(e, []string{"--obligation", o2.ID, "--reason", "superseded"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.WithdrawObligation(ctx, task.ID, o.ID, api.ObligationWithdrawRequest{AgentID: lead.ID, RunID: lead.RunID, Reason: "different"}); err == nil {
		t.Fatal("closed request should refuse different reason")
	}
	if err := cmdWithdraw(e, []string{strconv.FormatInt(m2.Seq, 10), "--obligation", o.ID, "--reason", "mismatch"}); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("conflicting selectors: %v", err)
	}
	for _, args := range [][]string{{"99999", "--reason", "x"}, {"--obligation", "obl_0000000000000000", "--reason", "x"}, {"--reason", " "}} {
		if err := cmdWithdraw(e, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := cmdWithdraw(env{hub: srv.URL, task: task.ID}, []string{"--obligation", o.ID, "--reason", "x"}); err == nil || !strings.Contains(err.Error(), "agent session") {
		t.Fatalf("missing session: %v", err)
	}
	if err := cmdWithdraw(env{hub: srv.URL, task: task.ID, agent: worker.ID, runID: worker.RunID}, []string{"--obligation", o.ID, "--reason", "x"}); err == nil {
		t.Fatal("other agent was not refused")
	}
	all, err := st.ListObligations(ctx, task.ID, store.ObligationFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var notices int
	for _, entry := range all {
		if entry.SourceKind == api.EnvelopeKindNotice {
			notices++
		}
	}
	if notices != 2 {
		t.Fatalf("expected one notice for each withdrawal, got %d", notices)
	}
	workerEnv := env{hub: srv.URL, task: task.ID, agent: worker.ID, runID: worker.RunID}
	allOutput, err := captureCLIOutput(t, func() error { return cmdObligations(workerEnv, []string{"--all"}) })
	if err != nil || !strings.Contains(allOutput, "outcome=withdrawn") || strings.Contains(allOutput, "[overdue:") {
		t.Fatalf("obligations --all: %q %v", allOutput, err)
	}
	summary, err := captureCLIOutput(t, func() error { return cmdMessageChecks(e, []string{"--summary", "--since", "0"}) })
	if err != nil || !strings.Contains(summary, "Withdrawn: 2") || strings.Contains(summary, "Unacknowledged past") {
		t.Fatalf("message-checks --summary: %q %v", summary, err)
	}
}

func TestWithdrawHelpAndBadFlagsNeverContactHub(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; w.WriteHeader(500) }))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001"}
	for _, args := range [][]string{{"--help"}, {"123", "--help"}} {
		if err := cmdWithdraw(e, args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--bad"}, {"bogus", "--reason", "x"}, {"1", "--reason", " "}} {
		if err := cmdWithdraw(e, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if requests != 0 {
		t.Fatalf("preflight contacted hub %d times", requests)
	}
}

func TestWithdrawCLIRefusesAmbiguousSequence(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ObligationList{Obligations: []api.Obligation{
			{ID: "obl_0000000000000001", MessageSeq: 12},
			{ID: "obl_0000000000000002", MessageSeq: 12},
		}})
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	if err := cmdWithdraw(e, []string{"12", "--reason", "superseded"}); err == nil || !strings.Contains(err.Error(), "use --obligation") {
		t.Fatalf("ambiguous sequence: %v", err)
	}
	if posts != 0 {
		t.Fatalf("ambiguous sequence sent %d withdrawal requests", posts)
	}
}
