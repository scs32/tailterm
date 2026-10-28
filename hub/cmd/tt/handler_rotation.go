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
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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

// rotationBackoffPath is the host's back-off record after an automatic
// ceiling abort. Its prefix is not handler-rotation-, so no journal reader
// sees it.
func rotationBackoffPath(hub, task string) string {
	return filepath.Join(relayDir(), "handler-backoff-"+rotationStateKey(hub, task)+".json")
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
	// The host's evidence for a dead_primary rotation, as sent at prepare, so
	// a replayed prepare carries the same payload.
	DeathEvidence *api.HandlerDeathEvidence `json:"deathEvidence,omitempty"`
	// ResumeFailures counts the runner's resumes of this rotation that ended
	// with no successor online; it is kept here so a relay restart keeps it.
	ResumeFailures  int    `json:"resumeFailures,omitempty"`
	LastResumeError string `json:"lastResumeError,omitempty"`
	// StuckAt is when the runner found the rotation at the resume ceiling;
	// rotationStuckAbortAfter counts from it (bug wi_63793d7e551231f6).
	StuckAt time.Time `json:"stuckAt,omitempty"`
	// SpawnedAt is when the successor's launch was recorded: its first
	// completed turn is awaited until api.HandlerSuccessorReadyWait after it.
	SpawnedAt time.Time `json:"spawnedAt,omitempty"`
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
	// rotationPhaseAborted is a rotation the hub has aborted whose successor
	// cleanup has not been confirmed; it is never resumed, only finished.
	rotationPhaseAborted = "aborted"
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
	case rotationPhasePreparing, rotationPhasePrepared, rotationPhaseSpawned, rotationPhaseCommitted, rotationPhaseAborted:
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
	// probe reads, on this host only, whether a handler's process and session
	// are gone. A nil probe confirms nothing.
	probe handlerDeathProbe
	// readyWait is how long one call polls, every readyPoll, for the online
	// successor's first completed turn before it returns a wait; the runner
	// uses 0 and checks once per tick. clock is the readiness clock; nil is
	// time.Now.
	readyWait time.Duration
	readyPoll time.Duration
	clock     func() time.Time
}

func (d rotationDeps) now() time.Time {
	if d.clock != nil {
		return d.clock()
	}
	return time.Now()
}

func productionRotationDeps() rotationDeps {
	runner := productionTeamRunner()
	return rotationDeps{spawn: cmdSpawn, cleanup: runner.cleanup, host: spawn.Host, online: 2 * time.Minute, poll: 2 * time.Second,
		probe: productionDeathProbe().probe, readyPoll: 10 * time.Second}
}

// errSuccessorNotReady is a wait, not a failure: the successor is online but
// has not completed a turn yet, and api.HandlerSuccessorReadyWait has not
// passed since its launch. The rotation stays prepared and the old handler
// keeps every obligation.
var errSuccessorNotReady = errors.New("the successor has not completed a turn yet")

// successorMayBeReady reads the successor's current activity without writing.
// A completed turn with a model reply is ready; any output at all in this run
// means a reply was made, so the hub, which reads every report of the run,
// decides at commit.
func successorMayBeReady(a api.Agent) bool {
	return a.Activity != nil && (api.HandlerSuccessorReady(*a.Activity) || a.Activity.Tokens.Output > 0)
}

// A death probe answers gone, alive or unknown. Only gone carries evidence,
// and only gone lets the runner ask the hub for a dead primary rotation.
const (
	handlerProbeGone    = "gone"
	handlerProbeAlive   = "alive"
	handlerProbeUnknown = "unknown"
)

type handlerDeathProbe func(ctx context.Context, hub string, a api.Agent) (state, detail string, evidence *api.HandlerDeathEvidence)

// deathProbe confirms a handler gone from two facts read on its own host: its
// recorded tmux session is absent from a readable listing, and its runtime
// process is absent by PID and start identity. When the receipt also records
// a pane process, as the wrapper does, that must be absent too. Any fact it
// cannot read is unknown, never gone. It signals nothing.
//
// A handler whose wrapper reported exited (wi_1d987e7f296b6a2e) keeps its
// session, because the wrapper leaves a login shell in the pane. For that
// roster status only, a present session is read further: see idleShell. A
// lost exit report leaves the roster status unchanged and the session
// present, so the probe answers alive until the wrapper's retry of that
// report (exitReporter) reaches the hub (wi_b01a7afa10979dd0).
type deathProbe struct {
	receipt  func(hub, agent, run string) (runtimeProcessReceipt, error)
	sessions func(context.Context) ([]ownedSession, error)
	pid      runtimePIDProbe
	// panes lists the pane process IDs of one session by its tmux ID, and
	// processes maps every process ID on the host to its parent's, start
	// identity and executable. Only the exited case reads them; a nil reader
	// confirms nothing.
	panes     func(ctx context.Context, sessionID string) ([]int, error)
	processes func(context.Context) (map[int]discoveredProcess, error)
	socket    func() string
	host      func() string
	now       func() time.Time
}

func productionDeathProbe() deathProbe {
	return deathProbe{
		receipt: func(hub, agent, run string) (runtimeProcessReceipt, error) {
			var r runtimeProcessReceipt
			data, err := os.ReadFile(runtimeProcessPath(hub, agent, run))
			if err != nil {
				return r, err
			}
			return r, json.Unmarshal(data, &r)
		},
		sessions: localSessions, pid: nativeRuntimePIDProbe, panes: sessionPanePIDs, processes: processTable,
		socket: func() string { return os.Getenv("TT_TMUX_SOCKET") }, host: spawn.Host, now: time.Now,
	}
}

func (p deathProbe) probe(ctx context.Context, hub string, a api.Agent) (string, string, *api.HandlerDeathEvidence) {
	hub = strings.TrimRight(hub, "/")
	receipt, err := p.receipt(hub, a.ID, a.RunID)
	if err != nil {
		return handlerProbeUnknown, "the handler run's process receipt is unreadable on this host", nil
	}
	if strings.TrimRight(receipt.Hub, "/") != hub || receipt.Task != a.TaskID || receipt.Agent != a.ID || receipt.Run != a.RunID || a.RunID == "" {
		return handlerProbeUnknown, "the process receipt names another hub, project, agent or run", nil
	}
	if receipt.PID < 1 || receipt.Started == "" {
		return handlerProbeUnknown, "the process receipt has no runtime process ID and start identity", nil
	}
	if receipt.Socket != p.socket() {
		return handlerProbeUnknown, "the handler's tmux socket is not the one this runner reads", nil
	}
	// With no session name there is nothing to look for in the listing, so
	// its absence cannot be confirmed.
	session := receipt.Session
	if session == "" {
		session = a.Session
	}
	if session == "" {
		return handlerProbeUnknown, "neither the process receipt nor the roster names the handler's tmux session", nil
	}
	// An exit the wrapper posted must also be in its own receipt on this
	// host; an exited status alone is never enough.
	exited := a.Status == api.AgentExited
	if exited && receipt.ExitedAt.IsZero() {
		return handlerProbeUnknown, "the roster says the handler exited but its process receipt records no exit", nil
	}
	sessions, err := p.sessions(ctx)
	if err != nil {
		return handlerProbeUnknown, "the tmux session listing is unreadable", nil
	}
	var present []ownedSession
	for _, s := range sessions {
		if s.Agent == a.ID || (receipt.Session != "" && s.Name == receipt.Session) || (a.Session != "" && s.Name == a.Session) {
			if !exited {
				return handlerProbeAlive, "tmux session " + s.Name + " is present", nil
			}
			present = append(present, s)
		}
	}
	if len(present) > 0 {
		return p.idleShell(ctx, hub, a, receipt, session, present)
	}
	if alive, err := p.pid(receipt.PID, receipt.Started); err != nil {
		return handlerProbeUnknown, "the runtime process could not be checked: " + err.Error(), nil
	} else if alive {
		return handlerProbeAlive, fmt.Sprintf("runtime process %d is running", receipt.PID), nil
	}
	if receipt.PanePID > 0 {
		if receipt.PaneStarted == "" {
			return handlerProbeUnknown, "the pane process has no start identity", nil
		}
		if alive, err := p.pid(receipt.PanePID, receipt.PaneStarted); err != nil {
			return handlerProbeUnknown, "the pane process could not be checked: " + err.Error(), nil
		} else if alive {
			return handlerProbeAlive, fmt.Sprintf("pane process %d is running", receipt.PanePID), nil
		}
	}
	ev := &api.HandlerDeathEvidence{Host: p.host(), AgentID: a.ID, RunID: a.RunID, ObservedAt: p.now().UTC(), SessionName: session, SessionID: receipt.SessionID,
		SessionCreated: receipt.SessionCreated, SessionState: api.HandlerDeathStateGone, PID: receipt.PID, PanePID: receipt.PanePID, ProcessStarted: receipt.Started,
		ProcessState: api.HandlerDeathStateGone}
	if !receipt.ExitedAt.IsZero() {
		exited := receipt.ExitedAt.UTC()
		ev.ExitedAt = &exited
	}
	return handlerProbeGone, "the session and the runtime process are absent", ev
}

// idleShell reads the session an exited handler left behind. It answers gone
// only when the runtime process is absent, the one session is this exact
// run's, it has one pane, that pane's process is the one the receipt records,
// and it has exactly one child with no children of its own: the wrapper's
// login shell, started after the recorded exit and running the shell the
// receipt names. Anything running under that shell answers alive, whatever
// its name, and so does a child running another executable, such as a
// runtime started with exec in the shell: exec keeps the shell's PID and
// start identity but not its executable. So a runtime rerun by hand is never
// replaced. Every unreadable, unrecorded or unexpected fact is unknown.
func (p deathProbe) idleShell(ctx context.Context, hub string, a api.Agent, receipt runtimeProcessReceipt, session string, present []ownedSession) (string, string, *api.HandlerDeathEvidence) {
	if alive, err := p.pid(receipt.PID, receipt.Started); err != nil {
		return handlerProbeUnknown, "the runtime process could not be checked: " + err.Error(), nil
	} else if alive {
		return handlerProbeAlive, fmt.Sprintf("runtime process %d is running", receipt.PID), nil
	}
	if len(present) != 1 {
		return handlerProbeUnknown, fmt.Sprintf("%d tmux sessions name the handler", len(present)), nil
	}
	s := present[0]
	if !s.valid() || strings.TrimRight(s.Hub, "/") != hub || s.Task != a.TaskID || s.Agent != a.ID || s.Run != a.RunID || s.Name != session ||
		(receipt.SessionID != "" && s.ID != receipt.SessionID) || (receipt.SessionCreated != "" && s.Created != receipt.SessionCreated) {
		return handlerProbeUnknown, "tmux session " + s.Name + " is not identified as the exited run's own session", nil
	}
	// A run started before the wrapper recorded these has none, and its pane
	// cannot be told from a runtime that replaced the shell.
	if receipt.PanePID < 1 || receipt.PaneStarted == "" || receipt.Shell == "" {
		return handlerProbeUnknown, "the process receipt records no pane process or login shell", nil
	}
	if p.panes == nil || p.processes == nil {
		return handlerProbeUnknown, "this host cannot read the session's pane and process table", nil
	}
	panes, err := p.panes(ctx, s.ID)
	if err != nil {
		return handlerProbeUnknown, "the session's panes are unreadable", nil
	}
	if len(panes) != 1 || panes[0] < 1 {
		return handlerProbeUnknown, fmt.Sprintf("the session has %d panes, not one", len(panes)), nil
	}
	if panes[0] != receipt.PanePID {
		return handlerProbeUnknown, fmt.Sprintf("pane process %d is not the recorded pane process %d", panes[0], receipt.PanePID), nil
	}
	processes, err := p.processes(ctx)
	if err != nil {
		return handlerProbeUnknown, "the process table is unreadable", nil
	}
	if state, detail := idleShellTree(processes, receipt); state != handlerProbeGone {
		return state, detail, nil
	}
	exitedAt := receipt.ExitedAt.UTC()
	return handlerProbeGone, "the runtime process is absent and the session holds only the wrapper and its idle shell", &api.HandlerDeathEvidence{Host: p.host(), AgentID: a.ID, RunID: a.RunID,
		ObservedAt: p.now().UTC(), SessionName: s.Name, SessionID: s.ID, SessionCreated: s.Created, SessionState: api.HandlerDeathStateIdleShell, PID: receipt.PID,
		ProcessStarted: receipt.Started, ProcessState: api.HandlerDeathStateGone, ExitedAt: &exitedAt, PaneRootPID: panes[0]}
}

// idleShellTree checks the tree below the receipt's pane process in a table
// of processes: the pane process with the recorded start identity, exactly
// one child, that child childless, running the recorded shell (ps may print
// it as a path or with a login shell's leading dash) and started no earlier
// than the recorded exit, to the second ps reports.
func idleShellTree(processes map[int]discoveredProcess, receipt runtimeProcessReceipt) (string, string) {
	root, ok := processes[receipt.PanePID]
	if !ok {
		return handlerProbeUnknown, fmt.Sprintf("pane process %d is not in the process table", receipt.PanePID)
	}
	if root.started != receipt.PaneStarted {
		return handlerProbeUnknown, fmt.Sprintf("pane process %d is not the recorded pane process: its start identity differs", root.pid)
	}
	children := func(of int) []int {
		var out []int
		for pid, p := range processes {
			if p.parent == of && pid != of {
				out = append(out, pid)
			}
		}
		sort.Ints(out)
		return out
	}
	shells := children(root.pid)
	if len(shells) != 1 {
		return handlerProbeUnknown, fmt.Sprintf("pane process %d has %d child processes, not one shell", root.pid, len(shells))
	}
	if below := children(shells[0]); len(below) > 0 {
		return handlerProbeAlive, fmt.Sprintf("process %d runs under the pane's shell", below[0])
	}
	shell := processes[shells[0]]
	if filepath.Base(strings.TrimPrefix(shell.executable, "-")) != filepath.Base(receipt.Shell) {
		return handlerProbeAlive, fmt.Sprintf("process %d in the pane is not the wrapper's login shell", shell.pid)
	}
	started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", shell.started, time.Local)
	if err != nil {
		return handlerProbeUnknown, fmt.Sprintf("the start of process %d in the pane is unreadable", shell.pid)
	}
	if started.Before(receipt.ExitedAt.Truncate(time.Second)) {
		return handlerProbeUnknown, fmt.Sprintf("process %d in the pane started before the recorded exit", shell.pid)
	}
	return handlerProbeGone, ""
}

// sessionPanePIDs lists the pane process IDs of one tmux session, all windows.
func sessionPanePIDs(ctx context.Context, sessionID string) ([]int, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, errors.New("invalid tmux session ID")
	}
	raw, err := startupTmux(ctx, "list-panes", "-s", "-t", sessionID, "-F", "#{pane_pid}")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid < 1 {
			return nil, errors.New("unreadable pane process ID")
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

const maxProcessTable = 65536

// processTable reads every process ID with its parent's, start identity and
// executable. One unparsable or repeated row makes the whole table unreadable.
func processTable(ctx context.Context) (map[int]discoveredProcess, error) {
	raw, err := exec.CommandContext(ctx, "ps", "-ax", "-o", "pid=,ppid=,lstart=,comm=").Output()
	if err != nil {
		return nil, err
	}
	return parseProcessTable(string(raw))
}

func parseProcessTable(raw string) (map[int]discoveredProcess, error) {
	processes := map[int]discoveredProcess{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		row := runtimeProcessRow.FindStringSubmatch(line)
		if row == nil {
			return nil, errors.New("unreadable process table row")
		}
		pid, err1 := strconv.Atoi(row[1])
		parent, err2 := strconv.Atoi(row[2])
		if err1 != nil || err2 != nil || pid < 1 || parent < 0 {
			return nil, errors.New("unreadable process table row")
		}
		if _, repeated := processes[pid]; repeated || len(processes) >= maxProcessTable {
			return nil, errors.New("the process table repeats a process or is too large")
		}
		processes[pid] = discoveredProcess{pid, parent, strings.TrimSpace(row[3]), row[4]}
	}
	return processes, nil
}

// rotationRefusal reports a hub refusal that changed nothing.
func rotationRefusal(err error) (string, bool) {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		switch httpErr.Code {
		case api.HandlerRotationRefusedLiveLease, api.HandlerRotationRefusedWorking, api.HandlerRotationRefusedPendingTool, api.HandlerRotationRefusedOpen,
			api.HandlerRotationRefusedPaused, api.HandlerRotationRefusedNotPrimary, api.HandlerRotationRefusedSuccessor, api.HandlerRotationRefusedNameTaken,
			api.HandlerRotationRefusedAgentCaller, api.HandlerRotationRefusedStaleHandler,
			api.HandlerRotationRefusedDeathUnconfirmed, api.HandlerRotationRefusedNotSilent,
			api.HandlerRotationRefusedSuccessorNotReady, api.HandlerRotationRefusedNotBlocked, api.HandlerRotationRefusedNoAgentSlot:
			return httpErr.Code, true
		}
	}
	return "", false
}

func rotationBusy(err error) bool {
	code, ok := rotationRefusal(err)
	return ok && (code == api.HandlerRotationRefusedLiveLease || code == api.HandlerRotationRefusedWorking || code == api.HandlerRotationRefusedPendingTool)
}

// noAgentSlot reports the hub's refusal of a prepare whose successor would
// not fit under the project's agent cap. Nothing was prepared.
func noAgentSlot(err error) bool {
	code, ok := rotationRefusal(err)
	return ok && code == api.HandlerRotationRefusedNoAgentSlot
}

// rotationWait reports an outcome the runner waits out without counting or
// logging it: a busy handler, a successor that has not completed a turn yet,
// a primary that is no longer provider blocked when the hub checks, or a
// project with no free agent slot for the successor.
func rotationWait(err error) bool {
	code, _ := rotationRefusal(err)
	return rotationBusy(err) || errors.Is(err, errSuccessorNotReady) || code == api.HandlerRotationRefusedNotBlocked || noAgentSlot(err)
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

// exitedHandlerNamed is the exited database handler the host's death evidence
// names by agent and exact run, if the roster has one.
func exitedHandlerNamed(d api.TaskDetail, evidence *api.HandlerDeathEvidence) (api.Agent, bool) {
	if evidence == nil {
		return api.Agent{}, false
	}
	for _, a := range d.Agents {
		if a.Role == api.AgentRoleDatabaseHandler && a.Status == api.AgentExited && a.ID == evidence.AgentID && a.RunID == evidence.RunID && a.RunID != "" {
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
	return runHandlerRotation(ctx, d, e, c, task, reason, trigger, spec, auth, nil, nil)
}

// prepareRefusedError marks the hub's refusal of a prepare, which changed
// nothing, from a refusal of a later step of the same call.
type prepareRefusedError struct{ err error }

func (e prepareRefusedError) Error() string { return e.err.Error() }
func (e prepareRefusedError) Unwrap() error { return e.err }

// staleDueEntry reports the hub's refusal of a prepare whose pinned handler
// run or revision is no longer the project's primary: the due entry the
// runner read is stale, and its next due request reads the change. A commit
// refused for the same reason is not one: it is reported.
func staleDueEntry(err error) bool {
	code, ok := rotationRefusal(err)
	return ok && errors.As(err, &prepareRefusedError{}) && (code == api.HandlerRotationRefusedStaleHandler || code == api.HandlerRotationRefusedNotPrimary)
}

// journalAuthorizationConflict refuses an owner authorization that differs
// from the one this host's rotation journal records: a resume replays the
// journal's authorization, so the flags would be silently ignored. An aborted
// journal is never resumed: it is finished and a fresh rotation starts with
// the flags given.
func journalAuthorizationConflict(j *handlerRotationJournal, auth rotationAuthorization, task string) error {
	if j == nil || j.Phase == rotationPhaseAborted || !auth.set() || (j.AuthorizedBy == auth.by && j.AuthorizationReason == auth.reason) {
		return nil
	}
	recorded := "no authorization"
	if j.AuthorizedBy != "" {
		recorded = fmt.Sprintf("the authorization of %s", j.AuthorizedBy)
	}
	return fmt.Errorf("this host's rotation journal for the project records %s, not the --authorized-by and --authorization-reason given; resume it with the same authorization, or abort it with tt handler rotate --abort --task %s and rotate again", recorded, task)
}

// deathUnconfirmedRefusal reports a hub refusal that says a dead primary is
// not, or no longer, confirmed dead and silent.
func deathUnconfirmedRefusal(err error) bool {
	code, ok := rotationRefusal(err)
	return ok && (code == api.HandlerRotationRefusedDeathUnconfirmed || code == api.HandlerRotationRefusedNotSilent)
}

// runHandlerRotation is rotateHandler with the host's death evidence, which
// only the runner's dead primary branch passes for a new rotation, and the
// due entry the runner acts on. A new rotation with a due entry prepares
// against that entry's exact agent, run and handler revision, so the hub
// refuses it if the primary changed since the due list. A resumed rotation
// uses the identity and evidence in its journal.
func runHandlerRotation(ctx context.Context, d rotationDeps, e env, c *api.Client, task, reason, trigger string, spec handlerSpec, auth rotationAuthorization, evidence *api.HandlerDeathEvidence, due *api.HandlerRotationDue) (api.HandlerRotation, error) {
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
	// An aborted rotation's successor is cleaned up before a fresh one starts.
	if j != nil && j.Phase == rotationPhaseAborted {
		if err = finishAbortedRotation(ctx, d, e, c, task, path, j); err != nil {
			return zero, err
		}
		j = nil
	}
	if err = journalAuthorizationConflict(j, auth, task); err != nil {
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
		rotations, err := c.ListHandlerRotations(ctx, task)
		if err != nil {
			return zero, err
		}
		for _, r := range rotations {
			if r.State == api.HandlerRotationPrepared {
				return zero, fmt.Errorf("handler rotation %s is prepared without this host's journal; run tt handler rotate --abort --task %s", r.ID, task)
			}
		}
		var old api.Agent
		var revision int64
		if due != nil {
			// The runner's due entry, exactly: the hub refuses the prepare as
			// stale or not the primary if it changed since the due list.
			old, revision = due.Agent, due.HandlerRevision
		} else {
			detail, err := c.GetTask(ctx, task)
			if err != nil {
				return zero, err
			}
			var ok bool
			old, ok = currentPrimaryHandler(detail)
			// Only a dead primary rotation may name a handler whose wrapper
			// reported exited; the hub decides whether it is the primary.
			if exited, found := exitedHandlerNamed(detail, evidence); found {
				old, ok = exited, true
			}
			if !ok {
				return zero, errors.New("the project has no open database handler to rotate")
			}
			revision = detail.Task.HandlerRevision
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
		j = &handlerRotationJournal{Version: 1, Hub: hub, Task: task, PrepareRequestID: newRotationKey("rotation-prepare"), HandlerRevision: revision,
			OldAgentID: old.ID, OldRunID: old.RunID, SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(old.Name, revision),
			Reason: reason, Trigger: trigger, AuthorizedBy: auth.by, AuthorizationReason: auth.reason, DeathEvidence: evidence}
		if err = save(rotationPhasePreparing); err != nil {
			return zero, err
		}
	}
	if j.Phase == rotationPhasePreparing {
		r, err := c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: j.PrepareRequestID,
			ExpectedHandlerRevision: j.HandlerRevision, OldAgentID: j.OldAgentID, OldRunID: j.OldRunID, SuccessorAgentID: j.SuccessorAgentID,
			SuccessorName: j.SuccessorName, Reason: j.Reason, Trigger: j.Trigger, AuthorizedBy: j.AuthorizedBy, AuthorizationReason: j.AuthorizationReason,
			DeathEvidence: j.DeathEvidence})
		if _, refused := rotationRefusal(err); refused {
			// A refused prepare changed nothing; the next attempt starts fresh.
			if removeErr := os.Remove(path); removeErr != nil {
				return zero, removeErr
			}
			return zero, prepareRefusedError{err}
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
			return zero, successorStartError{fmt.Errorf("successor launch unconfirmed; rerun to resume or abort with tt handler rotate --abort: %w", err)}
		}
		j.SpawnedAt = d.now().UTC()
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
				return zero, successorStartError{fmt.Errorf("successor %s did not come online within %s; the rotation stays prepared: rerun to resume or abort with tt handler rotate --abort", j.SuccessorName, d.online)}
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(d.poll):
			}
		}
		// Online is not ready: the successor's exact run must complete a turn
		// with a model reply before it takes the obligations (bug
		// wi_ce7310979bc45479). Until then, and for at most
		// api.HandlerSuccessorReadyWait from its launch, this is a wait.
		if j.SpawnedAt.IsZero() {
			// Journaled before readiness existed: the bound starts now.
			j.SpawnedAt = d.now().UTC()
			if err := writePrivateJSON(path, *j); err != nil {
				return zero, err
			}
		}
		readyBy := j.SpawnedAt.Add(api.HandlerSuccessorReadyWait)
		notReady := func() error {
			if !d.now().Before(readyBy) {
				return successorStartError{fmt.Errorf("successor %s did not complete a turn within %s of its launch; the rotation stays prepared and the old handler keeps its obligations: rerun to resume or abort with tt handler rotate --abort", j.SuccessorName, api.HandlerSuccessorReadyWait)}
			}
			return fmt.Errorf("%w: successor %s, waiting until %s", errSuccessorNotReady, j.SuccessorName, readyBy.UTC().Format(time.RFC3339))
		}
		pollUntil := d.now().Add(d.readyWait)
		for {
			a, err := c.GetAgent(ctx, task, j.SuccessorAgentID)
			if err != nil {
				return zero, err
			}
			if successorMayBeReady(a) {
				break
			}
			if !d.now().Before(readyBy) || !d.now().Before(pollUntil) {
				return zero, notReady()
			}
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(d.readyPoll):
			}
		}
		commit := api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: "rotation-commit-" + j.RotationID, RotationID: j.RotationID}
		if j.Reason == api.HandlerRotationReasonDeadPrimary {
			// The successor took time to start: look again before the commit,
			// and leave the old handler primary on any doubt.
			old, err := c.GetAgent(ctx, task, j.OldAgentID)
			if err != nil {
				return zero, err
			}
			state, detail := handlerProbeUnknown, "this host has no death probe"
			if d.probe != nil && old.RunID == j.OldRunID {
				state, detail, commit.DeathEvidence = d.probe(ctx, hub, old)
			}
			if state != handlerProbeGone || commit.DeathEvidence == nil {
				if abortErr := abortJournaledRotation(ctx, d, e, c, task, path, j); abortErr != nil {
					return zero, fmt.Errorf("the second death probe of %s says %s (%s); aborting the rotation failed and will retry: %w", old.Name, state, detail, abortErr)
				}
				return zero, fmt.Errorf("%w: the second probe of %s says %s (%s); the rotation was aborted and it stays primary", errDeathNotConfirmed, old.Name, state, detail)
			}
		}
		if _, err := c.HandlerRotationAction(ctx, task, commit); err != nil {
			if code, _ := rotationRefusal(err); code == api.HandlerRotationRefusedSuccessorNotReady {
				return zero, notReady()
			}
			if j.Reason == api.HandlerRotationReasonDeadPrimary && deathUnconfirmedRefusal(err) {
				if abortErr := abortJournaledRotation(ctx, d, e, c, task, path, j); abortErr != nil {
					return zero, fmt.Errorf("the hub refused the commit (%v); aborting the rotation failed and will retry: %w", err, abortErr)
				}
				return zero, fmt.Errorf("%w: %v; the rotation was aborted and the old handler stays primary", errDeathNotConfirmed, err)
			}
			return zero, err
		}
		if err = save(rotationPhaseCommitted); err != nil {
			return zero, err
		}
		// A committed rotation ends any back-off for the old primary.
		if err = os.Remove(rotationBackoffPath(hub, task)); err != nil && !errors.Is(err, os.ErrNotExist) {
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

// successorStartError marks a rotation left prepared because its successor
// could not be launched or did not come online. Only these count toward the
// runner's resume ceiling.
type successorStartError struct{ err error }

func (e successorStartError) Error() string { return e.err.Error() }
func (e successorStartError) Unwrap() error { return e.err }

// noticeSafeError makes an error text safe for a Board envelope: a launcher's
// error can carry terminal escapes, and the hub refuses control characters.
// Each one, and each invalid byte, becomes a space; the text is cut to 400
// runes.
func noticeSafeError(text string) string {
	runes := []rune(strings.ToValidUTF8(text, " "))
	for i, c := range runes {
		if unicode.IsControl(c) {
			runes[i] = ' '
		}
	}
	if len(runes) > 400 {
		runes = runes[:400]
	}
	return strings.TrimSpace(string(runes))
}

// recordResumeFailure adds one failed resume to the journal the runner just
// resumed and returns it as saved, with the time it went stuck once the
// count reaches the ceiling. It returns nil when that journal is gone or now
// records a different rotation.
func recordResumeFailure(ctx context.Context, hub, task string, resumed *handlerRotationJournal, cause error, now time.Time) (*handlerRotationJournal, error) {
	path := handlerRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, task)
	if err != nil || j == nil || j.PrepareRequestID != resumed.PrepareRequestID {
		return nil, err
	}
	j.ResumeFailures++
	j.LastResumeError = noticeSafeError(cause.Error())
	if j.ResumeFailures >= rotationResumeCeiling && j.StuckAt.IsZero() {
		j.StuckAt = now.UTC()
	}
	return j, writePrivateJSON(path, *j)
}

// markStuck saves the time a stuck rotation was first seen in a journal that
// has none, as one saved before StuckAt existed, and returns the journal as
// saved. It returns nil when that journal is gone or now records a different
// rotation.
func markStuck(ctx context.Context, hub, task string, stuck *handlerRotationJournal, now time.Time) (*handlerRotationJournal, error) {
	path := handlerRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, task)
	if err != nil || j == nil || j.PrepareRequestID != stuck.PrepareRequestID {
		return nil, err
	}
	if j.StuckAt.IsZero() {
		j.StuckAt = now.UTC()
		if err = writePrivateJSON(path, *j); err != nil {
			return nil, err
		}
	}
	return j, nil
}

// oldHandlerRecovered reports whether the due entry shows the stuck
// rotation's exact old run completing a turn after the rotation went stuck.
// Only a provider_blocked rotation counts it: any other old handler never
// stopped, so its next ordinary turn would abort a rotation that is still due
// and the runner would launch another at once.
func oldHandlerRecovered(d api.HandlerRotationDue, j *handlerRotationJournal) bool {
	a := d.Agent
	return j.Reason == api.HandlerRotationReasonProviderBlocked && a.ID == j.OldAgentID && a.RunID == j.OldRunID &&
		a.Activity != nil && api.HandlerSuccessorReady(*a.Activity) && a.Activity.LastEventAt.After(j.StuckAt)
}

// abortStuckRotation aborts this host's stuck rotation, as tt handler rotate
// --abort does, once its old handler has recovered or rotationStuckAbortAfter
// has passed since it went stuck. The caller has settled the stuck notice to
// every recipient first. Before either trigger, or when the journal changed
// since the caller read it, it does nothing; a failed abort keeps the journal,
// so the next tick tries again.
func (r *rotationRunner) abortStuckRotation(ctx context.Context, e env, c *api.Client, d api.HandlerRotationDue, stuck *handlerRotationJournal) error {
	if !oldHandlerRecovered(d, stuck) && r.deps.now().Before(stuck.StuckAt.Add(rotationStuckAbortAfter)) {
		return nil
	}
	hub := strings.TrimRight(c.Base, "/")
	path := handlerRotationJournalPath(hub, d.TaskID)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, d.TaskID)
	if err != nil || j == nil || j.PrepareRequestID != stuck.PrepareRequestID || j.RotationID == "" ||
		(j.Phase != rotationPhasePrepared && j.Phase != rotationPhaseSpawned) {
		return err
	}
	if !oldHandlerRecovered(d, j) {
		if err = recordRotationBackoff(hub, d.TaskID, j, r.deps.now()); err != nil {
			return err
		}
	}
	return abortJournaledRotation(ctx, r.deps, e, c, d.TaskID, path, j)
}

// rotationBackoff records the runner's automatic abort of a stuck rotation at
// its ceiling: until Until, the runner prepares no fresh rotation for the
// same old agent, run and reason. Aborts counts the ceiling aborts of that
// run and reason; each one doubles the interval.
type rotationBackoff struct {
	Version    int       `json:"version"`
	Hub        string    `json:"hub"`
	Task       string    `json:"task"`
	OldAgentID string    `json:"oldAgentId"`
	OldRunID   string    `json:"oldRunId"`
	Reason     string    `json:"reason"`
	RotationID string    `json:"rotationId"`
	Aborts     int       `json:"aborts"`
	Until      time.Time `json:"until"`
}

// rotationBackoffMax caps the back-off interval, which starts at
// rotationStuckAbortAfter.
const rotationBackoffMax = 24 * time.Hour

func loadRotationBackoff(hub, task string) *rotationBackoff {
	data, err := os.ReadFile(rotationBackoffPath(hub, task))
	if err != nil {
		return nil
	}
	var b rotationBackoff
	if json.Unmarshal(data, &b) != nil || b.Version != 1 || b.Hub != strings.TrimRight(hub, "/") || b.Task != task {
		return nil
	}
	return &b
}

// recordRotationBackoff saves the back-off for a ceiling abort of j. A later
// ceiling abort for the same old run and reason doubles the interval; a retry
// of the same abort counts once.
func recordRotationBackoff(hub, task string, j *handlerRotationJournal, now time.Time) error {
	b := rotationBackoff{Version: 1, Hub: strings.TrimRight(hub, "/"), Task: task, OldAgentID: j.OldAgentID, OldRunID: j.OldRunID, Reason: j.Reason, RotationID: j.RotationID, Aborts: 1}
	if prior := loadRotationBackoff(hub, task); prior != nil && prior.OldAgentID == b.OldAgentID && prior.OldRunID == b.OldRunID && prior.Reason == b.Reason {
		if prior.RotationID == b.RotationID {
			return nil
		}
		b.Aborts = prior.Aborts + 1
	}
	interval := rotationStuckAbortAfter
	for i := 1; i < b.Aborts && interval < rotationBackoffMax; i++ {
		interval *= 2
	}
	b.Until = now.UTC().Add(min(interval, rotationBackoffMax))
	return writePrivateJSON(rotationBackoffPath(hub, task), b)
}

// rotationBackedOff reports whether a fresh rotation of the due entry's exact
// run for reason waits out a back-off. Another run or reason ignores it.
func rotationBackedOff(hub string, d api.HandlerRotationDue, reason string, now time.Time) bool {
	b := loadRotationBackoff(hub, d.TaskID)
	return b != nil && b.OldAgentID == d.Agent.ID && b.OldRunID == d.Agent.RunID && b.Reason == reason && now.Before(b.Until)
}

// errDeathNotConfirmed marks a dead primary rotation the runner aborted
// because the death was not confirmed a second time.
var errDeathNotConfirmed = errors.New("dead primary not confirmed")

// abortJournaledRotation aborts the prepared rotation this host's journal
// names, saves the journal as aborted, cleans up its successor's session and
// removes the journal. A failed cleanup keeps the aborted journal, which the
// runner retries at later ticks whether or not the project is still due. The
// caller holds the journal lock.
func abortJournaledRotation(ctx context.Context, d rotationDeps, e env, c *api.Client, task, path string, j *handlerRotationJournal) error {
	if _, err := c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "rotation-abort-" + j.RotationID, RotationID: j.RotationID}); err != nil {
		return err
	}
	j.Phase = rotationPhaseAborted
	if err := writePrivateJSON(path, *j); err != nil {
		return err
	}
	return finishAbortedRotation(ctx, d, e, c, task, path, j)
}

// finishAbortedRotation cleans up the successor session of a rotation the hub
// has aborted, then removes the journal. A failed cleanup keeps the journal
// for the next attempt. The caller holds the journal lock.
func finishAbortedRotation(ctx context.Context, d rotationDeps, e env, c *api.Client, task, path string, j *handlerRotationJournal) error {
	_, err := c.GetAgent(ctx, task, j.SuccessorAgentID)
	var httpErr *api.HTTPError
	switch {
	case err == nil:
		if err = d.cleanup(ctx, e, task, j.SuccessorAgentID); err != nil {
			return fmt.Errorf("rotation aborted; successor session cleanup will retry: %w", err)
		}
	case errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound:
		// The successor never registered: there is no session to clean up.
	default:
		return fmt.Errorf("rotation aborted; successor session cleanup will retry: %w", err)
	}
	return os.Remove(path)
}

// sweepAbortedRotations finishes every aborted rotation this host journaled
// for hub, so a successor whose cleanup failed is retried even when its
// project is no longer due. It reads local files only unless one exists.
func sweepAbortedRotations(ctx context.Context, d rotationDeps, e env, c *api.Client) error {
	hub := strings.TrimRight(c.Base, "/")
	paths, err := filepath.Glob(filepath.Join(relayDir(), "handler-rotation-*.json"))
	if err != nil {
		return err
	}
	var errs []error
	for _, path := range paths {
		var saved struct {
			Hub   string `json:"hub"`
			Task  string `json:"task"`
			Phase string `json:"phase"`
		}
		if data, err := os.ReadFile(path); err != nil || json.Unmarshal(data, &saved) != nil || saved.Hub != hub || saved.Phase != rotationPhaseAborted ||
			path != handlerRotationJournalPath(hub, saved.Task) {
			continue
		}
		if err := finishJournaledAbort(ctx, d, e, c, saved.Task); err != nil {
			errs = append(errs, fmt.Errorf("handler rotation %s: %w", saved.Task, err))
		}
	}
	return errors.Join(errs...)
}

// finishJournaledAbort finishes the project's aborted rotation under the
// journal lock; a journal that is gone or no longer aborted is left alone.
func finishJournaledAbort(ctx context.Context, d rotationDeps, e env, c *api.Client, task string) error {
	hub := strings.TrimRight(c.Base, "/")
	path := handlerRotationJournalPath(hub, task)
	unlock, err := handlerLock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	j, err := loadRotationJournal(hub, task)
	if err != nil || j == nil || j.Phase != rotationPhaseAborted {
		return err
	}
	return finishAbortedRotation(ctx, d, e, c, task, path, j)
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
	if j != nil && j.Phase == rotationPhaseAborted {
		if err = finishAbortedRotation(ctx, d, e, c, task, path, j); err != nil {
			return zero, err
		}
		return c.GetHandlerRotation(ctx, task, j.RotationID)
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
		// With nothing prepared, the abort releases the seat held for the
		// current primary's successor, whatever its reason.
		detail, err := c.GetTask(ctx, task)
		if err != nil {
			return zero, err
		}
		if primary, ok := currentPrimaryHandler(detail); ok {
			r, err := releaseSuccessorHold(ctx, c, task, api.HandlerSuccessorHold{OldAgentID: primary.ID, OldRunID: primary.RunID})
			if err == nil {
				return r, nil
			}
			if !noSuccessorHold(err) {
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
	if j != nil {
		// The journal now records the abort, so a failed cleanup is retried.
		j.RotationID, j.SuccessorAgentID, j.Phase = open.ID, r.SuccessorAgentID, rotationPhaseAborted
		if err = writePrivateJSON(path, *j); err != nil {
			return r, err
		}
		return r, finishAbortedRotation(ctx, d, e, c, task, path, j)
	}
	if _, err = c.GetAgent(ctx, task, r.SuccessorAgentID); err == nil {
		if err = d.cleanup(ctx, e, task, r.SuccessorAgentID); err != nil {
			return r, fmt.Errorf("rotation aborted; successor session cleanup will retry: %w", err)
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
	// The command waits for the successor's first completed turn, up to the
	// readiness bound, on top of its own budget; the runner checks once per
	// tick instead.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute+api.HandlerSuccessorReadyWait)
	defer cancel()
	deps := productionRotationDeps()
	deps.readyWait = api.HandlerSuccessorReadyWait
	var r api.HandlerRotation
	if *abort {
		r, err = abortHandlerRotation(ctx, deps, e, c, *task)
	} else {
		// A different authorization than the journal's would be ignored by
		// the resume: refuse before a spec is saved or anything is spawned.
		if auth.set() {
			j, err := loadRotationJournal(strings.TrimRight(c.Base, "/"), *task)
			if err != nil {
				return err
			}
			if err = journalAuthorizationConflict(j, auth, *task); err != nil {
				return err
			}
		}
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
		err = ownerRotateNoSlotHint(err, *task)
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(r)
		return nil
	}
	if r.State == api.HandlerRotationAborted {
		fmt.Println(abortedRotationLine(r))
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

// ownerRotateNoSlotHint tells the owner that a rotate refused no_agent_slot
// holds the next free agent seat, and how to end the hold; any other error is
// returned as is.
func ownerRotateNoSlotHint(err error, task string) error {
	if !noAgentSlot(err) {
		return err
	}
	return fmt.Errorf("%w; the next free agent seat is held for this rotation; rerun tt handler rotate --task %s once an agent is closed, or release the seat with tt handler rotate --abort --task %s", err, task, task)
}

// abortedRotationLine reports an abort: of a prepared rotation, or, with no
// rotation ID, the release of a successor hold.
func abortedRotationLine(r api.HandlerRotation) string {
	if r.ID == "" {
		return fmt.Sprintf("Released the agent seat held for the %s rotation of %s; nothing was prepared.", r.Reason, r.OldName)
	}
	return fmt.Sprintf("Aborted handler rotation %s; %s stays primary.", r.ID, r.OldName)
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
		if len(h.Skipped) > 0 {
			fmt.Printf("Not moved, the sender re-sent them after the close: %d\n", len(h.Skipped))
			for _, v := range h.Skipped {
				fmt.Printf("  #%d not moved, re-sent as #%d (%s)  %s\n", v.OldMessageSeq, v.NewMessageSeq, v.Reason, v.Subject)
			}
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
		for _, l := range h.LiveLeases {
			if l.NewLeaseGeneration == 0 {
				continue
			}
			plan := ""
			if l.LaunchPlanRewritten {
				plan = ", launch plan rewritten"
			}
			fmt.Printf("  %s item %s (%s) moved to the successor: lease generation %d -> %d%s\n", l.EntryID, l.ItemID, l.State, l.LeaseGeneration, l.NewLeaseGeneration, plan)
		}
		if in := h.InFlight; in != nil {
			fmt.Printf("In flight when the old handler died (reported, not recovered): activity %s", in.ActivityState)
			if in.PendingTool != "" {
				fmt.Printf(", pending tool %s", in.PendingTool)
			}
			fmt.Printf(", last recorded activity %s, silence applied %d minute(s)\n", in.LastActivityAt.UTC().Format(time.RFC3339), in.SilenceMinutes)
			fmt.Printf("Work-item saves by the dead run since %s: %d listed, %d older left out\n", in.WritesSince.UTC().Format(time.RFC3339), len(in.RecentWrites), in.WritesOmitted)
			for _, w := range in.RecentWrites {
				fmt.Printf("  %s r%d at %s (%s)\n", w.ItemID, w.Revision, w.UpdatedAt.UTC().Format(time.RFC3339), strings.Join(w.ChangedFields, ", "))
			}
		}
	}
	for _, line := range rotationDeathEvidenceLines(r) {
		fmt.Println(line)
	}
	return nil
}

// rotationDeathEvidenceLines is the host evidence of a dead_primary rotation,
// one line per observation; none for any other rotation.
func rotationDeathEvidenceLines(r api.HandlerRotation) []string {
	var lines []string
	if r.Exit != nil {
		lines = append(lines, fmt.Sprintf("Wrapper exit report: status %d, reported %s (event %d)", r.Exit.Code, r.Exit.ReportedAt.UTC().Format(time.RFC3339), r.Exit.EventSeq))
	}
	for _, v := range []struct {
		label string
		ev    *api.HandlerDeathEvidence
	}{{"at prepare", r.DeathEvidence}, {"at commit", r.CommitDeathEvidence}} {
		if v.ev == nil {
			continue
		}
		line := fmt.Sprintf("Death evidence %s: host %s observed %s: tmux session %s %s, runtime process %d (started %s) %s", v.label, v.ev.Host, v.ev.ObservedAt.UTC().Format(time.RFC3339),
			v.ev.SessionName, v.ev.SessionState, v.ev.PID, v.ev.ProcessStarted, v.ev.ProcessState)
		if v.ev.PanePID > 0 {
			line += fmt.Sprintf(", pane process %d gone", v.ev.PanePID)
		}
		if v.ev.PaneRootPID > 0 {
			line += fmt.Sprintf(", pane holds only wrapper process %d and its idle shell", v.ev.PaneRootPID)
		}
		if v.ev.ExitedAt != nil {
			line += ", wrapper exit recorded " + v.ev.ExitedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, line)
	}
	return lines
}

func cmdHandlerPolicy(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New("usage: tt handler policy get|set --task ID [--revision N --enabled=BOOL --max-items N --max-total-tokens N --on-template-change=BOOL --dead-silence-minutes N] [--json]")
	}
	operation := args[0]
	fs := flag.NewFlagSet("handler policy", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "expected policy revision (required for set)")
	enabled := fs.Bool("enabled", true, "rotate when a limit is reached")
	maxItems := fs.Int64("max-items", 0, "finished leased items per handler run; 0 turns the limit off")
	maxTokens := fs.Int64("max-total-tokens", 0, "total tokens per handler run; 0 turns the limit off")
	onTemplate := fs.Bool("on-template-change", true, "rotate when the handler prompt template changes")
	deadSilence := fs.Int64("dead-silence-minutes", api.DefaultHandlerDeadSilenceMinutes, "minutes of silence before a dead, busy primary is replaced automatically; 0 turns that off")
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
		// Omitted, the hub keeps the saved silence.
		if flagPresent(args, "dead-silence-minutes") {
			if *deadSilence < 0 || *deadSilence > api.MaxHandlerDeadSilenceMinutes {
				return fmt.Errorf("--dead-silence-minutes takes 0 (off) to %d", api.MaxHandlerDeadSilenceMinutes)
			}
			req.DeadSilenceMinutes = deadSilence
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
	fmt.Printf("Handler rotation policy for %s (revision %d): enabled=%t max-items=%s max-total-tokens=%s on-template-change=%t dead-silence-minutes=%s\n",
		out.TaskID, out.Revision, out.Enabled, limit(out.MaxItems), limit(out.MaxTotalTokens), out.OnTemplateChange, limit(out.DeadSilenceMinutes))
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

// rotationResumeCeiling is how many failed resumes of one prepared rotation
// the runner makes before it stops and tells the owner helper and the handler.
const rotationResumeCeiling = 3

// rotationStuckAbortAfter is how long after it went stuck the runner aborts a
// stuck rotation whose old handler has not recovered, so a prepared rotation
// does not hold the project's queue dispatch until a person aborts it (bug
// wi_63793d7e551231f6).
const rotationStuckAbortAfter = 60 * time.Minute

const rotationStuckSubject = "A handler rotation is stuck because its successor never came online or never completed a turn"

var hostRotationRunner = &rotationRunner{deps: productionRotationDeps(), now: time.Now, interval: time.Minute, notified: map[string]bool{}}

func relayHandlerRotationTick(ctx context.Context) error {
	e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	e.loadConfig()
	if e.hub == "" {
		return nil
	}
	// Spend no hub request on a host that neither runs a database handler nor
	// keeps a trace of one: a host whose handler died must keep ticking.
	if runs, err := hostRunsHandler(ctx, e.hub); err != nil {
		return err
	} else if !runs && !hostHasHandlerTrace(e.hub) {
		return nil
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

// hostHasHandlerTrace reports whether this host has ever been set up to run a
// database handler for hub: a saved launch spec, a rotation journal, or a
// handler's session ownership receipt. It reads local files only.
func hostHasHandlerTrace(hub string) bool {
	hub = strings.TrimRight(hub, "/")
	for _, pattern := range []string{"handler-spec-*.json", "handler-rotation-*.json"} {
		paths, _ := filepath.Glob(filepath.Join(relayDir(), pattern))
		for _, path := range paths {
			var saved struct {
				Hub string `json:"hub"`
			}
			if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &saved) == nil && saved.Hub != "" && strings.TrimRight(saved.Hub, "/") == hub {
				return true
			}
		}
	}
	paths, _ := filepath.Glob(filepath.Join(relayDir(), "*.session.json"))
	for _, path := range paths {
		var s ownedSession
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &s) == nil && s.valid() && strings.TrimRight(s.Hub, "/") == hub &&
			(s.Role == api.AgentRoleDatabaseHandler || strings.HasPrefix(s.Name, "tt-handler-")) {
			return true
		}
	}
	return false
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

// staleTemplateHold reports a template successor hold for the due entry's
// run that this host no longer finds due: the limit policy or its template
// check is off, or the run's recorded template is this host's.
func staleTemplateHold(d api.HandlerRotationDue, spec *handlerSpec) bool {
	h := d.SuccessorHold
	if h == nil || h.Reason != api.HandlerRotationReasonTemplate {
		return false
	}
	return !d.Policy.Enabled || !d.Policy.OnTemplateChange || !slices.Contains(rotationDueReasons(d, spec), api.HandlerRotationReasonTemplate)
}

// noSuccessorHold reports the hub's answer to a release_hold for a run that
// holds no seat.
func noSuccessorHold(err error) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusNotFound
}

// releaseSuccessorHold ends the seat the hub holds for the hold's exact run.
// The hub keeps no receipt for it: NotFound (noSuccessorHold) means no such
// hold remains.
func releaseSuccessorHold(ctx context.Context, c *api.Client, task string, h api.HandlerSuccessorHold) (api.HandlerRotation, error) {
	return c.HandlerRotationAction(ctx, task, api.HandlerRotationRequest{Operation: api.HandlerRotationReleaseHold,
		RequestID: "rotation-release-hold-" + strings.TrimPrefix(h.OldRunID, "run_"), OldAgentID: h.OldAgentID, OldRunID: h.OldRunID})
}

func (r *rotationRunner) tick(ctx context.Context, e env, c *api.Client, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Before(r.quietTil) {
		return nil
	}
	// Aborted rotations are finished first, whether or not they are listed.
	sweepErr := sweepAbortedRotations(ctx, r.deps, e, c)
	list, err := c.HandlerRotationsDue(ctx, host, handlerTemplateDigest(""))
	if err != nil {
		return errors.Join(sweepErr, err)
	}
	if len(list.Entries) == 0 {
		r.quietTil = now.Add(rotationQuietCache)
		return sweepErr
	}
	r.quietTil = now.Add(r.interval)
	hub := strings.TrimRight(c.Base, "/")
	errs := []error{sweepErr}
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
			// The sweep already tried an aborted rotation; it is never resumed.
			if journal.Phase == rotationPhaseAborted {
				continue
			}
			// Resume this host's rotation; a busy refusal waits for the next tick.
			if spec == nil {
				errs = append(errs, fmt.Errorf("handler rotation %s: the launch spec was removed during a rotation", d.TaskID))
				continue
			}
			// A rotation whose successor failed to start at every resume up to
			// the ceiling is not resumed again. Once its stuck notice has
			// reached every recipient, it is aborted when the old handler
			// recovers or at rotationStuckAbortAfter; until then it stays
			// prepared for a person to resume or abort.
			if journal.ResumeFailures >= rotationResumeCeiling {
				if journal.StuckAt.IsZero() {
					if journal, err = markStuck(ctx, hub, d.TaskID, journal, r.deps.now()); err != nil || journal == nil {
						if err != nil {
							errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
						}
						continue
					}
				}
				if err := r.notifyStuckRotation(ctx, c, host, d, journal); err != nil {
					errs = append(errs, err)
					continue
				}
				if err := r.abortStuckRotation(ctx, e, c, d, journal); err != nil {
					errs = append(errs, fmt.Errorf("handler rotation %s: automatic abort of the stuck rotation: %w", d.TaskID, err))
				}
				continue
			}
			_, err := rotateHandler(ctx, r.deps, e, c, d.TaskID, journal.Reason, journal.Trigger, *spec, rotationAuthorization{})
			if err != nil && !rotationWait(err) && !deathUnconfirmedRefusal(err) {
				errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
			}
			if errors.As(err, &successorStartError{}) {
				saved, err := recordResumeFailure(ctx, hub, d.TaskID, journal, err, r.deps.now())
				if err != nil {
					errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
				} else if saved != nil && saved.ResumeFailures >= rotationResumeCeiling {
					if err := r.notifyStuckRotation(ctx, c, host, d, saved); err != nil {
						errs = append(errs, err)
					}
				}
			}
			continue
		}
		if d.OpenRotation != nil {
			continue // another host or process owns it
		}
		// The seat a refused template prepare holds is released once this
		// host no longer finds the template due; a manual hold is the owner's.
		if staleTemplateHold(d, spec) {
			if _, err := releaseSuccessorHold(ctx, c, d.TaskID, *d.SuccessorHold); err != nil && !noSuccessorHold(err) {
				errs = append(errs, fmt.Errorf("handler rotation %s: release the successor seat: %w", d.TaskID, err))
			}
		}
		if d.DeadCandidate {
			// A busy primary the hub no longer hears from. It is never idle, so
			// no limit applies; it is replaced only when it has been silent
			// long enough and this host confirms it gone.
			if d.SilenceMet && !rotationBackedOff(hub, d, api.HandlerRotationReasonDeadPrimary, r.deps.now()) {
				if err := r.replaceDeadPrimary(ctx, e, c, host, d, spec); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		// A primary stopped by a provider rate limit or server error while it
		// holds obligations is replaced whatever the limit policy says (bug
		// wi_ce7310979bc45479); the hub re-checks the block at prepare.
		if slices.Contains(d.DueReasons, api.HandlerRotationReasonProviderBlocked) {
			if spec == nil {
				if err := r.notifyMissingSpec(ctx, c, host, d, []string{api.HandlerRotationReasonProviderBlocked}); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if rotationBackedOff(hub, d, api.HandlerRotationReasonProviderBlocked, r.deps.now()) {
				continue
			}
			_, err := runHandlerRotation(ctx, r.deps, e, c, d.TaskID, api.HandlerRotationReasonProviderBlocked, api.HandlerRotationTriggerRunner, *spec, rotationAuthorization{}, nil, &d)
			if noAgentSlot(err) {
				err = r.notifyNoAgentSlot(ctx, c, host, d, api.HandlerRotationReasonProviderBlocked, err)
			}
			if err != nil && !rotationWait(err) && !staleDueEntry(err) {
				errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
			}
			continue
		}
		// The hub lists a project for a dead primary even with its limit
		// policy off, so the limits need the policy checked here.
		if !d.Policy.Enabled {
			continue
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
		if !d.Idle || rotationBackedOff(hub, d, reasons[0], r.deps.now()) {
			continue
		}
		_, err = runHandlerRotation(ctx, r.deps, e, c, d.TaskID, reasons[0], api.HandlerRotationTriggerRunner, *spec, rotationAuthorization{}, nil, &d)
		if noAgentSlot(err) {
			err = r.notifyNoAgentSlot(ctx, c, host, d, reasons[0], err)
		}
		if err != nil && !rotationWait(err) && !staleDueEntry(err) {
			errs = append(errs, fmt.Errorf("handler rotation %s: %w", d.TaskID, err))
		}
	}
	return errors.Join(errs...)
}

// replaceDeadPrimary probes this host and, only when the handler's session
// and process are confirmed gone, asks the hub for a dead primary rotation.
// alive or unknown sends the hub nothing.
func (r *rotationRunner) replaceDeadPrimary(ctx context.Context, e env, c *api.Client, host string, d api.HandlerRotationDue, spec *handlerSpec) error {
	if spec == nil {
		return r.notifyDeadWithoutSpec(ctx, c, host, d)
	}
	if r.deps.probe == nil {
		return nil
	}
	state, _, evidence := r.deps.probe(ctx, strings.TrimRight(c.Base, "/"), d.Agent)
	if state != handlerProbeGone || evidence == nil {
		return nil
	}
	_, err := runHandlerRotation(ctx, r.deps, e, c, d.TaskID, api.HandlerRotationReasonDeadPrimary, api.HandlerRotationTriggerRunner, *spec, rotationAuthorization{}, evidence, &d)
	if noAgentSlot(err) {
		return r.notifyNoAgentSlot(ctx, c, host, d, api.HandlerRotationReasonDeadPrimary, err)
	}
	if err != nil && !rotationBusy(err) && !deathUnconfirmedRefusal(err) && !staleDueEntry(err) {
		return fmt.Errorf("handler rotation %s: %w", d.TaskID, err)
	}
	return nil
}

// notifyDeadWithoutSpec posts one owner notice per exact run: without a
// saved launch spec this host cannot start a successor, so it does not probe.
func (r *rotationRunner) notifyDeadWithoutSpec(ctx context.Context, c *api.Client, host string, d api.HandlerRotationDue) error {
	key := "handler-dead-no-spec-" + strings.TrimPrefix(d.Agent.ID, "agt_") + "-" + strings.TrimPrefix(d.Agent.RunID, "run_")
	if r.notified[key] {
		return nil
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "An offline busy primary handler cannot be replaced because this host has no saved launch spec",
		Body: api.EnvelopeBody{Text: fmt.Sprintf("Primary database handler %s (%s / %s) is offline, still recorded as busy, and has been silent for the %d minute(s) the project's policy needs. Host %s has no saved launch spec, so it did not check whether the handler is dead and replaced nothing. Save the handler's launch settings on that host with tt handler spec --task %s -- <tt spawn launch flags>; the runner then replaces the handler once its process and session are confirmed gone. To turn the automatic replacement off: tt handler policy set --task %s --revision %d --dead-silence-minutes 0.",
			d.Agent.Name, d.Agent.ID, d.Agent.RunID, d.Policy.DeadSilenceMinutes, host, d.TaskID, d.TaskID, d.Policy.Revision)}}
	if _, err := c.PostMessage(ctx, d.TaskID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), RequestID: key}); err != nil {
		return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
	}
	r.notified[key] = true
	return nil
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

// rotationNoSlotSubject is the subject of the runner's notice that a due
// rotation waits for a free agent slot.
const rotationNoSlotSubject = "A handler rotation waits because the project has no free agent slot"

// rotationNoSlotListed caps the done or retired agents a no-slot notice names.
const rotationNoSlotListed = 10

// notifyNoAgentSlot posts one board notice per exact old run and reason that
// the hub refused the rotation's prepare for want of an agent slot. It names
// the done and retired ordinary agents an owner could close; it closes
// nothing. A failed post is tried again on a later tick.
func (r *rotationRunner) notifyNoAgentSlot(ctx context.Context, c *api.Client, host string, d api.HandlerRotationDue, reason string, refusal error) error {
	key := "handler-rotation-no-slot-" + strings.TrimPrefix(d.Agent.ID, "agt_") + "-" + strings.TrimPrefix(d.Agent.RunID, "run_") + "-" + reason
	if r.notified[key] {
		return nil
	}
	detail, err := c.GetTask(ctx, d.TaskID)
	if err != nil {
		return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
	}
	// Ordinary team members only: a persistent role or the owner helper is
	// never offered for closing.
	var idle []string
	for _, a := range detail.Agents {
		if a.Role == "" && (a.Status == api.AgentDone || a.Status == api.AgentRetired) {
			idle = append(idle, a.Name+" ("+a.Status+")")
		}
	}
	slices.Sort(idle)
	listed := "none"
	if len(idle) > rotationNoSlotListed {
		listed = strings.Join(idle[:rotationNoSlotListed], ", ") + fmt.Sprintf(" and %d more", len(idle)-rotationNoSlotListed)
	} else if len(idle) > 0 {
		listed = strings.Join(idle, ", ")
	}
	var httpErr *api.HTTPError
	why := refusal.Error()
	if errors.As(refusal, &httpErr) && httpErr.Msg != "" {
		why = httpErr.Msg
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: rotationNoSlotSubject,
		Refs: map[string]string{"host": host, "project": d.TaskID, "handler": d.Agent.ID},
		Body: api.EnvelopeBody{Text: fmt.Sprintf("The %s rotation of database handler %s (%s / %s) in project %s (%s) on host %s cannot be prepared. Hub: %s. Nothing was prepared, so queue dispatch is unaffected and the old handler stays the primary with its leases and obligations. Queue admission and automatic handler provisioning hold the next free agent seat for the successor. The runner tries again every tick and rotates once a slot is free. Done or retired ordinary agents an owner could close: %s. This notice closes nothing.",
			reason, d.Agent.Name, d.Agent.ID, d.Agent.RunID, detail.Task.Name, d.TaskID, host, noticeSafeError(why), listed)}}
	_, err = c.PostMessage(ctx, d.TaskID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), RequestID: key})
	// The identity is this run's and reason's notice, so a conflict means an
	// earlier relay already sent it while the counts or the roster differed.
	if err != nil && !requestIdentityConflict(err) {
		return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
	}
	r.notified[key] = true
	return nil
}

// notifyStuckRotation tells the project's owner helper and the handler being
// replaced, once each per rotation, that the runner stopped resuming it. Each
// request identity names the rotation and its recipient, and the runner
// remembers each identity it has settled, so every tick reads the roster but
// posts only to a recipient who was not there before, such as an owner helper
// that registered after the Board copy. A restarted relay posts no second copy
// to anyone. A failed post is tried again on a later tick.
func (r *rotationRunner) notifyStuckRotation(ctx context.Context, c *api.Client, host string, d api.HandlerRotationDue, j *handlerRotationJournal) error {
	key := "handler-rotation-stuck-" + strings.TrimPrefix(j.PrepareRequestID, "rotation-prepare-")
	detail, err := c.GetTask(ctx, d.TaskID)
	if err != nil {
		return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
	}
	// The owner helper first; with none registered the notice goes to the Board.
	recipients := []api.Agent{{}}
	for _, a := range detail.Agents {
		if a.Role == api.AgentRoleOwnerHelper && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired {
			recipients[0] = a
		}
	}
	old := j.OldAgentID
	for _, a := range detail.Agents {
		if a.ID == j.OldAgentID && a.Status != api.AgentClosed {
			recipients = append(recipients, a)
			old = a.Name + " (" + a.ID + ")"
		}
	}
	text := fmt.Sprintf("The rotation of database handler %s in project %s (%s) on host %s is stuck: its successor %s (%s) was not online, or did not complete a turn, after %d failed resume attempts by the runner. Last error: %s. The runner has stopped resuming it. The rotation stays prepared, no other successor is launched, and the old handler stays the primary with its leases and obligations. %s %s",
		old, detail.Task.Name, d.TaskID, host, j.SuccessorName, j.SuccessorAgentID, j.ResumeFailures, noticeSafeError(j.LastResumeError), stuckRotationAutoAbort(j.Reason), stuckRotationRecovery(j.Phase, d.TaskID))
	for _, to := range recipients {
		requestID := key + "-board"
		if to.ID != "" {
			requestID = key + "-" + strings.TrimPrefix(to.ID, "agt_")
		}
		if r.notified[requestID] {
			continue
		}
		env := api.Envelope{Kind: api.EnvelopeKindNotice, To: to.Name, Subject: rotationStuckSubject,
			Refs: map[string]string{"host": host, "project": d.TaskID, "successor": j.SuccessorAgentID}, Body: api.EnvelopeBody{Text: text}}
		_, err := c.PostMessage(ctx, d.TaskID, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: to.ID, RequestID: requestID})
		// The identity is this rotation's notice to this recipient, so a
		// conflict means an earlier run already sent it with other words (the
		// project was renamed, the old handler closed, or a manual resume
		// moved the rotation on). The recipient has been told; it is not retried.
		if err != nil && !requestIdentityConflict(err) {
			return fmt.Errorf("handler rotation notice %s: %w", d.TaskID, err)
		}
		r.notified[requestID] = true
	}
	return nil
}

// requestIdentityConflict reports the hub's refusal of a request identity
// that was already used with different message data.
func requestIdentityConflict(err error) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && strings.Contains(httpErr.Msg, "request ID was already used")
}

// stuckRotationAutoAbort is the stuck notice's statement of when the runner
// aborts the rotation itself and how long it then backs off. It names no
// clock time, so the notice reads the same at every tick and after a restart.
func stuckRotationAutoAbort(reason string) string {
	when := fmt.Sprintf("%d minutes after it went stuck", int(rotationStuckAbortAfter/time.Minute))
	if reason == api.HandlerRotationReasonProviderBlocked {
		when = "when the old handler completes a turn again, or " + when + ", whichever comes first"
	}
	return "The runner aborts the rotation automatically " + when + ": the old handler then stays the primary with its leases and obligations, and queue dispatch, which leases neither handler while the rotation is prepared, resumes. " +
		fmt.Sprintf("After an abort at the time limit, the runner prepares no new rotation for the same handler run and reason for %d minutes, doubling after each such abort up to %d hours; a changed run or reason starts one at its next tick, and tt handler rotate at once.",
			int(rotationStuckAbortAfter/time.Minute), int(rotationBackoffMax/time.Hour))
}

// stuckRotationRecovery is the stuck notice's advice for the journal's
// phase. A resume launches the successor only while the rotation is prepared;
// once the launch is recorded a resume only waits for that session again.
func stuckRotationRecovery(phase, task string) string {
	const fix = "Fix the launch (the saved spec from tt handler spec, or the host's capacity)"
	if phase == rotationPhaseSpawned {
		return fmt.Sprintf("The successor was launched, and a resume only waits for that same session again: it never launches another. %s, then on that host abort the rotation with tt handler rotate --abort --task %s; a rotation that is still due then starts fresh with a new successor, at the runner's next tick or at once with tt handler rotate --task %s. Only if that launched session can still come online does a resume, the same tt handler rotate --task %s, finish this rotation.",
			fix, task, task, task)
	}
	return fmt.Sprintf("The successor's launch was never confirmed, so a resume launches it again. %s, then resume it on that host with tt handler rotate --task %s, or abort it with tt handler rotate --abort --task %s.", fix, task, task)
}
