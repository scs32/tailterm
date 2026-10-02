package main

import (
	"bytes"
	"context"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
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
		if m.From.AgentID != a.ID || m.ReplyTo != want.replyTo || m.Text != want.text {
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
	if !strings.Contains(line, `tt helper reply --task tsk_0123456789abcdef 42 --text "your reply"`) || !strings.Contains(line, "tt ack 42") || strings.Contains(line, "Reply on the shared board") {
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
