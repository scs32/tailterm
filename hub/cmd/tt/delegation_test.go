package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestOwnerDelegationCLI(t *testing.T) {
	h := newDelegationHub(t, 0)
	e, c, task, lead := h.agent, h.client, h.task, h.lead
	ctx := context.Background()
	agents, err := c.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.ID == e.agent {
			e.runID = a.RunID
		}
	}
	owner := env{hub: e.hub, task: e.task}
	delegate := env{hub: e.hub, task: e.task, agent: lead.ID, agentName: lead.Name, runID: lead.RunID}

	if _, err := captureCLIOutput(t, func() error { return cmdOwner(e, []string{"delegation", "list"}) }); err == nil || !strings.Contains(err.Error(), "not agent sessions") {
		t.Fatalf("agent session listed windows: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--scope", "everything"})
	}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "30s", "--request-id", "short"})
	}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("short window: %v", err)
	}
	open := []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys", "--reason", "at the dentist", "--request-id", "open-1"}
	out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
	if err != nil || !strings.Contains(out, "open  → lead  decisions-merges-deploys") {
		t.Fatalf("open %q %v", out, err)
	}
	again, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
	if err != nil || again != out {
		t.Fatalf("replay %q %v", again, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "1h", "--request-id", "open-2"})
	}); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("second open: %v", err)
	}

	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Delegation CLI fixture", RequestID: "delegation-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded delegation fixture order"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "owner", "--subject", "Approve the fixture deploy for tonight", "--ask", "Deploy tonight?", "--ref", "category=deploy",
			"--work-item", item.ID, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order.Seq), "--request-id", "deploy-ask"})
	}); err != nil {
		t.Fatal(err)
	}
	owed, err := c.ListOwnerObligations(ctx, task.ID, true)
	if err != nil || len(owed) != 1 {
		t.Fatalf("owner requests %v %+v", err, owed)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(delegate, []string{"answer", owed[0].ID, "--text", "Deploy tonight"})
	}); err == nil || !strings.Contains(err.Error(), "--rationale") {
		t.Fatalf("missing rationale: %v", err)
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdOwner(delegate, []string{"answer", owed[0].ID, "--text", "Deploy tonight", "--rationale", "Tests passed and the window is quiet.", "--request-id", "delegate-answer"})
	}); err != nil || !strings.Contains(out, "closed (answered)") {
		t.Fatalf("delegate answer %q %v", out, err)
	}

	askFile := filepath.Join(t.TempDir(), "ask.json")
	if err := os.WriteFile(askFile, []byte(`{"question":"Which fixture rollout?","category":"decision","options":[{"id":"staged","label":"Staged","description":"A few first."},{"id":"all","label":"All","description":"Everyone."}],"recommendedOptionId":"staged","recommendationReason":"Safer."}`), 0600); err != nil {
		t.Fatal(err)
	}
	e.task = task.ID
	asked, err := captureCLIOutput(t, func() error { return cmdAsk(e, []string{"--request-id", "cli-ask", "--file", askFile, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var decision api.Message
	if err := json.Unmarshal([]byte(asked), &decision); err != nil {
		t.Fatal(err)
	}
	seq := fmt.Sprint(decision.Seq)
	if _, err := captureCLIOutput(t, func() error { return cmdAsk(delegate, []string{"answer", seq, "--option", "staged"}) }); err == nil {
		t.Fatal("delegate answered without a rationale")
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdAsk(delegate, []string{"answer", seq, "--option", "staged", "--rationale", "It is the recommendation and reversible.", "--request-id", "cli-answer"})
	}); err != nil || !strings.Contains(out, "Decision #"+seq+" answered") {
		t.Fatalf("delegate decision %q %v", out, err)
	}

	listed, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var list api.DelegationWindowList
	if err := json.Unmarshal([]byte(listed), &list); err != nil || len(list.Windows) != 1 {
		t.Fatalf("list %q %v", listed, err)
	}
	w := list.Windows[0]
	if w.Reason != "at the dentist" || w.OpenedSource == nil || w.OpenedSource.Kind != "tt" || len(w.Routes) != 2 {
		t.Fatalf("window %+v", w)
	}
	for _, r := range w.Routes {
		if r.AnswerSeq == 0 || r.Rationale == "" || r.AnsweredByName != "lead" {
			t.Fatalf("route %+v", r)
		}
	}
	if out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "close", w.ID, "--reason", "back"}) }); err != nil || !strings.Contains(out, "closed") {
		t.Fatalf("close %q %v", out, err)
	}
	text, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list"}) })
	if err != nil || !strings.Contains(text, "closed") || !strings.Contains(text, "rationale: Tests passed and the window is quiet.") || !strings.Contains(text, "rationale: It is the recommendation and reversible.") {
		t.Fatalf("list after close %q %v", text, err)
	}
}

// moveDelegationClock pins the clock --for is measured from and returns a
// function that advances it. The test must not run in parallel.
func moveDelegationClock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := time.Now()
	old := delegationNow
	delegationNow = func() time.Time { return now }
	t.Cleanup(func() { delegationNow = old })
	return func(d time.Duration) { now = now.Add(d) }
}

// delegationClock is one pinned clock for the CLI and the fixture hub, so a
// stored window's length depends only on what the test does, not on load.
type delegationClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *delegationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *delegationClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// delegationHub is a fixture hub on a delegationClock that counts open
// requests. firstOpenLate moves the clock before the hub stamps the first
// open, as a slow first call (or a hub clock ahead of the CLI's) would.
type delegationHub struct {
	owner, agent env
	client       *api.Client
	task         api.Task
	lead         api.Agent
	clock        *delegationClock
	opens        atomic.Int32
}

func newDelegationHub(t *testing.T, firstOpenLate time.Duration) *delegationHub {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Ten seconds into a minute, so derived per-minute keys hold while a test advances.
	h := &delegationHub{clock: &delegationClock{now: time.Now().Truncate(time.Minute).Add(10 * time.Second)}}
	st.SetClockForTest(h.clock.Now)
	old := delegationNow
	delegationNow = h.clock.Now
	t.Cleanup(func() { delegationNow = old })
	by := api.Caller{Node: "cli-test", User: "owner"}
	if h.task, err = st.CreateTask(ctx, api.CreateTaskRequest{Name: "CLI project", Orchestrator: "lead"}, by); err != nil {
		t.Fatal(err)
	}
	if h.lead, err = st.AddAgent(ctx, h.task.ID, api.AddAgentRequest{Name: "lead", Host: "host", Session: "lead", Runtime: "codex"}, by); err != nil {
		t.Fatal(err)
	}
	handler, err := st.AddAgent(ctx, h.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	hub := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delegation-windows") && h.opens.Add(1) == 1 {
			h.clock.Advance(firstOpenLate)
		}
		hub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	if h.client, err = api.NewClient(srv.URL, 0); err != nil {
		t.Fatal(err)
	}
	h.owner = env{hub: srv.URL, task: h.task.ID}
	h.agent = env{hub: srv.URL, task: h.task.ID, agent: handler.ID, agentName: handler.Name}
	return h
}

func (h *delegationHub) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return captureCLIOutput(t, func() error { return cmdOwner(h.owner, append([]string{"delegation"}, args...)) })
}

// send runs one command and returns how many opens it sent.
func (h *delegationHub) send(t *testing.T, args ...string) (string, error, int) {
	t.Helper()
	before := h.opens.Load()
	out, err := h.run(t, args...)
	return out, err, int(h.opens.Load() - before)
}

func (h *delegationHub) windows(t *testing.T) []api.DelegationWindow {
	t.Helper()
	listed, err := h.run(t, "list", "--json")
	var list api.DelegationWindowList
	if err != nil || json.Unmarshal([]byte(listed), &list) != nil {
		t.Fatalf("list %q %v", listed, err)
	}
	return list.Windows
}

func length(w api.DelegationWindow) time.Duration { return w.EndsAt.Sub(w.CreatedAt) }

// A repeated --for open with an explicit request ID replays across seconds
// and refuses anything else. A first open the hub stamped 4 s late stores a
// window 4 s short: its repeat is refused as a different length (the limit in
// docs/owner-delegation-windows.md), and the refusals still hold.
func TestOwnerDelegationOpenForReplaysAcrossSeconds(t *testing.T) {
	for _, late := range []time.Duration{0, 4 * time.Second} {
		t.Run(fmt.Sprintf("first open %s late", late), func(t *testing.T) {
			h := newDelegationHub(t, late)
			open := []string{"open", "--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys", "--reason", "at the dentist", "--request-id", "open-1"}
			out, err := h.run(t, open...)
			if err != nil || !strings.Contains(out, "open  → lead  decisions-merges-deploys") {
				t.Fatalf("open %q %v", out, err)
			}
			w := h.windows(t)[0]
			if length(w) != 2*time.Hour-late {
				t.Fatalf("window length %s", length(w))
			}
			for _, later := range []time.Duration{1500 * time.Millisecond, 3 * time.Second} {
				h.clock.Advance(later)
				again, err, sent := h.send(t, open...)
				if late == 0 && (err != nil || again != out || sent != 2) {
					t.Fatalf("replay %s later %q %v, %d opens", later, again, err, sent)
				}
				if late != 0 && (err == nil || !strings.Contains(err.Error(), "window "+w.ID+" to lead lasts 1h59m56s, not --for 2h0m0s") || sent != 1) {
					t.Fatalf("repeat of a late open %s later %q %v, %d opens", later, again, err, sent)
				}
			}

			// The same request ID with anything else is still refused. A minute
			// later, so no changed --for lands on the stored end time.
			h.clock.Advance(time.Minute)
			with := func(flag, value string) []string {
				args := append([]string{}, open...)
				for i := range args {
					if args[i] == flag {
						args[i+1] = value
					}
				}
				return args
			}
			for name, args := range map[string][]string{
				"delegate":        with("--delegate", "database"),
				"scope":           with("--scope", "decisions"),
				"reason":          with("--reason", "at lunch"),
				"shorter":         with("--for", "1h"),
				"a minute longer": with("--for", "2h1m"),
				"5s longer":       with("--for", (length(w) + 5*time.Second).String()),
				"5s shorter":      with("--for", (length(w) - 5*time.Second).String()),
			} {
				h.clock.Advance(time.Second)
				out, err, sent := h.send(t, args...)
				if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "request ID was already used") || sent != 1 {
					t.Fatalf("different %s: %q %v, %d opens", name, out, err, sent)
				}
				if name == "5s longer" && !strings.Contains(err.Error(), "window "+w.ID+" to lead lasts "+length(w).String()+", not --for ") {
					t.Fatalf("changed duration does not name the window: %v", err)
				}
			}
			if list, err := h.run(t, "list"); err != nil || list != out {
				t.Fatalf("windows after refusals %q, want only %q (%v)", list, out, err)
			}
		})
	}
}

// An --until open sends the end time it was given, so the clock does not
// matter: a repeat replays and a different end time is refused.
func TestOwnerDelegationOpenUntilReplayUnchanged(t *testing.T) {
	e, _, _, _ := cliWorkItemFixture(t)
	owner := env{hub: e.hub, task: e.task}
	advance := moveDelegationClock(t)
	run := func(until time.Time) (string, error) {
		return captureCLIOutput(t, func() error {
			return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--until", until.Format(time.RFC3339), "--request-id", "until-1"})
		})
	}
	until := time.Now().Add(2 * time.Hour)
	out, err := run(until)
	if err != nil || !strings.Contains(out, "open  → lead  decisions") {
		t.Fatalf("open %q %v", out, err)
	}
	advance(3 * time.Second)
	if again, err := run(until); err != nil || again != out {
		t.Fatalf("replay %q %v", again, err)
	}
	if out, err := run(until.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "request ID was already used") {
		t.Fatalf("different end time: %q %v", out, err)
	}
}

// A repeat after the window was closed reports it closed, never open, for an
// explicit and a derived key, whether the hub replays it itself (same second)
// or the CLI looks it up (later second).
func TestOwnerDelegationOpenReplayClosedWindow(t *testing.T) {
	for _, key := range []string{"explicit", "derived"} {
		t.Run(key, func(t *testing.T) {
			h := newDelegationHub(t, 0)
			open := []string{"open", "--delegate", "lead", "--for", "2h", "--reason", "at the dentist"}
			if key == "explicit" {
				open = append(open, "--request-id", "open-1")
			}
			out, err := h.run(t, open...)
			if err != nil || !strings.Contains(out, "open  → lead") {
				t.Fatalf("open %q %v", out, err)
			}
			id := h.windows(t)[0].ID
			if _, err := h.run(t, "close", id); err != nil {
				t.Fatal(err)
			}
			for _, later := range []time.Duration{0, 2 * time.Second} {
				h.clock.Advance(later)
				again, err, sent := h.send(t, open...)
				if err == nil || strings.Contains(again, "open  →") || !strings.Contains(err.Error(), "window "+id+" to lead closed at ") || !strings.Contains(err.Error(), "no delegation window is open") {
					t.Fatalf("repeat %s later after close: %q %v", later, again, err)
				}
				if sent != 1 {
					t.Fatalf("repeat %s later sent %d opens, want only the refused one", later, sent)
				}
			}
			if w := h.windows(t); len(w) != 1 || w[0].State != "closed" {
				t.Fatalf("windows %+v", w)
			}
		})
	}
}

// Another delegate's open window is not replayed, and the replay sends at
// most one open even with older matching windows.
func TestOwnerDelegationOpenReplayMatchesDelegateOnce(t *testing.T) {
	for _, later := range []time.Duration{0, time.Second} {
		t.Run(fmt.Sprintf("second database window ends %s later", later), func(t *testing.T) {
			h := newDelegationHub(t, 0)
			open := func(delegate, id string, length time.Duration) []string {
				return []string{"open", "--delegate", delegate, "--for", length.String(), "--request-id", id}
			}
			openNew := func(args []string) string {
				if out, err := h.run(t, args...); err != nil {
					t.Fatalf("open %q %v", out, err)
				}
				return h.windows(t)[0].ID
			}
			closeWindow := func(id string) {
				if _, err := h.run(t, "close", id); err != nil {
					t.Fatal(err)
				}
			}
			first := openNew(open("lead", "lead-1", 2*time.Hour))
			closeWindow(first)
			h.clock.Advance(time.Second)
			other := openNew(open("database", "database-1", 2*time.Hour))
			h.clock.Advance(time.Second)
			out, err, sent := h.send(t, open("lead", "lead-1", 2*time.Hour)...)
			if err == nil || strings.Contains(out, "open  →") || !strings.Contains(err.Error(), "window "+first+" to lead closed") || !strings.Contains(err.Error(), "the open delegation window is "+other+" to database") {
				t.Fatalf("lead repeat with the database window open: %q %v", out, err)
			}
			if sent != 1 {
				t.Fatalf("lead repeat sent %d opens, want 1", sent)
			}

			// A closed and an open database window: a repeat of the older
			// request ID resends once, to the newest. With the same end time
			// the hub replays the closed window, which is reported closed;
			// with a later one it refuses. The newer window opens 2 s after the
			// older and 2 s (or 1 s) shorter, inside the explicit tolerance.
			closeWindow(other)
			h.clock.Advance(time.Second)
			newer := open("database", "database-2", 2*time.Hour-2*time.Second+later)
			openNew(newer)
			h.clock.Advance(time.Second)
			out, err, sent = h.send(t, open("database", "database-1", 2*time.Hour)...)
			if err == nil || strings.Contains(out, "open  →") || sent != 2 {
				t.Fatalf("older database repeat: %q %v, %d opens, want the refused one and one replay", out, err, sent)
			}
			if want := "window " + other + " to database closed"; later == 0 && !strings.Contains(err.Error(), want) || later != 0 && !strings.Contains(err.Error(), "409") {
				t.Fatalf("older database repeat: %v", err)
			}
			h.clock.Advance(time.Second)
			if out, err, sent := h.send(t, newer...); err != nil || !strings.Contains(out, "open  → database") || sent != 2 {
				t.Fatalf("newest database repeat: %q %v, %d opens, want the refused one and one replay", out, err, sent)
			}
		})
	}
}

// The 15 second slack is for a derived key, whose request ID already holds
// --for: a first open the hub stamped 8 s late (latency or clock skew)
// replays, while an explicit request ID is refused as a different duration.
func TestOwnerDelegationOpenDerivedKeySlack(t *testing.T) {
	for _, key := range []string{"derived", "explicit"} {
		t.Run(key, func(t *testing.T) {
			h := newDelegationHub(t, 8*time.Second)
			open := []string{"open", "--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys"}
			if key == "explicit" {
				open = append(open, "--request-id", "late-1")
			}
			out, err := h.run(t, open...)
			if err != nil || !strings.Contains(out, "open  → lead") {
				t.Fatalf("open %q %v", out, err)
			}
			w := h.windows(t)[0]
			if length(w) != 2*time.Hour-8*time.Second {
				t.Fatalf("window length %s", length(w))
			}
			h.clock.Advance(2 * time.Second)
			again, err, sent := h.send(t, open...)
			if key == "derived" && (err != nil || again != out || sent != 2) {
				t.Fatalf("derived repeat %q %v, %d opens", again, err, sent)
			}
			if key == "explicit" && (err == nil || !strings.Contains(err.Error(), "window "+w.ID+" to lead lasts 1h59m52s") || sent != 1) {
				t.Fatalf("explicit repeat %q %v, %d opens", again, err, sent)
			}
		})
	}
}
