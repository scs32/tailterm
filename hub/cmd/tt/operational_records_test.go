package main

import (
	"context"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestOperationalCLIHTTPStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "op.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Operational CLI", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "fixture", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	defer srv.Close()
	e := env{hub: srv.URL, task: task.ID, agent: actor.ID, runID: actor.RunID}
	file := filepath.Join(t.TempDir(), "proposal.json")
	if err = os.WriteFile(file, []byte(`{"requestId":"cli-op","data":{"kind":"instruction"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := captureCLIOutput(t, func() error { return cmdOperationalRecord(e, []string{"propose", "--file", file}) })
	if err != nil {
		t.Fatal(err)
	}
	var first api.OperationalMutation
	if err = json.Unmarshal([]byte(output), &first); err != nil {
		t.Fatal(err)
	}
	if first.Record.State != "proposed" || first.Record.ActorRunID != actor.RunID || first.Receipt.ID == "" {
		t.Fatalf("lost structure: %+v", first)
	}
	output, err = captureCLIOutput(t, func() error { return cmdOperationalRecord(e, []string{"get", first.Record.ID}) })
	if err != nil {
		t.Fatal(err)
	}
	var fetched api.OperationalRecord
	if err = json.Unmarshal([]byte(output), &fetched); err != nil || fetched.ID != first.Record.ID {
		t.Fatalf("get %+v %v", fetched, err)
	}
	output, err = captureCLIOutput(t, func() error { return cmdOperationalRecord(e, []string{"propose", "--file", file}) })
	if err != nil {
		t.Fatal(err)
	}
	var replay api.OperationalMutation
	if err = json.Unmarshal([]byte(output), &replay); err != nil || !replay.Replay || replay.Receipt.ID != first.Receipt.ID {
		t.Fatal("CLI did not return original typed receipt")
	}
	if err = os.WriteFile(file, []byte(`{"requestId":"invalid","data":{"kind":"instruction","accepted":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = captureCLIOutput(t, func() error { return cmdOperationalRecord(e, []string{"propose", "--file", file}) }); err == nil {
		t.Fatal("CLI silently dropped unknown authority field")
	}
}
func TestOperationalCLIUnsupportedCapability(t *testing.T) {
	for _, caps := range []string{`{}`, `{"operationalRecords":{"supported":false,"versions":[1]}}`, `{"operationalRecords":{"supported":true,"versions":[9]}}`} {
		t.Run(caps, func(t *testing.T) {
			actions := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(caps))
					return
				}
				actions++
				w.WriteHeader(500)
			}))
			defer srv.Close()
			e := env{hub: srv.URL, task: "tsk_0000000000000000"}
			if err := cmdOperationalRecord(e, []string{"get", "opr_0000000000000000"}); err == nil || actions != 0 {
				t.Fatalf("unknown capability actions=%d err=%v", actions, err)
			}
		})
	}
}
