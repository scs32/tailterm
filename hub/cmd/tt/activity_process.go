package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if b.Hub == "" || b.Task != a.TaskID || b.Agent != a.ID || b.Run != a.RunID || b.Session == "" || a.Session != b.Session {
		return false, false, errors.New("runtime binding identity unavailable")
	}
	data, err := os.ReadFile(runtimeProcessPath(b.Hub, b.Agent, b.Run))
	if err != nil {
		return false, false, errors.New("runtime process receipt unavailable")
	}
	var receipt runtimeProcessReceipt
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.Hub != b.Hub || receipt.Task != b.Task || receipt.Agent != b.Agent || receipt.Run != b.Run || receipt.Session != b.Session || receipt.Socket != os.Getenv("TT_TMUX_SOCKET") || receipt.PID < 1 {
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
