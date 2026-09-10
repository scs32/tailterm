package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Durable local ownership receipts contain no credentials. Stable session IDs,
// creation time and run identity protect against renamed/reused session names.
type ownedSession struct {
	ID      string `json:"id"`
	Created string `json:"created"`
	Name    string `json:"name"`
	Hub     string `json:"hub"`
	Task    string `json:"task"`
	Agent   string `json:"agent"`
	Run     string `json:"run"`
}

var sessionIDPattern = regexp.MustCompile(`^\$[0-9]+$`)
var sessionTimePattern = regexp.MustCompile(`^[0-9]+$`)

func (s ownedSession) valid() bool {
	return sessionIDPattern.MatchString(s.ID) && sessionTimePattern.MatchString(s.Created) && s.Hub != "" && api.ValidID(s.Task, "tsk") && api.ValidID(s.Agent, "agt") && runIDPattern.MatchString(s.Run)
}
func (s ownedSession) path() string {
	return filepath.Join(relayDir(), bindingKey(runtimeBinding{Hub: s.Hub, Agent: s.Agent})+"-"+s.Run+".session.json")
}
func localSessions(ctx context.Context) ([]ownedSession, error) {
	fields := []string{"session_id", "session_created", "session_name", "TAILTERM_HUB", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN"}
	for i, f := range fields {
		fields[i] = `"#{q/e:` + f + `}"`
	}
	raw, err := startupTmux(ctx, "list-sessions", "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) && (strings.Contains(string(e.Stderr), "no server running") || strings.Contains(string(e.Stderr), "No such file or directory")) {
			return nil, nil
		}
		return nil, err
	}
	var sessions []ownedSession
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var f []string
		if json.Unmarshal([]byte(line), &f) != nil || len(f) != 7 {
			return nil, errors.New("cannot verify tmux session identities")
		}
		sessions = append(sessions, ownedSession{f[0], f[1], f[2], f[3], f[4], f[5], f[6]})
	}
	return sessions, nil
}
func rememberSessions(ctx context.Context, hub string) ([]ownedSession, error) {
	sessions, err := localSessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.Hub == hub && s.valid() {
			if err := writePrivateJSON(s.path(), s); err != nil {
				return nil, err
			}
		}
	}
	return sessions, nil
}
func stopOwnedSession(ctx context.Context, s ownedSession) error {
	sessions, err := localSessions(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, current := range sessions {
		if current.ID != s.ID {
			continue
		}
		if current.Created != s.Created || current.Hub != s.Hub || current.Task != s.Task || current.Agent != s.Agent || current.Run != s.Run {
			return errors.New("session identity changed; replacement left open")
		}
		found = true
	}
	if !found {
		return nil
	}
	// Check and kill inside tmux's command queue, without shell evaluation.
	cond := fmt.Sprintf("#{&&:#{==:#{session_created},%s},#{&&:#{==:#{TAILTERM_TASK},%s},#{&&:#{==:#{TAILTERM_AGENT},%s},#{==:#{TAILTERM_RUN},%s}}}}", s.Created, s.Task, s.Agent, s.Run)
	if _, err = startupTmux(ctx, "if-shell", "-F", "-t", s.ID, cond, "kill-session -t "+spawn.ShellQuote(s.ID), "display-message -p 'session identity changed'"); err != nil {
		return errors.New("tmux could not stop the session")
	}
	sessions, err = localSessions(ctx)
	if err != nil {
		return err
	}
	for _, current := range sessions {
		if current.ID == s.ID {
			return errors.New("tmux session is still present; cleanup will retry")
		}
	}
	return nil
}

type cleanupResult struct {
	Confirmed int      `json:"confirmed"`
	Errors    []string `json:"errors"`
}

// Explicit IDs come from the owner's SSH action on an agent's saved host and
// allow confirming already-absent sessions without guessing host names. They
// apply to individually closed agents as well as agents in a closed task.
func cleanupSessions(ctx context.Context, e env, task string, selected []string) (cleanupResult, error) {
	result := cleanupResult{Errors: []string{}}
	selectedSet := map[string]bool{}
	for _, id := range selected {
		selectedSet[id] = true
	}
	c, err := e.client(3 * time.Second)
	if err != nil {
		return result, err
	}
	sessions, err := rememberSessions(ctx, e.hub)
	if err != nil {
		return result, err
	}
	details := map[string]api.TaskDetail{}
	get := func(id string) (api.TaskDetail, error) {
		if d, ok := details[id]; ok {
			return d, nil
		}
		d, err := c.GetTask(ctx, id)
		if err == nil {
			details[id] = d
		}
		return d, err
	}
	paths, err := filepath.Glob(filepath.Join(relayDir(), "*.session.json"))
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s ownedSession
		if json.Unmarshal(data, &s) != nil || !s.valid() || s.Hub != e.hub || (task != "" && s.Task != task) || (len(selectedSet) > 0 && !selectedSet[s.Agent]) {
			continue
		}
		d, err := get(s.Task)
		if err != nil {
			result.Errors = append(result.Errors, "Hub unavailable; local session retained")
			continue
		}
		var a *api.Agent
		for i := range d.Agents {
			if d.Agents[i].ID == s.Agent {
				a = &d.Agents[i]
				break
			}
		}
		if a == nil || a.Status != api.AgentClosed {
			continue
		}
		// Individual closeout of an open project applies only to the exact
		// current run. Task closure may additionally remove older owned runs.
		if d.Task.Status != api.TaskClosed && a.RunID != s.Run {
			continue
		}
		if a.RunID == s.Run {
			seen[s.Agent] = true
		}
		err = stopOwnedSession(ctx, s)
		message := ""
		if err != nil {
			message = err.Error()
			result.Errors = append(result.Errors, message)
		}
		// Older runs still belong to this task, but cannot acknowledge a successor.
		if a.RunID != s.Run {
			if err == nil {
				_ = os.Remove(path)
			}
			continue
		}
		if _, ackErr := c.ReportCleanup(ctx, s.Task, s.Agent, api.CleanupRequest{RunID: s.Run, Error: message}); ackErr != nil {
			result.Errors = append(result.Errors, "Cleanup receipt pending; will retry")
			continue
		}
		if err == nil {
			result.Confirmed++
			_ = os.Remove(path)
		}
	}
	if len(selected) > 0 {
		// Receipt processing may have removed older sessions. Re-read tmux before
		// deciding whether an already-absent selected run can be confirmed.
		sessions, err = localSessions(ctx)
		if err != nil {
			return result, err
		}
		d, err := get(task)
		if err != nil {
			return result, err
		}
		for _, id := range selected {
			if seen[id] {
				continue
			}
			found := false
			for _, a := range d.Agents {
				if a.ID != id {
					continue
				}
				found = true
				if a.CleanupDone {
					continue
				}
				if a.Status != api.AgentClosed {
					result.Errors = append(result.Errors, "Agent is not closed")
					continue
				}
				occupied := false
				for _, s := range sessions {
					if s.Name == a.Session || s.Agent == id {
						occupied = true
						break
					}
				}
				if occupied {
					result.Errors = append(result.Errors, "Unverified session left open")
					continue
				}
				if _, err := c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: a.RunID}); err != nil {
					result.Errors = append(result.Errors, "Cleanup receipt pending; will retry")
				} else {
					result.Confirmed++
				}
			}
			if !found {
				result.Errors = append(result.Errors, "Agent is not registered")
			}
		}
	}
	return result, nil
}
func cmdCleanup(e env, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	task := fs.String("task", e.task, "task containing closed agents")
	hub := fs.String("hub", e.hub, "hub URL")
	agents := fs.String("agents", "", "agent IDs assigned to this host")
	jsonOut := fs.Bool("json", false, "JSON result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") {
		return errors.New("a valid task id is required")
	}
	var selected []string
	if *agents != "" {
		selected = strings.Split(*agents, ",")
		for _, id := range selected {
			if !api.ValidID(id, "agt") {
				return errors.New("invalid agent id")
			}
		}
	}
	e.hub = *hub
	e.loadConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := cleanupSessions(ctx, e, *task, selected)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(result)
	} else {
		fmt.Printf("Confirmed %d session cleanups.\n", result.Confirmed)
		for _, err := range result.Errors {
			fmt.Println(err)
		}
	}
	return nil
}
func relayCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = cleanupSessions(ctx, readEnv(), "", nil)
}
