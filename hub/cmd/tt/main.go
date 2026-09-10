// Command tt is the tailterm agent CLI: it registers agent sessions with the
// hub, posts events and messages, spawns sibling agents on this host, and runs
// durable inbox integration for agent runtimes.
package main

import (
	"context"
	"crypto/sha256"
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
  work-items <command>         list/get/create/update/dispatch/history for bugs and features
  queue <command>              list/get/history/changes/action/receipt for deliberate Queue work
  agents [--json]              list agents on this task
  event <kind> [--text T]      post started|running|done|needs_input|exited|closed
  post <text> [--to AGENT]     post a message to the task or one agent
  message-audit <command>      get/history/changes/correct/resolve/associate/receipt
  capabilities [--json]       show supported audit contracts and policy mode
  audit-export <command>       create/download an immutable project audit export
  ask --request-id KEY --file PATH [--json]  request an owner decision on the Board
  inbox [--unread] [--mark-read] [--json]
  context [--json]             print this exact run's bound work-item context
  spawn --name N --run CMD [--cwd D] [--prompt P] [--runtime R] [--task ID]
                               start a sibling agent session on this host
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
	os.Exit(1)
}

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
	case "context":
		err = cmdContext(e, args)
	case "spawn":
		err = cmdSpawn(e, args)
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
	workItemTask := fs.String("work-item-task", "", "project owning the linked item (default: --task)")
	workItemID := fs.String("work-item", "", "bug or feature linked to this message")
	workItemRevision := fs.Int64("work-item-revision", 0, "exact linked item revision")
	workOrderTask := fs.String("work-order-task", "", "project containing the work-order message")
	workOrderMessage := fs.Int64("work-order-message", 0, "recorded work-order message sequence")
	var related stringListFlag
	fs.Var(&related, "related", "related exact item as TASK/ITEM@REVISION (repeatable)")
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
	req := api.PostMessageRequest{Text: text, To: target, AgentID: e.agent, ReplyTo: *reply}
	if e.agent != "" && os.Getenv("TAILTERM_WORK_ITEM") != "" {
		self, selfErr := c.GetAgent(ctx, *task, e.agent)
		if selfErr != nil {
			return selfErr
		}
		if self.WorkItem != nil && *workItemID == "" {
			*workItemTask, *workItemID, *workItemRevision = self.WorkItem.ItemTaskID, self.WorkItem.ItemID, self.WorkItem.ItemRevision
			*workOrderTask, *workOrderMessage = self.WorkItem.WorkOrderMessage.TaskID, self.WorkItem.WorkOrderMessage.Seq
			if *requestID == "" {
				digest := sha256.Sum256([]byte(e.runID + "\x00" + target + "\x00" + fmt.Sprint(*reply) + "\x00" + text))
				*requestID = fmt.Sprintf("item-post-%x", digest[:12])
			}
		}
	}
	linked := *workItemID != "" || *workItemTask != "" || *workItemRevision != 0 || *workOrderTask != "" || *workOrderMessage != 0 || len(related) > 0
	if *intake {
		if linked || *requestID == "" {
			return errors.New("--intake requires --request-id and forbids work-item, related, and work-order flags")
		}
		req.AuditKind = api.MessageAuditIntake
		req.RequestID = *requestID
	}
	if linked {
		if *intake {
			return errors.New("--intake cannot be combined with work context")
		}
		if *workItemTask == "" {
			*workItemTask = *task
		}
		if *workOrderTask == "" {
			*workOrderTask = *workItemTask
		}
		if !api.ValidID(*workItemTask, "tsk") || !api.ValidID(*workItemID, "wi") || *workItemRevision < 1 || !api.ValidID(*workOrderTask, "tsk") || *workOrderMessage < 1 || *requestID == "" {
			return errors.New("linked post requires --request-id, --work-item, --work-item-revision and --work-order-message")
		}
		req.AuditKind = api.MessageAuditWork
		req.RequestID = *requestID
		req.WorkItems = []api.MessageWorkItem{{ItemTaskID: *workItemTask, ItemID: *workItemID, ItemRevision: *workItemRevision, Relationship: "primary"}}
		if len(related) > api.MaxMessageAuditRelated {
			return fmt.Errorf("at most %d related items are allowed", api.MaxMessageAuditRelated)
		}
		seen := map[string]bool{fmt.Sprintf("%s/%s", *workItemTask, *workItemID): true}
		for _, raw := range related {
			item, parseErr := parseRelatedItem(raw)
			if parseErr != nil {
				return parseErr
			}
			identity := item.ItemTaskID + "/" + item.ItemID
			if seen[identity] {
				return fmt.Errorf("duplicate primary/related item %s", identity)
			}
			seen[identity] = true
			req.WorkItems = append(req.WorkItems, item)
		}
		req.WorkOrderMessage = &api.MessageReference{TaskID: *workOrderTask, Seq: *workOrderMessage}
	} else if *requestID != "" && !*intake {
		// A retry key alone is a keyed legacy/unclassified post, not evidence of
		// linked work context.
		req.RequestID = *requestID
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
	_ = fs.Parse(args)
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
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

func cmdSpawn(e env, args []string) error {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	name := fs.String("name", "", "agent name (required)")
	agentID := fs.String("agent-id", "", "stable preallocated agent identity for launch/retry")
	expectedRunID := fs.String("expected-run-id", "", "exited database_handler run to restart")
	workItemTask := fs.String("work-item-task", "", "project owning the bound bug or feature (default: --task)")
	workItemID := fs.String("work-item", "", "single bug or feature bound to this new session")
	workItemRevision := fs.Int64("work-item-revision", 0, "exact work-item revision to restore")
	workOrderTask := fs.String("work-order-task", "", "project containing the recorded work-order message (default: work-item project)")
	workOrderMessage := fs.Int64("work-order-message", 0, "recorded bounded work-order message sequence")
	replacesAgent := fs.String("replaces-agent", "", "prior item-bound agent preserved by this new session")
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
	if *role == "" && *expectedRunID != "" {
		return errors.New("--expected-run-id is reserved for database_handler launches")
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
	briefing := agentTaskBriefingForLaunch(detail.Task, *name, *role, detail.Agents, *plannedTeamMembers)
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
		command = baseCommand + " " + spawn.ShellQuote(briefing)
	}
	parent := e.agent
	if *role == api.AgentRoleDatabaseHandler {
		parent = ""
	}
	req := api.AddAgentRequest{
		ExpectedRunID: *expectedRunID, Role: *role, AgentID: *agentID,
		Name: *name, Host: spawn.Host(), Session: session,
		Runtime: *runtime, Cwd: *cwd, ParentAgentID: parent,
	}
	if itemFlagCount > 0 {
		contextData := []byte(*workContextJSON)
		if *workContextFile != "" {
			var readErr error
			contextData, readErr = os.ReadFile(*workContextFile)
			if readErr != nil {
				return fmt.Errorf("read prepared work-item context: %w", readErr)
			}
		}
		if !json.Valid(contextData) {
			return errors.New("prepared work-item context is not valid JSON")
		}
		req.WorkItem = &api.AgentWorkItemRequest{
			ItemTaskID: *workItemTask, ItemID: *workItemID, ItemRevision: *workItemRevision,
			WorkOrderMessage: api.MessageReference{TaskID: *workOrderTask, Seq: *workOrderMessage},
			ReplacesAgentID:  *replacesAgent,
			ContextBundle:    append(json.RawMessage(nil), contextData...),
		}
		if queueFlagCount > 0 {
			req.WorkItem.QueueClaim = &api.QueueAdmissionClaim{EntryID: *queueEntry, Cycle: *queueCycle, ExpectedRevision: *queueRevision, ClaimantAgentID: *queueClaimantAgent, ClaimantRunID: *queueClaimantRun}
		}
	}
	opts := spawn.Options{
		Session: session, Cwd: *cwd, Command: command, Self: selfPath(),
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
			opts.Command = baseCommand + " " + spawn.ShellQuote(briefing)
			opts.Env["TAILTERM_BRIEFING"] = briefing
			opts.Env["TAILTERM_WORK_ITEM_TASK"] = agent.WorkItem.ItemTaskID
			opts.Env["TAILTERM_WORK_ITEM"] = agent.WorkItem.ItemID
			opts.Env["TAILTERM_WORK_ITEM_REVISION"] = fmt.Sprint(agent.WorkItem.ItemRevision)
		}
		opts.Env[spawn.EnvAgent], opts.Env[spawn.EnvAgentName], opts.Env[spawn.EnvSession], opts.Env["TAILTERM_RUN"] = agent.ID, agent.Name, session, agent.RunID
		delete(opts.Env, "TAILTERM_HANDLER_COMMAND")
		delete(opts.Env, "TAILTERM_HANDLER_PROMPT")
		// The briefing is already the runtime's quoted command argument. Keeping
		// a second copy in tmux's environment can exceed tmux's command limit.
		delete(opts.Env, "TAILTERM_BRIEFING")
		if err = spawn.Create(opts); err != nil {
			_, _ = c.CloseAgent(ctx, *task, agent.ID, agent.RunID)
			return err
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
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 512*1024 {
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
	_ = fs.Parse(args)
	kind := "claude"
	if fs.NArg() > 0 {
		kind = fs.Arg(0)
	}
	switch kind {
	case "claude":
		fmt.Print(adapters.ClaudeHooks(*path))
	case "codex":
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
		if n := unread(); n > 0 && !active {
			printJSON(map[string]any{"decision": "block", "reason": fmt.Sprintf("You have %d unread tailterm task message(s). Run `tt inbox --unread --mark-read`, act on them, and reply with `tt post` if needed.", n)})
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
