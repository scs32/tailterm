package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The owner helper (docs/owner-helper.md): the owner's own Claude Code or Codex
// session, registered as the project's owner_helper agent.

const helperUsage = "usage: tt helper register --task T [--name N] [--request-id K] [--take-session] [--json]\n" +
	"       tt helper env --task T\n" +
	"       tt helper inbox --task T\n" +
	"       tt helper reply --task T SEQ --text TEXT [--interim] [--request-id K]"

// ownerHelperFile is private host state for one project's helper: the exact
// registered identity and tmux session, and a pending request ID that makes a
// rerun after an uncertain outcome replay instead of registering twice. It
// holds no token.
type ownerHelperFile struct {
	Hub            string `json:"hub"`
	Task           string `json:"task"`
	Agent          string `json:"agent,omitempty"`
	Name           string `json:"name,omitempty"`
	Run            string `json:"run,omitempty"`
	Registration   string `json:"registration,omitempty"`
	Session        string `json:"session,omitempty"`
	SessionID      string `json:"sessionId,omitempty"`
	SessionCreated string `json:"sessionCreated,omitempty"`
	Thread         string `json:"thread,omitempty"`
	Runtime        string `json:"runtime,omitempty"`
	PendingRequest string `json:"pendingRequestId,omitempty"`
	PendingHash    string `json:"pendingHash,omitempty"`
	RequestID      string `json:"requestId,omitempty"`
	RequestHash    string `json:"requestHash,omitempty"`
}

func ownerHelperPath(hub, task string) string {
	return filepath.Join(relayDir(), fmt.Sprintf("%x-%s%s", sha256.Sum256([]byte(hub)), task, ownerHelperSuffix))
}

// Bounds on reading helper files. offlineOwnerHelper runs inside the tool
// hook's deadline, so it gives up past the candidate count.
const (
	ownerHelperFileBytes  = 64 << 10 // largest helper file read
	ownerHelperCandidates = 16       // most helper files one offline lookup reads
	ownerHelperSuffix     = ".owner-helper.json"
)

var errOwnerHelperFileInvalid = errors.New("owner helper file is not valid JSON or is too large")

// readOwnerHelperFile decodes the helper file at path over f, reading at most
// ownerHelperFileBytes. A file that is larger or not JSON gives
// errOwnerHelperFileInvalid; any other error is the read's own.
func readOwnerHelperFile(path string, f *ownerHelperFile) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, ownerHelperFileBytes+1))
	if err != nil {
		return err
	}
	if len(data) > ownerHelperFileBytes || json.Unmarshal(data, f) != nil {
		return errOwnerHelperFileInvalid
	}
	return nil
}

func loadOwnerHelperFile(hub, task string) (ownerHelperFile, error) {
	f := ownerHelperFile{Hub: hub, Task: task}
	err := readOwnerHelperFile(ownerHelperPath(hub, task), &f)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil && !errors.Is(err, errOwnerHelperFileInvalid) {
		return ownerHelperFile{Hub: hub, Task: task}, err
	}
	if err != nil || f.Hub != hub || f.Task != task {
		return ownerHelperFile{Hub: hub, Task: task}, errors.New("owner helper state is unreadable; remove " + ownerHelperPath(hub, task) + " and register again")
	}
	return f, nil
}

// sessionHelperThread is the runtime and thread this process's runtime
// session names: CLAUDE_CODE_SESSION_ID for claude, CODEX_THREAD_ID for
// codex. ok is false when neither is set or both are.
func sessionHelperThread() (runtime, thread string, ok bool) {
	codexThread, claudeThread := os.Getenv("CODEX_THREAD_ID"), os.Getenv("CLAUDE_CODE_SESSION_ID")
	switch {
	case codexThread != "" && claudeThread == "":
		return "codex", codexThread, true
	case claudeThread != "" && codexThread == "":
		return "claude", claudeThread, true
	}
	return "", "", false
}

// offlineOwnerHelper is the helper registered on this host from this exact
// runtime session, found without the hub: the one helper file in the relay
// directory whose runtime and thread are this session's. It trusts a file
// only under the name its own hub and task give it, and returns false for no
// match, for more than one, and past the read bounds. The hub is not asked,
// so a helper closed there still matches until it is registered again.
func offlineOwnerHelper() (ownerHelperFile, bool) {
	runtime, thread, ok := sessionHelperThread()
	if !ok || !threadIDPattern.MatchString(thread) {
		return ownerHelperFile{}, false
	}
	dir := relayDir()
	d, err := os.Open(dir)
	if err != nil {
		return ownerHelperFile{}, false
	}
	defer d.Close()
	var candidates []string
	for {
		names, err := d.Readdirnames(4096) // names only: no stat per entry
		for _, name := range names {
			if !strings.HasSuffix(name, ownerHelperSuffix) {
				continue
			}
			if len(candidates) == ownerHelperCandidates {
				return ownerHelperFile{}, false
			}
			candidates = append(candidates, name)
		}
		if err != nil {
			if err != io.EOF {
				return ownerHelperFile{}, false
			}
			break
		}
	}
	var found ownerHelperFile
	matches := 0
	for _, name := range candidates {
		var f ownerHelperFile
		if readOwnerHelperFile(filepath.Join(dir, name), &f) != nil {
			continue
		}
		if name != filepath.Base(ownerHelperPath(f.Hub, f.Task)) || f.Runtime != runtime || f.Thread != thread ||
			!api.ValidID(f.Agent, "agt") || !toolLedgerAgentOK(f.Agent) || !api.ValidID(f.Task, "tsk") || f.Run == "" {
			continue
		}
		found = f
		matches++
	}
	return found, matches == 1
}

func cmdHelper(e env, args []string) error {
	if len(args) == 0 {
		return errors.New(helperUsage)
	}
	switch args[0] {
	case "register":
		return helperRegister(e, args[1:])
	case "env":
		return helperEnv(e, args[1:])
	case "inbox":
		return helperInbox(e, args[1:])
	case "reply":
		return helperReply(e, args[1:])
	}
	return errors.New(helperUsage)
}

// helperSession is the tmux session the owner's runtime pane runs in.
type helperSession struct {
	ID, Created, Name string
	Panes             int
}

// currentHelperSession reads the session of $TMUX_PANE. Outside tmux it
// returns ok=false.
func currentHelperSession(ctx context.Context) (helperSession, bool, error) {
	pane := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || pane == "" {
		return helperSession{}, false, nil
	}
	fields := []string{"session_id", "session_created", "session_name"}
	for i, f := range fields {
		fields[i] = `"#{q/e:` + f + `}"`
	}
	raw, err := startupTmux(ctx, "display-message", "-p", "-t", pane, "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		return helperSession{}, false, fmt.Errorf("read this tmux session: %w", err)
	}
	var values []string
	if json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &values) != nil || len(values) != 3 {
		return helperSession{}, false, errors.New("cannot read this tmux session's identity")
	}
	s := helperSession{ID: values[0], Created: values[1], Name: values[2]}
	if !sessionIDPattern.MatchString(s.ID) || !sessionTimePattern.MatchString(s.Created) {
		return helperSession{}, false, errors.New("cannot read this tmux session's identity")
	}
	if !api.ValidName(s.Name) {
		return helperSession{}, false, fmt.Errorf("tmux session name %q cannot be an agent session; rename it first (tmux rename-session owner)", s.Name)
	}
	panes, err := startupTmux(ctx, "list-panes", "-s", "-t", s.ID, "-F", "#{pane_id}")
	if err != nil {
		return helperSession{}, false, fmt.Errorf("list this session's panes: %w", err)
	}
	s.Panes = len(strings.Fields(string(panes)))
	return s, true, nil
}

// tagHelperSession sets the identity tags in one tmux command, TAILTERM_ROLE
// first, so no relay pass sees the agent and run without the role.
func tagHelperSession(ctx context.Context, sessionID string, b runtimeBinding) error {
	args := []string{}
	for i, kv := range [][2]string{{"TAILTERM_ROLE", api.AgentRoleOwnerHelper}, {spawn.EnvHub, b.Hub}, {spawn.EnvTask, b.Task}, {spawn.EnvAgent, b.Agent}, {"TAILTERM_RUN", b.Run}} {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, "set-environment", "-t", sessionID, kv[0], kv[1])
	}
	_, err := startupTmux(ctx, args...)
	return err
}

// helperTagNames are the tags tt helper register sets on the owner's session.
var helperTagNames = []string{"TAILTERM_ROLE", spawn.EnvHub, spawn.EnvTask, spawn.EnvAgent, "TAILTERM_RUN"}

// clearHelperTags unsets the tags on a previous helper session, but only when
// that exact session (ID and creation time) still names this helper agent.
// The check and the unset run in tmux's command queue, without a shell.
func clearHelperTags(ctx context.Context, sessionID, created, agent string) error {
	if !sessionIDPattern.MatchString(sessionID) || !sessionTimePattern.MatchString(created) || !api.ValidID(agent, "agt") {
		return nil
	}
	sessions, err := localSessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.ID != sessionID {
			continue
		}
		if s.Created != created || s.Agent != agent || s.Role != api.AgentRoleOwnerHelper {
			return nil // another session now; never touch tags we did not set
		}
		unset := make([]string, len(helperTagNames))
		for i, name := range helperTagNames {
			unset[i] = "set-environment -u -t " + spawn.ShellQuote(sessionID) + " " + name
		}
		cond := fmt.Sprintf("#{&&:#{==:#{session_created},%s},#{&&:#{==:#{TAILTERM_AGENT},%s},#{==:#{TAILTERM_ROLE},%s}}}", created, agent, api.AgentRoleOwnerHelper)
		_, err = startupTmux(ctx, "if-shell", "-F", "-t", sessionID, cond, strings.Join(unset, " ; "))
		return err
	}
	return nil
}

// otherProjectHelper reports the live owner helper of another project (or
// hub) that this tmux session's tags name. One tmux session carries one
// project's helper: tagging it for a second project would stop the first
// one's wake and heartbeat. A helper that is closed, exited, gone from the hub
// or registered again elsewhere no longer uses the session; a lookup that
// fails any other way counts as live.
func otherProjectHelper(ctx context.Context, c *api.Client, sessionID, hub, task string) (ownedSession, bool, error) {
	// Only this session is read: an unrelated session never fails a register.
	fields := []string{spawn.EnvHub, spawn.EnvTask, spawn.EnvAgent, "TAILTERM_RUN", "TAILTERM_ROLE"}
	for i, f := range fields {
		fields[i] = `"#{q/e:` + f + `}"`
	}
	raw, err := startupTmux(ctx, "display-message", "-p", "-t", sessionID, "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		return ownedSession{}, false, fmt.Errorf("read this tmux session's tags: %w", err)
	}
	var f []string
	if json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &f) != nil || len(f) != 5 {
		return ownedSession{}, false, errors.New("cannot read this tmux session's tags")
	}
	s := ownedSession{ID: sessionID, Hub: f[0], Task: f[1], Agent: f[2], Run: f[3], Role: f[4]}
	if s.Role != api.AgentRoleOwnerHelper || (s.Hub == hub && s.Task == task) {
		return ownedSession{}, false, nil
	}
	if s.Hub != hub || !api.ValidID(s.Task, "tsk") || !api.ValidID(s.Agent, "agt") {
		return s, true, nil // not this hub's to look up
	}
	a, err := c.GetAgent(ctx, s.Task, s.Agent)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return ownedSession{}, false, nil
	}
	if err != nil {
		return s, true, nil
	}
	return s, a.Role == api.AgentRoleOwnerHelper && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.RunID == s.Run, nil
}

func helperRequestHash(req api.RegisterOwnerHelperRequest, thread string) string {
	req.RequestID = ""
	data, _ := json.Marshal(struct {
		Request api.RegisterOwnerHelperRequest
		Thread  string
	}{req, thread})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newHelperRequestID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "ohreg-" + hex.EncodeToString(raw[:]), nil
}

func claudeTranscriptExists(thread string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	paths, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", thread+".jsonl"))
	return err == nil && len(paths) > 0
}

// helperRuntimeIdentity refuses mixed provider identities before any state write.
func helperRuntimeIdentity() (runtime, thread, codex, codexHome string, err error) {
	codexThread := os.Getenv("CODEX_THREAD_ID")
	claudeThread := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if codexThread != "" {
		if claudeThread != "" || os.Getenv("CLAUDECODE") != "" {
			return "", "", "", "", errors.New("ambiguous Claude/Codex session identity; use the owner's dedicated runtime session")
		}
		if !threadIDPattern.MatchString(codexThread) {
			return "", "", "", "", errors.New("CODEX_THREAD_ID must be an exact Codex thread UUID")
		}
		codex, err = exec.LookPath("codex")
		if err != nil {
			return "", "", "", "", fmt.Errorf("resolve Codex executable: %w", err)
		}
		codex, err = filepath.Abs(codex)
		if err != nil {
			return "", "", "", "", err
		}
		codexHome = os.Getenv("CODEX_HOME")
		if codexHome != "" {
			codexHome, err = filepath.Abs(codexHome)
			if err != nil {
				return "", "", "", "", err
			}
		}
		return "codex", codexThread, codex, codexHome, nil
	}
	if os.Getenv("CLAUDECODE") == "" || !threadIDPattern.MatchString(claudeThread) {
		return "", "", "", "", errors.New("run tt helper register from the owner's Claude Code session or Codex session (CLAUDE_CODE_SESSION_ID or CODEX_THREAD_ID is not set)")
	}
	if !claudeTranscriptExists(claudeThread) {
		return "", "", "", "", fmt.Errorf("no Claude transcript for session %s under ~/.claude/projects", claudeThread)
	}
	return "claude", claudeThread, "", "", nil
}

func helperRegister(e env, args []string) error {
	fs := flag.NewFlagSet("helper register", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	name := fs.String("name", api.DefaultOwnerHelperName, "the helper's agent name")
	requestID := fs.String("request-id", "", "stable retry key (default: a new key per registration)")
	takeSession := fs.Bool("take-session", false, "register even when this tmux session is another project's live owner helper; that helper's wake stops")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New(helperUsage)
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	// An agent session may not register, except a pane that inherited an
	// owner helper's identity. The identity is looked up in its own project,
	// then in this one: another project's helper goes on to the session check
	// below.
	if e.agent != "" {
		isHelper := false
		homes := []string{*task}
		if api.ValidID(e.task, "tsk") && e.task != *task {
			homes = []string{e.task, *task}
		}
		for _, home := range homes {
			a, err := c.GetAgent(ctx, home, e.agent)
			if isHelper = err == nil && a.Role == api.AgentRoleOwnerHelper; isHelper {
				break
			}
		}
		if !isHelper {
			return errors.New("tt helper register runs in the owner's own Claude Code or Codex session, not an agent session")
		}
	}
	runtime, thread, codex, codexHome, err := helperRuntimeIdentity()
	if err != nil {
		return err
	}
	session, inTmux, err := currentHelperSession(ctx)
	if err != nil {
		return err
	}
	// One project per tmux session: checked before anything is registered or
	// written.
	if inTmux {
		other, live, err := otherProjectHelper(ctx, c, session.ID, e.hub, *task)
		if err != nil {
			return err
		}
		if live {
			where := "project " + other.Task
			if other.Hub != e.hub {
				where += " on " + other.Hub
			}
			if !*takeSession {
				return fmt.Errorf("tmux session %s is the owner helper of %s; one project per tmux session: register from another tmux session, or pass --take-session to move this one (that project's helper wake stops)", session.Name, where)
			}
			fmt.Fprintf(os.Stderr, "[tt] warning: taking tmux session %s from the owner helper of %s; its wake-ups stop and it shows offline until it registers from another tmux session\n", session.Name, where)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	req := api.RegisterOwnerHelperRequest{Name: *name, Host: spawn.Host(), Session: "terminal", Runtime: runtime, Cwd: cwd}
	if inTmux {
		req.Session = session.Name
	}
	state, err := loadOwnerHelperFile(e.hub, *task)
	if err != nil {
		return err
	}
	hash := helperRequestHash(req, thread)
	// Before thread-aware hashes, Claude stored only the request shape. A
	// matching key can recover its receipt, but is not proof of its thread.
	legacyReq := req
	legacyReq.RequestID = ""
	legacyBytes, _ := json.Marshal(legacyReq)
	legacySum := sha256.Sum256(legacyBytes)
	legacyPending := runtime == "claude" && state.PendingRequest != "" &&
		state.PendingHash == hex.EncodeToString(legacySum[:]) &&
		(*requestID == "" || *requestID == state.PendingRequest)
	if legacyPending && (state.Runtime != "" && state.Runtime != runtime || state.Thread != "" && state.Thread != thread) {
		return errors.New("legacy registration retry belongs to another saved runtime/thread; use a new key")
	}
	switch {
	case legacyPending:
		req.RequestID = state.PendingRequest
	case *requestID != "":
		if state.PendingRequest == *requestID && state.PendingHash != hash || state.RequestID == *requestID && state.RequestHash != hash {
			return errors.New("registration retry key belongs to another runtime/thread; use a new key")
		}
		req.RequestID = *requestID
	case state.PendingRequest != "" && state.PendingHash == hash:
		// The last attempt's outcome is unknown: replay it.
		req.RequestID = state.PendingRequest
	default:
		if req.RequestID, err = newHelperRequestID(); err != nil {
			return err
		}
	}
	if !legacyPending {
		state.PendingRequest, state.PendingHash = req.RequestID, hash
		if err := writePrivateJSON(ownerHelperPath(e.hub, *task), state); err != nil {
			return err
		}
	}
	clearPending := func() {
		if legacyPending {
			return // retain the original recovery evidence
		}
		state.PendingRequest, state.PendingHash = "", ""
		_ = writePrivateJSON(ownerHelperPath(e.hub, *task), state)
	}
	out, err := c.RegisterOwnerHelper(ctx, *task, req)
	if err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) {
			clearPending() // a definite refusal: nothing was registered
		}
		return err
	}
	// Verify before any local write, so a stale replay never tags a session.
	a := out.Agent
	if a == nil || out.Registration == nil || a.Role != api.AgentRoleOwnerHelper || a.Status == api.AgentClosed || a.Status == api.AgentExited || out.Registration.RunID != a.RunID || a.Runtime != runtime || out.Registration.Runtime != runtime {
		clearPending()
		return errors.New("the hub did not return a live owner helper run; register again")
	}
	current, err := c.GetAgent(ctx, *task, a.ID)
	if err != nil {
		return err
	}
	if current.Runtime != runtime || current.RunID != a.RunID || current.Role != api.AgentRoleOwnerHelper || current.Status == api.AgentClosed || current.Status == api.AgentExited {
		clearPending()
		return errors.New("the registered run is no longer the helper's current run; register again")
	}
	if legacyPending && (state.Thread == "" || state.Agent != a.ID || state.Run != a.RunID) {
		// An older saved helper thread can belong to the previous run, not this
		// uncertain registration. Never promote it into evidence for a new run.
		detail := "legacy registration receipt recovered; not ready: original thread evidence is missing. Run tt helper register with a new explicit --request-id from the intended runtime session to register deliberately"
		if *asJSON {
			printJSON(struct {
				api.OwnerActionResult
				Ready        bool   `json:"ready"`
				RecoveryOnly bool   `json:"recoveryOnly"`
				Detail       string `json:"detail"`
			}{OwnerActionResult: out, Ready: false, RecoveryOnly: true, Detail: detail})
		} else {
			fmt.Printf("recovered receipt %s for helper %s, run %s\n%s\n", out.Registration.ID, a.ID, a.RunID, detail)
		}
		return nil
	}
	b := runtimeBinding{Hub: e.hub, Task: *task, Agent: a.ID, Run: a.RunID, Thread: thread, Runtime: runtime, Codex: codex, CodexHome: codexHome, Role: api.AgentRoleOwnerHelper, Session: req.Session, Cwd: cwd, CreatedAt: time.Now().UTC()}
	if inTmux {
		if err := tagHelperSession(ctx, session.ID, b); err != nil {
			return fmt.Errorf("tag this tmux session: %w", err)
		}
	}
	if state.SessionID != "" && (!inTmux || state.SessionID != session.ID || state.SessionCreated != session.Created) {
		if err := clearHelperTags(ctx, state.SessionID, state.SessionCreated, state.Agent); err != nil {
			fmt.Fprintln(os.Stderr, "[tt] could not clear the previous helper session's tags:", err)
		}
	}
	if state.Agent != "" && state.Agent != a.ID {
		_ = os.Remove(filepath.Join(relayDir(), bindingKey(runtimeBinding{Hub: e.hub, Agent: state.Agent})+".binding.json"))
	}
	if inTmux {
		if err := writeRelayBinding(b); err != nil {
			return fmt.Errorf("write the wake binding: %w", err)
		}
	} else {
		_ = os.Remove(filepath.Join(relayDir(), bindingKey(b)+".binding.json"))
	}
	state = ownerHelperFile{Hub: e.hub, Task: *task, Agent: a.ID, Name: a.Name, Run: a.RunID, Registration: out.Registration.ID, Session: req.Session, Thread: thread, Runtime: runtime, RequestID: req.RequestID, RequestHash: hash}
	if inTmux {
		state.SessionID, state.SessionCreated = session.ID, session.Created
	}
	if err := writePrivateJSON(ownerHelperPath(e.hub, *task), state); err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	r := out.Registration
	fmt.Printf("owner helper %s (%s) %s: run %s", a.Name, a.ID, r.Mode, a.RunID)
	if r.PreviousRunID != "" {
		fmt.Printf(" (replaced %s)", r.PreviousRunID)
	}
	fmt.Printf(", receipt %s\n", r.ID)
	switch {
	case !inTmux:
		fmt.Println("registered without wake: not in tmux, so the relay cannot wake this session and it shows offline")
	case session.Panes != 1:
		fmt.Fprintf(os.Stderr, "[tt] warning: tmux session %s has %d panes; wake-ups need exactly one\n", session.Name, session.Panes)
	}
	fmt.Printf("act as the helper: eval \"$(tt helper env --task %s)\"\n", *task)
	return nil
}

// helperOffline reports an owner helper whose session is not running: it is
// shown offline, not stuck or crashed.
func helperOffline(a api.Agent) bool {
	return a.Role == api.AgentRoleOwnerHelper && !a.Online && a.Status != api.AgentClosed && a.Status != api.AgentExited
}

// verifiedHelper is this host's registered helper for task, checked against
// the hub: the live owner_helper agent whose current run is the registered one.
func verifiedHelper(e env, task string) (env, api.Agent, error) {
	if !api.ValidID(task, "tsk") {
		return env{}, api.Agent{}, errors.New(helperUsage)
	}
	state, err := loadOwnerHelperFile(e.hub, task)
	if err != nil {
		return env{}, api.Agent{}, err
	}
	if state.Agent == "" || state.Run == "" {
		return env{}, api.Agent{}, errors.New("no owner helper is registered on this host for this project; run tt helper register")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return env{}, api.Agent{}, err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	a, err := c.GetAgent(ctx, task, state.Agent)
	if err != nil {
		return env{}, api.Agent{}, err
	}
	switch {
	case a.Role != api.AgentRoleOwnerHelper:
		return env{}, api.Agent{}, errors.New("the registered agent is not an owner helper")
	case a.Status == api.AgentClosed || a.Status == api.AgentExited:
		return env{}, api.Agent{}, fmt.Errorf("the owner helper is %s; run tt helper register", a.Status)
	case a.RunID != state.Run:
		return env{}, api.Agent{}, errors.New("the owner helper was registered again elsewhere; run tt helper register here to take it back")
	}
	// Host state is shared across sessions. A replaced caller must never adopt
	// the successor identity just because its host file now names that run.
	if e.agent != "" && (e.agent != state.Agent || e.runID != state.Run) {
		return env{}, api.Agent{}, errors.New("this caller is not the registered helper run; register again")
	}
	runtime, thread, single := sessionHelperThread()
	both := !single && os.Getenv("CODEX_THREAD_ID") != ""
	if both || single && (a.Runtime != runtime || thread != state.Thread) ||
		state.Runtime != "" && state.Runtime != a.Runtime {
		return env{}, api.Agent{}, errors.New("this session is not the registered helper thread/runtime; register again")
	}
	helper := e
	helper.task, helper.agent, helper.agentName, helper.runID, helper.session = task, a.ID, a.Name, a.RunID, a.Session
	return helper, a, nil
}

func helperEnv(e env, args []string) error {
	fs := flag.NewFlagSet("helper env", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	helper, _, err := verifiedHelper(e, *task)
	if err != nil {
		return err
	}
	// Never the token: the owner's own configuration supplies it.
	fmt.Printf("export %s=%s %s=%s %s=%s TAILTERM_RUN=%s %s=%s\n", spawn.EnvHub, spawn.ShellQuote(helper.hub), spawn.EnvTask, helper.task, spawn.EnvAgent, helper.agent, helper.runID, spawn.EnvAgentName, spawn.ShellQuote(helper.agentName))
	return nil
}

// helperInbox reads the helper's unread messages as the helper, in-process,
// so a wake prompt needs no shell evaluation. It marks them read. A message
// the owner sent from Discord shows how to answer it there.
func helperInbox(e env, args []string) error {
	fs := flag.NewFlagSet("helper inbox", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	helper, _, err := verifiedHelper(e, *task)
	if err != nil {
		return err
	}
	c, err := helper.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	agents, err := c.ListAgents(ctx, helper.task)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	after, err := readCursor(ctx, c, helper.task, helper.agent)
	if err != nil {
		return err
	}
	msgs, err := c.ListMessages(ctx, helper.task, after, helper.agent, 50)
	if err != nil {
		return err
	}
	shown := msgs[:0]
	for _, m := range msgs {
		if m.From.AgentID != helper.agent {
			shown = append(shown, m)
		}
	}
	for _, m := range shown {
		recordUsageContext(helper, helper.task, "inbox", m.Seq)
		fmt.Println(helperMessageLine(m, names, helper.task))
	}
	if len(shown) == 0 {
		fmt.Println("(no messages)")
		return nil
	}
	return c.MarkRead(ctx, helper.task, api.MarkReadRequest{AgentID: helper.agent, UpTo: shown[len(shown)-1].Seq})
}

// helperMessageLine is the inbox line. The owner's Discord messages get a
// tt helper reply hint in place of the shared-board one, since only a
// helper reply goes back to their DM or thread.
func helperMessageLine(m api.Message, names map[string]string, task string) string {
	line := formatMessage(m, names)
	if m.From.AgentID != "" || m.Source == nil || m.Source.Kind != api.SourceDiscord {
		return line
	}
	line, _, _ = strings.Cut(line, "\n  Reply on the shared board:")
	return line + fmt.Sprintf("\n  From the owner on Discord. Answer with tt helper reply --task %s %d --text \"your reply\" (it goes back to their DM or thread and closes the obligation; no second typed answer is needed). Add --interim for a progress reply that keeps the obligation open, or tt ack %d to acknowledge without replying. Until you reply or ack, the hub holds your other posts.", task, m.Seq, m.Seq)
}

// helperReplyRequestID names one exact reply, so a rerun after a timeout
// returns the original post instead of sending the owner a second copy.
func helperReplyRequestID(task string, seq int64, text string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", task, seq, text)))
	return "helper-reply-" + hex.EncodeToString(sum[:12])
}

// helperReply posts the helper's answer to one message. The Discord bridge
// sends a helper reply to a Discord message back to the owner's DM or
// thread (docs/discord-helper-chat.md).
func helperReply(e env, args []string) error {
	fs := flag.NewFlagSet("helper reply", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	text := fs.String("text", "", "the reply")
	interim := fs.Bool("interim", false, "record progress and keep the obligation open instead of answering it")
	requestID := fs.String("request-id", "", "stable retry identity (default: derived from the project, SEQ, text and interim mode, so running the same reply again posts once)")
	// SEQ may come before or after the flags.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return errors.New(helperUsage)
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(positional[0], "#"), 10, 64)
	if err != nil || seq <= 0 {
		return fmt.Errorf("SEQ must be a message number, got %q", positional[0])
	}
	if strings.TrimSpace(*text) == "" {
		return errors.New("tt helper reply needs --text")
	}
	helper, _, err := verifiedHelper(e, *task)
	if err != nil {
		return err
	}
	c, err := helper.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if *requestID == "" {
		*requestID = helperReplyRequestID(helper.task, seq, *text)
		if *interim {
			*requestID += "-interim"
		}
	}
	req := api.PostMessageRequest{AgentID: helper.agent, RunID: helper.runID, ReplyTo: seq, Text: *text, RequestID: *requestID}
	// Include closed obligations: an identical retry must retain its envelope
	// after the first post has settled the obligation. Without an obligation,
	// preserve the existing plain reply and its display text.
	obligations, err := c.ListObligationsFrom(ctx, helper.task, helper.agent, seq, seq)
	if err != nil {
		return err
	}
	for _, o := range obligations {
		if o.MessageSeq != seq || o.Needs == api.ObligationNeedsDelivery {
			continue
		}
		if *interim {
			req.Envelope = &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Progress on the owner message", Body: api.EnvelopeBody{Text: *text}}
		} else if o.SourceKind == "human" || o.Needs == api.ObligationNeedsAnswer || o.SourceKind == api.EnvelopeKindBlock {
			req.Envelope = &api.Envelope{Kind: api.EnvelopeKindAnswer, Subject: "Answer to the owner message", Body: api.EnvelopeBody{Answer: *text}}
		} else if o.Needs == api.ObligationNeedsOutcome {
			req.Envelope = &api.Envelope{
				Kind: api.EnvelopeKindResult, Subject: "Final reply to the owner message",
				Refs:     map[string]string{"helperReply": "final"},
				Body:     api.EnvelopeBody{Outcome: "done", Text: *text, Status: map[string]string{"reply": "pass"}},
				Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: "The final helper reply is recorded in this message."}},
			}
		}
		if req.Envelope != nil {
			req.Text = "" // the hub renders the typed Board text
		}
		break
	}
	m, err := c.PostMessage(ctx, helper.task, req)
	if err != nil {
		return err
	}
	recordUsageContext(helper, helper.task, "post", m.Seq)
	fmt.Printf("posted #%d as %s, replying to #%d\n", m.Seq, helper.agentName, seq)
	return nil
}
