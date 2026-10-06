package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/bridge"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

// helperFixture is a temporary hub, a private tmux server with one owner
// session running a fake claude process, and a synthetic Claude transcript
// under a temporary HOME. Nothing touches the live hub or the owner's tmux.
type helperFixture struct {
	owner  env
	c      *api.Client
	task   api.Task
	lead   api.Agent
	sock   string
	bin    string
	thread string
}

func newHelperFixture(t *testing.T) helperFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	e, c, task, lead := cliWorkItemFixture(t)
	home := t.TempDir()
	t.Setenv("TAILTERM_TOKEN", "")
	t.Setenv("TAILTERM_HUB", "")
	t.Setenv("TAILTERM_TASK", "")
	t.Setenv("TAILTERM_AGENT", "")
	t.Setenv("TAILTERM_RUN", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "relay"))
	sock := "tt-helper-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	t.Setenv("TT_TMUX_SOCKET", sock)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	thread := "00000000-0000-4000-8000-" + strings.TrimPrefix(api.NewID("agt"), "agt_")[:12]
	writeHelperTranscript(t, home, thread)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", thread)
	// ps reports a symlink's own name, so this is a "claude" process.
	bin := t.TempDir()
	if err := os.Symlink("/bin/sleep", filepath.Join(bin, "claude")); err != nil {
		t.Fatal(err)
	}
	f := helperFixture{owner: env{hub: e.hub, task: task.ID}, c: c, task: task, lead: lead, sock: sock, bin: bin, thread: thread}
	f.session(t, "owner")
	return f
}

func writeHelperTranscript(t *testing.T, home, thread string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-work-tailterm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, thread+".jsonl"), []byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f helperFixture) tmux(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", append([]string{"-L", f.sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// session starts a one-pane session running the fake claude and points
// $TMUX_PANE at it, as if tt ran inside that Claude Code session.
func (f helperFixture) session(t *testing.T, name string) (id, created string) {
	t.Helper()
	f.tmux(t, "new-session", "-d", "-s", name, "-x", "200", "-y", "50", filepath.Join(f.bin, "claude")+" 300")
	pane := f.tmux(t, "display-message", "-p", "-t", name+":", "#{pane_id}")
	t.Setenv("TMUX", "/private/fixture/"+f.sock+",1,0")
	t.Setenv("TMUX_PANE", pane)
	parts := strings.Fields(f.tmux(t, "display-message", "-p", "-t", name+":", "#{session_id} #{session_created}"))
	return parts[0], parts[1]
}

func (f helperFixture) tags(t *testing.T, session string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(f.tmux(t, "show-environment", "-t", session), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "TAILTERM_") {
			out[k] = v
		}
	}
	return out
}

func (f helperFixture) register(t *testing.T, e env, args ...string) (api.OwnerActionResult, error) {
	t.Helper()
	out, err := captureCLIOutput(t, func() error {
		return cmdHelper(e, append([]string{"register", "--task", f.task.ID, "--json"}, args...))
	})
	var result api.OwnerActionResult
	if err == nil {
		if jsonErr := json.Unmarshal([]byte(out), &result); jsonErr != nil {
			t.Fatalf("register output %q: %v", out, jsonErr)
		}
	}
	return result, err
}

func (f helperFixture) mustRegister(t *testing.T) api.OwnerActionResult {
	t.Helper()
	out, err := f.register(t, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readBinding(t *testing.T, hub, agent string) (runtimeBinding, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(runtimeBinding{Hub: hub, Agent: agent})+".binding.json"))
	if os.IsNotExist(err) {
		return runtimeBinding{}, false
	}
	var b runtimeBinding
	if err != nil || json.Unmarshal(data, &b) != nil {
		t.Fatalf("binding %v %s", err, data)
	}
	return b, true
}

func TestHelperRegister(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	out := f.mustRegister(t)
	a := *out.Agent
	if a.Role != api.AgentRoleOwnerHelper || a.Session != "owner" || a.Host != spawn.Host() || a.Runtime != "claude" || out.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("agent %+v %+v", a, out.Registration)
	}
	tags := f.tags(t, "owner")
	want := map[string]string{"TAILTERM_ROLE": api.AgentRoleOwnerHelper, "TAILTERM_HUB": f.owner.hub, "TAILTERM_TASK": f.task.ID, "TAILTERM_AGENT": a.ID, "TAILTERM_RUN": a.RunID}
	for k, v := range want {
		if tags[k] != v {
			t.Fatalf("tag %s=%q, want %q (%v)", k, tags[k], v, tags)
		}
	}
	b, ok := readBinding(t, f.owner.hub, a.ID)
	if !ok || b.Thread != f.thread || b.Role != api.AgentRoleOwnerHelper || b.Session != "owner" || b.Run != a.RunID || b.Runtime != "claude" || !validBinding(b) {
		t.Fatalf("binding %+v", b)
	}
	if b.Thread == mustClaudeSessionID(t, a.ID) {
		t.Fatal("binding used the derived spawn session ID")
	}
	path := ownerHelperPath(f.owner.hub, f.task.ID)
	info, err := os.Stat(path)
	data, _ := os.ReadFile(path)
	if err != nil || info.Mode().Perm() != 0600 || strings.Contains(strings.ToLower(string(data)), "token") || strings.Contains(string(data), "pendingRequestId") {
		t.Fatalf("helper file %v %v %s", err, info.Mode(), data)
	}
	// Local sessions read the role; cleanup keeps no record for this session.
	sessions, err := rememberSessions(ctx, f.owner.hub)
	if err != nil || len(sessions) != 1 || sessions[0].Role != api.AgentRoleOwnerHelper {
		t.Fatalf("sessions %+v %v", sessions, err)
	}
	if records, _ := filepath.Glob(filepath.Join(relayDir(), "*.session.json")); len(records) != 0 {
		t.Fatalf("cleanup records for the helper session: %v", records)
	}

	// Refusals: a foreign agent session, outside Claude, no transcript, a bad session name.
	if _, err := f.register(t, env{hub: f.owner.hub, task: f.task.ID, agent: f.lead.ID, runID: f.lead.RunID}); err == nil || !strings.Contains(err.Error(), "not an agent session") {
		t.Fatalf("foreign agent: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	if _, err := f.register(t, f.owner); err == nil || !strings.Contains(err.Error(), "Claude Code session") {
		t.Fatalf("outside Claude: %v", err)
	}
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "00000000-0000-4000-8000-00000000dead")
	if _, err := f.register(t, f.owner); err == nil || !strings.Contains(err.Error(), "transcript") {
		t.Fatalf("no transcript: %v", err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", f.thread)
	f.tmux(t, "rename-session", "-t", "owner", "owner x")
	if _, err := f.register(t, f.owner); err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("bad session name: %v", err)
	}
	f.tmux(t, "rename-session", "-t", "owner x", "owner")
	if current, _ := f.c.GetAgent(ctx, f.task.ID, a.ID); current.RunID != a.RunID {
		t.Fatal("a refused registration changed the helper")
	}
	// A pane that inherited the helper's own identity may re-register.
	again, err := f.register(t, env{hub: f.owner.hub, task: f.task.ID, agent: a.ID, runID: a.RunID})
	if err != nil || again.Agent.ID != a.ID || again.Registration.Mode != api.OwnerHelperReplaced || again.Registration.PreviousRunID != a.RunID {
		t.Fatalf("inherited helper identity: %+v %v", again, err)
	}
	if f.tags(t, "owner")["TAILTERM_RUN"] != again.Agent.RunID {
		t.Fatal("tags kept the replaced run")
	}
	// Outside tmux: registered without wake.
	t.Setenv("TMUX", "")
	text, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"register", "--task", f.task.ID}) })
	if err != nil || !strings.Contains(text, "registered without wake") {
		t.Fatalf("outside tmux %q %v", text, err)
	}
	if _, ok := readBinding(t, f.owner.hub, a.ID); ok {
		t.Fatal("binding kept outside tmux")
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_AGENT"] != "" {
		t.Fatalf("previous session kept its tags: %v", tags)
	}
}

func mustClaudeSessionID(t *testing.T, agent string) string {
	t.Helper()
	id, err := claudeSessionID(agent)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHelperEnv(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	if _, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"env", "--task", f.task.ID}) }); err == nil {
		t.Fatal("env before registration")
	}
	a := *f.mustRegister(t).Agent
	owner := f.owner
	owner.token = "secret-token-" + strings.Repeat("x", 24)
	out, err := captureCLIOutput(t, func() error { return cmdHelper(owner, []string{"env", "--task", f.task.ID}) })
	if err != nil || !strings.HasPrefix(out, "export ") || !strings.Contains(out, "TAILTERM_AGENT="+a.ID) || !strings.Contains(out, "TAILTERM_RUN="+a.RunID) ||
		!strings.Contains(out, "TAILTERM_TASK="+f.task.ID) || strings.Contains(out, "secret-token") || strings.Contains(out, "TOKEN") {
		t.Fatalf("env %q %v", out, err)
	}
	// Registered again elsewhere (another host's run): this host's identity is stale.
	if _, err := f.c.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "elsewhere", Session: "other", Runtime: "claude", RequestID: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdHelper(owner, []string{"env", "--task", f.task.ID}) }); err == nil || !strings.Contains(err.Error(), "registered again") {
		t.Fatalf("stale env: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdHelper(owner, []string{"inbox", "--task", f.task.ID}) }); err == nil {
		t.Fatal("stale inbox")
	}
}

func TestHelperInbox(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	if _, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: a.ID, Text: "helper fixture directed text"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"inbox", "--task", f.task.ID}) })
	if err != nil || !strings.Contains(out, "helper fixture directed text") {
		t.Fatalf("inbox %q %v", out, err)
	}
	if after, _ := f.c.GetAgent(ctx, f.task.ID, a.ID); after.Unread != 0 {
		t.Fatalf("not marked read: %d", after.Unread)
	}
}

func TestHelperRegisterAfterClose(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	first := *f.mustRegister(t).Agent
	if _, err := f.c.CloseAgent(ctx, f.task.ID, first.ID, first.RunID); err != nil {
		t.Fatal(err)
	}
	second := f.mustRegister(t)
	if second.Agent.ID == first.ID || second.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("after close %+v", second)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_AGENT"] != second.Agent.ID || tags["TAILTERM_RUN"] != second.Agent.RunID {
		t.Fatalf("tags %v", tags)
	}
	if b, ok := readBinding(t, f.owner.hub, second.Agent.ID); !ok || b.Run != second.Agent.RunID {
		t.Fatalf("binding %+v", b)
	}
	if _, ok := readBinding(t, f.owner.hub, first.ID); ok {
		t.Fatal("closed helper's binding kept")
	}
	if state, _ := loadOwnerHelperFile(f.owner.hub, f.task.ID); state.Agent != second.Agent.ID || state.Run != second.Agent.RunID {
		t.Fatalf("helper file %+v", state)
	}
}

func TestHelperRegisterAfterExit(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	first := *f.mustRegister(t).Agent
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: first.ID, RunID: first.RunID}); err != nil {
		t.Fatal(err)
	}
	second := f.mustRegister(t)
	if second.Agent.ID != first.ID || second.Agent.RunID == first.RunID || second.Registration.Mode != api.OwnerHelperReattached {
		t.Fatalf("after exit %+v", second)
	}
	// A deliberate re-register of a live helper replaces the run.
	third := f.mustRegister(t)
	if third.Agent.ID != first.ID || third.Agent.RunID == second.Agent.RunID || third.Registration.Mode != api.OwnerHelperReplaced || third.Replay {
		t.Fatalf("re-register %+v", third)
	}
}

func TestHelperRegisterUncertainRetry(t *testing.T) {
	f := newHelperFixture(t)
	exerciseHelperUncertainRetry(t, f)
}

func TestHelperCodexUncertainRetry(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	exerciseHelperUncertainRetry(t, f)
}

func exerciseHelperUncertainRetry(t *testing.T, f helperFixture) {
	t.Helper()
	ctx := context.Background()
	upstream, err := url.Parse(f.owner.hub)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var drop atomic.Bool
	drop.Store(true)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/owner-helper") && drop.Swap(false) {
			// The hub commits the registration; the reply is lost.
			body, _ := io.ReadAll(r.Body)
			resp, err := http.Post(f.owner.hub+r.URL.Path, "application/json", bytes.NewReader(body))
			if err == nil {
				resp.Body.Close()
			}
			panic(http.ErrAbortHandler)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	e := env{hub: front.URL, task: f.task.ID}
	if _, err := f.register(t, e); err == nil {
		t.Fatal("lost reply reported success")
	}
	state, _ := loadOwnerHelperFile(front.URL, f.task.ID)
	if state.PendingRequest == "" || state.Agent != "" {
		t.Fatalf("pending not kept: %+v", state)
	}
	agents, _ := f.c.ListAgents(ctx, f.task.ID)
	var committed api.Agent
	for _, a := range agents {
		if a.Role == api.AgentRoleOwnerHelper {
			committed = a
		}
	}
	if committed.ID == "" {
		t.Fatal("the hub did not commit the first attempt")
	}
	// The rerun replays the same request: no second registration.
	out, err := f.register(t, e)
	if err != nil || !out.Replay || out.Agent.RunID != committed.RunID || out.Registration.RequestID != state.PendingRequest {
		t.Fatalf("rerun %+v %v", out, err)
	}
	if now, _ := f.c.GetAgent(ctx, f.task.ID, committed.ID); now.RunID != committed.RunID {
		t.Fatal("rerun registered again")
	}
	if state, _ = loadOwnerHelperFile(front.URL, f.task.ID); state.PendingRequest != "" || state.Agent != committed.ID {
		t.Fatalf("pending not cleared: %+v", state)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_RUN"] != committed.RunID {
		t.Fatalf("tags %v", tags)
	}
}

func TestHelperTagsSetAtomically(t *testing.T) {
	f := newHelperFixture(t)
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	wrapDir := t.TempDir()
	log := filepath.Join(wrapDir, "tmux.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n", spawn.ShellQuote(log), spawn.ShellQuote(real))
	if err := os.WriteFile(filepath.Join(wrapDir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := *f.mustRegister(t).Agent
	data, _ := os.ReadFile(log)
	var tagging []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "set-environment") {
			tagging = append(tagging, line)
		}
	}
	sessionID := f.tmux(t, "display-message", "-p", "-t", "owner:", "#{session_id}")
	want := "-L " + f.sock + " set-environment -t " + sessionID + " TAILTERM_ROLE owner_helper ; set-environment -t " + sessionID + " TAILTERM_HUB " + f.owner.hub
	if len(tagging) != 1 || !strings.HasPrefix(tagging[0], want) || !strings.Contains(tagging[0], "TAILTERM_AGENT "+a.ID) || !strings.HasSuffix(tagging[0], "TAILTERM_RUN "+a.RunID) {
		t.Fatalf("tag commands %q", tagging)
	}
}

func TestHelperReregisterClearsOldSessionTags(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	oldID, oldCreated := f.tmux(t, "display-message", "-p", "-t", "owner:", "#{session_id}"), f.tmux(t, "display-message", "-p", "-t", "owner:", "#{session_created}")
	a := *f.mustRegister(t).Agent
	f.session(t, "owner-b")
	second := *f.mustRegister(t).Agent
	if second.ID != a.ID || second.Session != "owner-b" {
		t.Fatalf("second %+v", second)
	}
	if tags := f.tags(t, "owner"); len(tags) != 0 {
		t.Fatalf("old session kept tags: %v", tags)
	}
	if tags := f.tags(t, "owner-b"); tags["TAILTERM_AGENT"] != a.ID || tags["TAILTERM_ROLE"] != api.AgentRoleOwnerHelper {
		t.Fatalf("new session tags %v", tags)
	}
	// Tags that name another agent, or another session under the same ID, are never touched.
	f.tmux(t, "set-environment", "-t", "owner", "TAILTERM_ROLE", api.AgentRoleOwnerHelper)
	f.tmux(t, "set-environment", "-t", "owner", "TAILTERM_AGENT", "agt_00000000000000aa")
	if err := clearHelperTags(ctx, oldID, oldCreated, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := clearHelperTags(ctx, oldID, "1", "agt_00000000000000aa"); err != nil {
		t.Fatal(err)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_AGENT"] != "agt_00000000000000aa" {
		t.Fatalf("foreign tags touched: %v", tags)
	}
}

func TestOwnerHelperDelegatedAnswerCLI(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	f.mustRegister(t)
	if out, err := captureCLIOutput(t, func() error {
		return cmdOwner(f.owner, []string{"delegation", "open", "--delegate", api.DefaultOwnerHelperName, "--for", "3h", "--scope", "decisions", "--request-id", "helper-window"})
	}); err != nil || !strings.Contains(out, api.DefaultOwnerHelperName) {
		t.Fatalf("open %q %v", out, err)
	}
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner helper delegation fixture", RequestID: "helper-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "Bounded helper fixture order"})
	if err != nil {
		t.Fatal(err)
	}
	lead := env{hub: f.owner.hub, task: f.task.ID, agent: f.lead.ID, agentName: f.lead.Name, runID: f.lead.RunID}
	ask := func(key, expected string) api.Obligation {
		t.Helper()
		args := []string{"--kind", "request", "--to", "owner", "--subject", "Choose the helper fixture order for tonight", "--ask", "Which order?",
			"--work-item", item.ID, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order.Seq), "--request-id", key}
		if expected != "" {
			args = append(args, "--expected-answer", expected)
		}
		before, err := f.c.ListOwnerObligations(ctx, f.task.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := captureCLIOutput(t, func() error { return cmdSend(lead, args) }); err != nil {
			t.Fatal(err)
		}
		after, err := f.c.ListOwnerObligations(ctx, f.task.ID, false)
		if err != nil || len(after) != len(before)+1 {
			t.Fatalf("owner requests %v %d -> %d", err, len(before), len(after))
		}
		seen := map[string]bool{}
		for _, o := range before {
			seen[o.ID] = true
		}
		for _, o := range after {
			if !seen[o.ID] {
				return o
			}
		}
		t.Fatal("new owner request missing")
		return api.Obligation{}
	}
	o := ask("helper-ask", "")
	helper, a, err := verifiedHelper(f.owner, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The routed request reached the helper as a directed notice.
	msgs, err := f.c.ListMessages(ctx, f.task.ID, 0, a.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range msgs {
		if m.To == a.ID && m.Envelope != nil && m.Envelope.Kind == api.EnvelopeKindNotice && m.Envelope.Refs["delegation"] != "" {
			notices++
		}
	}
	if notices != 2 { // the window notice and the routed request
		t.Fatalf("helper notices %d", notices)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdOwner(helper, []string{"answer", o.ID, "--text", "Staged first"}) }); err == nil {
		t.Fatal("helper answered without a rationale")
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdOwner(helper, []string{"answer", o.ID, "--text", "Staged first", "--rationale", "Staged limits the blast radius.", "--request-id", "helper-answer"})
	}); err != nil || !strings.Contains(out, "closed (answered)") {
		t.Fatalf("helper answer %q %v", out, err)
	}
	// Matrix approvals stay with the owner.
	matrix := ask("helper-matrix", "verification-matrix-approval:"+strings.Repeat("d", 64))
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(helper, []string{"answer", matrix.ID, "--approve", "--rationale", "because", "--request-id", "helper-matrix-answer"})
	}); err == nil {
		t.Fatal("helper answered a matrix approval")
	}
}

func TestCleanupSkipsOwnerHelper(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-helper-cleanup-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	ctx := context.Background()
	defer startupTmux(ctx, "kill-server")
	task := api.Task{ID: api.NewID("tsk"), Status: api.TaskOpen}
	a := api.Agent{ID: api.NewID("agt"), TaskID: task.ID, RunID: api.NewID("run"), Session: "owner", Role: api.AgentRoleOwnerHelper, Status: api.AgentClosed, CleanupDone: true}
	acks := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			acks++
		}
		json.NewEncoder(w).Encode(api.TaskDetail{Task: task, Agents: []api.Agent{a}})
	}))
	defer hub.Close()
	e := env{hub: hub.URL}
	if _, err := startupTmux(ctx, "new-session", "-d", "-s", "owner", "-e", "TAILTERM_ROLE=owner_helper", "-e", "TAILTERM_HUB="+e.hub, "-e", "TAILTERM_TASK="+task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+a.RunID, "sleep 300"); err != nil {
		t.Fatal(err)
	}
	// An older build's record for the session still never stops it.
	sessions, err := localSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Role != api.AgentRoleOwnerHelper {
		t.Fatalf("sessions %+v %v", sessions, err)
	}
	legacy := sessions[0]
	legacy.Role = ""
	if err := writePrivateJSON(legacy.path(), legacy); err != nil {
		t.Fatal(err)
	}
	for _, selected := range [][]string{nil, {a.ID}} {
		if _, err := cleanupSessions(ctx, e, task.ID, selected); err != nil {
			t.Fatal(err)
		}
		if _, err := startupTmux(ctx, "has-session", "-t", "=owner"); err != nil {
			t.Fatal("cleanup killed the owner helper's session")
		}
	}
	if err := stopOwnedSession(ctx, legacy); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("stop helper session: %v", err)
	}
	if _, err := startupTmux(ctx, "has-session", "-t", "=owner"); err != nil || acks != 0 {
		t.Fatalf("helper session stopped or cleanup acknowledged (%d)", acks)
	}
}

func TestRetryClaudeBindingSkipsOwnerHelper(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	s := ownedSession{ID: "$1", Created: "100", Name: "owner", Hub: "http://hub.test", Task: api.NewID("tsk"), Agent: api.NewID("agt"), Run: api.NewID("run"), Role: api.AgentRoleOwnerHelper}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Agent{ID: s.Agent, TaskID: s.Task, RunID: s.Run, Session: s.Name, Runtime: "claude", Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning})
	}))
	defer hub.Close()
	c, err := api.NewClient(hub.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := retryClaudeBinding(context.Background(), s, c); err == nil {
		t.Fatal("the relay rebound the owner helper")
	}
	if paths, _ := filepath.Glob(filepath.Join(relayDir(), "*.binding.json")); len(paths) != 0 {
		t.Fatalf("binding written: %v", paths)
	}
}

func TestOwnerHelperTeamCountAndOfflineLabel(t *testing.T) {
	agents := []api.Agent{{Name: "lead"}, {Name: "builder"}, {Name: "db", Role: api.AgentRoleDatabaseHandler}, {Name: api.DefaultOwnerHelperName, Role: api.AgentRoleOwnerHelper}}
	if got := ordinaryTeamMemberCount(agents, "lead"); got != 2 {
		t.Fatalf("team members %d", got)
	}
	helper := api.Agent{ID: api.NewID("agt"), Name: api.DefaultOwnerHelperName, Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, Activity: &api.AgentActivity{State: "unknown"}}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.AgentList{Agents: []api.Agent{helper}})
	}))
	defer hub.Close()
	out, err := captureCLIOutput(t, func() error { return cmdAgents(env{hub: hub.URL, task: api.NewID("tsk")}, nil) })
	if err != nil || !strings.Contains(out, "role=owner_helper activity=offline") {
		t.Fatalf("agents %q %v", out, err)
	}
	helper.Online = true
	if helperOffline(helper) {
		t.Fatal("online helper shown offline")
	}
}

// wi_66b40c00f6e5eb03, order #24191 amended by #24238: final replies
// settle the owner obligation; interim replies keep it open with progress.
func TestHelperReplyClosesOwnerObligation(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	asked, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, Text: "Please answer this owner question"})
	if err != nil {
		t.Fatal(err)
	}
	answer := "**Answer**\n\n- first\n- second\n\n```sh\necho synthetic\n```"
	if _, err := captureCLIOutput(t, func() error {
		return cmdHelper(f.owner, []string{"reply", "--task", f.task.ID, fmt.Sprint(asked.Seq), "--text", answer})
	}); err != nil {
		t.Fatal(err)
	}
	obligations, err := f.c.ListObligations(ctx, f.task.ID, a.ID, a.RunID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obligations {
		if o.MessageSeq == asked.Seq {
			if o.State != api.ObligationClosed || o.Outcome != api.OutcomeAnswered || o.OutcomeSeq == 0 {
				t.Fatalf("final helper reply left owner obligation unsettled: %+v", o)
			}
			return
		}
	}
	t.Fatal("owner obligation missing")
}

func TestHelperReplyInterimRecordsProgress(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	asked, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, Text: "Please investigate this owner question"})
	if err != nil {
		t.Fatal(err)
	}
	text := "Working on it\n\n- first step finished"
	reply := func(interim bool) (string, error) {
		args := []string{"reply", "--task", f.task.ID, fmt.Sprint(asked.Seq), "--text", text}
		if interim {
			args = append(args, "--interim")
		}
		return captureCLIOutput(t, func() error { return cmdHelper(f.owner, args) })
	}
	first, err := reply(true)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := reply(true); err != nil || again != first {
		t.Fatalf("interim retry = %q %v, want %q", again, err, first)
	}
	obligations, err := f.c.ListObligations(ctx, f.task.ID, a.ID, a.RunID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range obligations {
		if o.MessageSeq == asked.Seq {
			found = true
			if o.State != api.ObligationWorking || o.LastProgressAt == nil || o.Outcome != "" || o.ClosedAt != nil {
				t.Fatalf("interim reply did not record open progress: %+v", o)
			}
		}
	}
	if !found {
		t.Fatal("owner obligation missing")
	}
	// The same text can be a final answer: interim and final use distinct
	// retry identities. Replaying the earlier interim cannot reopen it.
	if final, err := reply(false); err != nil || final == first {
		t.Fatalf("final reply = %q %v, interim = %q", final, err, first)
	}
	if again, err := reply(true); err != nil || again != first {
		t.Fatalf("interim retry after final = %q %v", again, err)
	}
	msgs, err := f.c.ListMessages(ctx, f.task.ID, asked.Seq, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Envelope == nil || msgs[0].Envelope.Kind != api.EnvelopeKindNotice || msgs[0].Envelope.Body.Text != text || msgs[1].Envelope == nil || msgs[1].Envelope.Kind != api.EnvelopeKindAnswer || msgs[1].Envelope.Body.Answer != text {
		t.Fatalf("interim/final messages = %+v", msgs)
	}
	obligations, err = f.c.ListObligations(ctx, f.task.ID, a.ID, a.RunID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obligations {
		if o.MessageSeq == asked.Seq && (o.State != api.ObligationClosed || o.Outcome != api.OutcomeAnswered || o.OutcomeSeq != msgs[1].Seq) {
			t.Fatalf("interim retry reopened the final answer: %+v", o)
		}
	}
}

func TestHelperReplyWithoutObligation(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	asked, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "A board message without an obligation"})
	if err != nil {
		t.Fatal(err)
	}
	answer := "**Reply**\n\n- first\n- second"
	if _, err := captureCLIOutput(t, func() error {
		return cmdHelper(f.owner, []string{"reply", "--task", f.task.ID, fmt.Sprint(asked.Seq), "--text", answer})
	}); err != nil {
		t.Fatal(err)
	}
	msgs, err := f.c.ListMessages(ctx, f.task.ID, asked.Seq, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != answer || msgs[0].Envelope != nil || msgs[0].ReplyTo != asked.Seq || msgs[0].From.AgentID != a.ID {
		t.Fatalf("no-obligation reply changed: %+v", msgs)
	}
}

func helperOwnerWorkEnvelope(kind, to string) api.Envelope {
	e := api.Envelope{Kind: kind, To: to, Subject: "Complete the synthetic owner request"}
	switch kind {
	case api.EnvelopeKindRequest:
		e.Body.Ask = "Please finish this synthetic request."
	case api.EnvelopeKindAssign:
		e.Body.Objective = "Finish this synthetic assignment."
		e.Body.Owns = []string{"synthetic fixture"}
		e.Body.Acceptance = map[string]string{"a1": "Reply with the result."}
	case api.EnvelopeKindReview:
		e.Body.Candidate = "synthetic candidate"
		e.Body.Scope = "Review this synthetic request."
		e.Body.Acceptance = map[string]string{"a1": "Reply with the review."}
	}
	return e
}

func TestHelperReplyTypedOwnerWork(t *testing.T) {
	for _, kind := range []string{api.EnvelopeKindRequest, api.EnvelopeKindAssign, api.EnvelopeKindReview} {
		t.Run(kind, func(t *testing.T) {
			f := newHelperFixture(t)
			ctx := context.Background()
			a := *f.mustRegister(t).Agent
			e := helperOwnerWorkEnvelope(kind, a.Name)
			asked, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, Envelope: &e})
			if err != nil {
				t.Fatal(err)
			}
			text := "**Synthetic result**\n\n- first\n- second\n\n```sh\necho synthetic\n```"
			reply := func(interim bool) (string, error) {
				args := []string{"reply", "--task", f.task.ID, fmt.Sprint(asked.Seq), "--text", text}
				if interim {
					args = append(args, "--interim")
				}
				return captureCLIOutput(t, func() error { return cmdHelper(f.owner, args) })
			}
			interim, err := reply(true)
			if err != nil {
				t.Fatal(err)
			}
			obligations, err := f.c.ListObligations(ctx, f.task.ID, a.ID, a.RunID, false, false)
			if err != nil || len(obligations) != 1 || obligations[0].State != api.ObligationWorking || obligations[0].LastProgressAt == nil || obligations[0].Outcome != "" {
				t.Fatalf("typed owner interim = %+v %v", obligations, err)
			}
			final, err := reply(false)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := reply(false); err != nil || again != final {
				t.Fatalf("typed final retry = %q %v", again, err)
			}
			if again, err := reply(true); err != nil || again != interim {
				t.Fatalf("typed interim retry = %q %v", again, err)
			}
			msgs, err := f.c.ListMessages(ctx, f.task.ID, asked.Seq, "", 10)
			if err != nil || len(msgs) != 2 || msgs[0].Envelope == nil || msgs[0].Envelope.Kind != api.EnvelopeKindNotice || msgs[0].Envelope.Body.Text != text || msgs[1].Envelope == nil || msgs[1].Envelope.Kind != api.EnvelopeKindResult || msgs[1].Envelope.Body.Text != text {
				t.Fatalf("typed owner reply messages = %+v %v", msgs, err)
			}
			obligations, err = f.c.ListObligations(ctx, f.task.ID, a.ID, a.RunID, false, false)
			if err != nil || len(obligations) != 1 || obligations[0].State != api.ObligationClosed || obligations[0].Outcome != api.OutcomeResult || obligations[0].OutcomeSeq != msgs[1].Seq {
				t.Fatalf("typed owner final = %+v %v", obligations, err)
			}
		})
	}
}

// wi_2de2c1273e34473a a10, f0: tt helper reply posts as the verified helper
// with replyTo (SEQ before or after the flags), answering one of two
// pending messages is accepted, and an unverified helper is refused.
func TestHelperReply(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	reply := func(args ...string) (string, error) {
		return captureCLIOutput(t, func() error { return cmdHelper(f.owner, append([]string{"reply"}, args...)) })
	}
	if _, err := reply("--task", f.task.ID, "1", "--text", "too early"); err == nil || !strings.Contains(err.Error(), "no owner helper is registered") {
		t.Fatalf("reply before registering: %v", err)
	}
	a := *f.mustRegister(t).Agent
	var asked []api.Message
	for _, text := range []string{"first question", "second question"} {
		m, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		asked = append(asked, m)
	}
	out, err := reply("--task", f.task.ID, fmt.Sprint(asked[1].Seq), "--text", "answer to the second")
	if err != nil || !strings.Contains(out, fmt.Sprintf("replying to #%d", asked[1].Seq)) {
		t.Fatalf("reply = %q %v", out, err)
	}
	if _, err := reply("--text", "answer to the first", "--task", f.task.ID, fmt.Sprintf("#%d", asked[0].Seq)); err != nil {
		t.Fatalf("flags before SEQ: %v", err)
	}
	msgs, err := f.c.ListMessages(ctx, f.task.ID, asked[1].Seq, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages after the questions = %+v", msgs)
	}
	for i, want := range []struct {
		text    string
		replyTo int64
	}{{"answer to the second", asked[1].Seq}, {"answer to the first", asked[0].Seq}} {
		m := msgs[i]
		if m.From.AgentID != a.ID || m.ReplyTo != want.replyTo || m.Envelope == nil || m.Envelope.Kind != api.EnvelopeKindAnswer || m.Envelope.Body.Answer != want.text {
			t.Fatalf("reply %d = %+v, want from %s replying to %d", i, m, a.ID, want.replyTo)
		}
	}
	// Review round one f4: running the same reply again (a retry after a
	// timeout) returns the original post; a different text is a new reply.
	again, err := reply("--task", f.task.ID, fmt.Sprint(asked[1].Seq), "--text", "answer to the second")
	if err != nil || again != out {
		t.Fatalf("retried reply = %q %v, want the original %q", again, err, out)
	}
	if n, _ := f.c.ListMessages(ctx, f.task.ID, asked[1].Seq, "", 10); len(n) != 2 {
		t.Fatalf("a retried reply posted again: %d messages", len(n))
	}
	if helperReplyRequestID(f.task.ID, 5, "a") == helperReplyRequestID(f.task.ID, 5, "b") || helperReplyRequestID(f.task.ID, 5, "a") == helperReplyRequestID(f.task.ID, 6, "a") {
		t.Fatal("different replies share a request ID")
	}
	for _, bad := range [][]string{{"--task", f.task.ID, "--text", "no seq"}, {"--task", f.task.ID, "x", "--text", "t"}, {"--task", f.task.ID, "5"}, {"--task", f.task.ID, "5", "6", "--text", "t"}} {
		if _, err := reply(bad...); err == nil {
			t.Errorf("reply %v was accepted", bad)
		}
	}
	// Registered again from elsewhere: this host's helper is no longer verified.
	if _, err := f.c.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "other-mac", Session: "owner", Runtime: "claude", RequestID: "ohreg-elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reply("--task", f.task.ID, fmt.Sprint(asked[0].Seq), "--text", "stale"); err == nil || !strings.Contains(err.Error(), "registered again elsewhere") {
		t.Fatalf("a stale helper replied: %v", err)
	}
}

// a10: the helper inbox shows the tt helper reply hint for a message the
// owner sent from Discord, and the usual hint for everything else.
func TestHelperInboxDiscordHint(t *testing.T) {
	names := map[string]string{}
	discordMsg := api.Message{Seq: 42, From: api.Sender{Node: api.BridgeNode, User: "owner"}, Text: "what's stuck?", Source: &api.MessageSource{Kind: api.SourceDiscord, ID: "1", UserID: "2"}}
	line := helperMessageLine(discordMsg, names, "tsk_0123456789abcdef")
	if !strings.Contains(line, `tt helper reply --task tsk_0123456789abcdef 42 --text "your reply"`) || !strings.Contains(line, "tt ack 42") || strings.Contains(line, "Reply on the shared board") || !strings.Contains(line, "--interim") || !strings.Contains(line, "keeps the obligation open") || !strings.Contains(line, "no second typed answer is needed") {
		t.Fatalf("Discord hint = %q", line)
	}
	if !strings.HasPrefix(line, strings.SplitN(formatMessage(discordMsg, names), "\n", 2)[0]) {
		t.Fatalf("the message line itself changed: %q", line)
	}
	plain := api.Message{Seq: 43, From: api.Sender{Node: "workspace", User: "owner"}, Text: "from TailOS"}
	if got := helperMessageLine(plain, names, "tsk_0123456789abcdef"); got != formatMessage(plain, names) {
		t.Fatalf("a non-Discord message changed: %q", got)
	}
	agentMsg := api.Message{Seq: 44, From: api.Sender{AgentID: "agt_0123456789abcdef"}, Text: "from an agent"}
	if got := helperMessageLine(agentMsg, names, "tsk_0123456789abcdef"); got != formatMessage(agentMsg, names) {
		t.Fatalf("an agent message changed: %q", got)
	}
}

// One tmux session carries one project's helper: a second project's
// registration there is refused before anything is registered or written, so
// the first project's wake and heartbeat keep working.
func TestHelperRegisterSecondProjectSameSession(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	second, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "Second project", Orchestrator: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	registerSecond := func(args ...string) (string, string, error) {
		t.Helper()
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		out, runErr := captureCLIOutput(t, func() error {
			return cmdHelper(env{hub: f.owner.hub}, append([]string{"register", "--task", second.ID}, args...))
		})
		_ = w.Close()
		os.Stderr = old
		warning, _ := io.ReadAll(r)
		_ = r.Close()
		return out, string(warning), runErr
	}
	secondHelpers := func() []api.Agent {
		t.Helper()
		agents, err := f.c.ListAgents(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		var helpers []api.Agent
		for _, agent := range agents {
			if agent.Role == api.AgentRoleOwnerHelper {
				helpers = append(helpers, agent)
			}
		}
		return helpers
	}
	tagsBefore := f.tags(t, "owner")
	stateBefore, _ := os.ReadFile(ownerHelperPath(f.owner.hub, f.task.ID))
	bindingBefore, _ := readBinding(t, f.owner.hub, a.ID)

	_, _, err = registerSecond()
	if err == nil || !strings.Contains(err.Error(), "one project per tmux session") || !strings.Contains(err.Error(), f.task.ID) || !strings.Contains(err.Error(), "--take-session") {
		t.Fatalf("second project in the first project's session: %v", err)
	}
	if helpers := secondHelpers(); len(helpers) != 0 {
		t.Fatalf("refused registration reached the hub: %+v", helpers)
	}
	if _, err := os.Stat(ownerHelperPath(f.owner.hub, second.ID)); !os.IsNotExist(err) {
		t.Fatalf("refused registration wrote helper state: %v", err)
	}
	if tags := f.tags(t, "owner"); fmt.Sprint(tags) != fmt.Sprint(tagsBefore) || tags["TAILTERM_TASK"] != f.task.ID || tags["TAILTERM_AGENT"] != a.ID {
		t.Fatalf("tags %v, want %v", tags, tagsBefore)
	}
	if state, _ := os.ReadFile(ownerHelperPath(f.owner.hub, f.task.ID)); !bytes.Equal(state, stateBefore) {
		t.Fatalf("first project's helper file changed: %s", state)
	}
	b, ok := readBinding(t, f.owner.hub, a.ID)
	if !ok || b != bindingBefore || b.Task != f.task.ID || b.Run != a.RunID {
		t.Fatalf("first project's binding %+v, want %+v", b, bindingBefore)
	}
	// The first project's wake still verifies its pane and keeps it online.
	if _, err := inspectRuntimePane(ctx, b, "Claude", false); err != nil {
		t.Fatalf("first project's pane identity: %v", err)
	}
	var p relayProgress
	before, _ := f.c.GetAgent(ctx, f.task.ID, a.ID)
	time.Sleep(1100 * time.Millisecond)
	if err := relayHelperHeartbeat(ctx, b, &p, f.c, time.Now(), activityProbeNative); err != nil {
		t.Fatal(err)
	}
	if after, _ := f.c.GetAgent(ctx, f.task.ID, a.ID); !after.LastSeenAt.After(before.LastSeenAt) || !after.Online {
		t.Fatalf("heartbeat %s -> %s online=%v", before.LastSeenAt, after.LastSeenAt, after.Online)
	}

	// The explicit flag moves the session, with a warning naming the cost.
	_, warning, err := registerSecond("--take-session")
	helpers := secondHelpers()
	if err != nil || len(helpers) != 1 || !strings.Contains(warning, f.task.ID) || !strings.Contains(warning, "wake-ups stop") {
		t.Fatalf("take-session: %v %+v %q", err, helpers, warning)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_TASK"] != second.ID || tags["TAILTERM_AGENT"] != helpers[0].ID {
		t.Fatalf("tags after take-session: %v", tags)
	}
	if _, err := inspectRuntimePane(ctx, b, "Claude", false); err == nil {
		t.Fatal("the first project's pane identity still verifies after take-session")
	}

	// A closed helper no longer uses the session: no flag is needed.
	if _, err := f.c.CloseAgent(ctx, second.ID, helpers[0].ID, helpers[0].RunID); err != nil {
		t.Fatal(err)
	}
	back, err := f.register(t, f.owner)
	if err != nil || back.Agent.ID != a.ID {
		t.Fatalf("register after the other project's helper closed: %+v %v", back, err)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_TASK"] != f.task.ID || tags["TAILTERM_RUN"] != back.Agent.RunID {
		t.Fatalf("tags after the closed helper: %v", tags)
	}
}

// The second-project check reads only this session's tags, so an unrelated
// session that tmux cannot list as JSON never fails a register.
func TestHelperRegisterIgnoresUnrelatedSessionName(t *testing.T) {
	f := newHelperFixture(t)
	f.tmux(t, "new-session", "-d", "-s", `a"b`, "sleep 300")
	if _, err := localSessions(context.Background()); err == nil {
		t.Fatal(`fixture: a session named a"b no longer breaks the session listing`)
	}
	first := f.mustRegister(t)
	again := f.mustRegister(t)
	if again.Agent.ID != first.Agent.ID || again.Registration.Mode != api.OwnerHelperReplaced {
		t.Fatalf("re-register %+v", again.Registration)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_TASK"] != f.task.ID || tags["TAILTERM_RUN"] != again.Agent.RunID {
		t.Fatalf("tags %v", tags)
	}
}

// A genuine Codex thread supplies no Claude identity.
func TestHelperCodexRegister(t *testing.T) {
	f := newHelperFixture(t)
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", f.thread)
	t.Setenv("CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex"))
	if err := os.WriteFile(filepath.Join(f.bin, "codex"), []byte("#!/bin/sh\necho 'Queued message'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := *f.mustRegister(t).Agent
	b, ok := readBinding(t, f.owner.hub, a.ID)
	if a.Runtime != "codex" || !ok || b.Runtime != "codex" || b.Thread != f.thread || b.Run != a.RunID || b.Role != api.AgentRoleOwnerHelper || b.Codex != filepath.Join(f.bin, "codex") || b.CodexHome != os.Getenv("CODEX_HOME") || !validBinding(b) {
		t.Fatalf("genuine Codex registration: agent=%+v binding=%+v", a, b)
	}
}

func helperTestRuntime(t *testing.T, f helperFixture, runtime, thread string) {
	t.Helper()
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDECODE", "")
	if runtime == "codex" {
		t.Setenv("CODEX_THREAD_ID", thread)
		t.Setenv("CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex"))
		if err := os.WriteFile(filepath.Join(f.bin, "codex"), []byte("#!/bin/sh\necho 'Queued message'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", f.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		t.Setenv("CLAUDECODE", "1")
		t.Setenv("CLAUDE_CODE_SESSION_ID", thread)
		writeHelperTranscript(t, os.Getenv("HOME"), thread)
	}
}

func TestHelperCodexIdentityRefusalsAndBind(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	a := *f.mustRegister(t).Agent
	e := env{hub: f.owner.hub, task: f.task.ID, agent: a.ID, runID: a.RunID}
	original, _ := readBinding(t, e.hub, e.agent)
	for _, command := range []string{"status", "inbox"} {
		autoBindRuntime(e, command)
	}
	if err := bindRuntime(e, f.thread); err != nil {
		t.Fatal(err)
	}
	kept, _ := readBinding(t, e.hub, e.agent)
	if kept != original {
		t.Fatalf("binding overwritten: %+v -> %+v", original, kept)
	}
	for _, field := range []string{"hub", "agent", "session"} {
		foreignBinding := original
		switch field {
		case "hub":
			foreignBinding.Hub = "http://foreign.invalid"
		case "agent":
			foreignBinding.Agent = api.NewID("agt")
		case "session":
			foreignBinding.Session = "other"
		}
		path := filepath.Join(relayDir(), bindingKey(original)+".binding.json")
		if err := writePrivateJSON(path, foreignBinding); err != nil {
			t.Fatal(err)
		}
		if err := bindRuntime(e, f.thread); err == nil {
			t.Fatalf("foreign %s binding accepted", field)
		}
		if err := writePrivateJSON(path, original); err != nil {
			t.Fatal(err)
		}
	}
	foreign := "00000000-0000-4000-8000-00000000dead"
	if err := bindRuntime(e, foreign); err == nil {
		t.Fatal("helper rebound to foreign thread")
	}
	t.Setenv("CODEX_THREAD_ID", foreign)
	autoBindRuntime(e, "status")
	if kept, _ := readBinding(t, e.hub, e.agent); kept != original {
		t.Fatal("auto-bind changed registered thread")
	}
	for _, command := range []string{"env", "inbox", "reply"} {
		args := []string{command, "--task", f.task.ID}
		if command == "reply" {
			args = append(args, "1", "--text", "stale")
		}
		if _, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, args) }); err == nil {
			t.Fatalf("foreign thread %s accepted", command)
		}
	}
	t.Setenv("CODEX_THREAD_ID", "bad")
	if _, err := f.register(t, f.owner); err == nil {
		t.Fatal("invalid thread accepted")
	}
	t.Setenv("CODEX_THREAD_ID", f.thread)
	t.Setenv("CLAUDECODE", "1")
	if _, err := f.register(t, f.owner); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("mixed identity: %v", err)
	}
	t.Setenv("CLAUDECODE", "")
	t.Setenv("PATH", t.TempDir()) // no fallback to the host's real Codex
	if _, err := f.register(t, f.owner); err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("missing Codex: %v", err)
	}
	if current, _ := f.c.GetAgent(context.Background(), f.task.ID, a.ID); current.RunID != a.RunID {
		t.Fatal("refusal changed run")
	}
}

func TestHelperRuntimeHandback(t *testing.T) {
	for _, sequence := range [][]string{{"claude", "codex", "claude"}, {"codex", "claude", "codex"}} {
		t.Run(strings.Join(sequence, "-"), func(t *testing.T) {
			f := newHelperFixture(t)
			ctx := context.Background()
			var previous api.Agent
			var previousBinding runtimeBinding
			for i, runtime := range sequence {
				thread := fmt.Sprintf("00000000-0000-4000-8000-%012x", i+1)
				helperTestRuntime(t, f, runtime, thread)
				a := *f.mustRegister(t).Agent
				b, _ := readBinding(t, f.owner.hub, a.ID)
				if b.Runtime != runtime || b.Thread != thread || b.Run != a.RunID {
					t.Fatalf("handback binding %+v", b)
				}
				if i > 0 && (previous.ID != a.ID || previous.RunID == a.RunID) {
					t.Fatal("handback did not replace same helper run")
				}
				if i > 0 {
					helperTestRuntime(t, f, previousBinding.Runtime, previousBinding.Thread)
					if _, err := captureCLIOutput(t, func() error {
						return cmdHelper(f.owner, []string{"reply", "--task", f.task.ID, "1", "--text", "old thread"})
					}); err == nil {
						t.Fatal("actual stale owner thread adopted same-host replacement")
					}
					helperTestRuntime(t, f, runtime, thread)
					stale := env{hub: f.owner.hub, task: f.task.ID, agent: previous.ID, runID: previous.RunID}
					if _, err := captureCLIOutput(t, func() error {
						return cmdHelper(stale, []string{"reply", "--task", f.task.ID, "1", "--text", "old run"})
					}); err == nil {
						t.Fatal("actual stale helper CLI adopted replacement")
					}
					if previousBinding.Runtime == "codex" {
						if err := bindRuntime(stale, previousBinding.Thread); err == nil {
							t.Fatal("stale bind accepted")
						}
					}
					if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: previous.ID, RunID: previous.RunID, Kind: api.EventHeartbeat}); err == nil {
						t.Fatal("stale heartbeat accepted")
					}
				}
				probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
				if err := relayHelperHeartbeat(ctx, b, &relayProgress{}, f.c, time.Now(), probe); err != nil {
					t.Fatal(err)
				}
				m, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, Text: "handoff wake"})
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				queue := func(_ context.Context, got runtimeBinding, prompt string) error {
					calls++
					if got.Run != a.RunID || got.Thread != thread || !strings.Contains(prompt, "tt helper inbox --task "+f.task.ID) {
						t.Fatalf("wake %+v %q", got, prompt)
					}
					return nil
				}
				if i > 0 {
					if err := relayOne(ctx, previousBinding, &relayProgress{}, f.c, time.Now(), queue); err != nil {
						t.Fatal(err)
					}
					if calls != 0 {
						t.Fatal("stale binding woke")
					}
				}
				if err := relayOne(ctx, b, &relayProgress{}, f.c, time.Now(), queue); err != nil || calls != 1 {
					t.Fatalf("current wake calls=%d err=%v", calls, err)
				}
				if _, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"inbox", "--task", f.task.ID}) }); err != nil {
					t.Fatal(err)
				}
				if _, err := captureCLIOutput(t, func() error {
					return cmdHelper(f.owner, []string{"reply", "--task", f.task.ID, fmt.Sprint(m.Seq), "--text", "new run answer"})
				}); err != nil {
					t.Fatal(err)
				}
				agents, err := f.c.ListAgents(ctx, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				helpers := 0
				for _, got := range agents {
					if got.Role == api.AgentRoleOwnerHelper && got.Status != api.AgentClosed && got.Status != api.AgentExited {
						helpers++
					}
				}
				if helpers != 1 {
					t.Fatalf("active helpers=%d", helpers)
				}
				previous, previousBinding = a, b
			}
		})
	}
}

func TestHelperCodexRetryThreadIdentity(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	out, err := f.register(t, f.owner, "--request-id", "exact-thread-key")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := readBinding(t, f.owner.hub, out.Agent.ID)
	t.Setenv("CODEX_THREAD_ID", "00000000-0000-4000-8000-00000000dead")
	if _, err := f.register(t, f.owner, "--request-id", "exact-thread-key"); err == nil {
		t.Fatal("explicit key moved registered thread")
	}
	if after, _ := readBinding(t, f.owner.hub, out.Agent.ID); after != before {
		t.Fatal("retry changed binding")
	}
	t.Setenv("CODEX_THREAD_ID", f.thread)
	replay, err := f.register(t, f.owner, "--request-id", "exact-thread-key")
	if err != nil || !replay.Replay || replay.Agent.RunID != out.Agent.RunID {
		t.Fatalf("same-thread replay %+v %v", replay, err)
	}
}

// Actual registration, Bridge.Run/Dispatch, real relay/native queue and helper
// CLI reply against a temporary hub/state and fake Discord REST, never live data.
func TestHelperCodexDiscordRoundTrip(t *testing.T) {
	for _, route := range []string{"dm", "thread", "dm-request", "thread-request"} {
		t.Run(route, func(t *testing.T) {
			f := newHelperFixture(t)
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "discord-hub.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ownerToken := "owner-test-0123456789abcdef0123456789"
			bridgeToken := "bridge-test-0123456789abcdef01234567"
			identity, err := server.TokenIdentities(map[string]api.Caller{ownerToken: {Node: "workspace", User: "owner"}, bridgeToken: {Node: api.BridgeNode, User: "owner"}})
			if err != nil {
				t.Fatal(err)
			}
			hub := httptest.NewServer(server.New(st, identity))
			defer hub.Close()
			c, err := api.NewClient(hub.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			c.Token = ownerToken
			task, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: "Codex Discord fixture"})
			if err != nil {
				t.Fatal(err)
			}
			f.owner = env{hub: hub.URL, task: task.ID, token: ownerToken}
			f.c = c
			f.task = task
			helperTestRuntime(t, f, "codex", f.thread)
			queueLog := filepath.Join(t.TempDir(), "queue-argv")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + spawn.ShellQuote(queueLog) + "\nprintf 'Queued message\\n'\n"
			if err := os.WriteFile(filepath.Join(f.bin, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			a := *f.mustRegister(t).Agent
			b, _ := readBinding(t, hub.URL, a.ID)
			if _, err := c.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}); err != nil {
				t.Fatal(err)
			}
			state, err := bridge.OpenState(filepath.Join(t.TempDir(), "bridge.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			if err := state.SetChannel(ctx, task.ID, "project", 0); err != nil {
				t.Fatal(err)
			}
			const ownerID = "709500000000000001"
			inbound := discord.Message{ID: "709500000000000099", ChannelID: "709500000000000004", Content: "synthetic owner question", Author: discord.User{ID: ownerID}}
			replyChannel := "709500000000000004"
			if strings.HasPrefix(route, "thread") {
				inbound.ChannelID = "709500000000000003"
				inbound.GuildID = "guild"
				replyChannel = inbound.ID
			}
			type sentMessage struct{ channel, content string }
			var mu sync.Mutex
			var sent []sentMessage
			var next atomic.Int64
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/users/@me":
					json.NewEncoder(w).Encode(discord.User{ID: "bot", Bot: true})
				case r.URL.Path == "/users/@me/channels":
					json.NewEncoder(w).Encode(discord.Channel{ID: "709500000000000004", Type: discord.ChannelDM})
				case strings.HasSuffix(r.URL.Path, "/commands"):
					w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/threads"):
					json.NewEncoder(w).Encode(discord.Channel{ID: inbound.ID, Type: discord.ChannelPublicThread, ParentID: "709500000000000003"})
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/messages"):
					w.Write([]byte(`[]`))
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
					var msg discord.MessageSend
					json.NewDecoder(r.Body).Decode(&msg)
					channel := strings.Split(r.URL.Path, "/")[2]
					mu.Lock()
					sent = append(sent, sentMessage{channel, msg.Content})
					mu.Unlock()
					json.NewEncoder(w).Encode(discord.Message{ID: fmt.Sprint(709500000000001000 + next.Add(1)), ChannelID: channel, Content: msg.Content, Author: discord.User{ID: "bot", Bot: true}})
				case strings.Contains(r.URL.Path, "/reactions/"):
					w.WriteHeader(http.StatusNoContent)
				case r.Method == "PATCH":
					w.Write([]byte(`{"id":"card"}`))
				default:
					w.Write([]byte(`[]`))
				}
			}))
			defer fake.Close()
			bridgeURL := hub.URL
			finalKind, finalOutcome := api.EnvelopeKindAnswer, api.OutcomeAnswered
			if strings.HasSuffix(route, "request") {
				finalKind, finalOutcome = api.EnvelopeKindResult, api.OutcomeResult
				// Inject a typed owner request at the bridge's HTTP boundary so
				// the actual helper CLI and Discord reply route exercise b1.
				target, _ := url.Parse(hub.URL)
				proxy := httputil.NewSingleHostReverseProxy(target)
				ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
						var req api.PostMessageRequest
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						r.Body.Close()
						if req.Source != nil && req.Source.ID == inbound.ID {
							e := helperOwnerWorkEnvelope(api.EnvelopeKindRequest, a.Name)
							e.Body.Ask = inbound.Content
							req.Envelope, req.Text = &e, ""
						}
						raw, _ := json.Marshal(req)
						r.Body = io.NopCloser(bytes.NewReader(raw))
						r.ContentLength = int64(len(raw))
					}
					proxy.ServeHTTP(w, r)
				}))
				defer ingress.Close()
				bridgeURL = ingress.URL
			}
			bridgeClient, _ := api.NewClient(bridgeURL, time.Second)
			bridgeClient.Token = bridgeToken
			br, err := bridge.New(bridge.Config{Hub: bridgeClient, Discord: &discord.Client{Base: fake.URL, Token: "synthetic", HTTP: fake.Client()}, AppID: "app", GuildID: "guild", Owners: []string{ownerID}, HelperTask: task.ID, HelperChannel: "709500000000000003", State: state, MirrorInterval: 20 * time.Millisecond, SyncInterval: time.Hour, DigestInterval: time.Hour, CardInterval: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- br.Run(runCtx) }()
			var stopOnce sync.Once
			stop := func() {
				stopOnce.Do(func() {
					cancel()
					select {
					case err := <-done:
						if err != context.Canceled {
							t.Errorf("bridge shutdown: %v", err)
						}
					case <-time.After(5 * time.Second):
						t.Error("bridge loops did not join")
					}
				})
			}
			defer stop()

			wait := func(what string, condition func() bool) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if condition() {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("timed out: " + what)
			}
			wait("bridge startup", func() bool { dm, _ := state.Get(ctx, "helper:dm:"+ownerID); return dm == "709500000000000004" })
			raw, _ := json.Marshal(inbound)
			br.Dispatch(discord.Dispatch{Type: "MESSAGE_CREATE", Data: raw})
			var source api.Message
			wait("Discord input", func() bool {
				msgs, _ := c.ListMessages(ctx, task.ID, 0, a.ID, 100)
				for _, m := range msgs {
					if m.Source != nil && m.Source.ID == inbound.ID {
						source = m
						return true
					}
				}
				return false
			})
			if source.To != a.ID || source.Source.Kind != api.SourceDiscord || source.Source.UserID != ownerID || source.From.Node != api.BridgeNode {
				t.Fatalf("Discord source %+v", source)
			}
			// Native inbox path leaves the read cursor with the helper.
			if err := relayOne(ctx, b, &relayProgress{}, c, time.Now(), nativeQueue); err != nil {
				t.Fatal(err)
			}
			argv, err := os.ReadFile(queueLog)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(argv), "queue\n--thread\n"+f.thread+"\n--message\n") || !strings.Contains(string(argv), "tt helper inbox --task "+task.ID) {
				t.Fatalf("native queue argv %q", argv)
			}
			if current, _ := c.GetAgent(ctx, task.ID, a.ID); current.ReadUpTo >= source.Seq {
				t.Fatal("relay marked owner message read")
			}
			inbox, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"inbox", "--task", task.ID}) })
			if err != nil || !strings.Contains(inbox, inbound.Content) {
				t.Fatalf("helper inbox %q %v", inbox, err)
			}
			answer := "**Synthetic Codex answer**\n\n- first\n- second\n\n```sh\necho synthetic\n```"
			reply := func(interim bool) error {
				_, err := captureCLIOutput(t, func() error {
					args := []string{"reply", "--task", task.ID, fmt.Sprint(source.Seq), "--text", answer}
					if interim {
						args = append(args, "--interim")
					}
					return cmdHelper(f.owner, args)
				})
				return err
			}
			if err := reply(true); err != nil {
				t.Fatal(err)
			}
			wait("Discord interim progress", func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, m := range sent {
					if m.channel == replyChannel && strings.Contains(m.content, " · NOTICE · ") && strings.Contains(m.content, answer) {
						return true
					}
				}
				return false
			})
			progress, err := c.ListObligations(ctx, task.ID, a.ID, a.RunID, false, false)
			if err != nil || len(progress) != 1 || progress[0].State != api.ObligationWorking || progress[0].LastProgressAt == nil || progress[0].ClosedAt != nil {
				t.Fatalf("Discord interim progress = %+v %v", progress, err)
			}
			if err := reply(true); err != nil {
				t.Fatal(err)
			}
			if err := reply(false); err != nil {
				t.Fatal(err)
			}
			wait("Discord answer", func() bool {
				mu.Lock()
				defer mu.Unlock()
				for _, m := range sent {
					if m.channel == replyChannel && strings.Contains(m.content, " · "+strings.ToUpper(finalKind)+" · ") && strings.Contains(m.content, answer) {
						return true
					}
				}
				return false
			})
			obligations, err := c.ListObligations(ctx, task.ID, a.ID, a.RunID, false, false)
			if err != nil {
				t.Fatal(err)
			}
			answered := false
			for _, o := range obligations {
				if o.MessageSeq == source.Seq {
					answered = o.State == api.ObligationClosed && o.Outcome == finalOutcome && o.OutcomeSeq != 0
				}
			}
			if !answered {
				t.Fatalf("Discord reply did not close the owner obligation: %+v", obligations)
			}
			if err := reply(false); err != nil {
				t.Fatal(err)
			} // identical CLI retry posts once
			br.Dispatch(discord.Dispatch{Type: "MESSAGE_CREATE", Data: raw}) // Gateway redelivery is idempotent
			// Stop and join loops before final route/count and provenance inspection.
			time.Sleep(100 * time.Millisecond)
			stop()
			mu.Lock()
			answers := 0
			for _, m := range sent {
				if m.channel != "project" && strings.Contains(m.content, answer) {
					answers++
					if m.channel != replyChannel {
						t.Fatalf("wrong reply route %q", m.channel)
					}
				}
			}
			mu.Unlock()
			if answers != 2 { // one interim plus one final, despite both retries
				t.Fatalf("Discord answers=%d", answers)
			}
			msgs, err := c.ListMessages(ctx, task.ID, source.Seq, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			replies := 0
			for _, m := range msgs {
				if m.Envelope != nil && m.Envelope.Kind == finalKind && (m.Envelope.Body.Answer == answer || m.Envelope.Body.Text == answer) {
					replies++
					if m.From.AgentID != a.ID || m.ReplyTo != source.Seq {
						t.Fatalf("reply provenance %+v", m)
					}
				}
			}
			if replies != 1 {
				t.Fatalf("hub replies=%d", replies)
			}
		})
	}
}

func TestHelperCodexPendingRetryCannotMoveThread(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	upstream, _ := url.Parse(f.owner.hub)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var drop atomic.Bool
	drop.Store(true)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/owner-helper") && drop.Swap(false) {
			body, _ := io.ReadAll(r.Body)
			resp, err := http.Post(f.owner.hub+r.URL.Path, "application/json", bytes.NewReader(body))
			if err == nil {
				resp.Body.Close()
			}
			panic(http.ErrAbortHandler)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	e := env{hub: front.URL, task: f.task.ID}
	if _, err := f.register(t, e); err == nil {
		t.Fatal("lost reply reported success")
	}
	pending, err := loadOwnerHelperFile(front.URL, f.task.ID)
	if err != nil || pending.PendingRequest == "" {
		t.Fatalf("pending %+v %v", pending, err)
	}
	t.Setenv("CODEX_THREAD_ID", "00000000-0000-4000-8000-00000000dead")
	if _, err := f.register(t, e, "--request-id", pending.PendingRequest); err == nil {
		t.Fatal("pending explicit retry moved thread")
	}
	next, err := f.register(t, e)
	if err != nil || next.Replay || next.Registration.RequestID == pending.PendingRequest {
		t.Fatalf("new thread reused pending registration %+v %v", next, err)
	}
	b, _ := readBinding(t, front.URL, next.Agent.ID)
	if b.Thread != os.Getenv("CODEX_THREAD_ID") || b.Run != next.Agent.RunID {
		t.Fatalf("new thread binding %+v", b)
	}
}

func TestHelperCodexOutsideTmux(t *testing.T) {
	f := newHelperFixture(t)
	helperTestRuntime(t, f, "codex", f.thread)
	a := *f.mustRegister(t).Agent
	t.Setenv("TMUX", "")
	text, err := captureCLIOutput(t, func() error { return cmdHelper(f.owner, []string{"register", "--task", f.task.ID}) })
	if err != nil || !strings.Contains(text, "registered without wake") {
		t.Fatalf("outside tmux %q %v", text, err)
	}
	if _, ok := readBinding(t, f.owner.hub, a.ID); ok {
		t.Fatal("outside tmux kept wake binding")
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_AGENT"] != "" {
		t.Fatalf("old session tags %v", tags)
	}
	if state, err := loadOwnerHelperFile(f.owner.hub, f.task.ID); err != nil || state.Runtime != "codex" || state.Thread != f.thread {
		t.Fatalf("outside tmux state %+v %v", state, err)
	}
}

// A previous CLI saved only the request hash before the hub committed a reply
// that never reached it. Upgrading must not silently register a second run.
func TestHelperLegacyClaudePendingReplay(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, identity := range []string{"unknown", "exact", "foreign-thread", "foreign-runtime", "previous-run"} {
			t.Run(fmt.Sprintf("explicit-%v/%s", explicit, identity), func(t *testing.T) {
				f := newHelperFixture(t)
				upstream, _ := url.Parse(f.owner.hub)
				proxy := httputil.NewSingleHostReverseProxy(upstream)
				var drop atomic.Bool
				drop.Store(true)
				type firstAttempt struct {
					req    api.RegisterOwnerHelperRequest
					result api.OwnerActionResult
				}
				first := make(chan firstAttempt, 1)
				front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/owner-helper") && drop.Swap(false) {
						body, _ := io.ReadAll(r.Body)
						var attempt firstAttempt
						json.Unmarshal(body, &attempt.req)
						resp, err := http.Post(f.owner.hub+r.URL.Path, "application/json", bytes.NewReader(body))
						if err == nil {
							json.NewDecoder(resp.Body).Decode(&attempt.result)
							resp.Body.Close()
						}
						first <- attempt
						panic(http.ErrAbortHandler)
					}
					proxy.ServeHTTP(w, r)
				}))
				defer front.Close()
				e := env{hub: front.URL, task: f.task.ID}
				if _, err := f.register(t, e); err == nil {
					t.Fatal("lost reply reported success")
				}
				attempt := <-first
				committed := attempt.result
				state, err := loadOwnerHelperFile(front.URL, f.task.ID)
				if err != nil || state.PendingRequest == "" || committed.Agent == nil {
					t.Fatalf("pending %+v committed %+v %v", state, committed, err)
				}
				// Exact pre-upgrade wire/state shape: request-only hash, no pending thread.
				attempt.req.RequestID = ""
				raw, _ := json.Marshal(attempt.req)
				sum := sha256.Sum256(raw)
				state.PendingHash = hex.EncodeToString(sum[:])
				switch identity {
				case "exact", "foreign-thread", "foreign-runtime", "previous-run":
					state.Agent = committed.Agent.ID
					state.Run = committed.Agent.RunID
					state.Thread = f.thread
					if identity == "foreign-thread" {
						state.Thread = "00000000-0000-4000-8000-00000000dead"
					}
					if identity == "previous-run" {
						state.Run = api.NewID("run")
					}
					if identity == "foreign-runtime" {
						state.Runtime = "codex"
					}
				}
				if err := writePrivateJSON(ownerHelperPath(front.URL, f.task.ID), state); err != nil {
					t.Fatal(err)
				}
				var oldBinding runtimeBinding
				if identity == "previous-run" {
					oldBinding = runtimeBinding{Hub: front.URL, Task: f.task.ID, Agent: state.Agent, Run: state.Run, Thread: state.Thread, Runtime: "claude", Role: api.AgentRoleOwnerHelper, Session: "owner"}
					if err := writeRelayBinding(oldBinding); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"register", "--task", f.task.ID, "--json"}
				if explicit {
					args = append(args, "--request-id", state.PendingRequest)
				}
				text, err := captureCLIOutput(t, func() error { return cmdHelper(e, args) })
				if identity == "foreign-thread" || identity == "foreign-runtime" {
					if err == nil || !strings.Contains(err.Error(), "another saved runtime/thread") {
						t.Fatalf("foreign legacy caller %q %v", text, err)
					}
					if current, _ := f.c.GetAgent(context.Background(), f.task.ID, committed.Agent.ID); current.RunID != committed.Agent.RunID {
						t.Fatal("foreign legacy caller changed run")
					}
					if after, _ := loadOwnerHelperFile(front.URL, f.task.ID); after != state {
						t.Fatal("foreign legacy caller changed saved evidence")
					}
					return
				}
				var replay struct {
					api.OwnerActionResult
					Ready        bool
					RecoveryOnly bool
					Detail       string
				}
				if jsonErr := json.Unmarshal([]byte(text), &replay); jsonErr != nil {
					t.Fatalf("output %q %v", text, jsonErr)
				}
				if err != nil || !replay.Replay || replay.Agent.RunID != committed.Agent.RunID || replay.Registration.RequestID != state.PendingRequest {
					t.Fatalf("legacy replay %+v %v", replay, err)
				}
				if identity == "exact" {
					binding, ok := readBinding(t, front.URL, committed.Agent.ID)
					if replay.RecoveryOnly || !ok || binding.Thread != f.thread || binding.Run != committed.Agent.RunID || binding.Runtime != "claude" {
						t.Fatalf("exact legacy replay %+v binding %+v", replay, binding)
					}
					again, err := f.register(t, e, "--request-id", state.PendingRequest)
					if err != nil || !again.Replay || again.Agent.RunID != committed.Agent.RunID {
						t.Fatalf("exact legacy explicit replay %+v %v", again, err)
					}
					return
				}
				if replay.Ready || !replay.RecoveryOnly || !strings.Contains(replay.Detail, "not ready") || !strings.Contains(replay.Detail, "new explicit --request-id") {
					t.Fatalf("unknown thread reported ready %+v", replay)
				}
				if binding, ok := readBinding(t, front.URL, committed.Agent.ID); identity == "previous-run" {
					if !ok || binding != oldBinding {
						t.Fatal("receipt recovery changed previous run binding")
					}
				} else if ok {
					t.Fatal("unknown legacy thread installed wake binding")
				}
				if tags := f.tags(t, "owner"); tags["TAILTERM_AGENT"] != "" {
					t.Fatalf("unknown legacy thread tagged session %v", tags)
				}
				if after, _ := loadOwnerHelperFile(front.URL, f.task.ID); after != state {
					t.Fatal("legacy recovery erased pending evidence")
				}
				humanText, err := captureCLIOutput(t, func() error { return cmdHelper(e, []string{"register", "--task", f.task.ID}) })
				if err != nil || !strings.Contains(humanText, "not ready") || !strings.Contains(humanText, "new explicit --request-id") {
					t.Fatalf("partial human output %q %v", humanText, err)
				}
				// Repeating recovery is still the same receipt, never a fresh run.
				again, err := f.register(t, e)
				if err != nil || !again.Replay || again.Agent.RunID != committed.Agent.RunID {
					t.Fatalf("repeated recovery %+v %v", again, err)
				}
				fresh, err := f.register(t, e, "--request-id", "deliberate-fresh-thread")
				if err != nil || fresh.Replay || fresh.Agent.RunID == committed.Agent.RunID {
					t.Fatalf("explicit deliberate registration %+v %v", fresh, err)
				}
				binding, ok := readBinding(t, front.URL, fresh.Agent.ID)
				if !ok || binding.Thread != f.thread || binding.Run != fresh.Agent.RunID {
					t.Fatalf("deliberate thread binding %+v", binding)
				}
			})
		}
	}
}

// A pane that inherited another project's helper identity is not an agent
// session: it reaches the one-project-per-tmux-session check, so the refusal
// names the flag that moves the session. A real agent session stays refused.
func TestHelperRegisterInheritedOtherProjectIdentity(t *testing.T) {
	f := newHelperFixture(t)
	ctx := context.Background()
	a := *f.mustRegister(t).Agent
	second, err := f.c.CreateTask(ctx, api.CreateTaskRequest{Name: "Second project", Orchestrator: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	inherited := env{hub: f.owner.hub, task: f.task.ID, agent: a.ID, runID: a.RunID}
	registerSecond := func(e env, args ...string) (string, error) {
		t.Helper()
		old := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		_, runErr := captureCLIOutput(t, func() error {
			return cmdHelper(e, append([]string{"register", "--task", second.ID}, args...))
		})
		_ = w.Close()
		os.Stderr = old
		warning, _ := io.ReadAll(r)
		_ = r.Close()
		return string(warning), runErr
	}
	secondHelpers := func() []api.Agent {
		t.Helper()
		agents, err := f.c.ListAgents(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		var helpers []api.Agent
		for _, agent := range agents {
			if agent.Role == api.AgentRoleOwnerHelper {
				helpers = append(helpers, agent)
			}
		}
		return helpers
	}
	tagsBefore := f.tags(t, "owner")
	stateBefore, _ := os.ReadFile(ownerHelperPath(f.owner.hub, f.task.ID))
	unchanged := func(what string) {
		t.Helper()
		if helpers := secondHelpers(); len(helpers) != 0 {
			t.Fatalf("%s: refused registration reached the hub: %+v", what, helpers)
		}
		if _, err := os.Stat(ownerHelperPath(f.owner.hub, second.ID)); !os.IsNotExist(err) {
			t.Fatalf("%s: refused registration wrote helper state: %v", what, err)
		}
		if tags := f.tags(t, "owner"); fmt.Sprint(tags) != fmt.Sprint(tagsBefore) {
			t.Fatalf("%s: tags %v, want %v", what, tags, tagsBefore)
		}
		if state, _ := os.ReadFile(ownerHelperPath(f.owner.hub, f.task.ID)); !bytes.Equal(state, stateBefore) {
			t.Fatalf("%s: first project's helper file changed: %s", what, state)
		}
		if current, _ := f.c.GetAgent(ctx, f.task.ID, a.ID); current.RunID != a.RunID {
			t.Fatalf("%s: first project's helper changed", what)
		}
	}

	// The inherited helper identity gets the session refusal, not the agent one.
	_, err = registerSecond(inherited)
	if err == nil || strings.Contains(err.Error(), "not an agent session") || !strings.Contains(err.Error(), "one project per tmux session") || !strings.Contains(err.Error(), f.task.ID) || !strings.Contains(err.Error(), "--take-session") {
		t.Fatalf("inherited helper identity of another project: %v", err)
	}
	unchanged("inherited helper identity")

	// A real agent session, and an identity that is not shown to be an owner
	// helper, are refused as agent sessions even with the flag.
	for what, e := range map[string]env{
		"agent session":             {hub: f.owner.hub, task: f.task.ID, agent: f.lead.ID, runID: f.lead.RunID},
		"agent session, no project": {hub: f.owner.hub, agent: f.lead.ID, runID: f.lead.RunID},
		"helper of no such project": {hub: f.owner.hub, task: second.ID, agent: a.ID, runID: a.RunID},
		"unknown agent":             {hub: f.owner.hub, task: f.task.ID, agent: api.NewID("agt")},
	} {
		if _, err := registerSecond(e, "--take-session"); err == nil || !strings.Contains(err.Error(), "not an agent session") {
			t.Fatalf("%s: %v", what, err)
		}
		unchanged(what)
	}

	// This project's own helper re-registers even when the pane's inherited
	// project is another one.
	again, err := f.register(t, env{hub: f.owner.hub, task: second.ID, agent: a.ID, runID: a.RunID})
	if err != nil || again.Agent.ID != a.ID || again.Registration.Mode != api.OwnerHelperReplaced || again.Registration.PreviousRunID != a.RunID {
		t.Fatalf("own helper with another inherited project: %+v %v", again, err)
	}
	a = *again.Agent
	inherited.runID = a.RunID

	// The explicit flag moves the session, with the warning naming the cost.
	warning, err := registerSecond(inherited, "--take-session")
	helpers := secondHelpers()
	if err != nil || len(helpers) != 1 || !strings.Contains(warning, f.task.ID) || !strings.Contains(warning, "wake-ups stop") {
		t.Fatalf("take-session: %v %+v %q", err, helpers, warning)
	}
	if tags := f.tags(t, "owner"); tags["TAILTERM_TASK"] != second.ID || tags["TAILTERM_AGENT"] != helpers[0].ID {
		t.Fatalf("tags after take-session: %v", tags)
	}
}
