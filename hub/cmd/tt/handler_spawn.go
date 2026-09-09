package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// A registering attempt cannot yet have executed. Once creating is durable,
// absence is ambiguous: tmux or the command could have exited before our reply.
// Never turn that uncertainty into a second execution automatically.
type handlerAttempt struct {
	Version     int           `json:"version"`
	Hub         string        `json:"hub"`
	Task        string        `json:"task"`
	Agent       string        `json:"agent"`
	Payload     string        `json:"payload"`
	ExpectedRun string        `json:"expectedRun,omitempty"`
	Run         string        `json:"run,omitempty"`
	Phase       string        `json:"phase"`
	Session     *ownedSession `json:"session,omitempty"`
}

func handlerAttemptPath(hub, task string) string {
	key := sha256.Sum256([]byte(hub + "\x00" + task + "\x00database_handler"))
	return filepath.Join(relayDir(), fmt.Sprintf("handler-%x.json", key))
}

func handlerLock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func saveHandlerAttempt(path string, attempt handlerAttempt) error {
	if err := writePrivateJSON(path, attempt); err != nil {
		return err
	}
	// Persist the atomic rename as well as the file contents before spawning.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func handlerPayload(req api.AddAgentRequest, opts spawn.Options) string {
	command, supplied := opts.Env["TAILTERM_HANDLER_COMMAND"]
	if !supplied {
		command = opts.Command
		if briefing := opts.Env["TAILTERM_BRIEFING"]; briefing != "" {
			command = strings.TrimSuffix(command, " "+spawn.ShellQuote(briefing))
		}
	}
	data, _ := json.Marshal([]string{req.Name, req.Host, req.Runtime, req.Cwd, req.ParentAgentID, opts.Self, command,
		opts.Env["TAILTERM_HANDLER_PROMPT"], opts.Env["TAILTERM_PERMISSION_RUNTIME"], opts.Env["TAILTERM_PERMISSION_MODE"], opts.Env["TAILTERM_ALLOWED_TOOLS"]})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func handlerOwned(ctx context.Context, hub, task, agent, run, name string, receipt *ownedSession) (*ownedSession, error) {
	sessions, err := localSessions(ctx)
	if err != nil {
		return nil, err
	}
	var found *ownedSession
	for _, s := range sessions {
		exact := s.Hub == hub && s.Task == task && s.Agent == agent && s.Run == run && s.valid()
		if exact {
			if found != nil {
				return nil, errors.New("multiple sessions claim this handler run; inspect them before retrying")
			}
			if receipt != nil && (receipt.ID != s.ID || receipt.Created != s.Created) {
				return nil, errors.New("handler session identity changed; replacement left untouched")
			}
			copy := s
			found = &copy
		} else if s.Name == name || (s.Hub == hub && s.Task == task && s.Agent == agent) {
			return nil, errors.New("handler session name or identity is occupied by another run; inspect it before retrying")
		}
	}
	return found, nil
}

func ensureHandler(ctx context.Context, c *api.Client, taskID string, req api.AddAgentRequest, opts spawn.Options) (api.Agent, error) {
	var empty api.Agent
	if c == nil || !api.ValidID(taskID, "tsk") || !api.ValidID(req.AgentID, "agt") || req.Role != "database_handler" || (req.ExpectedRunID != "" && !runIDPattern.MatchString(req.ExpectedRunID)) {
		return empty, errors.New("handler launch requires a project, stable agent identity and database_handler role")
	}
	if !api.ValidName(req.Name) || req.Host == "" || req.Runtime == "" || opts.Cwd != req.Cwd || opts.Self == "" || strings.TrimSpace(opts.Command) == "" || req.ParentAgentID != "" {
		return empty, errors.New("handler launch requires matching host execution settings and no helper parent")
	}
	u, err := url.Parse(c.Base)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return empty, errors.New("invalid handler hub URL")
	}
	hub := strings.TrimRight(c.Base, "/")
	path := handlerAttemptPath(hub, taskID)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return empty, err
	}
	defer unlock()
	var journal handlerAttempt
	raw, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	if exists {
		if json.Unmarshal(raw, &journal) != nil || journal.Version != 1 || journal.Hub != hub || journal.Task != taskID || journal.Agent != req.AgentID ||
			(journal.Phase != "registering" && journal.Phase != "prepared" && journal.Phase != "creating" && journal.Phase != "created") {
			return empty, errors.New("invalid or conflicting handler attempt journal; inspect host state before retrying")
		}
	}
	req.Session = "tt-handler-" + strings.TrimPrefix(req.AgentID, "agt_")
	payload := handlerPayload(req, opts)
	current, getErr := c.GetAgent(ctx, taskID, req.AgentID)
	found := getErr == nil
	if getErr != nil {
		var httpErr *api.HTTPError
		if !errors.As(getErr, &httpErr) || httpErr.Status != 404 {
			return empty, getErr
		}
	}
	if found && (current.ID != req.AgentID || current.TaskID != taskID || current.Role != req.Role || current.Name != req.Name || current.Host != req.Host || current.Runtime != req.Runtime || current.Cwd != req.Cwd) {
		return empty, errors.New("saved handler identity or launch settings differ from this host request")
	}
	if found && current.Status == api.AgentClosed {
		return empty, errors.New("handler is closed; it cannot be relaunched")
	}
	restart := found && current.RunID == req.ExpectedRunID && current.Status == api.AgentExited
	if exists && journal.Payload != payload && !restart {
		return empty, errors.New("handler execution settings changed; restore the saved plan or explicitly restart its exited run")
	}
	if found && current.Status == api.AgentExited && !restart {
		return empty, errors.New("handler exited; an explicit restart with its current expected run ID is required")
	}
	if req.ExpectedRunID != "" && !restart && (!exists || journal.ExpectedRun != req.ExpectedRunID || !found || current.RunID == req.ExpectedRunID) {
		return empty, errors.New("handler restart run changed; refresh the project before retrying")
	}
	if restart {
		var receipt *ownedSession
		if journal.Run == current.RunID {
			receipt = journal.Session
		}
		old, err := handlerOwned(ctx, hub, taskID, req.AgentID, current.RunID, req.Session, receipt)
		if err != nil {
			return empty, err
		}
		if old != nil {
			if err = stopOwnedSession(ctx, *old); err != nil {
				return empty, err
			}
		}
		exists = false
	}
	if !exists {
		journal = handlerAttempt{Version: 1, Hub: hub, Task: taskID, Agent: req.AgentID, Payload: payload, ExpectedRun: req.ExpectedRunID, Phase: "registering"}
		// Without a receipt/journal, an already registered but absent process may
		// have executed. Adopt only a session whose complete ownership is visible.
		if found && !restart {
			owned, err := handlerOwned(ctx, hub, taskID, req.AgentID, current.RunID, req.Session, nil)
			if err != nil {
				return empty, err
			}
			if owned == nil {
				return empty, errors.New("handler is registered but its host attempt is missing; inspect the process before explicitly restarting")
			}
			journal.Run, journal.Phase, journal.Session = current.RunID, "created", owned
		}
		if err = saveHandlerAttempt(path, journal); err != nil {
			return empty, err
		}
	}
	if !found || restart {
		current, err = c.AddAgent(ctx, taskID, req)
		if err != nil {
			return empty, fmt.Errorf("handler registration unconfirmed; retry the same saved plan: %w", err)
		}
	}
	if current.ID != req.AgentID || current.TaskID != taskID || current.Role != req.Role || !runIDPattern.MatchString(current.RunID) {
		return empty, errors.New("hub returned an unexpected handler identity")
	}
	if journal.Run != "" && journal.Run != current.RunID {
		return empty, errors.New("handler run changed outside this attempt; refresh the project")
	}
	if journal.Phase == "registering" {
		journal.Run, journal.Phase = current.RunID, "prepared"
		if err = saveHandlerAttempt(path, journal); err != nil {
			return empty, err
		}
	}
	owned, err := handlerOwned(ctx, hub, taskID, req.AgentID, current.RunID, req.Session, journal.Session)
	if err != nil {
		return empty, err
	}
	if owned == nil {
		if current.Status == api.AgentRetired {
			return empty, errors.New("handler is retired; resume it explicitly before launching")
		}
		if journal.Phase != "prepared" {
			return empty, errors.New("handler launch is ambiguous and no owned session is present; inspect host state and explicitly restart the exited run, do not replay the launch")
		}
		journal.Phase = "creating"
		if err = saveHandlerAttempt(path, journal); err != nil {
			return empty, err
		}
		launch := opts
		launch.Session = req.Session
		launch.Env = make(map[string]string, len(opts.Env)+6)
		for k, v := range opts.Env {
			if k != "TAILTERM_HANDLER_COMMAND" && k != "TAILTERM_HANDLER_PROMPT" && k != "TAILTERM_BRIEFING" {
				launch.Env[k] = v
			}
		}
		launch.Env[spawn.EnvHub], launch.Env[spawn.EnvTask], launch.Env[spawn.EnvAgent] = hub, taskID, current.ID
		launch.Env[spawn.EnvAgentName], launch.Env[spawn.EnvSession], launch.Env["TAILTERM_RUN"] = current.Name, req.Session, current.RunID
		if err = spawn.Create(launch); err != nil {
			return empty, fmt.Errorf("handler launch unconfirmed; inspect its owned session before retrying: %w", err)
		}
		owned, err = handlerOwned(ctx, hub, taskID, req.AgentID, current.RunID, req.Session, nil)
		if err != nil {
			return empty, err
		}
		if owned == nil {
			return empty, errors.New("handler launch returned without an owned session; inspect host state before explicit restart")
		}
	}
	journal.Phase, journal.Session = "created", owned
	if err = saveHandlerAttempt(path, journal); err != nil {
		return empty, err
	}
	if err = writePrivateJSON(owned.path(), *owned); err != nil {
		return empty, fmt.Errorf("handler started but cleanup receipt could not be saved; retry: %w", err)
	}
	current.Session = owned.Name
	return current, nil
}
