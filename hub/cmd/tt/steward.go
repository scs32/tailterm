package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/teamplan"
)

// Backlog steward (docs/backlog-steward.md): one persistent project role
// whose context is the backlog. The owner provisions it with tt steward
// setup, which launches the template through tt spawn --role backlog_steward.

// stewardTemplate is PROJECT_ROLE_TEMPLATES.backlog_steward from
// client/team-examples.js, read through the embedded team plan bundle.
type stewardTemplate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Title     string `json:"title"`
	Role      string `json:"role"`
	Runtime   string `json:"runtime"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
	Prompt    string `json:"prompt"`
}

func (t stewardTemplate) digest() string {
	return stewardTemplateDigest(t.Model, t.Reasoning, t.Prompt)
}

func loadStewardTemplate(ctx context.Context) (stewardTemplate, error) {
	var out stewardTemplate
	if err := teamplan.Run(ctx, map[string]any{"action": "project-role", "role": api.AgentRoleBacklogSteward}, &out); err != nil {
		return out, err
	}
	if out.Role != api.AgentRoleBacklogSteward || out.Runtime == "" || strings.TrimSpace(out.Prompt) == "" || !api.ValidName(out.Name) {
		return out, errors.New("the embedded backlog steward template is incomplete")
	}
	return out, nil
}

// stewardDeps are the host effects of setup and rotation; tests replace them.
type stewardDeps struct {
	template func(context.Context) (stewardTemplate, error)
	spawn    func(env, []string) error
}

func productionStewardDeps() stewardDeps {
	return stewardDeps{template: loadStewardTemplate, spawn: cmdSpawn}
}

// stewardIdentity is this host's record of the project's steward for setup
// and rotation. It holds no credential.
type stewardIdentity struct {
	Version        int    `json:"version"`
	Hub            string `json:"hub"`
	Task           string `json:"task"`
	AgentID        string `json:"agentId"`
	Name           string `json:"name"`
	Cwd            string `json:"cwd"`
	PermissionMode string `json:"permissionMode,omitempty"`
	RotationID     string `json:"rotationId,omitempty"`
}

func stewardIdentityPath(hub, task string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(hub, "/")))
	return filepath.Join(relayDir(), hex.EncodeToString(sum[:8])+"-"+task+".steward.json")
}

func loadStewardIdentity(hub, task string) (*stewardIdentity, error) {
	data, err := os.ReadFile(stewardIdentityPath(hub, task))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id stewardIdentity
	if err = json.Unmarshal(data, &id); err != nil || id.Version != 1 || id.Hub != strings.TrimRight(hub, "/") || id.Task != task || !api.ValidID(id.AgentID, "agt") || !api.ValidName(id.Name) {
		return nil, errors.New("the saved backlog steward identity is invalid; inspect it before retrying")
	}
	return &id, nil
}

func saveStewardIdentity(id stewardIdentity) error {
	path := stewardIdentityPath(id.Hub, id.Task)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := writePrivateJSON(path, id); err != nil {
		return err
	}
	// Persist the rename before the hub sees the identity.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// freeStewardName keeps base unless an open agent holds it, then takes the
// lowest free base-N.
func freeStewardName(agents []api.Agent, base, self string) string {
	taken := map[string]bool{}
	for _, a := range agents {
		if a.Status != api.AgentClosed && a.ID != self {
			taken[strings.ToLower(a.Name)] = true
		}
	}
	if !taken[strings.ToLower(base)] {
		return base
	}
	for n := 2; ; n++ {
		if name := fmt.Sprintf("%s-%d", base, n); !taken[strings.ToLower(name)] {
			return name
		}
	}
}

// stewardLaunchArgs are the tt spawn flags for the steward template.
func stewardLaunchArgs(t stewardTemplate, hub, task, agentID, name, cwd, permissionMode string) []string {
	args := []string{"--role", api.AgentRoleBacklogSteward, "--agent-id", agentID, "--name", name, "--task", task, "--hub", hub,
		"--run", t.Runtime, "--runtime", t.Runtime, "--cwd", cwd, "--prompt", t.Prompt}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.Reasoning != "" {
		args = append(args, "--reasoning", t.Reasoning)
	}
	if permissionMode != "" {
		args = append(args, "--permission-mode", permissionMode)
	}
	return args
}

// setupSteward launches the project's steward, or relaunches the one this
// host's identity file names. The saved ID is reused while its registration
// is unconfirmed (404) or the agent is not closed; a closed steward (pause,
// owner close, the old side of a rotation) gets a fresh identity.
func setupSteward(ctx context.Context, d stewardDeps, e env, c *api.Client, task, cwd, name, permissionMode string) (api.Agent, error) {
	var zero api.Agent
	hub := strings.TrimRight(c.Base, "/")
	path := stewardIdentityPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return zero, err
	}
	defer unlock()
	tmpl, err := d.template(ctx)
	if err != nil {
		return zero, err
	}
	saved, err := loadStewardIdentity(hub, task)
	if err != nil {
		return zero, err
	}
	detail, err := c.GetTask(ctx, task)
	if err != nil {
		return zero, err
	}
	fresh := func(base string) stewardIdentity {
		id := api.NewID("agt")
		return stewardIdentity{Version: 1, Hub: hub, Task: task, AgentID: id, Name: freeStewardName(detail.Agents, base, id), Cwd: cwd, PermissionMode: permissionMode}
	}
	base := name
	if base == "" {
		base = tmpl.Name
	}
	var id stewardIdentity
	expectedRun := ""
	if saved == nil {
		if cwd == "" {
			return zero, errors.New("the first tt steward setup needs --cwd")
		}
		id = fresh(base)
	} else {
		id = *saved
		if cwd != "" && permissionMode == "" {
			permissionMode = saved.PermissionMode
		}
		if cwd == "" {
			cwd, permissionMode = saved.Cwd, saved.PermissionMode
		}
		current, getErr := c.GetAgent(ctx, task, saved.AgentID)
		var httpErr *api.HTTPError
		switch {
		case getErr == nil && current.Status == api.AgentClosed:
			if name == "" {
				base = saved.Name
			}
			id = fresh(base)
		case getErr == nil:
			if current.Role != api.AgentRoleBacklogSteward {
				return zero, errors.New("the saved steward identity names an agent that is not a backlog steward")
			}
			if cwd != current.Cwd {
				return zero, fmt.Errorf("steward %s runs in %s; setup cannot move it to %s", current.Name, current.Cwd, cwd)
			}
			if current.Status == api.AgentExited {
				expectedRun = current.RunID
			}
			id.Name = current.Name
		case errors.As(getErr, &httpErr) && httpErr.Status == 404:
			// The registration outcome was lost before the hub saved it: retry
			// the same identity.
		default:
			return zero, getErr
		}
		id.Cwd, id.PermissionMode = cwd, permissionMode
	}
	if err = saveStewardIdentity(id); err != nil {
		return zero, err
	}
	args := stewardLaunchArgs(tmpl, hub, task, id.AgentID, id.Name, id.Cwd, id.PermissionMode)
	if expectedRun != "" {
		args = append(args, "--expected-run-id", expectedRun)
	}
	launch := e
	launch.hub, launch.task, launch.agent, launch.agentName, launch.runID = hub, task, "", "", ""
	if err = d.spawn(launch, args); err != nil {
		return zero, fmt.Errorf("steward launch unconfirmed; rerun tt steward setup to resume the same identity: %w", err)
	}
	return c.GetAgent(ctx, task, id.AgentID)
}

// launchStewardBriefing reads what a launch's briefing says about the
// project's backlog steward.
// A project whose roster has no open steward needs no request, so its
// briefings and requests stay exactly as before; a hub without the steward
// route has no steward.
func launchStewardBriefing(ctx context.Context, c *api.Client, task, role string, agents []api.Agent) (stewardBriefing, error) {
	var out stewardBriefing
	present := role == api.AgentRoleBacklogSteward
	for _, a := range agents {
		if a.Role == api.AgentRoleBacklogSteward && a.Status != api.AgentClosed {
			present = true
		}
	}
	if !present {
		return out, nil
	}
	status, err := c.BacklogSteward(ctx, task)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read the project's backlog steward: %w", err)
	}
	if status.Steward != nil {
		out.Active = status.Steward.Name
	}
	return out, nil
}

func cmdSteward(e env, args []string) error {
	usage := errors.New("usage: tt steward template|setup (see tt steward SUBCOMMAND --help)")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "template":
		return cmdStewardTemplate(e, args[1:])
	case "setup":
		return cmdStewardSetup(e, args[1:], productionStewardDeps())
	}
	return usage
}

func cmdStewardTemplate(e env, args []string) error {
	fs := flag.NewFlagSet("steward template", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	t, err := loadStewardTemplate(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(struct {
			stewardTemplate
			Digest string `json:"digest"`
		}{t, t.digest()})
		return nil
	}
	fmt.Printf("%s (%s): runtime %s, model %s, reasoning %s, digest %s\n\n%s\n", t.Title, t.Role, t.Runtime, t.Model, t.Reasoning, t.digest(), t.Prompt)
	return nil
}

func cmdStewardSetup(e env, args []string, d stewardDeps) error {
	fs := flag.NewFlagSet("steward setup", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID (required)")
	cwd := fs.String("cwd", "", "working directory (required on first setup)")
	name := fs.String("name", "", "agent name (default: the template's, backlog-steward)")
	permissionMode := fs.String("permission-mode", "", "permission preset (default: host settings)")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 || (*name != "" && !api.ValidName(*name)) {
		return errors.New("usage: tt steward setup --task ID --cwd DIR [--name N] [--permission-mode MODE]")
	}
	if err := requireOwnerSession(e, "tt steward setup"); err != nil {
		return err
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
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a, err := setupSteward(ctx, d, e, c, *task, *cwd, *name, *permissionMode)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(a)
		return nil
	}
	fmt.Printf("Backlog steward %s (%s / %s) is %s on %s.\n", a.Name, a.ID, a.RunID, a.Status, a.Host)
	return nil
}
