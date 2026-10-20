package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The wrapper writes this local receipt for its exact runtime child. It stays
// on the launch host and contains no transcript, arguments or credentials.
// SessionError is set only when both reads of the tmux session identity
// failed. The receipt then has no SessionID and SessionCreated unless a
// complete adopted receipt already saved for the same run supplied them and
// its pane fields. PanePID and PaneStarted identify the session's single pane
// process; Shell is the login shell the wrapper leaves in that pane after the
// runtime exits. Only the wrapper records Shell; adoption never does.
// ExitReportedAt is when the hub accepted the wrapper's exit report, or
// showed on a refusal that it already holds this run's exit.
// ExitReportStopped says why the wrapper stopped retrying a lost exit report
// without that acceptance. Only the wrapper records either, and only after
// ExitedAt.
type runtimeProcessReceipt struct {
	Hub            string    `json:"hub"`
	Task           string    `json:"task"`
	Agent          string    `json:"agent"`
	Run            string    `json:"run"`
	Session        string    `json:"session"`
	Socket         string    `json:"socket,omitempty"`
	PID            int       `json:"pid"`
	PanePID        int       `json:"panePid,omitempty"`
	PaneStarted    string    `json:"paneStarted,omitempty"`
	Shell          string    `json:"shell,omitempty"`
	Started        string    `json:"started,omitempty"`
	SessionID      string    `json:"sessionId,omitempty"`
	SessionCreated string    `json:"sessionCreated,omitempty"`
	SessionError   string    `json:"sessionError,omitempty"`
	ExitedAt       time.Time `json:"exitedAt,omitempty"`
	// See the type comment.
	ExitReportedAt    time.Time `json:"exitReportedAt,omitempty"`
	ExitReportStopped string    `json:"exitReportStopped,omitempty"`
}

// A busy host can take longer than the first deadline just to start tmux, and
// a receipt saved without its session identity is never completed later: the
// session probe reports that run as unknown for the rest of its life. So a
// failed read is retried once with a longer deadline before the receipt is
// saved without it.
var runtimeSessionReadTimeouts = [2]time.Duration{2 * time.Second, 10 * time.Second}

func runtimeProcessPath(hub, agent, run string) string {
	b := runtimeBinding{Hub: hub, Agent: agent}
	return filepath.Join(relayDir(), bindingKey(b)+"-"+run+".process.json")
}

func runtimeProcessIdentity(pid int) (string, error) {
	if pid < 1 {
		return "", errors.New("invalid runtime pid")
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(string(out))
	if identity == "" {
		return "", errors.New("process creation identity unavailable")
	}
	return identity, nil
}

func recordRuntimeProcess(e env, pid int) {
	if e.hub == "" || !api.ValidID(e.task, "tsk") || !api.ValidID(e.agent, "agt") || !runIDPattern.MatchString(e.runID) {
		return
	}
	identity, _ := runtimeProcessIdentity(pid)
	// The same choice spawn.Wrap makes for the login shell it starts after the
	// runtime exits; the exited-primary probe matches that shell by it.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	receipt := runtimeProcessReceipt{Hub: e.hub, Task: e.task, Agent: e.agent, Run: e.runID, Session: os.Getenv(spawn.EnvSession), Socket: os.Getenv("TT_TMUX_SOCKET"), PID: pid, Started: identity, Shell: shell}
	for attempt, timeout := range runtimeSessionReadTimeouts {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		sessions, err := localSessions(ctx)
		cancel()
		if err != nil {
			receipt.SessionError = fmt.Sprintf("tmux session identity unread after %d attempts: %v", attempt+1, err)
			continue
		}
		receipt.SessionError = ""
		for _, s := range sessions {
			if s.valid() && s.Hub == receipt.Hub && s.Task == receipt.Task && s.Agent == receipt.Agent && s.Run == receipt.Run && s.Name == receipt.Session {
				receipt.SessionID, receipt.SessionCreated = s.ID, s.Created
				break
			}
		}
		if receipt.SessionID != "" {
			receipt.PanePID, receipt.PaneStarted = wrapperPane(receipt.SessionID)
		}
		break
	}
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	if receipt.SessionError != "" {
		// Runtime adoption may have saved a complete receipt for this run while
		// the reads were failing. Its pane fields with no Shell mark it:
		// discovery always records the pane it started from and never the
		// shell, and the wrapper always records the shell, so a receipt the
		// wrapper wrote earlier is not a source. The session identity and the
		// pane of that session are kept; PID and Started stay the wrapper's own
		// exact child.
		var saved runtimeProcessReceipt
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &saved) == nil && saved.Hub == receipt.Hub && saved.Task == receipt.Task && saved.Agent == receipt.Agent && saved.Run == receipt.Run && saved.Session == receipt.Session && saved.Socket == receipt.Socket && saved.SessionID != "" && saved.SessionCreated != "" && saved.PanePID > 0 && saved.PaneStarted != "" && saved.Shell == "" {
			receipt.SessionID, receipt.SessionCreated = saved.SessionID, saved.SessionCreated
			receipt.PanePID, receipt.PaneStarted = saved.PanePID, saved.PaneStarted
		}
	}
	_ = writePrivateJSON(path, receipt)
}

// wrapperPane reads the wrapper's session's single pane process and its start
// identity. Anything else, or any failed read, records no pane: the probes
// then treat the pane as unrecorded, and the session identity stays valid.
func wrapperPane(sessionID string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeSessionReadTimeouts[0])
	defer cancel()
	panes, err := sessionPanePIDs(ctx, sessionID)
	if err != nil || len(panes) != 1 {
		return 0, ""
	}
	started, err := runtimeProcessIdentity(panes[0])
	if err != nil {
		return 0, ""
	}
	return panes[0], started
}

// ownRuntimeReceipt reads the wrapper's own receipt and its path. ok is false
// unless it names this exact hub, project, agent, run, session and socket.
func ownRuntimeReceipt(e env) (runtimeProcessReceipt, string, bool) {
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	var receipt runtimeProcessReceipt
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Hub != e.hub || receipt.Task != e.task || receipt.Agent != e.agent || receipt.Run != e.runID || receipt.Session != os.Getenv(spawn.EnvSession) || receipt.Socket != os.Getenv("TT_TMUX_SOCKET") {
		return runtimeProcessReceipt{}, path, false
	}
	return receipt, path, true
}

// recordRuntimeExit writes the runtime's exit into the wrapper's own receipt.
// It returns that receipt, and true only when the exit was saved.
func recordRuntimeExit(e env) (runtimeProcessReceipt, bool) {
	receipt, path, ok := ownRuntimeReceipt(e)
	if !ok {
		return receipt, false
	}
	receipt.ExitedAt = time.Now().UTC()
	return receipt, writePrivateJSON(path, receipt) == nil
}

// recordRuntimeExitReport records in the wrapper's own receipt, after its
// recorded exit, either when the hub accepted the exit report or why the
// wrapper stopped retrying it.
func recordRuntimeExitReport(e env, accepted time.Time, stopped string) {
	receipt, path, ok := ownRuntimeReceipt(e)
	if !ok || receipt.ExitedAt.IsZero() {
		return
	}
	if accepted.IsZero() {
		receipt.ExitReportStopped = stopped
	} else {
		receipt.ExitReportedAt = accepted.UTC()
	}
	_ = writePrivateJSON(path, receipt)
}

// exitReportText is the wrapper's exit report. The hub's exited-primary rule
// accepts only this exact text, so a retry posts it unchanged.
func exitReportText(code int) string { return fmt.Sprintf("Process exited (%d)", code) }

// exitReportBackoff is the wait before each retry of a lost exit report; the
// last wait repeats.
var exitReportBackoff = []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}

// exitReporter retries the wrapper's exit report after its first post
// failed. post sends the exited event, agent reads this agent from the hub,
// table reads the host's process table without the reader's own ps, self is
// the wrapper's process ID and after waits.
type exitReporter struct {
	post  func(ctx context.Context, text string) error
	agent func(ctx context.Context) (api.Agent, error)
	table func(ctx context.Context) (map[int]discoveredProcess, error)
	self  func() int
	after func(time.Duration) <-chan time.Time
}

func productionExitReporter(e env) exitReporter {
	return exitReporter{
		post: func(_ context.Context, text string) error { return postEvent(e, api.EventExited, text, nil) },
		agent: func(ctx context.Context) (api.Agent, error) {
			c, err := e.client(5 * time.Second)
			if err != nil {
				return api.Agent{}, err
			}
			ctx, cancel := context.WithTimeout(ctx, max(5*time.Second, minCommandTimeout))
			defer cancel()
			return c.GetAgent(ctx, e.task, e.agent)
		},
		table: wrapperProcessTable, self: os.Getpid, after: time.After,
	}
}

// deliver retries the exit report until the hub accepts it, refuses it for
// good, or ctx ends with the pane's login shell. It posts only while the
// wrapper's own receipt records the exit and no outcome, and only while the
// pane holds just the wrapper and its idle login shell, the death probe's own
// rule: a runtime started again in that shell is never reported exited. An
// unreadable process table, a busy pane, a transport failure, a 429 or a 5xx
// waits for the next attempt.
func (r exitReporter) deliver(ctx context.Context, e env, code int) {
	text := exitReportText(code)
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-r.after(exitReportBackoff[min(attempt, len(exitReportBackoff)-1)]):
		}
		if ctx.Err() != nil {
			return
		}
		receipt, _, ok := ownRuntimeReceipt(e)
		if !ok || receipt.ExitedAt.IsZero() || !receipt.ExitReportedAt.IsZero() || receipt.ExitReportStopped != "" {
			return
		}
		if receipt.PanePID < 1 || receipt.PanePID != r.self() || receipt.PaneStarted == "" || receipt.Shell == "" {
			recordRuntimeExitReport(e, time.Time{}, "the receipt records no pane process of this wrapper, so its host cannot confirm the exit")
			return
		}
		processes, err := r.table(ctx)
		if err != nil {
			continue
		}
		if state, _ := idleShellTree(processes, receipt); state != handlerProbeGone {
			continue
		}
		err = r.post(ctx, text)
		if err == nil {
			recordRuntimeExitReport(e, time.Now(), "")
			return
		}
		var refused *api.HTTPError
		if !errors.As(err, &refused) || refused.Status < 400 || refused.Status == http.StatusTooManyRequests || refused.Status >= 500 {
			continue
		}
		if refused.Status != http.StatusConflict {
			recordRuntimeExitReport(e, time.Time{}, fmt.Sprintf("the hub refused the exit report: %d %s", refused.Status, refused.Msg))
			return
		}
		// An earlier post may have reached the hub with its response lost:
		// the hub then holds this run's exit and refuses another event.
		a, err := r.agent(ctx)
		if err != nil {
			continue
		}
		if a.RunID == e.runID && a.Status == api.AgentExited {
			recordRuntimeExitReport(e, time.Now(), "")
		} else {
			recordRuntimeExitReport(e, time.Time{}, fmt.Sprintf("the hub refused the exit report: %d %s; the agent is %s on %s", refused.Status, refused.Msg, a.Status, a.RunID))
		}
		return
	}
}

// wrapperProcessTable reads the host's process table without the ps process
// it starts. The wrapper is the pane's process, so its own ps would be a
// second child beside the login shell while it reads.
func wrapperProcessTable(ctx context.Context) (map[int]discoveredProcess, error) {
	var out processTableOutput
	cmd := exec.CommandContext(ctx, "ps", "-ax", "-o", "pid=,ppid=,lstart=,comm=")
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ps := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	return parseWrapperProcessTable(out.String(), ps)
}

func parseWrapperProcessTable(raw string, ps int) (map[int]discoveredProcess, error) {
	processes, err := parseProcessTable(raw)
	if err != nil {
		return nil, err
	}
	delete(processes, ps)
	return processes, nil
}

// processTableOutput bounds ps output to what maxProcessTable rows can need.
type processTableOutput struct{ bytes.Buffer }

func (w *processTableOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > maxProcessTable*256 {
		return 0, errors.New("process table output too large")
	}
	return w.Buffer.Write(p)
}

type runtimePIDProbe func(pid int, created string) (alive bool, err error)
type runtimeSessionProbe func(runtimeProcessReceipt) error

func nativeRuntimeSessionProbe(receipt runtimeProcessReceipt) error {
	if receipt.SessionID == "" || receipt.SessionCreated == "" {
		return errors.New("tmux creation identity unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sessions, err := localSessions(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Name != receipt.Session {
			continue
		}
		if s.valid() && s.ID == receipt.SessionID && s.Created == receipt.SessionCreated && s.Hub == receipt.Hub && s.Task == receipt.Task && s.Agent == receipt.Agent && s.Run == receipt.Run {
			return nil
		}
		return errors.New("tmux session identity changed")
	}
	return errors.New("tmux session identity unavailable")
}

func nativeRuntimePIDProbe(pid int, created string) (bool, error) {
	if pid < 1 || created == "" {
		return false, errors.New("runtime process identity unavailable")
	}
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	observed, err := runtimeProcessIdentity(pid)
	if err != nil {
		return false, err
	}
	if observed != created {
		return false, errors.New("runtime pid reused or creation identity changed")
	}
	return true, nil
}

func probeExactRuntimeProcess(b runtimeBinding, a api.Agent, tmuxProbe func(string) (bool, error), sessionProbe runtimeSessionProbe, pidProbe runtimePIDProbe) (bool, bool, error) {
	return probeRuntimeWithDiscovery(b, a, tmuxProbe, sessionProbe, pidProbe, nil)
}

func probeRuntimeWithDiscovery(b runtimeBinding, a api.Agent, tmuxProbe func(string) (bool, error), sessionProbe runtimeSessionProbe, pidProbe runtimePIDProbe, discover runtimeDiscovery) (bool, bool, error) {
	if b.Hub == "" || b.Task != a.TaskID || b.Agent != a.ID || b.Run != a.RunID || b.Session == "" || a.Session != b.Session {
		return false, false, errors.New("runtime binding identity unavailable")
	}
	data, err := os.ReadFile(runtimeProcessPath(b.Hub, b.Agent, b.Run))
	if errors.Is(err, os.ErrNotExist) && discover != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		receipt, discoveryErr := discover(ctx, b, a)
		if discoveryErr != nil || ctx.Err() != nil {
			return false, false, errors.New("runtime discovery unavailable")
		}
		if !matchingRuntimeReceipt(receipt, b) || !receipt.ExitedAt.IsZero() || receipt.Started == "" || receipt.SessionID == "" || receipt.SessionCreated == "" {
			return false, false, errors.New("discovered runtime identity mismatch")
		}
		if err := sessionProbe(receipt); err != nil {
			return false, false, err
		}
		var alive bool
		alive, err = pidProbe(receipt.PID, receipt.Started)
		if err != nil || !alive {
			return false, false, errors.New("discovered runtime unavailable")
		}
		var rechecked runtimeProcessReceipt
		rechecked, err = discover(ctx, b, a)
		if err != nil || ctx.Err() != nil || rechecked != receipt {
			return false, false, errors.New("runtime discovery identity changed")
		}
		// Publish without replacing a receipt created concurrently by the wrapper.
		path := runtimeProcessPath(b.Hub, b.Agent, b.Run)
		var temp *os.File
		temp, err = os.CreateTemp(filepath.Dir(path), ".adopt-")
		if errors.Is(err, os.ErrNotExist) {
			if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
				temp, err = os.CreateTemp(filepath.Dir(path), ".adopt-")
			}
		}
		if err != nil {
			return false, false, err
		}
		defer os.Remove(temp.Name())
		err = json.NewEncoder(temp).Encode(receipt)
		if err == nil {
			err = temp.Sync()
		}
		closeErr := temp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Link(temp.Name(), path)
		}
		if err != nil {
			return false, false, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return false, false, errors.New("runtime process receipt unavailable")
	}
	var receipt runtimeProcessReceipt
	if err := json.Unmarshal(data, &receipt); err != nil || !matchingRuntimeReceipt(receipt, b) {
		return false, false, errors.New("runtime process receipt identity mismatch")
	}
	tmuxAlive, err := tmuxProbe(b.Session)
	if err != nil {
		return false, false, err
	}
	if !tmuxAlive {
		return false, false, nil
	}
	if err := sessionProbe(receipt); err != nil {
		return true, false, err
	}
	if !receipt.ExitedAt.IsZero() {
		return true, false, nil
	}
	alive, err := pidProbe(receipt.PID, receipt.Started)
	if err != nil {
		return true, false, err
	}
	return true, alive, nil
}

func matchingRuntimeReceipt(r runtimeProcessReceipt, b runtimeBinding) bool {
	return r.Hub == b.Hub && r.Task == b.Task && r.Agent == b.Agent && r.Run == b.Run && r.Session == b.Session && r.Socket == os.Getenv("TT_TMUX_SOCKET") && r.PID > 0
}

type runtimeDiscovery func(context.Context, runtimeBinding, api.Agent) (runtimeProcessReceipt, error)

// Discovery reads only explicit tmux ownership fields and process identity
// columns. No process environment, arguments or terminal content is inspected.
const maxRuntimeDiscoveryOutput = 256 << 10
const maxRuntimeLineage = 32

type runtimeOutput struct{ bytes.Buffer }

func (w *runtimeOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > maxRuntimeDiscoveryOutput {
		return 0, errors.New("runtime discovery output budget exhausted")
	}
	return w.Buffer.Write(p)
}
func runtimeDiscoveryCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	var out runtimeOutput
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out.Bytes(), err
}

var runtimeProcessRow = regexp.MustCompile(`^\s*([0-9]+)\s+([0-9]+)\s+([A-Za-z]{3}\s+[A-Za-z]{3}\s+[0-9]+\s+[0-9:]+\s+[0-9]{4})\s+(.+?)\s*$`)

type discoveredProcess struct {
	pid, parent         int
	started, executable string
}

// readProcessTable reads the identity columns of every process on the host.
func readProcessTable(ctx context.Context) (map[int]discoveredProcess, error) {
	raw, err := runtimeDiscoveryCommand(ctx, "ps", "-ax", "-o", "pid=,ppid=,lstart=,comm=")
	if err != nil {
		return nil, err
	}
	processes := map[int]discoveredProcess{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		row := runtimeProcessRow.FindStringSubmatch(line)
		if row == nil {
			return nil, errors.New("process identity format unavailable")
		}
		pid, _ := strconv.Atoi(row[1])
		parent, _ := strconv.Atoi(row[2])
		if pid < 1 || len(processes) >= 4096 {
			return nil, errors.New("process discovery budget exhausted")
		}
		if _, exists := processes[pid]; exists {
			return nil, errors.New("duplicate process identity")
		}
		processes[pid] = discoveredProcess{pid, parent, strings.TrimSpace(row[3]), row[4]}
	}
	return processes, nil
}

// processDescendsFrom reports whether anchor is p itself or one of its
// ancestors, following parents that were created no later than their child.
func processDescendsFrom(processes map[int]discoveredProcess, p discoveredProcess, anchor int) (bool, error) {
	current := p
	seen := map[int]bool{}
	for depth := 0; depth < maxRuntimeLineage; depth++ {
		if seen[current.pid] {
			break
		}
		seen[current.pid] = true
		if current.pid == anchor {
			return true, nil
		}
		parent, ok := processes[current.parent]
		if !ok {
			break
		}
		childTime, e1 := time.Parse("Mon Jan _2 15:04:05 2006", current.started)
		parentTime, e2 := time.Parse("Mon Jan _2 15:04:05 2006", parent.started)
		if e1 != nil || e2 != nil || parentTime.After(childTime) {
			return false, errors.New("process lineage creation mismatch")
		}
		if depth == maxRuntimeLineage-1 {
			return false, errors.New("runtime lineage budget exhausted")
		}
		current = parent
	}
	return false, nil
}

func nativeRuntimeDiscovery(ctx context.Context, b runtimeBinding, a api.Agent) (runtimeProcessReceipt, error) {
	var zero runtimeProcessReceipt
	fields := []string{"session_id", "session_created", "session_name", "TAILTERM_HUB", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "pane_pid"}
	for i, f := range fields {
		fields[i] = `"#{q/e:` + f + `}"`
	}
	args := []string{"list-panes", "-s", "-t", b.Session, "-F", "[" + strings.Join(fields, ",") + "]"}
	socket := os.Getenv("TT_TMUX_SOCKET")
	if socket != "" {
		args = append([]string{"-L", socket}, args...)
	}
	raw, err := runtimeDiscoveryCommand(ctx, "tmux", args...)
	if err != nil {
		return zero, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		return zero, errors.New("ambiguous owned runtime pane")
	}
	var values []string
	if json.Unmarshal([]byte(lines[0]), &values) != nil || len(values) != 8 {
		return zero, errors.New("invalid pane identity")
	}
	s := ownedSession{ID: values[0], Created: values[1], Name: values[2], Hub: values[3], Task: values[4], Agent: values[5], Run: values[6]}
	if !s.valid() || s.Hub != b.Hub || s.Task != b.Task || s.Agent != b.Agent || s.Run != b.Run || s.Name != b.Session {
		return zero, errors.New("owned pane identity mismatch")
	}
	anchor, err := strconv.Atoi(values[7])
	if err != nil || anchor < 1 {
		return zero, errors.New("pane pid unavailable")
	}
	processes, err := readProcessTable(ctx)
	if err != nil {
		return zero, err
	}
	root, ok := processes[anchor]
	if !ok {
		return zero, errors.New("pane process unavailable")
	}
	sessionEpoch, err := strconv.ParseInt(s.Created, 10, 64)
	rootTime, rootErr := time.ParseInLocation("Mon Jan _2 15:04:05 2006", root.started, time.Local)
	if err != nil || rootErr != nil || rootTime.Unix() < sessionEpoch {
		return zero, errors.New("pane creation predates owned session")
	}
	if a.Runtime != "" && a.Runtime != b.Runtime {
		return zero, errors.New("runtime identity mismatch")
	}
	var found []discoveredProcess
	for _, p := range processes {
		executable := filepath.Base(p.executable)
		runtime := b.Runtime
		if runtime == "" {
			runtime = "codex"
		}
		if executable != runtime {
			continue
		}
		owned, err := processDescendsFrom(processes, p, anchor)
		if err != nil {
			return zero, err
		}
		if owned {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return zero, errors.New("exact runtime lineage unavailable or ambiguous")
	}
	p := found[0]
	return runtimeProcessReceipt{Hub: b.Hub, Task: b.Task, Agent: b.Agent, Run: b.Run, Session: b.Session, Socket: socket, SessionID: s.ID, SessionCreated: s.Created, PID: p.pid, Started: p.started, PanePID: anchor, PaneStarted: root.started}, nil
}
