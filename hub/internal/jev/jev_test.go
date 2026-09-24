package jev

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type fixture struct {
	st    *store.Store
	task  api.Task
	agent api.Agent
	by    api.Caller
}

func newFixture(t *testing.T, path string) fixture {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "jev-test", User: "owner"}
	ctx := context.Background()
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "jev"}, by)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "builder", Host: "h", Session: "s", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{st: st, task: task, agent: agent, by: by}
}

func (f fixture) post(t *testing.T, text string) api.Message {
	t.Helper()
	m, err := f.st.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{AgentID: f.agent.ID, Text: text}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f fixture) check(t *testing.T, seq int64) api.MessageCheck {
	t.Helper()
	checks, err := f.st.ListMessageChecks(context.Background(), f.task.ID, seq-1, 1)
	if err != nil || len(checks) != 1 || checks[0].Seq != seq {
		t.Fatalf("check %d: %v %+v", seq, err, checks)
	}
	return checks[0]
}

// fakeJev answers every requested noul with 0.25 and records request bodies.
func fakeJev(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body []byte) bool) (*httptest.Server, func() string) {
	var mu sync.Mutex
	var bodies []string
	sent := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(bodies, "\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if handler != nil && handler(w, r, body) {
			return
		}
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(body, &req)
		answers := map[string]any{}
		for name := range req.Questions {
			answers[name] = map[string]any{"type": "noul", "noul": 0.25}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return srv, sent
}

func scorer(f fixture, url string) *Scorer {
	return &Scorer{Client: &Client{URL: url, Key: "test-key", Model: "jev-latest", HTTP: &http.Client{}},
		Checks: f.st, Concurrency: 4, Timeout: 200 * time.Millisecond, Interval: 20 * time.Millisecond}
}

func waitForStatus(t *testing.T, f fixture, seq int64, status string) api.MessageCheck {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := f.check(t, seq)
		if c.JevStatus == status {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %d stayed %s, want %s", seq, c.JevStatus, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Criterion a9: scoring, hang, 500 and restart.
func TestScorerRecordsScoresAndFailsOpen(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "hub.sqlite"))
	f.st.EnableJevScoring()
	srv, bodies := fakeJev(t, nil)
	typed := f.post(t, "RESULT: Tests pass for the recipient check\nOutcome: done\nStatus: a1 pass\nEvidence: e1: go test -> ok")
	free := f.post(t, "hey lead, done")

	ctx, cancel := context.WithCancel(context.Background())
	done := scorer(f, srv.URL).Start(ctx)
	got := waitForStatus(t, f, typed.Seq, api.JevStatusScored)
	if got.Jev == nil || got.Jev.Model != "jev-test" || got.Jev.Nouls["manipulation"] != 0.25 || got.Jev.Nouls["subject_plain"] != 0.25 {
		t.Fatalf("typed scores %+v", got.Jev)
	}
	if freeCheck := waitForStatus(t, f, free.Seq, api.JevStatusScored); len(freeCheck.Jev.Nouls) != 4 {
		t.Fatalf("free text should get the four base questions, got %v", freeCheck.Jev.Nouls)
	}
	cancel()
	<-done
	if !strings.Contains(bodies(), `"model":"jev-latest"`) {
		t.Fatalf("model not sent: %v", bodies())
	}

	// A hanging server: the post still committed, and the row ends unavailable.
	// The handler returns when the client gives up, so the server can close.
	slow, _ := fakeJev(t, func(w http.ResponseWriter, r *http.Request, _ []byte) bool { <-r.Context().Done(); return true })
	hung := f.post(t, "RESULT: second result for the hang check")
	ctx, cancel = context.WithCancel(context.Background())
	done = scorer(f, slow.URL).Start(ctx)
	if c := waitForStatus(t, f, hung.Seq, api.JevStatusUnavailable); c.Jev == nil || c.Jev.Error == "" {
		t.Fatalf("hung check %+v", c)
	}
	cancel()
	<-done

	// A 500 is retried once, then recorded unavailable.
	var attempts atomic.Int32
	broken, _ := fakeJev(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	failed := f.post(t, "hey lead, another")
	ctx, cancel = context.WithCancel(context.Background())
	done = scorer(f, broken.URL).Start(ctx)
	waitForStatus(t, f, failed.Seq, api.JevStatusUnavailable)
	cancel()
	<-done
	if attempts.Load() != 2 {
		t.Fatalf("500 attempts = %d, want 2", attempts.Load())
	}
}

func TestPendingChecksResumeAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	f := newFixture(t, path)
	f.st.EnableJevScoring()
	m := f.post(t, "hey lead, done before the restart")
	f.st.Close()

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.st = reopened
	if f.check(t, m.Seq).JevStatus != api.JevStatusPending {
		t.Fatal("pending row lost across restart")
	}
	srv, _ := fakeJev(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := scorer(f, srv.URL).Start(ctx)
	waitForStatus(t, f, m.Seq, api.JevStatusScored)
	cancel()
	<-done
}

// Criterion a10: secrets never leave the hub.
func TestScorerRedactsSecrets(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "hub.sqlite"))
	f.st.EnableJevScoring()
	srv, bodies := fakeJev(t, nil)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123"
	m := f.post(t, "deploy with key "+secret+" and token: supersecretvalue123")
	ctx, cancel := context.WithCancel(context.Background())
	done := scorer(f, srv.URL).Start(ctx)
	waitForStatus(t, f, m.Seq, api.JevStatusScored)
	cancel()
	<-done
	sent := bodies()
	if strings.Contains(sent, secret) || strings.Contains(sent, "supersecretvalue123") || !strings.Contains(sent, "[REDACTED]") {
		t.Fatalf("secret reached Jev: %s", sent)
	}
}
