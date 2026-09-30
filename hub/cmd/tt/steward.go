package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
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
	cleanup  func(context.Context, env, string, string) error
	host     func() string
	online   time.Duration
	poll     time.Duration
	// after runs once a rotation phase is saved; tests use it to stop the routine.
	after func(phase string) error
}

func productionStewardDeps() stewardDeps {
	runner := productionTeamRunner()
	return stewardDeps{template: loadStewardTemplate, spawn: cmdSpawn, cleanup: runner.cleanup, host: spawn.Host, online: 2 * time.Minute, poll: 2 * time.Second}
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

// stewardLaunchArgs are the tt spawn flags for the steward template. The
// project's lifecycle generation rises with each pause and resume, and
// admission requires the current one.
func stewardLaunchArgs(t stewardTemplate, hub, task, agentID, name, cwd, permissionMode string, lifecycle int64) []string {
	args := []string{"--role", api.AgentRoleBacklogSteward, "--agent-id", agentID, "--name", name, "--task", task, "--hub", hub,
		"--run", t.Runtime, "--runtime", t.Runtime, "--cwd", cwd, "--prompt", t.Prompt}
	if lifecycle > 0 {
		args = append(args, "--expected-lifecycle-generation", strconv.FormatInt(lifecycle, 10))
	}
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
	args := stewardLaunchArgs(tmpl, hub, task, id.AgentID, id.Name, id.Cwd, id.PermissionMode, detail.Task.LifecycleGeneration)
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
	out.SummaryRevision = status.SummaryRevision
	return out, nil
}

func cmdSteward(e env, args []string) error {
	usage := errors.New("usage: tt steward template|setup|summary|rotate|rotation|policy (see tt steward SUBCOMMAND --help)")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "template":
		return cmdStewardTemplate(e, args[1:])
	case "setup":
		return cmdStewardSetup(e, args[1:], productionStewardDeps())
	case "summary":
		return cmdStewardSummary(e, args[1:])
	case "rotate":
		return cmdStewardRotate(e, args[1:], productionStewardDeps())
	case "rotation":
		return cmdStewardRotation(e, args[1:])
	case "policy":
		return cmdStewardPolicy(e, args[1:])
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

// cmdStewardSummary reads and saves the backlog summary. Any session reads
// it; only the active steward's exact run or the owner saves it.
func cmdStewardSummary(e env, args []string) error {
	usage := errors.New("usage: tt steward summary get [--revision N] | set --revision N --body-file F --request-id KEY | history [--task ID] [--json]")
	if len(args) == 0 || (args[0] != "get" && args[0] != "set" && args[0] != "history") {
		return usage
	}
	operation := args[0]
	fs := flag.NewFlagSet("steward summary", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "get: the revision to read (default latest); set: the current revision you read")
	bodyFile := fs.String("body-file", "", "set: file holding the new summary (at most 64 KiB)")
	requestID := fs.String("request-id", "", "set: stable retry key")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return usage
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	switch operation {
	case "get":
		rev := *revision
		if rev < 0 {
			rev = 0
		}
		b, err := c.BacklogSummary(ctx, *task, rev)
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 404 && rev == 0 {
			return errors.New("no backlog summary is saved yet; save revision 1 with tt steward summary set --revision 0 --body-file F --request-id KEY")
		}
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(b)
			return nil
		}
		fmt.Printf("Backlog summary revision %d (%d bytes, digest %s, saved %s)\n\n%s\n", b.Revision, b.Bytes, b.Digest, b.CreatedAt.Format(time.RFC3339), b.Body)
	case "set":
		if *revision < 0 || *bodyFile == "" || *requestID == "" {
			return errors.New("tt steward summary set needs --revision (the current revision, 0 for the first), --body-file and --request-id")
		}
		body, err := os.ReadFile(*bodyFile)
		if err != nil {
			return err
		}
		if len(body) > api.MaxBacklogSummaryLen {
			return fmt.Errorf("the summary is %d bytes; the limit is %d", len(body), api.MaxBacklogSummaryLen)
		}
		b, err := c.SaveBacklogSummary(ctx, *task, api.SaveBacklogSummaryRequest{ExpectedRevision: *revision, Body: string(body), RequestID: *requestID, AgentID: e.agent, RunID: e.runID})
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(b)
			return nil
		}
		fmt.Printf("Saved backlog summary revision %d (%d bytes, digest %s).\n", b.Revision, b.Bytes, b.Digest)
	case "history":
		revisions, err := c.BacklogSummaryRevisions(ctx, *task)
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(revisions)
			return nil
		}
		for _, b := range revisions {
			by := b.AgentID
			if by == "" {
				by = "owner"
			}
			fmt.Printf("r%d  %s  %6d bytes  %s  by %s\n", b.Revision, b.CreatedAt.Format(time.RFC3339), b.Bytes, b.Digest[:12], by)
		}
	}
	return nil
}

// ---- Steward rotation (docs/backlog-steward.md) ----

// stewardRotationJournal records this host's phase of a steward rotation, so
// a rerun resumes it: never a second successor, re-issue or cleanup.
type stewardRotationJournal struct {
	Version          int    `json:"version"`
	Hub              string `json:"hub"`
	Task             string `json:"task"`
	Phase            string `json:"phase"`
	PrepareRequestID string `json:"prepareRequestId"`
	RotationID       string `json:"rotationId,omitempty"`
	OldAgentID       string `json:"oldAgentId"`
	OldRunID         string `json:"oldRunId"`
	OldName          string `json:"oldName"`
	SuccessorAgentID string `json:"successorAgentId"`
	SuccessorName    string `json:"successorName"`
	Reason           string `json:"reason"`
	Trigger          string `json:"trigger"`
	Cwd              string `json:"cwd"`
	PermissionMode   string `json:"permissionMode,omitempty"`
}

const stewardPhaseCleaned = "cleaned"

func stewardRotationJournalPath(hub, task string) string {
	return filepath.Join(relayDir(), "steward-rotation-"+rotationStateKey(hub, task)+".json")
}

func loadStewardRotationJournal(hub, task string) (*stewardRotationJournal, error) {
	data, err := os.ReadFile(stewardRotationJournalPath(hub, task))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j stewardRotationJournal
	if err = json.Unmarshal(data, &j); err != nil || j.Version != 1 || j.Hub != strings.TrimRight(hub, "/") || j.Task != task {
		return nil, errors.New("steward rotation journal is invalid; inspect it before retrying")
	}
	switch j.Phase {
	case rotationPhasePreparing, rotationPhasePrepared, rotationPhaseSpawned, rotationPhaseCommitted, stewardPhaseCleaned:
	default:
		return nil, errors.New("steward rotation journal has an unknown phase")
	}
	return &j, nil
}

// stewardRotationRefusal reports a hub refusal that changed nothing.
func stewardRotationRefusal(err error) (string, bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		switch httpErr.Code {
		case api.StewardRefusedWorking, api.StewardRefusedPendingTool, api.StewardRefusedRotationOpen, api.StewardRefusedPaused, api.StewardRefusedNotSteward,
			api.StewardRefusedSummaryMissing, api.StewardRefusedSuccessor, api.StewardRefusedNameTaken, api.StewardRefusedAgentCaller:
			return httpErr.Code, true
		}
	}
	return "", false
}

func stewardRotationBusy(err error) bool {
	code, ok := stewardRotationRefusal(err)
	return ok && (code == api.StewardRefusedWorking || code == api.StewardRefusedPendingTool)
}

// rotateSteward starts a rotation, or resumes the one this host's journal
// records: preparing, prepared, spawned, committed, cleaned. The journal is
// removed once the setup identity file names the successor.
func rotateSteward(ctx context.Context, d stewardDeps, e env, c *api.Client, task, reason, trigger string) (api.StewardRotation, error) {
	var zero api.StewardRotation
	hub := strings.TrimRight(c.Base, "/")
	path := stewardRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return zero, err
	}
	defer unlock()
	j, err := loadStewardRotationJournal(hub, task)
	if err != nil {
		return zero, err
	}
	save := func(phase string) error {
		j.Phase = phase
		if err := writePrivateJSON(path, *j); err != nil {
			return err
		}
		if d.after != nil {
			return d.after(phase)
		}
		return nil
	}
	if j == nil {
		rotations, err := c.ListStewardRotations(ctx, task)
		if err != nil {
			return zero, err
		}
		status, err := c.BacklogSteward(ctx, task)
		if err != nil {
			return zero, err
		}
		if status.Steward == nil {
			return zero, errors.New("the project has no active backlog steward to rotate")
		}
		old := *status.Steward
		for _, r := range rotations {
			if r.State == api.StewardRotationPrepared && r.OldAgentID == old.ID {
				return zero, fmt.Errorf("steward rotation %s is prepared without this host's journal; run tt steward rotate --abort --task %s", r.ID, task)
			}
		}
		if old.Host != d.host() {
			return zero, fmt.Errorf("steward %s runs on %s; rotate it from that host", old.Name, old.Host)
		}
		if reason == "" {
			reason = api.StewardRotationReasonManual
		}
		permissionMode := ""
		if id, err := loadStewardIdentity(hub, task); err != nil {
			return zero, err
		} else if id != nil && id.AgentID == old.ID {
			permissionMode = id.PermissionMode
		}
		j = &stewardRotationJournal{Version: 1, Hub: hub, Task: task, PrepareRequestID: newRotationKey("steward-rotation-prepare"), OldAgentID: old.ID, OldRunID: old.RunID,
			OldName: old.Name, SuccessorAgentID: api.NewID("agt"), SuccessorName: api.StewardSuccessorName(old.Name), Reason: reason, Trigger: trigger, Cwd: old.Cwd, PermissionMode: permissionMode}
		if err = save(rotationPhasePreparing); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhasePreparing {
		r, err := c.StewardRotationAction(ctx, task, api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: j.PrepareRequestID,
			OldAgentID: j.OldAgentID, OldRunID: j.OldRunID, SuccessorAgentID: j.SuccessorAgentID, SuccessorName: j.SuccessorName, Reason: j.Reason, Trigger: j.Trigger})
		if _, refused := stewardRotationRefusal(err); refused {
			// A refused prepare changed nothing; the next attempt starts fresh.
			if removeErr := os.Remove(path); removeErr != nil {
				return zero, removeErr
			}
			return zero, err
		}
		if err != nil {
			return zero, fmt.Errorf("prepare unconfirmed; rerun to replay it: %w", err)
		}
		j.RotationID = r.ID
		if err = save(rotationPhasePrepared); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhasePrepared || j.Phase == rotationPhaseSpawned {
		// A pause or owner close may have closed the old steward since this
		// host prepared: the rotation can never commit. Abort it and clean up
		// a registered successor instead of launching or committing.
		if r, stale, err := stewardRotationStale(ctx, c, task, j); err != nil {
			return zero, err
		} else if stale {
			return abortStaleStewardRotation(ctx, d, e, c, task, path, j, r)
		}
	}
	if j.Phase == rotationPhasePrepared {
		tmpl, err := d.template(ctx)
		if err != nil {
			return zero, err
		}
		detail, err := c.GetTask(ctx, task)
		if err != nil {
			return zero, err
		}
		args := append(stewardLaunchArgs(tmpl, hub, task, j.SuccessorAgentID, j.SuccessorName, j.Cwd, j.PermissionMode, detail.Task.LifecycleGeneration), "--steward-successor")
		launch := e
		launch.hub, launch.task, launch.agent, launch.agentName, launch.runID = hub, task, "", "", ""
		if err := d.spawn(launch, args); err != nil {
			return zero, fmt.Errorf("successor launch unconfirmed; the rotation stays prepared and %s stays the steward: rerun to resume or abort with tt steward rotate --abort: %w", j.OldName, err)
		}
		if err = save(rotationPhaseSpawned); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhaseSpawned {
		deadline := time.Now().Add(d.online)
		for {
			a, err := c.GetAgent(ctx, task, j.SuccessorAgentID)
			if err == nil && a.Online && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				break
			}
			if !time.Now().Before(deadline) {
				return zero, fmt.Errorf("successor %s did not come online within %s; the rotation stays prepared and %s stays the steward: rerun to resume or abort with tt steward rotate --abort", j.SuccessorName, d.online, j.OldName)
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(d.poll):
			}
		}
		if _, err := c.StewardRotationAction(ctx, task, api.StewardRotationRequest{Operation: api.StewardRotationCommit, RequestID: "steward-rotation-commit-" + j.RotationID, RotationID: j.RotationID}); err != nil {
			return zero, err
		}
		if err = save(rotationPhaseCommitted); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhaseCommitted {
		if err := d.cleanup(ctx, e, task, j.OldAgentID); err != nil {
			return zero, fmt.Errorf("rotation committed; old session cleanup will retry on rerun: %w", err)
		}
		if err = save(stewardPhaseCleaned); err != nil {
			return zero, err
		}
	}
	if err := saveStewardIdentity(stewardIdentity{Version: 1, Hub: hub, Task: task, AgentID: j.SuccessorAgentID, Name: j.SuccessorName, Cwd: j.Cwd, PermissionMode: j.PermissionMode, RotationID: j.RotationID}); err != nil {
		return zero, err
	}
	if err := os.Remove(path); err != nil {
		return zero, err
	}
	return c.GetStewardRotation(ctx, task, j.RotationID)
}

// stewardRotationStale reports whether the journal's rotation can no longer
// commit: another path aborted it, or its old steward closed.
func stewardRotationStale(ctx context.Context, c *api.Client, task string, j *stewardRotationJournal) (api.StewardRotation, bool, error) {
	r, err := c.GetStewardRotation(ctx, task, j.RotationID)
	if err != nil {
		return r, false, err
	}
	if r.State == api.StewardRotationAborted {
		return r, true, nil
	}
	if r.State != api.StewardRotationPrepared {
		return r, false, nil
	}
	old, err := c.GetAgent(ctx, task, j.OldAgentID)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return r, true, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, old.Status == api.AgentClosed, nil
}

// abortStaleStewardRotation aborts a stale rotation from its journal (the
// caller holds the journal lock), cleans up a registered successor and
// removes the journal. It reports the abort as an error, so the owner knows
// to provision the steward with tt steward setup.
func abortStaleStewardRotation(ctx context.Context, d stewardDeps, e env, c *api.Client, task, path string, j *stewardRotationJournal, r api.StewardRotation) (api.StewardRotation, error) {
	if r.State == api.StewardRotationPrepared {
		var err error
		if r, err = c.StewardRotationAction(ctx, task, api.StewardRotationRequest{Operation: api.StewardRotationAbort, RequestID: "steward-rotation-abort-" + j.RotationID, RotationID: j.RotationID}); err != nil {
			return r, err
		}
	}
	if successor, err := c.GetAgent(ctx, task, j.SuccessorAgentID); err == nil && !successor.CleanupDone {
		if err = d.cleanup(ctx, e, task, j.SuccessorAgentID); err != nil {
			return r, fmt.Errorf("stale steward rotation %s aborted; successor session cleanup will retry on rerun: %w", r.ID, err)
		}
	}
	if err := os.Remove(path); err != nil {
		return r, err
	}
	return r, fmt.Errorf("steward rotation %s was stale (steward %s closed before commit) and is now aborted; provision the steward with tt steward setup --task %s", r.ID, j.OldName, task)
}

// abortStewardRotation aborts the project's prepared rotation, closes and
// cleans up a registered successor, and leaves the old steward in place.
func abortStewardRotation(ctx context.Context, d stewardDeps, e env, c *api.Client, task string) (api.StewardRotation, error) {
	var zero api.StewardRotation
	hub := strings.TrimRight(c.Base, "/")
	path := stewardRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return zero, err
	}
	defer unlock()
	j, err := loadStewardRotationJournal(hub, task)
	if err != nil {
		return zero, err
	}
	if j != nil && (j.Phase == rotationPhaseCommitted || j.Phase == stewardPhaseCleaned) {
		return zero, errors.New("the rotation is committed; rerun tt steward rotate to finish the old session's cleanup")
	}
	rotations, err := c.ListStewardRotations(ctx, task)
	if err != nil {
		return zero, err
	}
	var open *api.StewardRotation
	for i := range rotations {
		if rotations[i].State == api.StewardRotationPrepared {
			open = &rotations[i]
		}
	}
	if open == nil {
		if j != nil {
			if err = os.Remove(path); err != nil {
				return zero, err
			}
		}
		return zero, errors.New("no prepared steward rotation to abort")
	}
	if j != nil && j.RotationID != "" && j.RotationID != open.ID {
		return zero, errors.New("this host's rotation journal names a different rotation; inspect it before aborting")
	}
	r, err := c.StewardRotationAction(ctx, task, api.StewardRotationRequest{Operation: api.StewardRotationAbort, RequestID: "steward-rotation-abort-" + open.ID, RotationID: open.ID})
	if err != nil {
		return zero, err
	}
	if _, err = c.GetAgent(ctx, task, r.SuccessorAgentID); err == nil {
		if err = d.cleanup(ctx, e, task, r.SuccessorAgentID); err != nil {
			return r, fmt.Errorf("rotation aborted; successor session cleanup will retry: %w", err)
		}
	}
	if j != nil {
		if err = os.Remove(path); err != nil {
			return r, err
		}
	}
	return r, nil
}

func cmdStewardRotate(e env, args []string, d stewardDeps) error {
	fs := flag.NewFlagSet("steward rotate", flag.ContinueOnError)
	task := fs.String("task", "", "project ID (required)")
	reason := fs.String("reason", api.StewardRotationReasonManual, "manual, tokens or template")
	abort := fs.Bool("abort", false, "abort the project's prepared rotation")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("usage: tt steward rotate --task ID [--reason manual|tokens|template] [--abort] [--json]")
	}
	if err := requireOwnerSession(e, "tt steward rotate"); err != nil {
		return err
	}
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var r api.StewardRotation
	if *abort {
		r, err = abortStewardRotation(ctx, d, e, c, *task)
	} else {
		r, err = rotateSteward(ctx, d, e, c, *task, *reason, api.StewardRotationTriggerOwner)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(r)
		return nil
	}
	if r.State == api.StewardRotationAborted {
		fmt.Printf("Aborted steward rotation %s; %s stays the steward.\n", r.ID, r.OldName)
		return nil
	}
	moved := 0
	if r.Receipt != nil {
		moved = r.Receipt.Reissued
	}
	fmt.Printf("Rotated backlog steward %s to %s (%s) with summary revision %d; %d open obligation(s) moved; old session cleanup confirmed.\n", r.OldName, r.SuccessorName, r.ID, r.SummaryRevision, moved)
	return nil
}

func cmdStewardRotation(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "list") {
		return errors.New("usage: tt steward rotation get ID | list [--task ID] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("steward rotation", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	jsonOut := fs.Bool("json", false, "JSON output")
	rest := args[1:]
	id := ""
	if operation == "get" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || (operation == "get" && id == "") || fs.NArg() != 0 {
		return errors.New("usage: tt steward rotation get ID | list [--task ID] [--json]")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if operation == "list" {
		rotations, err := c.ListStewardRotations(ctx, *task)
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(rotations)
			return nil
		}
		for _, r := range rotations {
			fmt.Printf("%s  %-9s  %s -> %s  summary r%d  reason=%s trigger=%s\n", r.ID, r.State, r.OldName, r.SuccessorName, r.SummaryRevision, r.Reason, r.Trigger)
		}
		return nil
	}
	r, err := c.GetStewardRotation(ctx, *task, id)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(r)
		return nil
	}
	fmt.Printf("Rotation %s: %s, %s (%s) -> %s (%s), summary revision %d, reason %s, trigger %s\n", r.ID, r.State, r.OldName, r.OldAgentID, r.SuccessorName, r.SuccessorAgentID, r.SummaryRevision, r.Reason, r.Trigger)
	if h := r.Handoff; h != nil {
		fmt.Printf("Backlog summary: revision %d, digest %s, saved by the old run: %t\n", h.SummaryRevision, h.SummaryDigest, h.SummaryFromOldRun)
		fmt.Printf("Moved obligations: %d\n", len(h.Reissued))
		for _, p := range h.Reissued {
			fmt.Printf("  #%d -> #%d  %s (was %s)\n", p.OldMessageSeq, p.NewMessageSeq, p.Subject, p.OldState)
		}
		fmt.Printf("Unanswered decisions the old steward proposed: %d\n", len(h.OpenDecisions))
		for _, d := range h.OpenDecisions {
			fmt.Printf("  #%d %s\n", d.MessageSeq, d.Question)
		}
	}
	return nil
}

func cmdStewardPolicy(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New("usage: tt steward policy get|set --task ID [--revision N --enabled=BOOL --max-total-tokens N --on-template-change=BOOL] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("steward policy", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "expected policy revision (required for set)")
	enabled := fs.Bool("enabled", true, "rotate when a limit is reached")
	maxTokens := fs.Int64("max-total-tokens", 0, "total tokens per steward run; 0 turns the limit off")
	onTemplate := fs.Bool("on-template-change", true, "rotate when the steward template changes")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("a valid --task project ID is required")
	}
	if operation == "set" {
		if err := requireOwnerSession(e, "tt steward policy set"); err != nil {
			return err
		}
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	current, err := c.StewardRotationPolicy(ctx, *task)
	if err != nil {
		return err
	}
	out := current
	if operation == "set" {
		if *revision < 0 {
			return errors.New("tt steward policy set needs --revision, the revision shown by tt steward policy get")
		}
		req := api.StewardRotationPolicyRequest{ExpectedRevision: *revision, Enabled: current.Enabled, MaxTotalTokens: current.MaxTotalTokens, OnTemplateChange: current.OnTemplateChange}
		if flagPresent(args, "enabled") {
			req.Enabled = *enabled
		}
		if flagPresent(args, "max-total-tokens") {
			req.MaxTotalTokens = *maxTokens
		}
		if flagPresent(args, "on-template-change") {
			req.OnTemplateChange = *onTemplate
		}
		if out, err = c.SetStewardRotationPolicy(ctx, *task, req); err != nil {
			return err
		}
	}
	if *jsonOut {
		printJSON(out)
		return nil
	}
	limit := "off"
	if out.MaxTotalTokens > 0 {
		limit = strconv.FormatInt(out.MaxTotalTokens, 10)
	}
	fmt.Printf("Steward rotation policy for %s (revision %d): enabled=%t max-total-tokens=%s on-template-change=%t\n", out.TaskID, out.Revision, out.Enabled, limit, out.OnTemplateChange)
	return nil
}

// stewardRotationRunner is the relay's steward rotation tick. It asks the hub
// nothing unless a steward session for the hub runs on this host, at most one
// due request a minute while one does, and caches an empty answer.
type stewardRotationRunner struct {
	deps     stewardDeps
	now      func() time.Time
	interval time.Duration
	mu       sync.Mutex
	quietTil time.Time
	digest   string
}

var hostStewardRunner = &stewardRotationRunner{deps: productionStewardDeps(), now: time.Now, interval: time.Minute}

func relayStewardRotationTick(ctx context.Context) error {
	e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	e.loadConfig()
	if e.hub == "" {
		return nil
	}
	return hostStewardRunner.hostTick(ctx, e, func() (*api.Client, error) {
		c, err := e.client(20 * time.Second)
		if err == nil {
			attachRelayBudget(c, activeRelayBudget)
		}
		return c, err
	}, spawn.Host())
}

// hostRunsSteward reports whether a backlog steward session for hub runs in
// this host's tmux server. It reads local state only.
func hostRunsSteward(ctx context.Context, hub string) (bool, error) {
	sessions, err := localSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range sessions {
		if s.valid() && strings.TrimRight(s.Hub, "/") == strings.TrimRight(hub, "/") && strings.HasPrefix(s.Name, "tt-steward-") {
			return true, nil
		}
	}
	return false, nil
}

func (r *stewardRotationRunner) hostTick(ctx context.Context, e env, client func() (*api.Client, error), host string) error {
	if runs, err := hostRunsSteward(ctx, e.hub); err != nil || !runs {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	return r.tick(ctx, e, c, host)
}

func (r *stewardRotationRunner) tick(ctx context.Context, e env, c *api.Client, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Before(r.quietTil) {
		return nil
	}
	if r.digest == "" {
		// The template is fixed for this binary; without it the runner still
		// rotates on tokens and simply cannot detect a template change.
		if t, err := r.deps.template(ctx); err == nil {
			r.digest = t.digest()
		}
	}
	list, err := c.StewardRotationsDue(ctx, host, r.digest)
	if err != nil {
		return err
	}
	if len(list.Entries) == 0 {
		r.quietTil = now.Add(rotationQuietCache)
		return nil
	}
	r.quietTil = now.Add(r.interval)
	hub := strings.TrimRight(c.Base, "/")
	var errs []error
	for _, d := range list.Entries {
		journal, err := loadStewardRotationJournal(hub, d.TaskID)
		if err != nil {
			errs = append(errs, fmt.Errorf("steward rotation %s: %w", d.TaskID, err))
			continue
		}
		if journal != nil {
			if _, err := rotateSteward(ctx, r.deps, e, c, d.TaskID, journal.Reason, journal.Trigger); err != nil && !stewardRotationBusy(err) {
				errs = append(errs, fmt.Errorf("steward rotation %s: %w", d.TaskID, err))
			}
			continue
		}
		if d.OpenRotation != nil || len(d.DueReasons) == 0 || !d.Idle {
			continue
		}
		if _, err := rotateSteward(ctx, r.deps, e, c, d.TaskID, d.DueReasons[0], api.StewardRotationTriggerRunner); err != nil && !stewardRotationBusy(err) {
			errs = append(errs, fmt.Errorf("steward rotation %s: %w", d.TaskID, err))
		}
	}
	return errors.Join(errs...)
}
