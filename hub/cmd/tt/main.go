// Command tt is the tailterm agent CLI: it registers agent sessions with the
// hub, posts events and messages, spawns sibling agents on this host, and runs
// the per-session watcher that pushes messages into idle agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

const usage = `tt — tailterm agent CLI

Identity comes from the environment tailterm sets on agent sessions:
  TAILTERM_HUB   TAILTERM_TASK   TAILTERM_AGENT   TAILTERM_AGENT_NAME

Commands
  status                       identity, hub reachability, own agent, unread count
  tasks                        list tasks on the hub
  agents [--json]              list agents on this task
  event <kind> [--text T]      post started|running|done|needs_input|closed
  post <text> [--to AGENT]     post a message to the task or one agent
  inbox [--unread] [--mark-read] [--json]
  spawn --name N --run CMD [--cwd D] [--prompt P] [--runtime R] [--task ID]
                               start a sibling agent session on this host
  close [AGENT]                close an agent session on this host (default: self)
  watch                        push task messages into this agent when idle
  wrap -- CMD                  run CMD, reporting started/done to the hub
  runtimes [--json]            agent CLIs available on this host
  hooks <claude|codex|generic> print integration snippets
  hook <session-start|prompt|stop|notification|codex>
                               handlers invoked by agent runtimes
  new-task --name N [--goal G] create a task (prints its id)
`

type env struct {
	hub, task, agent, agentName, session string
}

func readEnv() env {
	return env{
		hub:       os.Getenv(spawn.EnvHub),
		task:      os.Getenv(spawn.EnvTask),
		agent:     os.Getenv(spawn.EnvAgent),
		agentName: os.Getenv(spawn.EnvAgentName),
		session:   os.Getenv(spawn.EnvSession),
	}
}

func (e env) client(timeout time.Duration) (*api.Client, error) {
	if e.hub == "" {
		return nil, errors.New("TAILTERM_HUB is not set")
	}
	return api.NewClient(e.hub, timeout)
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
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	e := readEnv()
	var err error
	switch cmd {
	case "status":
		err = cmdStatus(e)
	case "tasks":
		err = cmdTasks(e, args)
	case "agents":
		err = cmdAgents(e, args)
	case "event":
		err = cmdEvent(e, args)
	case "post":
		err = cmdPost(e, args)
	case "inbox":
		err = cmdInbox(e, args)
	case "spawn":
		err = cmdSpawn(e, args)
	case "close":
		err = cmdClose(e, args)
	case "watch":
		err = cmdWatch(e)
	case "wrap":
		err = cmdWrap(e, args)
	case "runtimes":
		err = cmdRuntimes(args)
	case "hooks":
		err = cmdHooks(args)
	case "hook":
		err = cmdHook(e, args)
	case "new-task":
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
		fmt.Printf("%s %s  %-12s %-12s %s@%s unread=%d\n", self, a.ID, a.Status, a.Name, a.Session, a.Host, a.Unread)
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
	_, err = c.PostEvent(ctx, task, api.PostEventRequest{Kind: kind, AgentID: e.agent, Text: text, Data: data})
	return err
}

func cmdEvent(e env, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: tt event <kind> [--text T]")
	}
	fs := flag.NewFlagSet("event", flag.ExitOnError)
	text := fs.String("text", "", "event text")
	_ = fs.Parse(args[1:])
	if !api.PostableKind(args[0]) {
		return fmt.Errorf("kind must be one of started running done needs_input closed")
	}
	return postEvent(e, args[0], *text, nil)
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

func cmdPost(e env, args []string) error {
	fs := flag.NewFlagSet("post", flag.ExitOnError)
	to := fs.String("to", "", "agent id or name")
	task := fs.String("task", e.task, "task id")
	_ = fs.Parse(args)
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
		return err
	}
	m, err := c.PostMessage(ctx, *task, api.PostMessageRequest{Text: text, To: target, AgentID: e.agent})
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
		from = m.From.User
	}
	to := ""
	if m.To != "" {
		to = " → " + or(names[m.To], m.To)
	}
	return fmt.Sprintf("#%d %s %s%s: %s", m.Seq, m.CreatedAt.Local().Format("15:04"), from, to, m.Text)
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
	run := fs.String("run", "", "command to run in the agent window (required)")
	cwd := fs.String("cwd", "", "working directory")
	prompt := fs.String("prompt", "", "appended to the command as a quoted argument")
	runtime := fs.String("runtime", "", "runtime label (default: first word of --run)")
	task := fs.String("task", e.task, "task id")
	hub := fs.String("hub", e.hub, "hub URL")
	asJSON := fs.Bool("json", false, "print the agent record as JSON")
	_ = fs.Parse(args)
	if *name == "" || *run == "" {
		return errors.New("usage: tt spawn --name N --run CMD [--cwd D] [--prompt P]")
	}
	if !api.ValidName(*name) {
		return errors.New("name must match [A-Za-z0-9_-]{1,64}")
	}
	if *task == "" || *hub == "" {
		return errors.New("task and hub are required (TAILTERM_TASK/TAILTERM_HUB or --task/--hub)")
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
	if *runtime == "" {
		*runtime = strings.Fields(*run)[0]
	}
	command := *run
	if *prompt != "" {
		command += " " + spawn.ShellQuote(*prompt)
	}
	c, err := api.NewClient(*hub, 10*time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	session := spawn.UniqueSession(*name)
	agent, err := c.AddAgent(ctx, *task, api.AddAgentRequest{
		AgentID: api.NewID("agt"), Name: *name, Host: spawn.Host(), Session: session,
		Runtime: *runtime, Cwd: *cwd, ParentAgentID: e.agent,
	})
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	err = spawn.Create(spawn.Options{
		Session: session, Cwd: *cwd, Command: command, Self: selfPath(),
		Env: map[string]string{
			spawn.EnvHub: *hub, spawn.EnvTask: *task, spawn.EnvAgent: agent.ID,
			spawn.EnvAgentName: agent.Name, spawn.EnvSession: session,
		},
	})
	if err != nil {
		_, _ = c.CloseAgent(ctx, *task, agent.ID)
		return err
	}
	if *asJSON {
		printJSON(agent)
	} else {
		fmt.Printf("spawned %s as %s in tmux session %s\n", agent.Name, agent.ID, session)
	}
	return nil
}

func cmdClose(e env, args []string) error {
	fs := flag.NewFlagSet("close", flag.ExitOnError)
	task := fs.String("task", e.task, "task id")
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
	id, err := resolveAgent(ctx, c, *task, ref)
	if err != nil {
		return err
	}
	a, err := c.GetAgent(ctx, *task, id)
	if err != nil {
		return err
	}
	if a.Host != spawn.Host() {
		return fmt.Errorf("agent %s runs on %s; tt close only controls sessions on this host", a.Name, a.Host)
	}
	if _, err := c.CloseAgent(ctx, *task, id); err != nil {
		return err
	}
	if a.ID == e.agent {
		fmt.Println("closing this session")
	}
	if spawn.HasSession(a.Session) {
		return spawn.Kill(a.Session)
	}
	return nil
}

func cmdWrap(e env, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("usage: tt wrap -- CMD")
	}
	command := strings.Join(args, " ")
	report := func(kind string, text string) {
		if e.task == "" || e.agent == "" || e.hub == "" {
			return
		}
		_ = postEvent(e, kind, text, nil)
	}
	return spawn.Wrap(command,
		func() { report(api.EventStarted, command) },
		func(code int) { report(api.EventDone, fmt.Sprintf("exit %d", code)) })
}

func cmdWatch(e env) error {
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	if e.agent == "" || e.session == "" {
		return errors.New("TAILTERM_AGENT and TAILTERM_SESSION must be set")
	}
	c, err := e.client(40 * time.Second)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("[tt watch] agent %s (%s) on task %s\n", e.agentName, e.agent, task)
	var after int64
	if list, err := c.Events(ctx, task, 0, 0, 1); err == nil {
		after = list.Next
	}
	if latest, err := c.Events(ctx, task, 0, 0, api.MaxLimit); err == nil && len(latest.Events) > 0 {
		after = latest.Events[len(latest.Events)-1].Seq
	}
	backoff := time.Second
	var lastInject time.Time
	flush := func() {
		// Space injections out so a burst of messages arrives as separate prompts.
		if wait := 2*time.Second - time.Since(lastInject); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}
		a, err := c.GetAgent(ctx, task, e.agent)
		if err != nil || !api.IdleStatus(a.Status) || a.Unread == 0 {
			return
		}
		cursor, err := readCursor(ctx, c, task, e.agent)
		if err != nil {
			return
		}
		msgs, err := c.ListMessages(ctx, task, cursor, e.agent, 20)
		if err != nil {
			return
		}
		names := map[string]string{}
		if agents, err := c.ListAgents(ctx, task); err == nil {
			for _, ag := range agents {
				names[ag.ID] = ag.Name
			}
		}
		var lines []string
		var last int64
		for _, m := range msgs {
			if m.From.AgentID == e.agent {
				continue
			}
			from := "human"
			if m.From.AgentID != "" {
				from = or(names[m.From.AgentID], m.From.AgentID)
			}
			lines = append(lines, fmt.Sprintf("[task message from %s] %s", from, strings.ReplaceAll(m.Text, "\n", " ")))
			last = m.Seq
		}
		if len(lines) == 0 {
			return
		}
		if err := spawn.Inject(e.session, strings.Join(lines, " ")); err != nil {
			fmt.Printf("[tt watch] inject failed: %v\n", err)
			return
		}
		lastInject = time.Now()
		_ = c.MarkRead(ctx, task, api.MarkReadRequest{AgentID: e.agent, UpTo: last})
		fmt.Printf("[tt watch] delivered %d message(s)\n", len(lines))
	}
	flush()
	for ctx.Err() == nil {
		list, err := c.Events(ctx, task, after, 25*time.Second, api.MaxLimit)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("[tt watch] hub error: %v (retry in %s)\n", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		relevant := false
		for _, ev := range list.Events {
			after = ev.Seq
			switch ev.Kind {
			case api.EventMessage:
				to, _ := ev.Data["to"].(string)
				if ev.AgentID != e.agent && (to == "" || to == e.agent) {
					relevant = true
				}
			case api.EventDone:
				if ev.AgentID == e.agent {
					relevant = true
				}
			case api.EventClosed, api.EventTaskClosed:
				if ev.AgentID == e.agent || ev.Kind == api.EventTaskClosed {
					fmt.Println("[tt watch] agent closed; exiting")
					return nil
				}
			}
		}
		if relevant {
			flush()
		}
	}
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
	quiet := func(kind, text string) { _ = postEvent(e, kind, text, nil) }
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
		quiet(api.EventStarted, "session start")
		fmt.Printf("You are agent %q (%s) on tailterm task %s. Other agents on this task can message you. "+
			"Use `tt inbox --unread --mark-read` to read messages, `tt post \"text\" [--to agent]` to reply, "+
			"`tt agents` to see teammates, `tt event needs_input --text \"...\"` to ask the humans, and "+
			"`tt spawn --name N --run \"cmd\"` to add a sibling agent on this host.\n", e.agentName, e.agent, e.task)
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
		case "permission_prompt", "idle_prompt", "elicitation_dialog":
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
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("usage: tt new-task --name N [--goal G]")
	}
	c, err := api.NewClient(*hub, 10*time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	t, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: *name, Goal: *goal})
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
