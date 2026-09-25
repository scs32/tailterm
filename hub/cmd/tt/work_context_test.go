package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestFormatWorkItemContextIsExplicitlyScoped(t *testing.T) {
	context := api.AgentWorkItemContext{
		Version: 1,
		Binding: api.AgentWorkItemBinding{
			AgentID: "agt_0000000000000001", RunID: "run_0000000000000002",
			ItemTaskID: "tsk_0000000000000003", ItemID: "wi_0000000000000004", ItemRevision: 7,
			ContextThroughMessageSeq: 816,
		},
		Bundle: []byte(`{"version":1,"history":{"description":"Only relevant content"}}`),
	}
	digest := sha256.Sum256(context.Bundle)
	context.Binding.ContextDigest = hex.EncodeToString(digest[:])
	formatted, err := formatWorkItemContext(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"exact agent run", "intentionally excluded", "contextThroughMessageSeq", "816", "Only relevant content"} {
		if !strings.Contains(formatted, required) {
			t.Fatalf("formatted context missing %q: %s", required, formatted)
		}
	}
}

func TestPreparedWorkContextByteBoundariesAndRestoredDigest(t *testing.T) {
	for _, size := range []int{269315, api.MaxAgentWorkItemContextBytes, api.MaxAgentWorkItemContextBytes + 1} {
		overhead := len(`{"source":""}`)
		count := size - overhead
		data := []byte(`{"source":"` + strings.Repeat("界", count/3) + strings.Repeat("x", count%3) + `"}`)
		path := filepath.Join(t.TempDir(), "synthetic.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		for _, file := range []bool{false, true} {
			inline, filename := string(data), ""
			if file {
				inline, filename = "", path
			}
			got, err := readPreparedWorkContext(inline, filename)
			if size > api.MaxAgentWorkItemContextBytes {
				if !errors.Is(err, api.ErrContextLimit) {
					t.Fatalf("over-limit = %v", err)
				}
				continue
			}
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("complete source changed: %v", err)
			}
		}
	}
	bundle := []byte(`{"source": "<>&界", "n":9007199254740993}`)
	hash := sha256.Sum256(bundle)
	ctx := api.AgentWorkItemContext{Version: 1, Bundle: bundle, Binding: api.AgentWorkItemBinding{ContextDigest: hex.EncodeToString(hash[:])}}
	formatted, err := formatWorkItemContext(ctx)
	if err != nil || !strings.Contains(formatted, string(bundle)) {
		t.Fatalf("format lost raw source: %v", err)
	}
	ctx.Bundle = json.RawMessage(`{"source":"changed"}`)
	if _, err := formatWorkItemContext(ctx); err == nil {
		t.Fatal("changed immutable digest accepted")
	}
	for _, invalid := range []string{"{broken", string([]byte{'"', 0xff, '"'})} {
		if _, err := readPreparedWorkContext(invalid, ""); err == nil {
			t.Fatal("invalid UTF-8 JSON accepted")
		}
	}
}

func TestWrapCompleteQuotedContextFileBound(t *testing.T) {
	const limit = 4*api.MaxAgentWorkItemContextBytes + 256*1024
	for _, size := range []int{limit, limit + 1} {
		path := filepath.Join(t.TempDir(), "synthetic-command")
		data := strings.Repeat("'", size)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := wrapCommand([]string{"--shell-file", path})
		if size > limit {
			if err == nil {
				t.Fatal("oversized command accepted")
			}
			continue
		}
		if err != nil || got != data {
			t.Fatalf("complete bounded command changed: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("consumed command retained")
		}
	}
}

// wi_dd57670ee65d974c@3/order3622: actual CLI authoring and API admission.
func TestAllocationIntentContextTransportParity(t *testing.T) {
	for _, size := range []int{269315, api.MaxAgentWorkItemContextBytes} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			c, task, loseReply := contextIntentFixture(t)
			lead := spawnLauncherAgent(t, c, task)
			item, order := realSpawnWorkItemAndOrder(t, c, task)
			confirmCLIFixtureOrder(t, c, task, item, order, lead)
			var bundle map[string]any
			if err := json.Unmarshal([]byte(syntheticSpawnContextBundle(t, task, item, order)), &bundle); err != nil {
				t.Fatal(err)
			}
			history := bundle["history"].(map[string]any)
			messages := history["messages"].([]any)
			messages = append(messages, map[string]any{"message": map[string]any{"taskId": task, "seq": order + 1, "text": "ASCII CJK 界 HTML <>& quote \" slash \\"}})
			history["messages"] = messages
			raw, err := json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			for encoded, literal := range map[string]string{`\u003c`: "<", `\u003e`: ">", `\u0026`: "&"} {
				raw = bytes.ReplaceAll(raw, []byte(encoded), []byte(literal))
			}
			if len(raw) > size {
				t.Fatalf("fixture overhead %d exceeds target %d", len(raw), size)
			}
			messages[len(messages)-1].(map[string]any)["message"].(map[string]any)["text"] =
				messages[len(messages)-1].(map[string]any)["message"].(map[string]any)["text"].(string) + strings.Repeat("x", size-len(raw))
			raw, err = json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			for encoded, literal := range map[string]string{`\u003c`: "<", `\u003e`: ">", `\u0026`: "&"} {
				raw = bytes.ReplaceAll(raw, []byte(encoded), []byte(literal))
			}
			if len(raw) != size {
				t.Fatalf("fixture size = %d, want %d", len(raw), size)
			}
			path := filepath.Join(t.TempDir(), "synthetic-context.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			agentID, runID := api.NewID("agt"), api.NewID("run")
			e := env{hub: c.Base, task: task, agent: lead.ID, runID: lead.RunID}
			args := []string{"--agent-id", agentID, "--work-item", item, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order), "--team-role", "member", "--work-context-file", path, "--expected-run-id", runID, "--request-id", "context-transport", "--json"}
			loseReply.Store(true)
			if err = cmdAllocationIntentCreate(e, args); err == nil {
				t.Fatal("fixture did not lose the committed intent reply")
			}
			if err = cmdAllocationIntentCreate(e, args); err != nil {
				t.Fatalf("unchanged uncertain retry failed: %v", err)
			}
			intent, err := c.GetAllocationIntent(context.Background(), task, agentID)
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err = json.Compact(&compact, raw); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(compact.Bytes())
			if intent.ContextDigest != hex.EncodeToString(hash[:]) {
				t.Fatal("intent digest differs from exact bytes AddAgent will admit")
			}
			req := api.AddAgentRequest{AgentID: agentID, ParentAgentID: lead.ID, Name: "context-worker", Session: "context-worker", Host: "fixture", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task, ItemID: item, ItemRevision: 1, TeamRole: api.TeamRoleMember, WorkOrderMessage: api.MessageReference{TaskID: task, Seq: order}, ContextBundle: raw}}
			agent, err := c.AddAgent(context.Background(), task, req)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := c.GetAgentWorkItemContext(context.Background(), task, agent.ID, agent.RunID)
			if err != nil || !bytes.Equal(restored.Bundle, compact.Bytes()) || restored.Binding.ContextDigest != intent.ContextDigest || agent.RunID != runID {
				t.Fatalf("bound readback changed: %v", err)
			}
			if err = cmdAllocationIntentCreate(e, args); err != nil {
				t.Fatalf("consumed-intent retry: %v", err)
			}
			_, err = c.AddAgent(context.Background(), task, req)
			var duplicate *api.HTTPError
			if !errors.As(err, &duplicate) || duplicate.Status != http.StatusConflict {
				t.Fatalf("duplicate admission must reject: %v", err)
			}
			replay, err := c.GetAgent(context.Background(), task, agentID)
			if err != nil || replay.ID != agent.ID || replay.RunID != agent.RunID {
				t.Fatalf("uncertain identity readback changed: %v", err)
			}
			final, err := c.GetAllocationIntent(context.Background(), task, agentID)
			if err != nil || final.ConsumedByRunID != runID || final.ExpectedLauncherAgentID != lead.ID || final.LauncherRunID != lead.RunID {
				t.Fatalf("Capacity tuple changed: %v", err)
			}
			agents, err := c.ListAgents(context.Background(), task)
			if err != nil || len(agents) != 2 {
				t.Fatalf("duplicate identity: %v count=%d", err, len(agents))
			}
		})
	}
}

func contextIntentFixture(t *testing.T) (*api.Client, string, *atomic.Bool) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "synthetic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	handler := server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "fixture"}, nil })
	lose := new(atomic.Bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/allocation-intents") && lose.Swap(false) {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusCreated {
				t.Errorf("intent not committed before reply loss: %d", recorded.Code)
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	task, err := c.CreateTask(context.Background(), api.CreateTaskRequest{Name: "context intent parity", Orchestrator: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	return c, task.ID, lose
}

func TestAllocationIntentLegacyRawDigestRemainsUnchanged(t *testing.T) {
	c, task, _ := contextIntentFixture(t)
	lead := spawnLauncherAgent(t, c, task)
	item, order := realSpawnWorkItemAndOrder(t, c, task)
	var raw bytes.Buffer
	if err := json.Indent(&raw, []byte(syntheticSpawnContextBundle(t, task, item, order)), "", "  "); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw.Bytes())
	agentID, runID := api.NewID("agt"), api.NewID("run")
	before, err := c.CreateAllocationIntent(context.Background(), task, api.CreateAllocationIntentRequest{AgentID: agentID, TargetTaskID: task, ItemTaskID: task, ItemID: item, ItemRevision: 1, WorkOrderMessage: api.MessageReference{TaskID: task, Seq: order}, ContextDigest: hex.EncodeToString(hash[:]), TeamRole: api.TeamRoleMember, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: runID, RequestID: "legacy-raw"})
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: c.Base, task: task, agent: lead.ID, runID: lead.RunID}
	args := []string{"--agent-id", agentID, "--work-item", item, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order), "--team-role", "member", "--work-context-json", raw.String(), "--expected-run-id", runID, "--request-id", "legacy-raw"}
	err = cmdAllocationIntentCreate(e, args)
	var conflict *api.HTTPError
	if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
		t.Fatalf("legacy raw intent must fail without rewrite: %v", err)
	}
	after, err := c.GetAllocationIntent(context.Background(), task, agentID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("legacy intent tuple was changed")
	}
}

func TestAllocationIntentFileAndInlineByteBounds(t *testing.T) {
	data := `{"source":"` + strings.Repeat("x", api.MaxAgentWorkItemContextBytes) + `"}`
	path := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	e := env{hub: "http://127.0.0.1:1", task: "tsk_1111111111111111", agent: "agt_1111111111111111", runID: "run_1111111111111111"}
	base := []string{"--agent-id", "agt_2222222222222222", "--work-item", "wi_1111111111111111", "--work-item-revision", "1", "--work-order-message", "1", "--team-role", "member", "--expected-run-id", "run_2222222222222222", "--request-id", "oversized"}
	for _, input := range [][]string{{"--work-context-file", path}, {"--work-context-json", data}} {
		if err := cmdAllocationIntentCreate(e, append(append([]string(nil), base...), input...)); !errors.Is(err, api.ErrContextLimit) {
			t.Fatalf("bound not checked before hub: %v", err)
		}
	}
}
