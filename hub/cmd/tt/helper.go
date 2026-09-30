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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The owner helper (docs/owner-helper.md): the owner's own Claude Code
// session, registered as the project's owner_helper agent.

const helperUsage = "usage: tt helper register --task T [--name N] [--request-id K] [--json]\n" +
	"       tt helper env --task T\n" +
	"       tt helper inbox --task T\n" +
	"       tt helper reply --task T SEQ --text TEXT [--request-id K]"

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
	PendingRequest string `json:"pendingRequestId,omitempty"`
	PendingHash    string `json:"pendingHash,omitempty"`
}

func ownerHelperPath(hub, task string) string {
	return filepath.Join(relayDir(), fmt.Sprintf("%x-%s.owner-helper.json", sha256.Sum256([]byte(hub)), task))
}

func loadOwnerHelperFile(hub, task string) (ownerHelperFile, error) {
	f := ownerHelperFile{Hub: hub, Task: task}
	data, err := os.ReadFile(ownerHelperPath(hub, task))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil || f.Hub != hub || f.Task != task {
		return ownerHelperFile{Hub: hub, Task: task}, errors.New("owner helper state is unreadable; remove " + ownerHelperPath(hub, task) + " and register again")
	}
	return f, nil
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

// helperSession is the tmux session the owner's Claude Code pane runs in.
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

func helperRequestHash(req api.RegisterOwnerHelperRequest) string {
	req.RequestID = ""
	data, _ := json.Marshal(req)
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

func helperRegister(e env, args []string) error {
	fs := flag.NewFlagSet("helper register", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	name := fs.String("name", api.DefaultOwnerHelperName, "the helper's agent name")
	requestID := fs.String("request-id", "", "stable retry key (default: a new key per registration)")
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
	// An agent session may not register, except a pane that inherited this
	// project's helper identity.
	if e.agent != "" {
		a, err := c.GetAgent(ctx, *task, e.agent)
		if err != nil || a.Role != api.AgentRoleOwnerHelper {
			return errors.New("tt helper register runs in the owner's own Claude Code session, not an agent session")
		}
	}
	thread := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if os.Getenv("CLAUDECODE") == "" || !threadIDPattern.MatchString(thread) {
		return errors.New("run tt helper register from the owner's Claude Code session (CLAUDE_CODE_SESSION_ID is not set)")
	}
	if !claudeTranscriptExists(thread) {
		return fmt.Errorf("no Claude transcript for session %s under ~/.claude/projects", thread)
	}
	session, inTmux, err := currentHelperSession(ctx)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	req := api.RegisterOwnerHelperRequest{Name: *name, Host: spawn.Host(), Session: "terminal", Runtime: "claude", Cwd: cwd}
	if inTmux {
		req.Session = session.Name
	}
	state, err := loadOwnerHelperFile(e.hub, *task)
	if err != nil {
		return err
	}
	hash := helperRequestHash(req)
	pending := false
	switch {
	case *requestID != "":
		req.RequestID = *requestID
	case state.PendingRequest != "" && state.PendingHash == hash:
		// The last attempt's outcome is unknown: replay it.
		req.RequestID, pending = state.PendingRequest, true
	default:
		if req.RequestID, err = newHelperRequestID(); err != nil {
			return err
		}
		state.PendingRequest, state.PendingHash, pending = req.RequestID, hash, true
		if err := writePrivateJSON(ownerHelperPath(e.hub, *task), state); err != nil {
			return err
		}
	}
	clearPending := func() {
		if pending {
			state.PendingRequest, state.PendingHash = "", ""
			_ = writePrivateJSON(ownerHelperPath(e.hub, *task), state)
		}
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
	if a == nil || out.Registration == nil || a.Role != api.AgentRoleOwnerHelper || a.Status == api.AgentClosed || a.Status == api.AgentExited || out.Registration.RunID != a.RunID {
		clearPending()
		return errors.New("the hub did not return a live owner helper run; register again")
	}
	current, err := c.GetAgent(ctx, *task, a.ID)
	if err != nil {
		return err
	}
	if current.RunID != a.RunID || current.Role != api.AgentRoleOwnerHelper || current.Status == api.AgentClosed || current.Status == api.AgentExited {
		clearPending()
		return errors.New("the registered run is no longer the helper's current run; register again")
	}
	b := runtimeBinding{Hub: e.hub, Task: *task, Agent: a.ID, Run: a.RunID, Thread: thread, Runtime: "claude", Role: api.AgentRoleOwnerHelper, Session: req.Session, Cwd: cwd, CreatedAt: time.Now().UTC()}
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
	state = ownerHelperFile{Hub: e.hub, Task: *task, Agent: a.ID, Name: a.Name, Run: a.RunID, Registration: out.Registration.ID, Session: req.Session, Thread: thread}
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
	return line + fmt.Sprintf("\n  From the owner on Discord. Answer with tt helper reply --task %s %d --text \"your reply\" (it goes back to their DM or thread), or tt ack %d. Until you reply or ack, the hub holds your other posts.", task, m.Seq, m.Seq)
}

// helperReply posts the helper's answer to one message. The Discord bridge
// sends a helper reply to a Discord message back to the owner's DM or
// thread (docs/discord-helper-chat.md).
func helperReply(e env, args []string) error {
	fs := flag.NewFlagSet("helper reply", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	text := fs.String("text", "", "the reply")
	requestID := fs.String("request-id", "", "stable retry identity for this exact reply")
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
	m, err := c.PostMessage(ctx, helper.task, api.PostMessageRequest{AgentID: helper.agent, RunID: helper.runID, ReplyTo: seq, Text: *text, RequestID: *requestID})
	if err != nil {
		return err
	}
	recordUsageContext(helper, helper.task, "post", m.Seq)
	fmt.Printf("posted #%d as %s, replying to #%d\n", m.Seq, helper.agentName, seq)
	return nil
}
