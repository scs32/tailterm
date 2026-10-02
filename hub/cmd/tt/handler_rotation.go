package main

import (
	"context"
	"crypto/rand"
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
)

// Handler rotation (docs/handler-rotation.md): the owner command and the
// host relay run the same routine. A host journal records each phase so a
// rerun resumes and never launches a second successor.

// handlerSpec is the saved tt spawn launch settings of a project's handler,
// without identity flags or credentials.
type handlerSpec struct {
	Version int      `json:"version"`
	Hub     string   `json:"hub"`
	Task    string   `json:"task"`
	Args    []string `json:"args"`
}

var handlerSpecFlags = map[string]bool{"run": true, "cwd": true, "prompt": true, "runtime": true, "model": true, "reasoning": true,
	"permission-mode": true, "approval-mode": true, "sandbox-mode": true, "allowed-tools-json": true}

func rotationStateKey(hub, task string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(hub, "/") + "\x00" + task))
	return hex.EncodeToString(sum[:16])
}

func handlerSpecPath(hub, task string) string {
	return filepath.Join(relayDir(), "handler-spec-"+rotationStateKey(hub, task)+".json")
}

func handlerRotationJournalPath(hub, task string) string {
	return filepath.Join(relayDir(), "handler-rotation-"+rotationStateKey(hub, task)+".json")
}

// newHandlerSpec accepts only launch settings: --flag value or --flag=value
// for the flags a handler launch uses. Identity, project and hub come from
// the rotation.
func newHandlerSpec(hub, task string, args []string) (handlerSpec, error) {
	spec := handlerSpec{Version: 1, Hub: strings.TrimRight(hub, "/"), Task: task}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(args[i], "-"), "-"), "=")
		if !strings.HasPrefix(args[i], "-") || !handlerSpecFlags[name] || seen[name] {
			return spec, fmt.Errorf("launch spec accepts each of --run, --cwd, --prompt, --runtime, --model, --reasoning, --permission-mode, --approval-mode, --sandbox-mode and --allowed-tools-json once; got %q", args[i])
		}
		if !hasValue {
			if i+1 >= len(args) {
				return spec, fmt.Errorf("--%s needs a value", name)
			}
			i++
			value = args[i]
		}
		if name == "cwd" {
			abs, err := filepath.Abs(value)
			if err != nil {
				return spec, err
			}
			value = abs
		}
		seen[name] = true
		spec.Args = append(spec.Args, "--"+name, value)
	}
	if strings.TrimSpace(spec.value("run")) == "" {
		return spec, errors.New("launch spec needs --run")
	}
	return spec, nil
}

func (s handlerSpec) value(name string) string {
	for i := 0; i+1 < len(s.Args); i += 2 {
		if s.Args[i] == "--"+name {
			return s.Args[i+1]
		}
	}
	return ""
}

func (s handlerSpec) runtime() string {
	if r := s.value("runtime"); r != "" {
		return r
	}
	if fields := strings.Fields(s.value("run")); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

func saveHandlerSpec(spec handlerSpec) error {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return err
	}
	return writePrivateJSON(handlerSpecPath(spec.Hub, spec.Task), spec)
}

func loadHandlerSpec(hub, task string) (*handlerSpec, error) {
	data, err := os.ReadFile(handlerSpecPath(hub, task))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var spec handlerSpec
	if err = json.Unmarshal(data, &spec); err != nil || spec.Version != 1 || spec.Hub != strings.TrimRight(hub, "/") || spec.Task != task {
		return nil, errors.New("saved handler launch spec is invalid; save it again with tt handler spec")
	}
	checked, err := newHandlerSpec(hub, task, spec.Args)
	if err != nil {
		return nil, err
	}
	return &checked, nil
}

type handlerRotationJournal struct {
	Version          int    `json:"version"`
	Hub              string `json:"hub"`
	Task             string `json:"task"`
	Phase            string `json:"phase"`
	PrepareRequestID string `json:"prepareRequestId"`
	RotationID       string `json:"rotationId,omitempty"`
	HandlerRevision  int64  `json:"handlerRevision"`
	OldAgentID       string `json:"oldAgentId"`
	OldRunID         string `json:"oldRunId"`
	SuccessorAgentID string `json:"successorAgentId"`
	SuccessorName    string `json:"successorName"`
	Reason           string `json:"reason"`
	Trigger          string `json:"trigger"`
	// An owner-authorized change of runtime, model or arm; a resume replays it.
	AuthorizedBy        string `json:"authorizedBy,omitempty"`
	AuthorizationReason string `json:"authorizationReason,omitempty"`
}

// rotationAuthorization is the owner's authorization for a rotation that may
// change the primary's runtime, model or arm: who authorized it and why.
type rotationAuthorization struct {
	by, reason string
}

func (a rotationAuthorization) set() bool { return a.by != "" }

const rotationAuthorizationHint = "an owner-authorized change of runtime, model or arm needs tt handler rotate --authorized-by NAME --authorization-reason TEXT"

const (
	rotationPhasePreparing = "preparing"
	rotationPhasePrepared  = "prepared"
	rotationPhaseSpawned   = "spawned"
	rotationPhaseCommitted = "committed"
)

func loadRotationJournal(hub, task string) (*handlerRotationJournal, error) {
	data, err := os.ReadFile(handlerRotationJournalPath(hub, task))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j handlerRotationJournal
	if err = json.Unmarshal(data, &j); err != nil || j.Version != 1 || j.Hub != strings.TrimRight(hub, "/") || j.Task != task {
		return nil, errors.New("handler rotation journal is invalid; inspect it before retrying")
	}
	switch j.Phase {
	case rotationPhasePreparing, rotationPhasePrepared, rotationPhaseSpawned, rotationPhaseCommitted:
	default:
		return nil, errors.New("handler rotation journal has an unknown phase")
	}
	return &j, nil
}

type rotationDeps struct {
	spawn   func(env, []string) error
	cleanup func(context.Context, env, string, string) error
	host    func() string
	online  time.Duration
	poll    time.Duration
	// after runs once a phase is saved; tests use it to stop the routine.
	after func(phase string) error
}

func productionRotationDeps() rotationDeps {
	runner := productionTeamRunner()
	return rotationDeps{spawn: cmdSpawn, cleanup: runner.cleanup, host: spawn.Host, online: 2 * time.Minute, poll: 2 * time.Second}
}

// rotationRefusal reports a hub refusal that changed nothing.
func rotationRefusal(err error) (string, bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		switch httpErr.Code {
		case api.HandlerRotationRefusedLiveLease, api.HandlerRotationRefusedWorking, api.HandlerRotationRefusedPendingTool, api.HandlerRotationRefusedOpen,
			api.HandlerRotationRefusedPaused, api.HandlerRotationRefusedNotPrimary, api.HandlerRotationRefusedSuccessor, api.HandlerRotationRefusedNameTaken,
			api.HandlerRotationRefusedAgentCaller, api.HandlerRotationRefusedStaleHandler:
			return httpErr.Code, true
		}
	}
	return "", false
}

func rotationBusy(err error) bool {
	code, ok := rotationRefusal(err)
	return ok && (code == api.HandlerRotationRefusedLiveLease || code == api.HandlerRotationRefusedWorking || code == api.HandlerRotationRefusedPendingTool)
}

// currentPrimaryHandler applies the hub's rule to a task detail: the
// explicit primary, else the oldest open handler.
func currentPrimaryHandler(d api.TaskDetail) (api.Agent, bool) {
	for _, a := range primaryHandlerFirst(d.Task, d.Agents) {
		if a.Role == api.AgentRoleDatabaseHandler && a.Status != api.AgentClosed && a.Status != api.AgentExited {
			return a, true
		}
	}
	return api.Agent{}, false
}

func newRotationKey(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// rotateHandler starts a rotation, or resumes the one this host's journal
// records. The phases are preparing, prepared, spawned and committed; the
// journal is removed once the old session's cleanup receipt is written. An
// authorization lets a new rotation change the runtime, model or arm; a
// resumed rotation uses the one in its journal.
func rotateHandler(ctx context.Context, d rotationDeps, e env, c *api.Client, task, reason, trigger string, spec handlerSpec, auth rotationAuthorization) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	hub := strings.TrimRight(c.Base, "/")
	path := handlerRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return zero, err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, task)
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
		detail, err := c.GetTask(ctx, task)
		if err != nil {
			return zero, err
		}
		rotations, err := c.ListHandlerRotations(ctx, task)
		if err != nil {
			return zero, err
		}
		for _, r := range rotations {
			if r.State == api.HandlerRotationPrepared {
				return zero, fmt.Errorf("handler rotation %s is prepared without this host's journal; run tt handler rotate --abort --task %s", r.ID, task)
			}
		}
		old, ok := currentPrimaryHandler(detail)
		if !ok {
			return zero, errors.New("the project has no open database handler to rotate")
		}
		if old.Host != d.host() {
			return zero, fmt.Errorf("handler %s runs on %s; rotate it from that host", old.Name, old.Host)
		}
		// The directory always matches; the runtime, model and arm may change
		// only with the owner's authorization.
		if spec.value("cwd") != old.Cwd || (spec.runtime() != old.Runtime && !auth.set()) {
			hint := ""
			if spec.value("cwd") == old.Cwd {
				hint = "; " + rotationAuthorizationHint
			}
			return zero, fmt.Errorf("the saved launch spec (runtime %q, cwd %q) differs from handler %s (runtime %q, cwd %q); save matching settings with tt handler spec%s", spec.runtime(), spec.value("cwd"), old.Name, old.Runtime, old.Cwd, hint)
		}
		if !auth.set() {
			if err := checkRotationArm(ctx, c, task, old, spec); err != nil {
				return zero, err
			}
		}
		if reason == "" {
			reason = api.HandlerRotationReasonManual
		}
		j = &handlerRotationJournal{Version: 1, Hub: hub, Task: task, PrepareRequestID: newRotationKey("rotation-prepare"), HandlerRevision: detail.Task.HandlerRevision,
			OldAgentID: old.ID, OldRunID: old.RunID, SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(old.Name, detail.Task.HandlerRevision),
			Reason: reason, Trigger: trigger, AuthorizedBy: auth.by, AuthorizationReason: auth.reason}
		if err = save(rotationPhasePreparing); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhasePreparing {
		r, err := c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: j.PrepareRequestID,
			ExpectedHandlerRevision: j.HandlerRevision, OldAgentID: j.OldAgentID, OldRunID: j.OldRunID, SuccessorAgentID: j.SuccessorAgentID,
			SuccessorName: j.SuccessorName, Reason: j.Reason, Trigger: j.Trigger, AuthorizedBy: j.AuthorizedBy, AuthorizationReason: j.AuthorizationReason})
		if _, refused := rotationRefusal(err); refused {
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
	if j.Phase == rotationPhasePrepared {
		args := append(append([]string(nil), spec.Args...), "--role", api.AgentRoleDatabaseHandler, "--agent-id", j.SuccessorAgentID,
			"--name", j.SuccessorName, "--task", task, "--hub", hub, "--handler-successor")
		launch := e
		launch.hub, launch.task, launch.agent, launch.agentName, launch.runID = hub, task, "", "", ""
		if err := d.spawn(launch, args); err != nil {
			return zero, fmt.Errorf("successor launch unconfirmed; rerun to resume or abort with tt handler rotate --abort: %w", err)
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
				return zero, fmt.Errorf("successor %s did not come online within %s; the rotation stays prepared: rerun to resume or abort with tt handler rotate --abort", j.SuccessorName, d.online)
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(d.poll):
			}
		}
		if _, err := c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: "rotation-commit-" + j.RotationID, RotationID: j.RotationID}); err != nil {
			return zero, err
		}
		if err = save(rotationPhaseCommitted); err != nil {
			return zero, err
		}
	}
	if err := d.cleanup(ctx, e, task, j.OldAgentID); err != nil {
		return zero, fmt.Errorf("rotation committed; old session cleanup will retry on rerun: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return zero, err
	}
	return c.GetHandlerRotation(ctx, task, j.RotationID)
}

// abortHandlerRotation aborts the project's prepared rotation, closes and
// cleans up a registered successor, and leaves the old handler primary.
func abortHandlerRotation(ctx context.Context, d rotationDeps, e env, c *api.Client, task string) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	hub := strings.TrimRight(c.Base, "/")
	path := handlerRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return zero, err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, task)
	if err != nil {
		return zero, err
	}
	if j != nil && j.Phase == rotationPhaseCommitted {
		return zero, errors.New("the rotation is committed; rerun tt handler rotate to finish the old session's cleanup")
	}
	rotations, err := c.ListHandlerRotations(ctx, task)
	if err != nil {
		return zero, err
	}
	var open *api.HandlerRotation
	for i := range rotations {
		if rotations[i].State == api.HandlerRotationPrepared {
			open = &rotations[i]
		}
	}
	if open == nil {
		if j != nil {
			if err = os.Remove(path); err != nil {
				return zero, err
			}
		}
		return zero, errors.New("no prepared handler rotation to abort")
	}
	if j != nil && j.RotationID != "" && j.RotationID != open.ID {
		return zero, errors.New("this host's rotation journal names a different rotation; inspect it before aborting")
	}
	r, err := c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "rotation-abort-" + open.ID, RotationID: open.ID})
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

func cmdHandler(e env, args []string) error {
	usage := errors.New("usage: tt handler rotate|rotation|policy|spec|arms|ab-report (see tt handler SUBCOMMAND --help)")
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "rotate":
		return cmdHandlerRotate(e, args[1:])
	case "rotation":
		return cmdHandlerRotation(e, args[1:])
	case "policy":
		return cmdHandlerPolicy(e, args[1:])
	case "spec":
		return cmdHandlerSpec(e, args[1:])
	case "arms":
		return cmdHandlerArms(e, args[1:])
	case "ab-report":
		return cmdHandlerABReport(e, args[1:])
	}
	return usage
}

// splitSpawnFlags separates this command's flags from tt spawn flags after --.
func splitSpawnFlags(args []string) ([]string, []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func requireOwnerSession(e env, what string) error {
	if e.agent != "" {
		return fmt.Errorf("%s is an owner command; run it outside an agent session", what)
	}
	return nil
}

func cmdHandlerSpec(e env, args []string) error {
	own, spawnArgs := splitSpawnFlags(args)
	fs := flag.NewFlagSet("handler spec", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(own); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || e.hub == "" {
		return errors.New("usage: tt handler spec --task ID [-- tt spawn launch flags]")
	}
	var spec *handlerSpec
	if len(spawnArgs) > 0 {
		if err := requireOwnerSession(e, "tt handler spec"); err != nil {
			return err
		}
		saved, err := newHandlerSpec(e.hub, *task, spawnArgs)
		if err != nil {
			return err
		}
		if err = saveHandlerSpec(saved); err != nil {
			return err
		}
		spec = &saved
	} else {
		loaded, err := loadHandlerSpec(e.hub, *task)
		if err != nil {
			return err
		}
		if loaded == nil {
			return errors.New("no handler launch spec is saved on this host for that project")
		}
		spec = loaded
	}
	if *jsonOut {
		printJSON(spec)
		return nil
	}
	fmt.Printf("Handler launch spec for %s: %s\n", spec.Task, strings.Join(spec.Args, " "))
	return nil
}

func cmdHandlerRotate(e env, args []string) error {
	own, spawnArgs := splitSpawnFlags(args)
	fs := flag.NewFlagSet("handler rotate", flag.ContinueOnError)
	task := fs.String("task", "", "project ID (required)")
	abort := fs.Bool("abort", false, "abort the project's prepared rotation")
	authorizedBy := fs.String("authorized-by", "", "who authorized a change of the primary's runtime, model or arm (with --authorization-reason)")
	authorizationReason := fs.String("authorization-reason", "", "why the change is authorized (with --authorized-by)")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(own); err != nil {
		return err
	}
	usage := errors.New("usage: tt handler rotate --task ID [--abort] [--authorized-by NAME --authorization-reason TEXT] [-- tt spawn launch flags]")
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return usage
	}
	auth := rotationAuthorization{by: strings.TrimSpace(*authorizedBy), reason: strings.TrimSpace(*authorizationReason)}
	if (auth.by == "") != (auth.reason == "") {
		return fmt.Errorf("--authorized-by and --authorization-reason go together; %w", usage)
	}
	if auth.set() && *abort {
		return fmt.Errorf("--abort takes no authorization; %w", usage)
	}
	if !api.ValidText(auth.by, 200) || !api.ValidText(auth.reason, 1000) {
		return errors.New("--authorized-by takes at most 200 bytes and --authorization-reason at most 1000, without control characters")
	}
	if err := requireOwnerSession(e, "tt handler rotate"); err != nil {
		return err
	}
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	deps := productionRotationDeps()
	var r api.HandlerRotation
	if *abort {
		r, err = abortHandlerRotation(ctx, deps, e, c, *task)
	} else {
		var spec *handlerSpec
		if len(spawnArgs) > 0 {
			saved, specErr := newHandlerSpec(c.Base, *task, spawnArgs)
			if specErr != nil {
				return specErr
			}
			if specErr = saveHandlerSpec(saved); specErr != nil {
				return specErr
			}
			spec = &saved
		} else if spec, err = loadHandlerSpec(c.Base, *task); err != nil {
			return err
		}
		if spec == nil {
			return errors.New("no handler launch spec is saved on this host; pass the handler's tt spawn launch flags after --, for example tt handler rotate --task ID -- --run codex --cwd DIR --prompt TEXT")
		}
		r, err = rotateHandler(ctx, deps, e, c, *task, api.HandlerRotationReasonManual, api.HandlerRotationTriggerOwner, *spec, auth)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(r)
		return nil
	}
	if r.State == api.HandlerRotationAborted {
		fmt.Printf("Aborted handler rotation %s; %s stays primary.\n", r.ID, r.OldName)
		return nil
	}
	moved := 0
	if r.Receipt != nil {
		moved = r.Receipt.Reissued
	}
	fmt.Printf("Rotated handler %s to %s (%s); %d open obligation(s) moved; old session cleanup confirmed.\n", r.OldName, r.SuccessorName, r.ID, moved)
	if line := rotationAuthorizationLine(r); line != "" {
		fmt.Println(line)
	}
	return nil
}

// rotationAuthorizationLine is the record of an owner-authorized change, or
// "" for an ordinary rotation.
func rotationAuthorizationLine(r api.HandlerRotation) string {
	a := r.Authorization
	if a == nil {
		return ""
	}
	line := fmt.Sprintf("Authorized by %s: %s", a.AuthorizedBy, a.Reason)
	if a.OldRuntime != "" || a.SuccessorRuntime != "" {
		line += fmt.Sprintf(" (runtime %s -> %s)", a.OldRuntime, a.SuccessorRuntime)
	}
	return line
}

func cmdHandlerRotation(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "list") {
		return errors.New("usage: tt handler rotation get ID | list [--task ID] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("handler rotation", flag.ContinueOnError)
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
		return errors.New("usage: tt handler rotation get ID | list [--task ID] [--json]")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if operation == "list" {
		rotations, err := c.ListHandlerRotations(ctx, *task)
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(rotations)
			return nil
		}
		for _, r := range rotations {
			authorized := ""
			if r.Authorization != nil {
				authorized = fmt.Sprintf(" authorized-by=%q", r.Authorization.AuthorizedBy)
			}
			fmt.Printf("%s  %-9s  %s -> %s  reason=%s trigger=%s%s\n", r.ID, r.State, r.OldName, r.SuccessorName, r.Reason, r.Trigger, authorized)
		}
		return nil
	}
	r, err := c.GetHandlerRotation(ctx, *task, id)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(r)
		return nil
	}
	fmt.Printf("Rotation %s: %s, %s (%s) -> %s (%s), reason %s, trigger %s\n", r.ID, r.State, r.OldName, r.OldAgentID, r.SuccessorName, r.SuccessorAgentID, r.Reason, r.Trigger)
	if line := rotationAuthorizationLine(r); line != "" {
		fmt.Println(line)
	}
	if h := r.Handoff; h != nil {
		fmt.Printf("Moved obligations: %d\n", len(h.Reissued))
		for _, p := range h.Reissued {
			fmt.Printf("  #%d -> #%d  %s (was %s)\n", p.OldMessageSeq, p.NewMessageSeq, p.Subject, p.OldState)
		}
		fmt.Printf("Pending scope confirmations: %d\n", len(h.PendingScopeConfirmations))
		for _, v := range h.PendingScopeConfirmations {
			fmt.Printf("  %s item %s r%d order #%d\n", v.EntryID, v.ItemID, v.ItemRevision, v.OrderSeq)
		}
		fmt.Printf("Open obligations the old handler authored: %d\n", len(h.AuthoredOpen))
		for _, v := range h.AuthoredOpen {
			fmt.Printf("  #%d to %s (%s) %s\n", v.MessageSeq, v.AgentID, v.State, v.Subject)
		}
		fmt.Printf("Open allocation intents it authored: %d\n", len(h.AllocationIntents))
		for _, v := range h.AllocationIntents {
			fmt.Printf("  %s item %s r%d %s\n", v.AgentID, v.ItemID, v.ItemRevision, v.TeamRole)
		}
		fmt.Printf("Queue entries claimed by its run: %d\n", len(h.QueueClaims))
		for _, v := range h.QueueClaims {
			fmt.Printf("  %s item %s cycle %d (%s)\n", v.EntryID, v.ItemID, v.Cycle, v.State)
		}
		fmt.Printf("Legacy required deliveries pinned to its run (not moved): %d\n", len(h.RequiredDeliveries))
		for _, v := range h.RequiredDeliveries {
			fmt.Printf("  %s #%d %s item %s (%s)\n", v.ID, v.MessageSeq, v.Kind, v.ItemID, v.Phase)
		}
		fmt.Printf("Live leases: %d\n", len(h.LiveLeases))
	}
	return nil
}

func cmdHandlerPolicy(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New("usage: tt handler policy get|set --task ID [--revision N --enabled=BOOL --max-items N --max-total-tokens N --on-template-change=BOOL] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("handler policy", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "expected policy revision (required for set)")
	enabled := fs.Bool("enabled", true, "rotate when a limit is reached")
	maxItems := fs.Int64("max-items", 0, "finished leased items per handler run; 0 turns the limit off")
	maxTokens := fs.Int64("max-total-tokens", 0, "total tokens per handler run; 0 turns the limit off")
	onTemplate := fs.Bool("on-template-change", true, "rotate when the handler prompt template changes")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("a valid --task project ID is required")
	}
	if operation == "set" {
		if err := requireOwnerSession(e, "tt handler policy set"); err != nil {
			return err
		}
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	current, err := c.HandlerRotationPolicy(ctx, *task)
	if err != nil {
		return err
	}
	out := current
	if operation == "set" {
		if *revision < 0 {
			return errors.New("tt handler policy set needs --revision, the revision shown by tt handler policy get")
		}
		req := api.HandlerRotationPolicyRequest{ExpectedRevision: *revision, Enabled: current.Enabled, MaxItems: current.MaxItems, MaxTotalTokens: current.MaxTotalTokens, OnTemplateChange: current.OnTemplateChange}
		if flagPresent(args, "enabled") {
			req.Enabled = *enabled
		}
		if flagPresent(args, "max-items") {
			req.MaxItems = *maxItems
		}
		if flagPresent(args, "max-total-tokens") {
			req.MaxTotalTokens = *maxTokens
		}
		if flagPresent(args, "on-template-change") {
			req.OnTemplateChange = *onTemplate
		}
		if out, err = c.SetHandlerRotationPolicy(ctx, *task, req); err != nil {
			return err
		}
	}
	if *jsonOut {
		printJSON(out)
		return nil
	}
	limit := func(v int64) string {
		if v == 0 {
			return "off"
		}
		return strconv.FormatInt(v, 10)
	}
	fmt.Printf("Handler rotation policy for %s (revision %d): enabled=%t max-items=%s max-total-tokens=%s on-template-change=%t\n",
		out.TaskID, out.Revision, out.Enabled, limit(out.MaxItems), limit(out.MaxTotalTokens), out.OnTemplateChange)
	return nil
}

// rotationRunner is the relay's rotation tick. It asks the hub once per tick
// which primary handlers on this host have an enabled policy, and caches an
// empty answer so an idle host does not spend its request budget.
type rotationRunner struct {
	deps rotationDeps
	now  func() time.Time
	// interval is the least time between due requests while a policy is
	// enabled; the relay loops every few seconds and rotation is rarely due.
	interval time.Duration
	mu       sync.Mutex
	quietTil time.Time
	notified map[string]bool
}

const rotationQuietCache = 5 * time.Minute

var hostRotationRunner = &rotationRunner{deps: productionRotationDeps(), now: time.Now, interval: time.Minute, notified: map[string]bool{}}

func relayHandlerRotationTick(ctx context.Context) error {
	e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	e.loadConfig()
	if e.hub == "" {
		return nil
	}
	// Spend no hub request on a host that runs no database handler.
	if runs, err := hostRunsHandler(ctx, e.hub); err != nil || !runs {
		return err
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	attachRelayBudget(c, activeRelayBudget)
	return hostRotationRunner.tick(ctx, e, c, spawn.Host())
}

// hostRunsHandler reports whether a database handler session for hub runs
// in this host's tmux server. It reads local state only.
func hostRunsHandler(ctx context.Context, hub string) (bool, error) {
	sessions, err := localSessions(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range sessions {
		if s.valid() && strings.TrimRight(s.Hub, "/") == strings.TrimRight(hub, "/") && strings.HasPrefix(s.Name, "tt-handler-") {
			return true, nil
		}
	}
	return false, nil
}

// rotationDueReasons decides what the runner acts on. Item and token limits
// come from the hub; the template check uses this host's saved prompt, or,
// with no saved spec, only flags a legacy run with no recorded template.
func rotationDueReasons(d api.HandlerRotationDue, spec *handlerSpec) []string {
	var reasons []string
	for _, r := range d.DueReasons {
		if r != api.HandlerRotationReasonTemplate {
			reasons = append(reasons, r)
		}
	}
	if !d.Policy.OnTemplateChange {
		return reasons
	}
	// Without a saved spec this host does not know the handler's prompt, so
	// only a legacy run with no recorded template is known to be stale.
	if spec == nil {
		if d.RecordedDigest == "" {
			reasons = append(reasons, api.HandlerRotationReasonTemplate)
		}
		return reasons
	}
	if d.RecordedDigest != handlerTemplateDigest(spec.value("prompt")) {
		reasons = append(reasons, api.HandlerRotationReasonTemplate)
	}
	return reasons
}

func (r *rotationRunner) tick(ctx context.Context, e env, c *api.Client, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Before(r.quietTil) {
		return nil
	}
	list, err := c.HandlerRotationsDue(ctx, host, handlerTemplateDigest(""))
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
		spec, err := loadHandlerSpec(hub, d.TaskID)
		if err != nil {
			errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
			continue
		}
		journal, err := loadRotationJournal(hub, d.TaskID)
		if err != nil {
			errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
			continue
		}
		if journal != nil {
			// Resume this host's rotation; a busy refusal waits for the next tick.
			if spec == nil {
				errs = append(errs, fmt.Errorf("handler rotation %s: the launch spec was removed during a rotation", d.TaskID))
				continue
			}
			if _, err := rotateHandler(ctx, r.deps, e, c, d.TaskID, journal.Reason, journal.Trigger, *spec, rotationAuthorization{}); err != nil && !rotationBusy(err) {
				errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
			}
			continue
		}
		if d.OpenRotation != nil {
			continue // another host or process owns it
		}
		reasons := rotationDueReasons(d, spec)
		if len(reasons) == 0 {
			continue
		}
		if spec == nil {
			if err := r.notifyMissingSpec(ctx, c, host, d, reasons); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if !d.Idle {
			continue
		}
		if _, err := rotateHandler(ctx, r.deps, e, c, d.TaskID, reasons[0], api.HandlerRotationTriggerRunner, *spec, rotationAuthorization{}); err != nil && !rotationBusy(err) {
			errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
		}
	}
	return errors.Join(errs...)
}

// notifyMissingSpec posts one owner notice per due episode of an exact run.
func (r *rotationRunner) notifyMissingSpec(ctx context.Context, c *api.Client, host string, d api.HandlerRotationDue, reasons []string) error {
	key := "handler-rotation-due-" + strings.TrimPrefix(d.Agent.ID, "agt_") + "-" + strings.TrimPrefix(d.Agent.RunID, "run_")
	if r.notified[key] {
		return nil
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Handler rotation is due but this host has no saved launch spec",
		Body: api.EnvelopeBody{Text: fmt.Sprintf("Database handler %s (%s / %s) is due for rotation (%s) under the project's policy, but host %s has no saved launch spec, so nothing was rotated. Save the handler's launch settings on that host with tt handler spec --task %s -- <tt spawn launch flags>, or rotate it now with tt handler rotate --task %s -- <flags>.",
			d.Agent.Name, d.Agent.ID, d.Agent.RunID, strings.Join(reasons, ", "), host, d.TaskID, d.TaskID)}}
	if _, err := c.PostMessage(ctx, d.TaskID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), RequestID: key}); err != nil {
		return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
	}
	r.notified[key] = true
	return nil
}
