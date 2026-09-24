// Command tt is the tailterm agent CLI: it registers agent sessions with the
// hub, posts events and messages, spawns sibling agents on this host, and runs
// durable inbox integration for agent runtimes.
package main

import (
	"context"
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
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

const usage = `tt — tailterm agent CLI

Identity comes from the environment tailterm sets on agent sessions:
  TAILTERM_HUB   TAILTERM_TASK   TAILTERM_AGENT   TAILTERM_AGENT_NAME

Commands
  doctor                       check hub, tmux, and installed runtimes
  relay [--status]             resume Codex agents for unread directed messages
  bind [--thread UUID]        bind this agent to its exact Codex thread
  brief                        print the shared task briefing
  status                       identity, hub reachability, own agent, unread count
  projects                     list projects on the hub (tasks is an alias)
  project-pause <get|pause|handoff|resume>  explicit project team lifecycle
  work-items <command>         list/get/create/update/dispatch/history/evidence for bugs and features
  queue <command>              list/get/history/changes/action/receipt for deliberate Queue work
  agents [--json]              list agents on this task
  event <kind> [--text T]      post started|running|done|needs_input|exited|closed
  post <text> [--to AGENT]     post a message to the task or one agent
  message-audit <command>      get/history/changes/correct/resolve/associate/receipt
  capabilities [--json]       show supported audit contracts and policy mode
  audit-export <command>       create/download an immutable project audit export
  ask --request-id KEY --file PATH [--json]  request an owner decision on the Board
  send --kind K --subject S [fields] | --file F   post a typed board message
  message-checks [--since 24h] [--summary] [--json]  typed-message adoption and Jev scores
  obligations [--overdue] [--all] [--json]  what you owe, and what is overdue in the project
  ack SEQ | progress SEQ [--text T]  acknowledge or record progress on an obligation
  reassign OBLIGATION_ID --to AGENT [--reason T]  move an open obligation (lead or owner)
  inbox [--unread] [--mark-read] [--wait 9m] [--json]
  context [--json]             print this exact run's bound work-item context
  owner extend|answer|cancel OBLIGATION_ID ...  the owner's controls over an obligation
  operational-record get       read a legacy operational record (writes retired in phase 3)
  current-assignment [--item ID --action-key KEY] [--json]  read a legacy directive
  delivery coverage [--json]   read legacy directive coverage (writes retired in phase 3)
  spawn --name N --run CMD [--cwd D] [--prompt P] [--runtime R] [--task ID]
        [--expected-lifecycle-generation N] [--resume-receipt-id ID]
                               start a sibling agent session on this host
  allocation-intent create --agent-id ID --work-item ID --work-item-revision N
                             --work-order-message N --team-role member|extra
                             --work-context-file F|--work-context-json J
                             [--launcher-agent-id ID --launcher-run-id ID]
                             [--expected-run-id ID] [--request-id KEY]
                               author a pre-admission member/extra intent bound
                               to the exact prepared context digest, a
                               preallocated expected run and the intended
                               launcher (default: the authoring agent itself);
                               freeze --expected-run-id when reusing
                               --request-id so an uncertain retry replays the
                               same record instead of conflicting
  allocation-intent get --agent-id ID
                               non-destructive readback of a recorded intent,
                               before or after consumption
  retire [AGENT]              disable inbox wake-ups; preserve terminal and results
  resume [AGENT]              re-enable inbox wake-ups for a retired agent
  close [--json] [AGENT]       exact-run closeout on this host (default: self)
  cleanup --task ID [--json]   retry exact cleanup for closed local agents
  watch                        deprecated; inbox delivery never types into panes
  wrap -- CMD                  run CMD, reporting started/exited to the hub
  tools [--runtime APP] [--cwd DIR] [--json]  inspect host or current Codex thread tools
  runtimes [--json]            agent CLIs available on this host
  hooks <claude|codex|generic> print integration snippets
  hook <session-start|prompt|stop|notification|codex>
                               handlers invoked by agent runtimes
  new-project --name N [--goal G] create a project (new-task is an alias)
`

type env struct {
	hub, task, agent, agentName, session, runID, token, configHub string
}

func readEnv() env {
	e := env{
		token:     os.Getenv("TAILTERM_TOKEN"),
		runID:     os.Getenv("TAILTERM_RUN"),
		hub:       os.Getenv(spawn.EnvHub),
		task:      os.Getenv(spawn.EnvTask),
		agent:     os.Getenv(spawn.EnvAgent),
		agentName: os.Getenv(spawn.EnvAgentName),
		session:   os.Getenv(spawn.EnvSession),
	}
	e.loadConfig()
	return e
}

func (e env) client(timeout time.Duration) (*api.Client, error) {
	if e.hub == "" {
		return nil, errors.New("TAILTERM_HUB is not set")
	}
	c, err := api.NewClient(e.hub, timeout)
	if c != nil {
		c.Token = e.token
	}
	return c, err
}

func (e env) requireTask() (string, error) {
	if e.task == "" {
		return "", errors.New("TAILTERM_TASK is not set (or pass --task)")
	}
	return e.task, nil
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "tt:", err)
	var coded *exitError
	if errors.As(err, &coded) {
		os.Exit(coded.code)
	}
	os.Exit(1)
}

// exitError carries a specific process exit code, such as 2 for invalid input.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	// SSH exec and tmux often start without the interactive shell's PATH.
	// Preserve existing precedence, then add standard user/package locations.
	home, _ := os.UserHomeDir()
	parts := []string{os.Getenv("PATH"), filepath.Dir(selfPath()), filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	_ = os.Setenv("PATH", strings.Join(parts, string(os.PathListSeparator)))
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	e := readEnv()
	autoBindRuntime(e, cmd)
	var err error
	switch cmd {
	case "relay":
		err = cmdRelay(args)
	case "bind":
		err = cmdBind(e, args)
	case "doctor":
		err = cmdDoctor(e)
	case "brief":
		err = cmdBrief(e)
	case "status":
		err = cmdStatus(e)
	case "tasks", "projects":
		err = cmdTasks(e, args)
	case "project-pause":
		err = cmdProjectPause(e, args)
	case "work-items":
		err = cmdWorkItems(e, args)
	case "queue":
		err = cmdQueue(e, args)
	case "agents":
		err = cmdAgents(e, args)
	case "event":
		err = cmdEvent(e, args)
	case "post":
		err = cmdPost(e, args)
	case "message-audit":
		err = cmdMessageAudit(e, args)
	case "capabilities":
		err = cmdCapabilities(e, args)
	case "audit-export":
		err = cmdAuditExport(e, args)
	case "ask":
		err = cmdAsk(e, args)
	case "inbox":
		err = cmdInbox(e, args)
	case "send":
		err = cmdSend(e, args)
	case "message-checks":
		err = cmdMessageChecks(e, args)
	case "obligations":
		err = cmdObligations(e, args)
	case "ack", "progress":
		err = cmdObligationAction(e, cmd, args)
	case "reassign":
		err = cmdReassign(e, args)
	case "context":
		err = cmdContext(e, args)
	case "operational-record":
		err = cmdOperationalRecord(e, args)
	case "current-assignment":
		err = cmdCurrentAssignment(e, args)
	case "delivery":
		err = cmdDelivery(e, args)
	case "owner":
		err = cmdOwner(e, args)
	case "spawn":
		err = cmdSpawn(e, args)
	case "allocation-intent":
		err = cmdAllocationIntent(e, args)
	case "retire", "resume":
		err = cmdRetirement(e, cmd, args)
	case "cleanup":
		err = cmdCleanup(e, args)
	case "close":
		err = cmdClose(e, args)
	case "watch":
		err = cmdWatch(e)
	case "wrap":
		err = cmdWrap(e, args)
	case "tools":
		err = cmdTools(args)
	case "runtimes":
		err = cmdRuntimes(args)
	case "hooks":
		err = cmdHooks(args)
	case "hook":
		err = cmdHook(e, args)
	case "new-task", "new-project":
		err = cmdNewTask(e, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		die(err)
	}
}

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func cmdStatus(e env) error {
	fmt.Printf("hub      %s\n", or(e.hub, "(unset)"))
	fmt.Printf("task     %s\n", or(e.task, "(unset)"))
	fmt.Printf("agent    %s %s\n", or(e.agent, "(unset)"), e.agentName)
	fmt.Printf("host     %s\n", spawn.Host())
	c, err := e.client(5 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	who, err := c.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	fmt.Printf("caller   %s (%s)\n", who.Node, who.User)
	if e.task != "" && e.agent != "" {
		a, err := c.GetAgent(ctx, e.task, e.agent)
		if err != nil {
			return err
		}
		fmt.Printf("status   %s\nunread   %d\n", a.Status, a.Unread)
	}
	return nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func cmdTasks(e env, args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(tasks)
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("%s  %-8s %s\n", t.ID, t.Status, t.Name)
	}
	return nil
}

func cmdAgents(e env, args []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	task := fs.String("task", e.task, "task id")
	_ = fs.Parse(args)
	if *task == "" {
		return errors.New("no task")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	agents, err := c.ListAgents(ctx, *task)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(agents)
		return nil
	}
	for _, a := range agents {
		self := " "
		if a.ID == e.agent {
			self = "*"
		}
		role := ""
		if a.Role != "" {
			role = " role=" + a.Role
		}
		fmt.Printf("%s %s  %-12s %-12s %s@%s%s unread=%d\n", self, a.ID, a.Status, a.Name, a.Session, a.Host, role, a.Unread)
	}
	return nil
}

func postEvent(e env, kind, text string, data map[string]any) error {
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(5 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	_, err = c.PostEvent(ctx, task, api.PostEventRequest{Kind: kind, AgentID: e.agent, RunID: e.runID, Text: text, Data: data})
	return err
}

func cmdEvent(e env, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: tt event <kind> [--text T]")
	}
	fs := flag.NewFlagSet("event", flag.ExitOnError)
	text := fs.String("text", "", "event text")
	reason := fs.String("reason", "", "needs_input reason: permission, authentication or tool")
	_ = fs.Parse(args[1:])
	if !api.PostableKind(args[0]) {
		return fmt.Errorf("kind must be one of started running done needs_input exited closed")
	}
	var data map[string]any
	if *reason != "" {
		if args[0] != api.EventNeedsInput || (*reason != "permission" && *reason != "authentication" && *reason != "tool") {
			return errors.New("reason requires needs_input and must be permission, authentication or tool")
		}
		data = map[string]any{"reason": *reason}
	}
	return postEvent(e, args[0], *text, data)
}

func resolveAgent(ctx context.Context, c *api.Client, task, ref string) (string, error) {
	if ref == "" || api.ValidID(ref, "agt") {
		return ref, nil
	}
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Name == ref && a.Status != api.AgentClosed {
			return a.ID, nil
		}
	}
	return "", fmt.Errorf("no open agent named %q on this task", ref)
}

func resolveCloseAgent(ctx context.Context, c *api.Client, task, ref string) (string, error) {
	if ref == "" || api.ValidID(ref, "agt") {
		return ref, nil
	}
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return "", err
	}
	var matches []api.Agent
	for _, a := range agents {
		if a.Name == ref && (a.Status != api.AgentClosed || !a.CleanupDone) {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple current or pending agents are named %q; use the exact agent ID", ref)
	}
	return "", fmt.Errorf("no current or pending agent named %q on this task", ref)
}

func cmdPost(e env, args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: tt post [--to AGENT] [--reply-to SEQ] [--task ID] <text>")
		fmt.Fprintln(fs.Output(), "Reply to a human on the shared board: tt post --reply-to SEQ \"message\" (omit --to).")
		fmt.Fprintln(fs.Output(), "Use -- before literal text starting with '-'; use - to read text from stdin.")
		fs.PrintDefaults()
	}
	to := fs.String("to", "", "agent id or name")
	reply := fs.Int64("reply-to", 0, "message sequence being answered")
	task := fs.String("task", e.task, "task id")
	requestID := fs.String("request-id", "", "stable retry identity for this exact post")
	intake := fs.Bool("intake", false, "explicitly classify this message as Intake")
	links := registerLinkFlags(fs)
	if err := fs.Parse(postArgs(args)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "-" || (text == "" && !isTerminal(os.Stdin)) {
		b, _ := io.ReadAll(os.Stdin)
		text = strings.TrimSpace(string(b))
	}
	if text == "" {
		return errors.New("usage: tt post <text> [--to AGENT]")
	}
	if *task == "" {
		return errors.New("no task")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	target, err := resolveAgent(ctx, c, *task, *to)
	if err != nil {
		return fmt.Errorf("%w; --to accepts agents only. To reply to a human, use tt post --reply-to SEQ \"message\" without --to (shared board reply)", err)
	}
	req := api.PostMessageRequest{Text: text, To: target, AgentID: e.agent, RunID: e.runID, ReplyTo: *reply}
	if err := links.apply(ctx, c, e, *task, target, *reply, text, *requestID, *intake, &req); err != nil {
		return err
	}
	m, err := c.PostMessage(ctx, *task, req)
	if err != nil {
		return err
	}
	fmt.Printf("posted #%d\n", m.Seq)
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func cmdInbox(e env, args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	unread := fs.Bool("unread", false, "only messages after this agent's read cursor")
	mark := fs.Bool("mark-read", false, "advance the read cursor past the shown messages")
	asJSON := fs.Bool("json", false, "JSON output")
	limit := fs.Int("limit", 50, "maximum messages")
	wait := fs.Duration("wait", 0, "with --unread, block up to this long (max 9m) until an unread message arrives")
	_ = fs.Parse(args)
	if *wait > 0 && (!*unread || e.agent == "") {
		return errors.New("--wait requires --unread and an agent identity")
	}
	if *wait > maxInboxWait {
		*wait = maxInboxWait
	}
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	if *wait > 0 {
		// Park cheaply: runtimes without relay wake-up (Claude) run this as one
		// shell command instead of ending their turn and never seeing new work.
		deadline := time.Now().Add(*wait)
		for {
			n, err := unreadCount(c, task, e.agent)
			if err != nil {
				return err
			}
			if n > 0 || !time.Now().Add(inboxPollInterval).Before(deadline) {
				break
			}
			time.Sleep(inboxPollInterval)
		}
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return err
	}
	names := map[string]string{}
	var after int64
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	if *unread && e.agent != "" {
		after, err = readCursor(ctx, c, task, e.agent)
		if err != nil {
			return err
		}
	}
	msgs, err := c.ListMessages(ctx, task, after, e.agent, *limit)
	if err != nil {
		return err
	}
	if *unread {
		filtered := msgs[:0]
		for _, m := range msgs {
			if m.From.AgentID != e.agent {
				filtered = append(filtered, m)
			}
		}
		msgs = filtered
	}
	if *asJSON {
		printJSON(msgs)
	} else {
		for _, m := range msgs {
			fmt.Println(formatMessage(m, names))
		}
		if len(msgs) == 0 {
			fmt.Println("(no messages)")
		}
	}
	if *mark && e.agent != "" && len(msgs) > 0 {
		return c.MarkRead(ctx, task, api.MarkReadRequest{AgentID: e.agent, UpTo: msgs[len(msgs)-1].Seq})
	}
	return nil
}

// maxInboxWait stays under Claude Code's 10-minute shell command limit.
const maxInboxWait = 9 * time.Minute

var inboxPollInterval = 5 * time.Second

func unreadCount(c *api.Client, task, agent string) (int, error) {
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	after, err := readCursor(ctx, c, task, agent)
	if err != nil {
		return 0, err
	}
	msgs, err := c.ListMessages(ctx, task, after, agent, 50)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range msgs {
		if m.From.AgentID != agent {
			n++
		}
	}
	return n, nil
}

func readCursor(ctx context.Context, c *api.Client, task, agent string) (int64, error) {
	a, err := c.GetAgent(ctx, task, agent)
	if err != nil {
		return 0, err
	}
	return a.ReadUpTo, nil
}

func formatMessage(m api.Message, names map[string]string) string {
	from := "human"
	if m.From.AgentID != "" {
		from = or(names[m.From.AgentID], m.From.AgentID)
	} else if m.From.User != "" {
		from = m.From.User + " (human)"
	}
	to := ""
	if m.To != "" {
		to = " → " + or(names[m.To], m.To)
	}
	if m.Broadcast {
		to += " [swarm: everyone]"
	}
	line := fmt.Sprintf("#%d %s %s%s: %s", m.Seq, m.CreatedAt.Local().Format("15:04"), from, to, m.Text)
	if m.From.AgentID == "" {
		line += fmt.Sprintf("\n  Reply on the shared board: tt post --reply-to %d \"your reply\" (omit --to; sender is human).", m.Seq)
	}
	return line
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "tt"
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// cmdAllocationIntent authors the durable, pre-admission allocation intent
// a fresh parented member/extra tt spawn now requires (independent review
// #2300/#2771/#2840 finding 5, as clarified by #2844/#2850/#2867/#2870):
// functional recorded-allocation consistency, bound to the exact
// preallocated agent identity/item/revision/order/team role, authored by
// the handler/lead BEFORE the worker's own tt spawn --team-role declares
// the same classification -- not derived from execution Start/Queue state.
func cmdAllocationIntent(e env, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: tt allocation-intent create|get ...")
	}
	switch args[0] {
	case "create":
		return cmdAllocationIntentCreate(e, args[1:])
	case "get":
		return cmdAllocationIntentGet(e, args[1:])
	default:
		return fmt.Errorf("unknown allocation-intent command %q", args[0])
	}
}

func cmdAllocationIntentCreate(e env, args []string) error {
	fs := flag.NewFlagSet("allocation-intent create", flag.ExitOnError)
	agentID := fs.String("agent-id", "", "preallocated agent identity this intent authorizes (required)")
	workItemTask := fs.String("work-item-task", "", "project owning the bug or feature (default: --task)")
	workItemID := fs.String("work-item", "", "exact work item this intent is bound to (required)")
	workItemRevision := fs.Int64("work-item-revision", 0, "exact work-item revision (required)")
	workOrderTask := fs.String("work-order-task", "", "project containing the work-order message (default: work-item project)")
	workOrderMessage := fs.Int64("work-order-message", 0, "recorded work-order message sequence (required)")
	teamRole := fs.String("team-role", "", "intended classification: member or extra (required)")
	workContextFile := fs.String("work-context-file", "", "the exact prepared context bundle the launch will use (required, one of file/json -- its sha256 becomes the bound context digest)")
	workContextJSON := fs.String("work-context-json", "", "same as --work-context-file, inline")
	expectedRunID := fs.String("expected-run-id", "", "preallocated run id this intent authorizes (default: freshly generated; required when --request-id is set)")
	launcherAgentID := fs.String("launcher-agent-id", "", "agent authorized to actually perform the launch (default: this authoring agent)")
	launcherRunID := fs.String("launcher-run-id", "", "that agent's current live run (required if --launcher-agent-id is set)")
	requestID := fs.String("request-id", "", "stable retry key: an identical repeat returns the same record, even after consumption")
	task := fs.String("task", e.task, "task id")
	asJSON := fs.Bool("json", false, "print the intent record as JSON")
	_ = fs.Parse(args)
	if !api.ValidID(*agentID, "agt") || *workItemID == "" || *workItemRevision < 1 || *workOrderMessage < 1 ||
		(*teamRole != api.TeamRoleMember && *teamRole != api.TeamRoleExtra) || (*workContextFile == "") == (*workContextJSON == "") {
		return fmt.Errorf("allocation-intent create requires --agent-id, --work-item, --work-item-revision, --work-order-message, --team-role %s or %s, and exactly one of --work-context-file/--work-context-json", api.TeamRoleMember, api.TeamRoleExtra)
	}
	if (*launcherAgentID == "") != (*launcherRunID == "") {
		return errors.New("--launcher-agent-id and --launcher-run-id must be given together")
	}
	// Independent review #3003/#3010 finding 2: a lost-response retry of an
	// unchanged `tt allocation-intent create --request-id K` command must
	// replay identically. Auto-generating a fresh ExpectedRunID on every
	// invocation broke that -- the store's retry-key lookup found the same
	// K but then rejected the newly generated ExpectedRunID as a payload
	// mismatch. --expected-run-id is now required whenever --request-id is
	// used, so the caller freezes it (e.g. once, before the first attempt)
	// and an unchanged retry sends the identical value.
	if *requestID != "" && *expectedRunID == "" {
		return errors.New("--expected-run-id is required when --request-id is set, so an unchanged retry sends the identical value instead of a freshly generated one")
	}
	if e.agent == "" || e.runID == "" {
		return errors.New("allocation-intent create requires an agent session identity (author agent/run); this session has none")
	}
	if *workItemTask == "" {
		*workItemTask = *task
	}
	if *workOrderTask == "" {
		*workOrderTask = *workItemTask
	}
	contextData, err := readPreparedWorkContext(*workContextJSON, *workContextFile)
	if err != nil {
		return err
	}
	// New intent digests must cover the same losslessly compacted RawMessage
	// the AddAgent client sends. Existing intents are never rewritten.
	contextData, err = compactPreparedWorkContext(contextData)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(contextData)
	if *expectedRunID == "" {
		*expectedRunID = api.NewID("run")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	in, err := c.CreateAllocationIntent(ctx, *task, api.CreateAllocationIntentRequest{
		AgentID: *agentID, TargetTaskID: *task, ItemTaskID: *workItemTask, ItemID: *workItemID, ItemRevision: *workItemRevision,
		WorkOrderMessage: api.MessageReference{TaskID: *workOrderTask, Seq: *workOrderMessage}, ContextDigest: hex.EncodeToString(digest[:]),
		TeamRole: *teamRole, AuthorAgentID: e.agent, AuthorRunID: e.runID, ExpectedRunID: *expectedRunID,
		ExpectedLauncherAgentID: *launcherAgentID, ExpectedLauncherRunID: *launcherRunID, RequestID: *requestID,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(in)
		return nil
	}
	fmt.Printf("recorded allocation intent for %s: item %s@%d, team-role %s, expected-run-id %s\n", in.AgentID, in.ItemID, in.ItemRevision, in.TeamRole, in.ExpectedRunID)
	return nil
}

func cmdAllocationIntentGet(e env, args []string) error {
	fs := flag.NewFlagSet("allocation-intent get", flag.ExitOnError)
	agentID := fs.String("agent-id", "", "agent identity to read back (required)")
	task := fs.String("task", e.task, "task id")
	asJSON := fs.Bool("json", false, "print the intent record as JSON")
	_ = fs.Parse(args)
	if !api.ValidID(*agentID, "agt") {
		return errors.New("allocation-intent get requires --agent-id")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	in, err := c.GetAllocationIntent(ctx, *task, *agentID)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(in)
		return nil
	}
	status := "unconsumed"
	if in.ConsumedAt != nil {
		status = "consumed by run " + in.ConsumedByRunID
	}
	fmt.Printf("allocation intent for %s: item %s@%d, team-role %s, expected-run-id %s, %s\n", in.AgentID, in.ItemID, in.ItemRevision, in.TeamRole, in.ExpectedRunID, status)
	return nil
}

func cmdSpawn(e env, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	agentID := fs.String("agent-id", "", "stable preallocated agent identity for launch/retry")
	expectedRunID := fs.String("expected-run-id", "", "exact preallocated resume run or exited database_handler run to restart")
	expectedLifecycleGeneration := fs.Int64("expected-lifecycle-generation", 0, "exact project lifecycle generation required for admission")
	resumeReceiptID := fs.String("resume-receipt-id", "", "durable project Resume receipt authorizing the exact fresh orchestrator")
	workItemTask := fs.String("work-item-task", "", "project owning the bound bug or feature (default: --task)")
	workItemID := fs.String("work-item", "", "single bug or feature bound to this new session")
	workItemRevision := fs.Int64("work-item-revision", 0, "exact work-item revision to restore")
	workOrderTask := fs.String("work-order-task", "", "project containing the recorded work-order message (default: work-item project)")
	workOrderMessage := fs.Int64("work-order-message", 0, "recorded bounded work-order message sequence")
	replacesAgent := fs.String("replaces-agent", "", "prior item-bound agent preserved by this new session")
	teamRole := fs.String("team-role", "", "this item's binding classification for a fresh parented launch: member (this item's allocated team member, never charged against the extra allowance) or extra (on top of that member, checked against the task's per-item allowance); required unless --replaces-agent is set, in which case the replaced binding's classification is inherited and this flag must be omitted or match it")
	workContextFile := fs.String("work-context-file", "", "handler-prepared item context JSON file")
	workContextJSON := fs.String("work-context-json", "", "handler/authorized-launch-prepared item context JSON")
	queueEntry := fs.String("queue-entry", "", "exact receiving Queue entry for cross-project admission")
	queueCycle := fs.Int64("queue-cycle", 0, "exact claimed Queue cycle")
	queueRevision := fs.Int64("queue-revision", 0, "exact claimed Queue entry revision")
	queueClaimantAgent := fs.String("queue-claimant-agent", "", "exact current orchestrator claimant identity")
	queueClaimantRun := fs.String("queue-claimant-run", "", "exact current orchestrator claimant run")
	role := fs.String("role", "", "project role (database_handler)")
	plannedTeamMembers := fs.Int("planned-team-members", 0, "planned non-database team members for this launch (1-32)")
	run := fs.String("run", "", "command to run in the agent window (required)")
	cwd := fs.String("cwd", "", "working directory")
	prompt := fs.String("prompt", "", "appended to the command as a quoted argument")
	runtime := fs.String("runtime", "", "runtime label (default: first word of --run)")
	model := fs.String("model", "", "model name or alias (default: runtime configuration)")
	reasoning := fs.String("reasoning", "", "reasoning effort (default: model/runtime configuration)")
	permissionMode := fs.String("permission-mode", "", "permission preset (default: host settings)")
	approvalMode := fs.String("approval-mode", "", "Codex approval policy (default: host setting)")
	sandboxMode := fs.String("sandbox-mode", "", "Codex sandbox mode (default: host setting)")
	allowedJSON := fs.String("allowed-tools-json", "[]", "Claude preapproved tool rules as JSON")
	task := fs.String("task", e.task, "task id")
	hub := fs.String("hub", e.hub, "hub URL")
	asJSON := fs.Bool("json", false, "print the agent record as JSON")
	_ = fs.Parse(args)
	if *name == "" || strings.TrimSpace(*run) == "" {
		return errors.New("usage: tt spawn --name N --run CMD [--cwd D] [--prompt P]")
	}
	if !api.ValidName(*name) {
		return errors.New("name must match [A-Za-z0-9_-]{1,64}")
	}
	if *role != "" && *role != api.AgentRoleDatabaseHandler {
		return errors.New("role must be database_handler when set")
	}
	if *role == api.AgentRoleDatabaseHandler && !api.ValidID(*agentID, "agt") {
		return errors.New("database_handler requires a stable --agent-id")
	}
	if *agentID != "" && !api.ValidID(*agentID, "agt") {
		return errors.New("invalid --agent-id")
	}
	if *role == "" && *expectedRunID != "" && *resumeReceiptID == "" {
		return errors.New("--expected-run-id is reserved for database_handler launches")
	}
	if *expectedLifecycleGeneration < 0 {
		return errors.New("--expected-lifecycle-generation must be non-negative")
	}
	if *plannedTeamMembers < 0 || *plannedTeamMembers > 32 {
		return errors.New("planned team members must be from 1 to 32 when set")
	}
	itemFlagCount := 0
	for _, set := range []bool{*workItemID != "", *workItemTask != "", *workItemRevision != 0, *workOrderTask != "", *workOrderMessage != 0, *replacesAgent != "", *workContextFile != "", *workContextJSON != ""} {
		if set {
			itemFlagCount++
		}
	}
	if itemFlagCount > 0 {
		if *role != "" || *workItemID == "" || *workItemRevision < 1 || *workOrderMessage < 1 || (*workContextFile == "") == (*workContextJSON == "") {
			return errors.New("item routing requires --work-item, --work-item-revision, --work-order-message and exactly one prepared --work-context-file/--work-context-json on an ordinary agent")
		}
		if *runtime == "generic" {
			return errors.New("item context restoration requires a supported agent runtime")
		}
		if *workItemTask == "" {
			*workItemTask = *task
		}
		if *workOrderTask == "" {
			*workOrderTask = *workItemTask
		}
		if !api.ValidID(*workItemTask, "tsk") || !api.ValidID(*workItemID, "wi") || !api.ValidID(*workOrderTask, "tsk") || (*replacesAgent != "" && !api.ValidID(*replacesAgent, "agt")) {
			return errors.New("invalid work-item routing identity")
		}
		// Classification is an explicit declaration, never inferred from
		// ParentAgentID or launch order: a fresh (non-replacement) binding
		// admitted through an agent session (e.parent will be non-empty)
		// must declare --team-role. A replacement inherits the prior
		// binding's role; any --team-role given for one must match it (the
		// hub validates the exact match) or be omitted.
		if *replacesAgent == "" && e.agent != "" && *teamRole != api.TeamRoleMember && *teamRole != api.TeamRoleExtra {
			return fmt.Errorf("a parented item-bound launch requires --team-role %s or %s", api.TeamRoleMember, api.TeamRoleExtra)
		}
		if *replacesAgent != "" && *teamRole != "" && *teamRole != api.TeamRoleMember && *teamRole != api.TeamRoleExtra {
			return fmt.Errorf("--team-role must be %s or %s when given", api.TeamRoleMember, api.TeamRoleExtra)
		}
	}
	if *resumeReceiptID != "" && (*role != "" || itemFlagCount != 0 || !projectPauseReceiptIDPattern.MatchString(*resumeReceiptID) || !api.ValidID(*agentID, "agt") || !runIDPattern.MatchString(*expectedRunID) || *expectedLifecycleGeneration <= 0) {
		return errors.New("--resume-receipt-id requires an ordinary unbound fresh orchestrator with exact --agent-id, --expected-run-id and positive --expected-lifecycle-generation")
	}
	queueFlagCount := 0
	for _, set := range []bool{*queueEntry != "", *queueCycle != 0, *queueRevision != 0, *queueClaimantAgent != "", *queueClaimantRun != ""} {
		if set {
			queueFlagCount++
		}
	}
	if queueFlagCount > 0 && (itemFlagCount == 0 || queueFlagCount != 5 || *workItemTask == *task || len(*queueEntry) != 20 || !strings.HasPrefix(*queueEntry, "que_") || *queueCycle < 1 || *queueRevision < 1 || !api.ValidID(*queueClaimantAgent, "agt") || !strings.HasPrefix(*queueClaimantRun, "run_")) {
		return errors.New("cross-project Queue admission requires the exact --queue-entry, --queue-cycle, --queue-revision, --queue-claimant-agent and --queue-claimant-run claim")
	}
	if itemFlagCount > 0 && *workItemTask != *task && queueFlagCount == 0 {
		return errors.New("cross-project item admission requires an exact current Queue claim")
	}
	if *task == "" || *hub == "" {
		return errors.New("task and hub are required (TAILTERM_TASK/TAILTERM_HUB or --task/--hub)")
	}
	if *runtime == "" {
		*runtime = strings.Fields(*run)[0]
	}
	if err := requireNativeClaudeCommand(*run, *runtime); err != nil {
		return err
	}
	if itemFlagCount > 0 && *runtime == "generic" {
		return errors.New("item context restoration requires a supported agent runtime")
	}
	// Helpers inherit an explicit policy only when using the same app.
	explicitMode, explicitTools, explicitReasoning, explicitApproval, explicitSandbox := false, false, false, false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "permission-mode" {
			explicitMode = true
		}
		if f.Name == "allowed-tools-json" {
			explicitTools = true
		}
		if f.Name == "reasoning" {
			explicitReasoning = true
		}
		if f.Name == "approval-mode" {
			explicitApproval = true
		}
		if f.Name == "sandbox-mode" {
			explicitSandbox = true
		}
	})
	if e.agent != "" && os.Getenv("TAILTERM_PERMISSION_RUNTIME") == *runtime {
		if !explicitMode {
			*permissionMode = os.Getenv("TAILTERM_PERMISSION_MODE")
		}
		if !explicitTools && os.Getenv("TAILTERM_ALLOWED_TOOLS") != "" {
			*allowedJSON = os.Getenv("TAILTERM_ALLOWED_TOOLS")
		}
		if !explicitReasoning {
			*reasoning = os.Getenv("TAILTERM_REASONING")
		}
		if !explicitApproval {
			*approvalMode = os.Getenv("TAILTERM_APPROVAL_MODE")
		}
		if !explicitSandbox {
			*sandboxMode = os.Getenv("TAILTERM_SANDBOX_MODE")
		}
	}
	if *cwd == "" && e.agent != "" {
		*cwd = os.Getenv("TAILTERM_LAUNCH_CWD")
	}
	if *cwd != "" {
		abs, err := filepath.Abs(*cwd)
		if err != nil {
			return err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return fmt.Errorf("cwd %s is not a directory", abs)
		}
		*cwd = abs
	}
	baseCommand, err := modelCommand(*run, *runtime, *model)
	if err != nil {
		return err
	}
	baseCommand, err = reasoningCommand(baseCommand, *runtime, *model, *reasoning, *run)
	if err != nil {
		return err
	}
	var allowed []string
	if err := json.Unmarshal([]byte(*allowedJSON), &allowed); err != nil {
		return errors.New("invalid allowed tools JSON")
	}
	baseCommand, err = permissionCommand(baseCommand, *runtime, *permissionMode, *cwd, allowed, *approvalMode, *sandboxMode)
	if err != nil {
		return err
	}
	baseCommand = codexHookCommand(baseCommand, *runtime, *run)
	if *permissionMode == "workspace-auto" || *sandboxMode == "workspace-write" {
		if err := os.MkdirAll(relayDir(), 0700); err != nil {
			return err
		}
	}
	command := baseCommand
	if *prompt != "" {
		command += " " + spawn.ShellQuote(*prompt)
	}
	e.hub = *hub
	e.loadConfig()
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	session := ""
	if *role == "" {
		session = spawn.UniqueSession(*name)
	}
	detail, err := c.GetTask(ctx, *task)
	if err != nil {
		return err
	}
	if itemFlagCount > 0 && *replacesAgent == "" && e.agent != "" {
		// Independent review #2300/#2771/#2840/#2916, findings 3/6: a
		// mixed-version deployment must fail closed and explicitly, not
		// silently. Before attempting a fresh parented item-bound launch,
		// confirm the hub actually supports the allocation-intent
		// requirement this exact candidate's Store.AddAgent enforces. An
		// old hub either omits the field or reports Supported=false; either
		// way, proceeding would either 404 confusingly at admission or
		// (on some future hub shape) silently fall back to weaker
		// accounting -- neither is acceptable. A coordinated rollout
		// window where the hub AND every launch CLI are upgraded together
		// is the actual supported unit; this check only detects the
		// mismatch honestly, it does not itself coordinate the rollout.
		// Independent review #3003/#3010 gap: Supported alone is not
		// sufficient -- a hub could advertise Supported=true for some
		// future incompatible revision of the contract while never having
		// shipped the exact version (1) this CLI actually speaks. The
		// advertised Versions list must contain a version this CLI
		// understands, not merely a truthy flag.
		caps, capErr := c.Capabilities(ctx)
		compatible := false
		for _, v := range caps.AllocationIntent.Versions {
			if v == api.AllocationIntentCapabilityVersion {
				compatible = true
				break
			}
		}
		if capErr != nil || !caps.AllocationIntent.Supported || !compatible {
			return fmt.Errorf("hub does not support a compatible allocation-intent version (required for a fresh parented item-bound launch); upgrade the hub before this CLI can launch parented item-bound work here: %v", capErr)
		}
	}
	launcherSelfPath := selfPath()
	briefing := agentTaskBriefingForLaunch(detail.Task, *name, *role, launcherSelfPath, detail.Agents, *plannedTeamMembers)
	if *permissionMode != "" {
		briefing += "\nRequested launch permission mode: " + *permissionMode + ". Permission denials are real failures, not approvals. Do not repeat an unchanged denied action. Report a precise Permission blocked status to the orchestrator and continue independent permitted work."
	}
	if *reasoning != "" {
		briefing += "\nRequested reasoning effort: " + *reasoning + "."
	}
	if *approvalMode != "" || *sandboxMode != "" {
		briefing += "\nRequested Codex approval/sandbox intent: approval=" + or(*approvalMode, "inherit") + ", sandbox=" + or(*sandboxMode, "inherit") + ". Permission denials are real failures, not approvals."
	}
	if *prompt != "" {
		briefing += "\nAssignment: " + *prompt
	}
	if *runtime != "generic" {
		if *role == api.AgentRoleDatabaseHandler {
			command, err = freshRuntimeCommand(baseCommand, *runtime, *agentID, briefing)
			if err != nil {
				return err
			}
		} else {
			command = baseCommand + " " + spawn.ShellQuote(briefing)
		}
	}
	parent := e.agent
	if *role == api.AgentRoleDatabaseHandler {
		parent = ""
	}
	req := api.AddAgentRequest{
		ExpectedRunID: *expectedRunID, ExpectedLifecycleGeneration: *expectedLifecycleGeneration,
		ResumeReceiptID: *resumeReceiptID, Role: *role, AgentID: *agentID,
		Name: *name, Host: spawn.Host(), Session: session,
		Runtime: *runtime, Cwd: *cwd, ParentAgentID: parent,
	}
	if itemFlagCount > 0 {
		contextData, readErr := readPreparedWorkContext(*workContextJSON, *workContextFile)
		if readErr != nil {
			return readErr
		}
		req.WorkItem = &api.AgentWorkItemRequest{
			ItemTaskID: *workItemTask, ItemID: *workItemID, ItemRevision: *workItemRevision,
			WorkOrderMessage: api.MessageReference{TaskID: *workOrderTask, Seq: *workOrderMessage},
			ReplacesAgentID:  *replacesAgent, TeamRole: *teamRole,
			ContextBundle: append(json.RawMessage(nil), contextData...),
		}
		if queueFlagCount > 0 {
			req.WorkItem.QueueClaim = &api.QueueAdmissionClaim{EntryID: *queueEntry, Cycle: *queueCycle, ExpectedRevision: *queueRevision, ClaimantAgentID: *queueClaimantAgent, ClaimantRunID: *queueClaimantRun}
		}
	}
	opts := spawn.Options{
		Session: session, Cwd: *cwd, Command: command, Self: launcherSelfPath,
		Env: map[string]string{
			"TAILTERM_PERMISSION_RUNTIME": *runtime, "TAILTERM_PERMISSION_MODE": *permissionMode, "TAILTERM_ALLOWED_TOOLS": *allowedJSON, "TAILTERM_LAUNCH_CWD": *cwd,
			"TAILTERM_REASONING": *reasoning, "TAILTERM_APPROVAL_MODE": *approvalMode, "TAILTERM_SANDBOX_MODE": *sandboxMode,
			"TAILTERM_TOKEN": e.token, spawn.EnvHub: *hub, spawn.EnvTask: *task,
			"TAILTERM_HANDLER_COMMAND": baseCommand, "TAILTERM_HANDLER_PROMPT": *prompt, "TAILTERM_BRIEFING": briefing,
		},
	}
	var agent api.Agent
	if *role == api.AgentRoleDatabaseHandler {
		agent, err = ensureHandler(ctx, c, *task, req, opts)
		if err != nil {
			return err
		}
	} else {
		agent, err = c.AddAgent(ctx, *task, req)
		if err != nil {
			return fmt.Errorf("register agent: %w", err)
		}
		if agent.WorkItem != nil {
			workContext, contextErr := c.GetAgentWorkItemContext(ctx, *task, agent.ID, agent.RunID)
			if contextErr != nil {
				_, _ = c.CloseAgent(ctx, *task, agent.ID, agent.RunID)
				return fmt.Errorf("restore work-item context: %w", contextErr)
			}
			contextBriefing, contextErr := formatWorkItemContext(workContext)
			if contextErr != nil {
				_, _ = c.CloseAgent(ctx, *task, agent.ID, agent.RunID)
				return contextErr
			}
			briefing += contextBriefing
			opts.Env["TAILTERM_BRIEFING"] = briefing
			opts.Env["TAILTERM_WORK_ITEM_TASK"] = agent.WorkItem.ItemTaskID
			opts.Env["TAILTERM_WORK_ITEM"] = agent.WorkItem.ItemID
			opts.Env["TAILTERM_WORK_ITEM_REVISION"] = fmt.Sprint(agent.WorkItem.ItemRevision)
		}
		if *runtime != "generic" {
			opts.Command, err = freshRuntimeCommand(baseCommand, *runtime, agent.ID, briefing)
			if err != nil {
				_, _ = c.CloseAgent(ctx, *task, agent.ID, agent.RunID)
				return err
			}
		}
		// An uncertain exact Resume retry must reuse the session identity saved
		// by the first admission instead of generating a second tmux session.
		if *resumeReceiptID != "" {
			session = agent.Session
			opts.Session = session
		}
		opts.Env[spawn.EnvAgent], opts.Env[spawn.EnvAgentName], opts.Env[spawn.EnvSession], opts.Env["TAILTERM_RUN"] = agent.ID, agent.Name, session, agent.RunID
		delete(opts.Env, "TAILTERM_HANDLER_COMMAND")
		delete(opts.Env, "TAILTERM_HANDLER_PROMPT")
		// The briefing is already the runtime's quoted command argument. Keeping
		// a second copy in tmux's environment can exceed tmux's command limit.
		delete(opts.Env, "TAILTERM_BRIEFING")
		var existingSession *ownedSession
		if *resumeReceiptID != "" {
			existingSession, err = handlerOwned(ctx, *hub, *task, agent.ID, agent.RunID, agent.Session, nil)
			if err != nil {
				return fmt.Errorf("verify resumed orchestrator session: %w", err)
			}
		}
		if existingSession == nil {
			if err = spawn.Create(opts); err != nil {
				if *resumeReceiptID == "" {
					_, _ = c.CloseAgent(ctx, *task, agent.ID, agent.RunID)
				}
				return err
			}
		}
		if *resumeReceiptID != "" {
			verifiedSession, verifyErr := handlerOwned(ctx, *hub, *task, agent.ID, agent.RunID, agent.Session, nil)
			if verifyErr != nil {
				return fmt.Errorf("verify resumed orchestrator session after launch: %w", verifyErr)
			}
			if verifiedSession == nil {
				return errors.New("resumed orchestrator session is not present with the saved exact run identity")
			}
			confirm := api.ConfirmProjectResumeRequest{Version: api.ProjectPauseCapabilityVersion,
				RequestID:                   "project-resume-confirm-" + strings.TrimPrefix(*resumeReceiptID, "ppr_"),
				ExpectedLifecycleGeneration: *expectedLifecycleGeneration, ResumeReceiptID: *resumeReceiptID,
				AgentID: agent.ID, RunID: agent.RunID}
			status, confirmErr := c.ConfirmProjectResume(ctx, *task, confirm)
			if confirmErr != nil {
				return fmt.Errorf("confirm resumed orchestrator session: %w", confirmErr)
			}
			if status.State != api.ProjectPauseActive {
				return errors.New("resume confirmation did not clear the project admission barrier")
			}
		}
	}
	// The relay also adopts already-running sessions.
	if _, rememberErr := rememberSessions(ctx, *hub); rememberErr != nil {
		fmt.Fprintln(os.Stderr, "[tt] Session cleanup registration will retry on the host relay.")
	}
	if *asJSON {
		printJSON(agent)
	} else {
		fmt.Printf("spawned %s as %s in tmux session %s\n", agent.Name, agent.ID, agent.Session)
	}
	return nil
}

func cmdClose(e env, args []string) error {
	fs := flag.NewFlagSet("close", flag.ExitOnError)
	task := fs.String("task", e.task, "task id")
	jsonOut := fs.Bool("json", false, "JSON result")
	_ = fs.Parse(args)
	ref := e.agent
	if fs.NArg() > 0 {
		ref = fs.Arg(0)
	}
	if ref == "" || *task == "" {
		return errors.New("usage: tt close [AGENT]")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	id, err := resolveCloseAgent(ctx, c, *task, ref)
	if err != nil {
		return err
	}
	a, err := c.GetAgent(ctx, *task, id)
	if err != nil {
		return err
	}
	detail, err := c.GetTask(ctx, *task)
	if err != nil {
		return err
	}
	if a.Role == api.AgentRoleDatabaseHandler {
		return errors.New("the active database handler remains available while the project is open")
	}
	if detail.Task.Status == api.TaskOpen && detail.Task.Orchestrator != "" && strings.EqualFold(a.Name, detail.Task.Orchestrator) {
		return errors.New("the project orchestrator remains available while the project is open")
	}
	if a.Host != spawn.Host() {
		return fmt.Errorf("agent %s runs on %s; tt close only controls sessions on this host", a.Name, a.Host)
	}
	if _, err := rememberSessions(ctx, e.hub); err != nil {
		return fmt.Errorf("verify local session ownership: %w", err)
	}
	if _, err := c.CloseAgent(ctx, *task, id, a.RunID); err != nil {
		return err
	}
	if a.ID == e.agent {
		fmt.Println("closing this session")
	}
	result, err := cleanupSessions(ctx, e, *task, []string{id})
	if err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return errors.New(strings.Join(result.Errors, "; "))
	}
	confirmed, err := c.GetAgent(ctx, *task, id)
	if err != nil {
		return fmt.Errorf("verify cleanup receipt: %w", err)
	}
	if !confirmed.CleanupDone {
		return errors.New("cleanup receipt pending; will retry")
	}
	if *jsonOut {
		printJSON(confirmed)
	} else {
		fmt.Printf("Closed %s (%s); session cleanup confirmed.\n", confirmed.Name, confirmed.RunID)
	}
	return nil
}

func cmdWrap(e env, args []string) error {
	command, err := wrapCommand(args)
	if err != nil {
		return err
	}
	report := func(kind, text string) {
		if e.task != "" && e.agent != "" && e.hub != "" {
			_ = postEvent(e, kind, text, nil)
		}
	}
	stopped := make(chan struct{})
	return spawn.Wrap(command, func() {
		report(api.EventStarted, "Process started")
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopped:
					return
				case <-ticker.C:
					report(api.EventHeartbeat, "")
				}
			}
		}()
	}, func(code int) { close(stopped); report(api.EventExited, fmt.Sprintf("Process exited (%d)", code)) })
}

func wrapCommand(args []string) (string, error) {
	if len(args) == 2 && args[0] == "--shell" {
		return args[1], nil
	}
	if len(args) == 2 && args[0] == "--shell-file" {
		path := args[1]
		if !filepath.IsAbs(path) {
			return "", errors.New("agent command file must be absolute")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 4*api.MaxAgentWorkItemContextBytes+256*1024 {
			return "", errors.New("agent command file is missing, unsafe or oversized")
		}
		data, err := os.ReadFile(path)
		removeErr := os.Remove(path)
		if err != nil {
			return "", err
		}
		if removeErr != nil {
			return "", fmt.Errorf("remove consumed agent command: %w", removeErr)
		}
		if len(data) == 0 {
			return "", errors.New("agent command file is empty")
		}
		return string(data), nil
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", errors.New("usage: tt wrap -- PROGRAM [ARGS], tt wrap --shell COMMAND, or tt wrap --shell-file PATH")
	}
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = spawn.ShellQuote(a)
	}
	return strings.Join(words, " "), nil
}

// Old watcher windows can safely remain after an upgrade: never send pane input.
func cmdWatch(e env) error {
	fmt.Println("[tt] Automatic terminal injection is disabled. Messages stay in the durable inbox; use tt inbox --unread --mark-read.")
	return nil
}

func cmdRuntimes(args []string) error {
	fs := flag.NewFlagSet("runtimes", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	rt := spawn.Runtimes()
	if *asJSON {
		printJSON(map[string]any{"host": spawn.Host(), "runtimes": rt, "tmux": tmuxVersionString()})
		return nil
	}
	fmt.Printf("host %s\ntmux %s\n", spawn.Host(), tmuxVersionString())
	for _, r := range rt {
		fmt.Println(r)
	}
	return nil
}

func tmuxVersionString() string {
	out, err := exec.Command(spawn.Tmux, "-V").Output()
	if err != nil {
		return "missing"
	}
	return strings.TrimSpace(string(out))
}

func cmdHooks(args []string) error {
	fs := flag.NewFlagSet("hooks", flag.ExitOnError)
	path := fs.String("path", "tt", "path to the tt binary in the hook commands")
	install := fs.Bool("install", false, "codex: merge the Tailterm Stop hook into ~/.codex/hooks.json")
	kind := "claude"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		kind, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		kind = fs.Arg(0)
	}
	switch kind {
	case "claude":
		fmt.Print(adapters.ClaudeHooks(*path))
	case "codex":
		if *install {
			return installCodexStopHook(*path)
		}
		fmt.Print(adapters.CodexConfig(*path))
	case "generic":
		fmt.Print(adapters.Generic(*path))
	default:
		return fmt.Errorf("unknown runtime %q (claude, codex, generic)", kind)
	}
	return nil
}

// cmdHook handles runtime callbacks. It must be quick and must never fail the
// runtime: hub errors are swallowed, and without an agent identity it is a no-op.
func cmdHook(e env, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: tt hook <session-start|prompt|stop|notification|codex>")
	}
	if e.agent == "" || e.task == "" || e.hub == "" {
		return nil
	}
	var input map[string]any
	if args[0] == "codex" && len(args) > 1 {
		input = spawn.ReadJSON([]byte(args[1]))
	} else {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		input = spawn.ReadJSON(b)
	}
	quiet := func(kind, text string) {
		var data map[string]any
		if kind == api.EventDone {
			data = map[string]any{"runtimeStop": true}
		}
		_ = postEvent(e, kind, text, data)
	}
	unread := func() int {
		c, err := e.client(2 * time.Second)
		if err != nil {
			return 0
		}
		ctx, cancel := ctxTimeout(2 * time.Second)
		defer cancel()
		a, err := c.GetAgent(ctx, e.task, e.agent)
		if err != nil {
			return 0
		}
		return a.Unread
	}
	switch args[0] {
	case "session-start":
		_ = cmdBrief(e)
		quiet(api.EventStarted, "session start")
		fmt.Printf("You are agent %q (%s) on tailterm task %s. Other agents on this task can message you. "+
			"Use `tt inbox --unread --mark-read` to read messages, `tt post --to agent \"text\"` to reply, "+
			"`tt agents` to see teammates, `tt event needs_input --text \"...\"` to ask the humans, and "+
			"Check the task policy before adding helpers.\n", e.agentName, e.agent, e.task)
		if n := unread(); n > 0 {
			fmt.Printf("You have %d unread task message(s); read them first.\n", n)
		}
	case "prompt":
		quiet(api.EventRunning, "")
		if n := unread(); n > 0 {
			fmt.Printf("[tailterm] %d unread task message(s): run `tt inbox --unread --mark-read`.\n", n)
		}
	case "stop":
		quiet(api.EventDone, "")
		active, _ := input["stop_hook_active"].(bool)
		if active {
			break
		}
		// Broker phase 2a: block turn end on unacknowledged obligations only;
		// overdue outcomes escalate instead of holding the turn open.
		if n, seqs := unackedObligations(e); n > 0 {
			printJSON(map[string]any{"decision": "block", "reason": fmt.Sprintf("You have %d unacknowledged tailterm obligation(s): %s. Run `tt obligations`, acknowledge with `tt ack SEQ`, then act and reply with `tt send --reply-to SEQ`.", n, seqs)})
		} else if n := unreadDirectedFreeText(e); n > 0 {
			// Only directed free text blocks; board-wide chatter never traps an agent.
			printJSON(map[string]any{"decision": "block", "reason": fmt.Sprintf("You have %d unread tailterm message(s) addressed to you. Run `tt inbox --unread --mark-read`, act on them, and reply if needed.", n)})
		}
	case "notification":
		kind, _ := input["notification_type"].(string)
		msg, _ := input["message"].(string)
		switch kind {
		case "permission_prompt":
			quiet(api.EventNeedsInput, "Permission blocked: "+msg)
		case "idle_prompt", "elicitation_dialog":
			quiet(api.EventNeedsInput, msg)
		}
	case "codex":
		kind, _ := input["type"].(string)
		if kind == "agent-turn-complete" {
			msg, _ := input["last-assistant-message"].(string)
			if len(msg) > 200 {
				msg = msg[:200]
			}
			quiet(api.EventDone, msg)
		}
	default:
		return fmt.Errorf("unknown hook %q", args[0])
	}
	return nil
}

func cmdNewTask(e env, args []string) error {
	fs := flag.NewFlagSet("new-task", flag.ExitOnError)
	name := fs.String("name", "", "task name (required)")
	goal := fs.String("goal", "", "task goal")
	hub := fs.String("hub", e.hub, "hub URL")
	allowSpawn := fs.Bool("allow-agent-spawn", false, "allow agents to add helpers")
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("usage: tt new-task --name N [--goal G]")
	}
	e.hub = *hub
	e.loadConfig()
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	t, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: *name, Goal: *goal, AllowAgentSpawn: *allowSpawn})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(t)
	} else {
		fmt.Println(t.ID)
	}
	return nil
}
