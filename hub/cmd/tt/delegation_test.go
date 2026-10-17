package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestOwnerDelegationCLI(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
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
	return pinDelegationClock(t, time.Now())
}

// moveDelegationClockSkewed pins the CLI clock skew away from the hub's, on
// whichever side leaves ten seconds before the next minute, so a derived
// per-minute key stays the same while the test advances the clock.
func moveDelegationClockSkewed(t *testing.T, skew time.Duration) func(time.Duration) {
	t.Helper()
	start := time.Now().Add(-skew)
	if start.Second() >= 50 {
		start = start.Add(2 * skew)
	}
	return pinDelegationClock(t, start)
}

func pinDelegationClock(t *testing.T, now time.Time) func(time.Duration) {
	t.Helper()
	old := delegationNow
	delegationNow = func() time.Time { return now }
	t.Cleanup(func() { delegationNow = old })
	return func(d time.Duration) { now = now.Add(d) }
}

func TestOwnerDelegationOpenForReplaysAcrossSeconds(t *testing.T) {
	e, _, _, _ := cliWorkItemFixture(t)
	owner := env{hub: e.hub, task: e.task}
	advance := moveDelegationClock(t)
	run := func(args ...string) (string, error) {
		return captureCLIOutput(t, func() error { return cmdOwner(owner, append([]string{"delegation", "open"}, args...)) })
	}
	open := []string{"--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys", "--reason", "at the dentist", "--request-id", "open-1"}
	out, err := run(open...)
	if err != nil || !strings.Contains(out, "open  → lead  decisions-merges-deploys") {
		t.Fatalf("open %q %v", out, err)
	}
	for _, later := range []time.Duration{1500 * time.Millisecond, 3 * time.Second} {
		advance(later)
		again, err := run(open...)
		if err != nil || again != out {
			t.Fatalf("replay %s later %q %v", later, again, err)
		}
	}

	// The same request ID with anything else is still refused.
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
		"5s longer":       with("--for", "2h5s"),
		"5s shorter":      with("--for", "1h59m55s"),
	} {
		advance(time.Second)
		if out, err := run(args...); err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "request ID was already used") {
			t.Fatalf("different %s: %q %v", name, out, err)
		}
	}
	// A changed duration names the stored window and its length.
	listed, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list", "--json"}) })
	var windows api.DelegationWindowList
	if err != nil || json.Unmarshal([]byte(listed), &windows) != nil || len(windows.Windows) != 1 {
		t.Fatalf("list %q %v", listed, err)
	}
	w := windows.Windows[0]
	if _, err := run(with("--for", "2h5s")...); err == nil || !strings.Contains(err.Error(), "window "+w.ID+" to lead lasts "+w.EndsAt.Sub(w.CreatedAt).Round(time.Second).String()+", not --for 2h0m5s") {
		t.Fatalf("changed duration: %v", err)
	}
	list, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list"}) })
	if err != nil || list != out {
		t.Fatalf("windows after refusals %q, want only %q (%v)", list, out, err)
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

// countDelegationOpens proxies the fixture hub and counts open requests.
func countDelegationOpens(t *testing.T, owner env) (env, func() int) {
	t.Helper()
	target, err := url.Parse(owner.hub)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var opens atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delegation-windows") {
			opens.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	owner.hub = srv.URL
	return owner, func() int { return int(opens.Load()) }
}

func delegationWindows(t *testing.T, owner env) []api.DelegationWindow {
	t.Helper()
	listed, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list", "--json"}) })
	var list api.DelegationWindowList
	if err != nil || json.Unmarshal([]byte(listed), &list) != nil {
		t.Fatalf("list %q %v", listed, err)
	}
	return list.Windows
}

// A repeat after the window was closed reports it closed, never open, for an
// explicit and a derived key, whether the hub replays it itself (same second)
// or the CLI looks it up (later second).
func TestOwnerDelegationOpenReplayClosedWindow(t *testing.T) {
	for _, key := range []string{"explicit", "derived"} {
		t.Run(key, func(t *testing.T) {
			e, _, _, _ := cliWorkItemFixture(t)
			owner, opens := countDelegationOpens(t, env{hub: e.hub, task: e.task})
			open := []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--reason", "at the dentist"}
			var advance func(time.Duration)
			if key == "explicit" {
				open = append(open, "--request-id", "open-1")
				advance = moveDelegationClock(t)
			} else {
				advance = moveDelegationClockSkewed(t, 5*time.Second)
			}
			out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
			if err != nil || !strings.Contains(out, "open  → lead") {
				t.Fatalf("open %q %v", out, err)
			}
			id := delegationWindows(t, owner)[0].ID
			if _, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "close", id}) }); err != nil {
				t.Fatal(err)
			}
			for _, later := range []time.Duration{0, 2 * time.Second} {
				advance(later)
				before := opens()
				again, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
				if err == nil || strings.Contains(again, "open  →") || !strings.Contains(err.Error(), "window "+id+" to lead closed at ") || !strings.Contains(err.Error(), "no delegation window is open") {
					t.Fatalf("repeat %s later after close: %q %v", later, again, err)
				}
				if sent := opens() - before; sent != 1 {
					t.Fatalf("repeat %s later sent %d opens, want only the refused one", later, sent)
				}
			}
			if w := delegationWindows(t, owner); len(w) != 1 || w[0].State != "closed" {
				t.Fatalf("windows %+v", w)
			}
		})
	}
}

// Another delegate's open window is not replayed, and the replay sends at
// most one open even with older matching windows. Each open pins the clock
// so stored lengths match --for; each repeat advances it a second.
func TestOwnerDelegationOpenReplayMatchesDelegateOnce(t *testing.T) {
	for _, later := range []time.Duration{0, time.Second} {
		t.Run(fmt.Sprintf("second database window ends %s later", later), func(t *testing.T) {
			e, _, _, _ := cliWorkItemFixture(t)
			owner, opens := countDelegationOpens(t, env{hub: e.hub, task: e.task})
			open := func(delegate, id string) (string, error) {
				return captureCLIOutput(t, func() error {
					return cmdOwner(owner, []string{"delegation", "open", "--delegate", delegate, "--for", "2h", "--request-id", id})
				})
			}
			openAt := func(at time.Time, delegate, id string) string {
				pinDelegationClock(t, at)
				if out, err := open(delegate, id); err != nil {
					t.Fatalf("open %s %q %v", id, out, err)
				}
				return delegationWindows(t, owner)[0].ID
			}
			closeWindow := func(id string) {
				if _, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "close", id}) }); err != nil {
					t.Fatal(err)
				}
			}
			repeat := func(delegate, id string) (string, error, int) {
				advance := moveDelegationClock(t)
				advance(2 * time.Second)
				before := opens()
				out, err := open(delegate, id)
				return out, err, opens() - before
			}
			first := openAt(time.Now(), "lead", "lead-1")
			closeWindow(first)
			at := time.Now()
			other := openAt(at, "database", "database-1")
			out, err, sent := repeat("lead", "lead-1")
			if err == nil || strings.Contains(out, "open  →") || !strings.Contains(err.Error(), "window "+first+" to lead closed") || !strings.Contains(err.Error(), "the open delegation window is "+other+" to database") {
				t.Fatalf("lead repeat with the database window open: %q %v", out, err)
			}
			if sent != 1 {
				t.Fatalf("lead repeat sent %d opens, want 1", sent)
			}

			// A closed and an open database window: a repeat of the older
			// request ID resends once, to the newest. With the same end time
			// the hub replays the closed window, which is reported closed;
			// with a later one it refuses.
			closeWindow(other)
			openAt(at.Add(later), "database", "database-2")
			out, err, sent = repeat("database", "database-1")
			if err == nil || strings.Contains(out, "open  →") || sent != 2 {
				t.Fatalf("older database repeat: %q %v, %d opens, want the refused one and one replay", out, err, sent)
			}
			if want := "window " + other + " to database closed"; later == 0 && !strings.Contains(err.Error(), want) || later != 0 && !strings.Contains(err.Error(), "409") {
				t.Fatalf("older database repeat: %v", err)
			}
			if out, err, sent := repeat("database", "database-2"); err != nil || !strings.Contains(out, "open  → database") || sent != 2 {
				t.Fatalf("newest database repeat: %q %v, %d opens, want the refused one and one replay", out, err, sent)
			}
		})
	}
}

// The 15 second slack is for a derived key, whose request ID already holds
// --for: with the CLI clock 8 s behind the hub it replays, while an explicit
// request ID with the same skew is refused as a different duration.
func TestOwnerDelegationOpenDerivedKeySlack(t *testing.T) {
	for _, key := range []string{"derived", "explicit"} {
		t.Run(key, func(t *testing.T) {
			e, _, _, _ := cliWorkItemFixture(t)
			owner, opens := countDelegationOpens(t, env{hub: e.hub, task: e.task})
			advance := moveDelegationClockSkewed(t, 8*time.Second)
			open := []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys"}
			if key == "explicit" {
				open = append(open, "--request-id", "skewed-1")
			}
			out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
			if err != nil || !strings.Contains(out, "open  → lead") {
				t.Fatalf("open %q %v", out, err)
			}
			w := delegationWindows(t, owner)[0]
			if off := (w.EndsAt.Sub(w.CreatedAt) - 2*time.Hour).Abs(); off < 6*time.Second || off > 11*time.Second {
				t.Fatalf("window length %s, want about 8 s off 2h", w.EndsAt.Sub(w.CreatedAt))
			}
			advance(2 * time.Second)
			before := opens()
			again, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
			if key == "derived" && (err != nil || again != out || opens()-before != 2) {
				t.Fatalf("derived repeat %q %v, %d opens", again, err, opens()-before)
			}
			if key == "explicit" && (err == nil || !strings.Contains(err.Error(), "window "+w.ID+" to lead lasts ") || opens()-before != 1) {
				t.Fatalf("explicit repeat %q %v, %d opens", again, err, opens()-before)
			}
		})
	}
}
