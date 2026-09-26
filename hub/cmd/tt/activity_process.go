package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Started        string    `json:"started,omitempty"`
	SessionID      string    `json:"sessionId,omitempty"`
	SessionCreated string    `json:"sessionCreated,omitempty"`
	ExitedAt       time.Time `json:"exitedAt,omitempty"`
}

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
	receipt := runtimeProcessReceipt{Hub: e.hub, Task: e.task, Agent: e.agent, Run: e.runID, Session: os.Getenv(spawn.EnvSession), Socket: os.Getenv("TT_TMUX_SOCKET"), PID: pid, Started: identity}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if sessions, err := localSessions(ctx); err == nil {
		for _, s := range sessions {
			if s.valid() && s.Hub == receipt.Hub && s.Task == receipt.Task && s.Agent == receipt.Agent && s.Run == receipt.Run && s.Name == receipt.Session {
				receipt.SessionID, receipt.SessionCreated = s.ID, s.Created
				break
			}
		}
	}
	cancel()
	_ = writePrivateJSON(runtimeProcessPath(e.hub, e.agent, e.runID), receipt)
}

func recordRuntimeExit(e env) {
	path := runtimeProcessPath(e.hub, e.agent, e.runID)
	var receipt runtimeProcessReceipt
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Hub != e.hub || receipt.Task != e.task || receipt.Agent != e.agent || receipt.Run != e.runID || receipt.Session != os.Getenv(spawn.EnvSession) || receipt.Socket != os.Getenv("TT_TMUX_SOCKET") {
		return
	}
	receipt.ExitedAt = time.Now().UTC()
	_ = writePrivateJSON(path, receipt)
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
	raw, err = runtimeDiscoveryCommand(ctx, "ps", "-ax", "-o", "pid=,ppid=,lstart=,comm=")
	if err != nil {
		return zero, err
	}
	processes := map[int]discoveredProcess{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		row := runtimeProcessRow.FindStringSubmatch(line)
		if row == nil {
			return zero, errors.New("process identity format unavailable")
		}
		pid, _ := strconv.Atoi(row[1])
		parent, _ := strconv.Atoi(row[2])
		if pid < 1 || len(processes) >= 4096 {
			return zero, errors.New("process discovery budget exhausted")
		}
		if _, exists := processes[pid]; exists {
			return zero, errors.New("duplicate process identity")
		}
		processes[pid] = discoveredProcess{pid, parent, strings.TrimSpace(row[3]), row[4]}
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
		current := p
		seen := map[int]bool{}
		owned := false
		for depth := 0; depth < maxRuntimeLineage; depth++ {
			if seen[current.pid] {
				break
			}
			seen[current.pid] = true
			if current.pid == anchor {
				owned = true
				break
			}
			parent, ok := processes[current.parent]
			if !ok {
				break
			}
			childTime, e1 := time.Parse("Mon Jan _2 15:04:05 2006", current.started)
			parentTime, e2 := time.Parse("Mon Jan _2 15:04:05 2006", parent.started)
			if e1 != nil || e2 != nil || parentTime.After(childTime) {
				return zero, errors.New("process lineage creation mismatch")
			}
			if depth == maxRuntimeLineage-1 {
				return zero, errors.New("runtime lineage budget exhausted")
			}
			current = parent
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
