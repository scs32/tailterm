package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

type handlerFixture struct {
	c      *api.Client
	task   string
	req    api.AddAgentRequest
	opts   spawn.Options
	marker string
	drop   atomic.Bool
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-handler-test-"+api.NewID("agt"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Cleanup(func() { _, _ = startupTmux(context.Background(), "kill-server") })
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &handlerFixture{}
	handler := server.New(st, func(r *http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "fixture"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/agents") && f.drop.CompareAndSwap(true, false) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.c, err = api.NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.c.CreateTask(context.Background(), api.CreateTaskRequest{Name: "isolated handler test"})
	if err != nil {
		t.Fatal(err)
	}
	f.task = task.ID
	dir := t.TempDir()
	f.marker = filepath.Join(dir, "executions")
	self := filepath.Join(dir, "fake-tt")
	if err = os.WriteFile(self, []byte("#!/bin/sh\nprintf x >> "+spawn.ShellQuote(f.marker)+"\nexec sleep 300\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f.req = api.AddAgentRequest{AgentID: api.NewID("agt"), Role: "database_handler", Name: "db-handler", Runtime: "generic", Host: "fixture", Cwd: dir}
	f.opts = spawn.Options{Cwd: dir, Self: self, Command: "fixture command", Env: map[string]string{"TAILTERM_TOKEN": "synthetic-secret", "TAILTERM_BRIEFING": "variable briefing"}}
	return f
}
func (f *handlerFixture) launch() (api.Agent, error) {
	return ensureHandler(context.Background(), f.c, f.task, f.req, f.opts)
}
func (f *handlerFixture) executions(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(f.marker)
		if len(data) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(f.marker)
	t.Fatalf("expected %d launches, got %q", n, data)
}
func handlerTestTmux(t *testing.T, args ...string) {
	t.Helper()
	if _, err := startupTmux(context.Background(), args...); err != nil {
		t.Fatal(args, err)
	}
}
func handlerJournal(t *testing.T, f *handlerFixture) handlerAttempt {
	t.Helper()
	data, err := os.ReadFile(handlerAttemptPath(f.c.Base, f.task, f.req.AgentID))
	if err != nil {
		t.Fatal(err)
	}
	var j handlerAttempt
	if err = json.Unmarshal(data, &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func TestHandlerConcurrentRetryAndRenamedSession(t *testing.T) {
	f := newHandlerFixture(t)
	var wg sync.WaitGroup
	results := make(chan api.Agent, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a, err := f.launch(); results <- a; errs <- err }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	run := ""
	session := ""
	for a := range results {
		if run != "" && run != a.RunID {
			t.Fatal("duplicate runs")
		}
		run = a.RunID
		session = a.Session
	}
	f.executions(t, 1)
	agents, err := f.c.ListAgents(context.Background(), f.task)
	if err != nil || len(agents) != 1 {
		t.Fatal(agents, err)
	}
	handlerTestTmux(t, "rename-session", "-t", "="+session, "handler-renamed")
	f.opts.Env["TAILTERM_TOKEN"] = "changed-synthetic-token"
	f.opts.Env["TAILTERM_BRIEFING"] = "updated variable briefing"
	a, err := f.launch()
	if err != nil || a.RunID != run || a.Session != "handler-renamed" {
		t.Fatal(a, err)
	}
	f.executions(t, 1)
	path := handlerAttemptPath(f.c.Base, f.task, f.req.AgentID)
	data, _ := os.ReadFile(path)
	stat, _ := os.Stat(path)
	if strings.Contains(string(data), "synthetic") || strings.Contains(string(data), "briefing") || stat.Mode().Perm() != 0600 {
		t.Fatal("journal leaked execution data or permissions")
	}
	f.opts.Command = "changed execution"
	if _, err = f.launch(); err == nil || !strings.Contains(err.Error(), "settings changed") {
		t.Fatal(err)
	}
}

func TestHandlerLostRegistrationResponseAndExplicitRestart(t *testing.T) {
	f := newHandlerFixture(t)
	f.drop.Store(true)
	if _, err := f.launch(); err == nil {
		t.Fatal("expected lost registration response")
	}
	a, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	f.executions(t, 1)
	_, err = f.c.PostEvent(context.Background(), f.task, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventExited})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.launch(); err == nil || !strings.Contains(err.Error(), "explicit restart") {
		t.Fatal(err)
	}
	f.req.ExpectedRunID = a.RunID
	f.drop.Store(true)
	if _, err = f.launch(); err == nil {
		t.Fatal("expected lost restart response")
	}
	next, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != a.ID || next.RunID == a.RunID {
		t.Fatal("restart changed identity or reused run")
	}
	f.executions(t, 2)
	retry, err := f.launch()
	if err != nil || retry.RunID != next.RunID {
		t.Fatal(retry, err)
	}
	f.executions(t, 2)
}

func TestHandlerFreshIdentityDoesNotReuseLegacyRoleJournal(t *testing.T) {
	f := newHandlerFixture(t)
	oldAgent := api.NewID("agt")
	legacyKey := sha256.Sum256([]byte(f.c.Base + "\x00" + f.task + "\x00database_handler"))
	legacyPath := filepath.Join(relayDir(), fmt.Sprintf("handler-%x.json", legacyKey))
	legacy := handlerAttempt{
		Version: 1,
		Hub:     f.c.Base,
		Task:    f.task,
		Agent:   oldAgent,
		Run:     api.NewID("run"),
		Phase:   "created",
	}
	if err := saveHandlerAttempt(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.launch(); err != nil {
		t.Fatal(err)
	}
	f.executions(t, 1)
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("legacy predecessor journal was changed")
	}
	if _, err = os.Stat(handlerAttemptPath(f.c.Base, f.task, f.req.AgentID)); err != nil {
		t.Fatal("fresh handler journal was not created", err)
	}
}

func TestHandlerAmbiguousAttemptDoesNotReplay(t *testing.T) {
	f := newHandlerFixture(t)
	a, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	f.executions(t, 1)
	handlerTestTmux(t, "kill-session", "-t", "="+a.Session)
	j := handlerJournal(t, f)
	j.Phase = "creating"
	j.Session = nil
	if err = saveHandlerAttempt(handlerAttemptPath(f.c.Base, f.task, f.req.AgentID), j); err != nil {
		t.Fatal(err)
	}
	if _, err = f.launch(); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal(err)
	}
	current, err := f.c.GetAgent(context.Background(), f.task, a.ID)
	if err != nil || current.Status == api.AgentClosed {
		t.Fatal(current, err)
	}
	f.executions(t, 1)
}

func TestHandlerRejectsReusedNameAndRun(t *testing.T) {
	for _, wrongRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "wrong-run"}[wrongRun], func(t *testing.T) {
			f := newHandlerFixture(t)
			a, err := f.launch()
			if err != nil {
				t.Fatal(err)
			}
			f.executions(t, 1)
			handlerTestTmux(t, "rename-session", "-t", "="+a.Session, "original-renamed")
			args := []string{"new-session", "-d", "-s", a.Session}
			if wrongRun {
				args = append(args, "-e", "TAILTERM_HUB="+f.c.Base, "-e", "TAILTERM_TASK="+f.task, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+api.NewID("run"))
			}
			args = append(args, "sleep 300")
			handlerTestTmux(t, args...)
			if _, err = f.launch(); err == nil || !strings.Contains(err.Error(), "occupied") {
				t.Fatal(err)
			}
			handlerTestTmux(t, "has-session", "-t", "="+a.Session)
			handlerTestTmux(t, "has-session", "-t", "=original-renamed")
			f.executions(t, 1)
		})
	}
}

func TestHandlerLockCancellationAndPayload(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	path := handlerAttemptPath("http://fixture.invalid", "tsk_0000000000000000", "agt_0000000000000000")
	unlock, err := handlerLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err = handlerLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	req := api.AddAgentRequest{Name: "handler", Runtime: "codex", Cwd: "/fixture"}
	opts := spawn.Options{Command: "codex 'first'", Env: map[string]string{"TAILTERM_BRIEFING": "first", "TAILTERM_HANDLER_COMMAND": "codex", "TAILTERM_HANDLER_PROMPT": "record bug"}}
	hash := handlerPayload(req, opts)
	opts.Command = "codex 'second'"
	opts.Env["TAILTERM_BRIEFING"] = "second"
	opts.Env["TAILTERM_TOKEN"] = "synthetic"
	if handlerPayload(req, opts) != hash {
		t.Fatal("briefing/token changed identity")
	}
	opts.Env["TAILTERM_HANDLER_PROMPT"] = "record feature"
	if handlerPayload(req, opts) == hash {
		t.Fatal("assignment omitted from execution identity")
	}
}
