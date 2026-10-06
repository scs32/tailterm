package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Agent-session `tt spawn --expected-run-id` (wi_f6c8f47f98ce427a): the exact
// run is permitted only when a handler-authored allocation intent names this
// launcher, agent, run, context and team role. Every test runs against its
// own store in a temp dir and a fake tmux that only logs its arguments.

// exactRunTmux swaps spawn.Tmux for a logging fake. Tests using it must not
// run in parallel.
func exactRunTmux(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(dir, "relay"))
	t.Setenv("TT_TMUX_SOCKET", "tt-exact-run-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	log = filepath.Join(dir, "tmux-args")
	fake := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\nfor a; do if [ \"$a\" = -V ]; then echo 'tmux 3.4'; exit 0; fi; done\nprintf '%s\\n' \"$@\" >> '" + log + "'\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	prior := spawn.Tmux
	spawn.Tmux = fake
	t.Cleanup(func() { spawn.Tmux = prior })
	return log
}

// exactRunTmuxArgs returns the fake tmux's logged arguments, one per element.
func exactRunTmuxArgs(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(data), "\n")
}

func exactRunNewSessions(t *testing.T, log string) int {
	t.Helper()
	n := 0
	for _, arg := range exactRunTmuxArgs(t, log) {
		if arg == "new-session" {
			n++
		}
	}
	return n
}

type exactRunFixture struct {
	c        *api.Client
	task     string
	handler  api.Agent // database handler, the intent's author
	launcher api.Agent // ordinary agent named as the task orchestrator
	item     string
	order    int64
	bundle   string
	agentID  string
	runID    string
	tmuxLog  string
	// loseAgentReply drops the next POST .../agents response after the hub
	// has committed it.
	loseAgentReply *atomic.Bool
	// reportInvalidated makes intent readbacks carry an invalidation time, as
	// the hub reports after a project pause. A real pause would also close
	// the launcher, so the readback alone is altered; the stored intent stays
	// unconsumed and admission is never reached.
	reportInvalidated *atomic.Bool
}

// exactRunCommandTimeout is the floor these tests put under the CLI's fixed
// command deadlines. Each case builds its own hub, and under full-matrix load
// one loopback read has outlasted the 10 s the commands ask for
// (wi_5e804f501074629f).
const exactRunCommandTimeout = 60 * time.Second

// raiseCommandTimeout sets minCommandTimeout for the rest of the test and
// restores the previous value afterwards. Tests using it must not run in
// parallel.
func raiseCommandTimeout(t *testing.T) {
	t.Helper()
	previous := minCommandTimeout
	minCommandTimeout = exactRunCommandTimeout
	t.Cleanup(func() { minCommandTimeout = previous })
}

func TestCommandTimeoutFloor(t *testing.T) {
	if productionMinCommandTimeout != 0 {
		t.Fatalf("default minCommandTimeout = %v, want 0", productionMinCommandTimeout)
	}
	// TestMain raised the floor for the package; check the shipped deadlines
	// without it.
	previous := minCommandTimeout
	minCommandTimeout = productionMinCommandTimeout
	t.Cleanup(func() { minCommandTimeout = previous })
	check := func(requested, want time.Duration) {
		t.Helper()
		c, err := env{hub: "http://127.0.0.1:1"}.client(requested)
		if err != nil {
			t.Fatal(err)
		}
		if c.HTTP.Timeout != want {
			t.Fatalf("client timeout for %v = %v, want %v", requested, c.HTTP.Timeout, want)
		}
		start := time.Now()
		ctx, cancel := ctxTimeout(requested)
		defer cancel()
		deadline, ok := ctx.Deadline()
		if got := deadline.Sub(start); !ok || got < want || got > want+time.Second {
			t.Fatalf("context deadline for %v is %v away (set %v), want %v", requested, got, ok, want)
		}
	}
	check(10*time.Second, 10*time.Second)
	check(2*time.Minute, 2*time.Minute)
	raiseCommandTimeout(t)
	check(10*time.Second, exactRunCommandTimeout)
	check(2*time.Minute, 2*time.Minute)
}

func newExactRunFixture(t *testing.T) *exactRunFixture {
	t.Helper()
	raiseCommandTimeout(t)
	f := &exactRunFixture{tmuxLog: exactRunTmux(t), loseAgentReply: new(atomic.Bool), reportInvalidated: new(atomic.Bool), agentID: api.NewID("agt"), runID: api.NewID("run")}
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	handler := server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "fixture"}, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/agents") && f.loseAgentReply.Swap(false) {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			if recorded.Code != http.StatusCreated {
				t.Errorf("admission not committed before reply loss: %d %s", recorded.Code, recorded.Body.String())
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/allocation-intents/") && f.reportInvalidated.Load() {
			recorded := httptest.NewRecorder()
			handler.ServeHTTP(recorded, r)
			var intent api.AllocationIntent
			if recorded.Code != http.StatusOK || json.Unmarshal(recorded.Body.Bytes(), &intent) != nil {
				t.Errorf("intent readback before invalidation: %d %s", recorded.Code, recorded.Body.String())
				return
			}
			now := time.Now().UTC()
			intent.InvalidatedAt, intent.InvalidatedPauseGeneration = &now, 1
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(intent)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if f.c, err = api.NewClient(srv.URL, exactRunCommandTimeout); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// An ordinary agent may launch a helper only when the task allows it; one
	// extra is the allowance the bug's original launch was granted.
	allowance := 1
	task, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "agent session exact run", Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: &allowance})
	if err != nil {
		t.Fatal(err)
	}
	f.task = task.ID
	if f.handler, err = f.c.AddAgent(ctx, f.task, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Session: "db-handler-session", Role: api.AgentRoleDatabaseHandler, Runtime: "generic", Host: "fixture", Cwd: "/fixture"}); err != nil {
		t.Fatal(err)
	}
	if f.launcher, err = f.c.AddAgent(ctx, f.task, api.AddAgentRequest{Name: "lead", Session: "lead-session", Runtime: "generic", Host: "fixture", Cwd: "/fixture"}); err != nil {
		t.Fatal(err)
	}
	f.item, f.order = realSpawnWorkItemAndOrder(t, f.c, f.task)
	confirmCLIFixtureOrder(t, f.c, f.task, f.item, f.order, f.handler)
	f.bundle = syntheticSpawnContextBundle(t, f.task, f.item, f.order)
	return f
}

func (f *exactRunFixture) env(a api.Agent) env {
	return env{hub: f.c.Base, task: f.task, agent: a.ID, runID: a.RunID}
}

// intent records an allocation intent for the fixture's agent and run,
// authored by author and naming launcher as the agent allowed to launch.
func (f *exactRunFixture) intent(t *testing.T, author, launcher api.Agent, teamRole string) {
	t.Helper()
	args := []string{"--agent-id", f.agentID, "--work-item", f.item, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(f.order),
		"--team-role", teamRole, "--work-context-json", f.bundle, "--expected-run-id", f.runID, "--json"}
	if launcher.ID != author.ID {
		args = append(args, "--launcher-agent-id", launcher.ID, "--launcher-run-id", launcher.RunID)
	}
	if err := cmdAllocationIntentCreate(f.env(author), args); err != nil {
		t.Fatalf("record allocation intent: %v", err)
	}
}

// spawnFlags is the exact prepared launch command, as flag/value pairs so a
// case can change or drop one.
func (f *exactRunFixture) spawnFlags(teamRole string) map[string]string {
	return map[string]string{
		"--name": "verifier", "--run": "codex", "--task": f.task,
		"--agent-id": f.agentID, "--expected-run-id": f.runID,
		"--work-item": f.item, "--work-item-revision": "1", "--work-order-message": fmt.Sprint(f.order),
		"--team-role": teamRole, "--work-context-json": f.bundle,
	}
}

func exactRunArgs(flags map[string]string) []string {
	var args []string
	for _, name := range []string{"--name", "--run", "--task", "--agent-id", "--expected-run-id", "--work-item", "--work-item-revision", "--work-order-message", "--team-role", "--work-context-json", "--replaces-agent",
		"--work-item-task", "--queue-entry", "--queue-cycle", "--queue-revision", "--queue-claimant-agent", "--queue-claimant-run"} {
		if value, ok := flags[name]; ok {
			args = append(args, name, value)
		}
	}
	return args
}

func (f *exactRunFixture) roster(t *testing.T) []api.Agent {
	t.Helper()
	agents, err := f.c.ListAgents(context.Background(), f.task)
	if err != nil {
		t.Fatal(err)
	}
	return agents
}

func TestAgentSessionExactRunSpawnAdmitsMatchingIntent(t *testing.T) {
	for _, tc := range []struct {
		name, teamRole string
		// launcher picks the launching session; the handler is always the author.
		launcher func(f *exactRunFixture) api.Agent
	}{
		{"extra with a separate launcher", api.TeamRoleExtra, func(f *exactRunFixture) api.Agent { return f.launcher }},
		{"member with the author as launcher", api.TeamRoleMember, func(f *exactRunFixture) api.Agent { return f.handler }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactRunFixture(t)
			launcher := tc.launcher(f)
			f.intent(t, f.handler, launcher, tc.teamRole)
			if err := cmdSpawn(f.env(launcher), exactRunArgs(f.spawnFlags(tc.teamRole))); err != nil {
				t.Fatalf("intent-authorized exact-run spawn: %v", err)
			}
			agent, err := f.c.GetAgent(context.Background(), f.task, f.agentID)
			if err != nil {
				t.Fatal(err)
			}
			if agent.ID != f.agentID || agent.RunID != f.runID || agent.ParentAgentID != launcher.ID {
				t.Fatalf("admitted agent=%s run=%s parent=%s, want %s/%s/%s", agent.ID, agent.RunID, agent.ParentAgentID, f.agentID, f.runID, launcher.ID)
			}
			intent, err := f.c.GetAllocationIntent(context.Background(), f.task, f.agentID)
			if err != nil {
				t.Fatal(err)
			}
			if intent.ConsumedAt == nil || intent.ConsumedByRunID != f.runID || intent.LauncherAgentID != launcher.ID || intent.LauncherRunID != launcher.RunID {
				t.Fatalf("intent not consumed by the exact run and launcher: %+v", intent)
			}
			if n := exactRunNewSessions(t, f.tmuxLog); n != 1 {
				t.Fatalf("new-session count = %d, want 1", n)
			}
			joined := "\n" + strings.Join(exactRunTmuxArgs(t, f.tmuxLog), "\n") + "\n"
			for _, want := range []string{"\nTAILTERM_AGENT=" + f.agentID + "\n", "\nTAILTERM_RUN=" + f.runID + "\n"} {
				if !strings.Contains(joined, want) {
					t.Fatalf("tmux launch lacks %q:\n%s", strings.TrimSpace(want), joined)
				}
			}
		})
	}
}

func TestAgentSessionExactRunSpawnRejectsWithoutExactIntent(t *testing.T) {
	const ownerSide = "requires a reserved owner-side item team launch with --agent-id"
	const shape = "requires a fresh item-bound launch with --agent-id, --team-role and this session's run identity"
	for _, tc := range []struct {
		name string
		// noIntent skips recording one; orchestratorAuthored records one
		// authored by the task orchestrator for itself instead of by the handler.
		// invalidated has the hub report the recorded intent as invalidated.
		noIntent, orchestratorAuthored, invalidated bool
		env                                         func(f *exactRunFixture) env
		flags                                       func(f *exactRunFixture, flags map[string]string)
		want                                        string
	}{
		{name: "no intent recorded", noIntent: true, want: "requires a recorded allocation intent"},
		{name: "wrong expected run", flags: func(f *exactRunFixture, flags map[string]string) { flags["--expected-run-id"] = api.NewID("run") }, want: "expected run does not match"},
		{name: "wrong launcher agent", env: func(f *exactRunFixture) env { return f.env(f.handler) }, want: "launcher agent does not match"},
		{name: "right launcher agent with wrong run", env: func(f *exactRunFixture) env {
			e := f.env(f.launcher)
			e.runID = api.NewID("run")
			return e
		}, want: "launcher run does not match"},
		{name: "empty session run", env: func(f *exactRunFixture) env {
			e := f.env(f.launcher)
			e.runID = ""
			return e
		}, want: shape},
		{name: "different context bytes", flags: func(f *exactRunFixture, flags map[string]string) {
			changed := strings.Replace(f.bundle, `"title":"t"`, `"title":"u"`, 1)
			if changed == f.bundle {
				panic("fixture context did not change")
			}
			flags["--work-context-json"] = changed
		}, want: "prepared context digest does not match"},
		{name: "wrong team role", flags: func(f *exactRunFixture, flags map[string]string) { flags["--team-role"] = api.TeamRoleMember }, want: "team role does not match"},
		{name: "wrong work-order message", flags: func(f *exactRunFixture, flags map[string]string) {
			flags["--work-order-message"] = fmt.Sprint(f.order + 1)
		}, want: "work-order message does not match"},
		{name: "wrong item revision", flags: func(f *exactRunFixture, flags map[string]string) { flags["--work-item-revision"] = "2" }, want: "work-item revision does not match"},
		{name: "intent authored by the orchestrator", orchestratorAuthored: true, want: "was not authored by this project's database handler"},
		{name: "intent invalidated", invalidated: true, want: "was invalidated and cannot authorize this launch"},
		// A complete cross-project Queue claim passes the existing Queue flag
		// checks, so the refusal is the agent-session one.
		{name: "queue claim flags set", flags: func(f *exactRunFixture, flags map[string]string) {
			flags["--work-item-task"] = api.NewID("tsk")
			flags["--queue-entry"] = "que_" + strings.TrimPrefix(api.NewID("agt"), "agt_")
			flags["--queue-cycle"], flags["--queue-revision"] = "1", "1"
			flags["--queue-claimant-agent"], flags["--queue-claimant-run"] = f.launcher.ID, f.launcher.RunID
		}, want: shape},
		{name: "replaces-agent set", flags: func(f *exactRunFixture, flags map[string]string) { flags["--replaces-agent"] = f.handler.ID }, want: shape},
		{name: "missing agent id", flags: func(f *exactRunFixture, flags map[string]string) { delete(flags, "--agent-id") }, want: ownerSide},
		{name: "missing work item", flags: func(f *exactRunFixture, flags map[string]string) { delete(flags, "--work-item") }, want: ownerSide},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExactRunFixture(t)
			switch {
			case tc.noIntent:
			case tc.orchestratorAuthored:
				f.intent(t, f.launcher, f.launcher, api.TeamRoleExtra)
			default:
				f.intent(t, f.handler, f.launcher, api.TeamRoleExtra)
			}
			e := f.env(f.launcher)
			if tc.env != nil {
				e = tc.env(f)
			}
			flags := f.spawnFlags(api.TeamRoleExtra)
			if tc.flags != nil {
				tc.flags(f, flags)
			}
			before := len(f.roster(t))
			f.reportInvalidated.Store(tc.invalidated)
			err := cmdSpawn(e, exactRunArgs(flags))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			after := f.roster(t)
			if len(after) != before {
				t.Fatalf("roster grew from %d to %d agents on a refused launch", before, len(after))
			}
			for _, a := range after {
				if a.ID == f.agentID || a.RunID == f.runID {
					t.Fatalf("refused launch still admitted %s/%s", a.ID, a.RunID)
				}
			}
			if !tc.noIntent {
				intent, err := f.c.GetAllocationIntent(context.Background(), f.task, f.agentID)
				if err != nil {
					t.Fatal(err)
				}
				if intent.ConsumedAt != nil {
					t.Fatalf("refused launch consumed the intent: %+v", intent)
				}
			}
			if n := exactRunNewSessions(t, f.tmuxLog); n != 0 {
				t.Fatalf("new-session count = %d, want 0", n)
			}
		})
	}
}

// A hub that returns a matching intent but admits a different run must not
// reach tmux, and the admitted agent is closed.
func TestAgentSessionExactRunSpawnClosesOnAdmittedRunMismatch(t *testing.T) {
	raiseCommandTimeout(t)
	log := exactRunTmux(t)
	taskID, itemID := api.NewID("tsk"), api.NewID("wi")
	handlerID, launcherID, launcherRun := api.NewID("agt"), api.NewID("agt"), api.NewID("run")
	agentID, expectedRun, admittedRun := api.NewID("agt"), api.NewID("run"), api.NewID("run")
	bundle := syntheticSpawnContextBundle(t, taskID, itemID, 7)
	var intent api.AllocationIntent
	var closed []string
	var sentRunID string
	admissions := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(api.TaskDetail{Task: api.Task{ID: taskID, Status: api.TaskOpen, PauseState: api.ProjectPauseActive},
				Agents: []api.Agent{{ID: handlerID, TaskID: taskID, Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Status: api.AgentRunning}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(api.CurrentCapabilities())
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+taskID+"/allocation-intents/"+agentID:
			_ = json.NewEncoder(w).Encode(intent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/"+taskID+"/agents":
			var req api.AddAgentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			admissions++
			sentRunID = req.ExpectedRunID
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.Agent{ID: agentID, TaskID: taskID, RunID: admittedRun, Name: req.Name, Host: req.Host, Session: req.Session, Runtime: req.Runtime, Status: api.AgentStarting})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tasks/"+taskID+"/agents/"+agentID:
			closed = append(closed, r.URL.Query().Get("runId"))
			_ = json.NewEncoder(w).Encode(api.Agent{ID: agentID, TaskID: taskID, RunID: admittedRun, Status: api.AgentClosed})
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	// The digest the CLI computes is over the compacted bundle; record the
	// same one by asking the CLI's own helper.
	compact, err := compactPreparedWorkContext([]byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(compact)
	intent = api.AllocationIntent{
		AgentID: agentID, TargetTaskID: taskID, ItemTaskID: taskID, ItemID: itemID, ItemRevision: 1,
		WorkOrderMessage: api.MessageReference{TaskID: taskID, Seq: 7}, ContextDigest: hex.EncodeToString(digest[:]), TeamRole: api.TeamRoleExtra,
		AuthorAgentID: handlerID, AuthorRunID: api.NewID("run"), ExpectedRunID: expectedRun,
		ExpectedLauncherAgentID: launcherID, ExpectedLauncherRunID: launcherRun,
	}
	e := env{hub: hub.URL, task: taskID, agent: launcherID, runID: launcherRun}
	err = cmdSpawn(e, []string{"--name", "verifier", "--run", "codex", "--task", taskID, "--agent-id", agentID, "--expected-run-id", expectedRun,
		"--work-item", itemID, "--work-item-revision", "1", "--work-order-message", "7", "--team-role", api.TeamRoleExtra, "--work-context-json", bundle})
	if err == nil || !strings.Contains(err.Error(), "run other than the expected") {
		t.Fatalf("error = %v, want the admitted-run mismatch", err)
	}
	if admissions != 1 || sentRunID != "" {
		t.Fatalf("admissions = %d with request expectedRunId %q, want one request omitting the field", admissions, sentRunID)
	}
	if len(closed) != 1 || closed[0] != admittedRun {
		t.Fatalf("close calls = %v, want exactly the admitted run %s", closed, admittedRun)
	}
	if n := exactRunNewSessions(t, log); n != 0 {
		t.Fatalf("new-session count = %d, want 0", n)
	}
}

func TestExpectedRunIDOwnerSideRejectionUnchanged(t *testing.T) {
	const want = "--expected-run-id requires a reserved owner-side item team launch with --agent-id"
	base := []string{"--name", "worker", "--run", "codex", "--task", api.NewID("tsk"), "--hub", "http://127.0.0.1:1", "--expected-run-id", api.NewID("run")}
	for name, extra := range map[string][]string{
		"no work item": {"--agent-id", api.NewID("agt")},
		"no agent id":  {"--work-item", api.NewID("wi"), "--work-item-revision", "1", "--work-order-message", "1", "--work-context-json", "{}"},
		"neither":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			err := cmdSpawn(env{}, append(append([]string(nil), base...), extra...))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func TestAgentSessionExactRunSpawnRetryDoesNotDuplicate(t *testing.T) {
	// exactlyOne asserts the roster holds the fixture's agent once, with the
	// exact run, and nothing else was admitted beyond the handler and launcher.
	exactlyOne := func(t *testing.T, f *exactRunFixture) {
		t.Helper()
		roster := f.roster(t)
		matches := 0
		for _, a := range roster {
			if a.ID == f.agentID && a.RunID == f.runID {
				matches++
			} else if a.ID != f.handler.ID && a.ID != f.launcher.ID {
				t.Fatalf("unexpected agent %s run %s on the roster", a.ID, a.RunID)
			}
		}
		if matches != 1 || len(roster) != 3 {
			t.Fatalf("roster has %d agents and %d exact matches, want 3 and 1", len(roster), matches)
		}
	}
	t.Run("repeat after success", func(t *testing.T) {
		f := newExactRunFixture(t)
		f.intent(t, f.handler, f.launcher, api.TeamRoleExtra)
		args := exactRunArgs(f.spawnFlags(api.TeamRoleExtra))
		if err := cmdSpawn(f.env(f.launcher), args); err != nil {
			t.Fatalf("first launch: %v", err)
		}
		err := cmdSpawn(f.env(f.launcher), args)
		if err == nil || !strings.Contains(err.Error(), "already consumed by run "+f.runID) {
			t.Fatalf("repeat error = %v, want the consumed intent naming run %s", err, f.runID)
		}
		exactlyOne(t, f)
		if n := exactRunNewSessions(t, f.tmuxLog); n != 1 {
			t.Fatalf("new-session count = %d, want 1", n)
		}
	})
	t.Run("lost response", func(t *testing.T) {
		f := newExactRunFixture(t)
		f.intent(t, f.handler, f.launcher, api.TeamRoleExtra)
		args := exactRunArgs(f.spawnFlags(api.TeamRoleExtra))
		f.loseAgentReply.Store(true)
		if err := cmdSpawn(f.env(f.launcher), args); err == nil {
			t.Fatal("fixture did not lose the committed admission reply")
		}
		if f.loseAgentReply.Load() {
			t.Fatal("the launch never reached admission")
		}
		if n := exactRunNewSessions(t, f.tmuxLog); n != 0 {
			t.Fatalf("new-session count after the lost reply = %d, want 0", n)
		}
		err := cmdSpawn(f.env(f.launcher), args)
		if err == nil || !strings.Contains(err.Error(), "already consumed by run "+f.runID) {
			t.Fatalf("retry error = %v, want the consumed intent naming run %s", err, f.runID)
		}
		exactlyOne(t, f)
		if n := exactRunNewSessions(t, f.tmuxLog); n != 0 {
			t.Fatalf("new-session count after the retry = %d, want 0", n)
		}
	})
}
