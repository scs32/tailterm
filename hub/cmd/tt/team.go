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
	"reflect"
	"strconv"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/teamplan"
)

type teamLaunchFields struct {
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Runtime        string   `json:"runtime"`
	Model          string   `json:"model"`
	Reasoning      string   `json:"reasoning"`
	ApprovalMode   string   `json:"approvalMode"`
	SandboxMode    string   `json:"sandboxMode"`
	PermissionMode string   `json:"permissionMode"`
	AllowedTools   []string `json:"allowedTools"`
	Run            string   `json:"run"`
	Cwd            string   `json:"cwd"`
	Prompt         string   `json:"prompt"`
	AgentID        string   `json:"agentId,omitempty"`
}

type teamLaunchEntry struct {
	Fields teamLaunchFields `json:"fields"`
}
type teamLaunchResolved struct {
	Plan        []teamLaunchEntry `json:"plan"`
	ItemRouting struct {
		WorkContextBundle json.RawMessage `json:"workContextBundle"`
	} `json:"itemRouting"`
}
type teamLaunchMember struct {
	Fields teamLaunchFields `json:"fields"`
	State  string           `json:"state"`
	RunID  string           `json:"runId,omitempty"`
}
type teamLaunchJournal struct {
	Version   int                `json:"version"`
	Hub       string             `json:"hub"`
	Task      string             `json:"task"`
	Item      string             `json:"item"`
	Revision  int64              `json:"revision"`
	Order     int64              `json:"order"`
	HandlerID string             `json:"handlerId"`
	Context   json.RawMessage    `json:"context"`
	Members   []teamLaunchMember `json:"members"`
}

func cmdTeam(e env, args []string) error {
	if len(args) == 0 || args[0] != "launch" {
		return errors.New("usage: tt team launch --item ID --order SEQ [--template planned] [--dry-run]")
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Println("usage: tt team launch --item ID --order SEQ [--template planned] [--dry-run] [--task ID] [--hub URL] [--cwd DIR]")
		return nil
	}
	fs := flag.NewFlagSet("team launch", flag.ContinueOnError)
	item := fs.String("item", "", "item ID")
	order := fs.Int64("order", 0, "recorded work-order message sequence")
	template := fs.String("template", "planned", "team template (planned)")
	dryRun := fs.Bool("dry-run", false, "print the plan without changing state")
	task := fs.String("task", e.task, "project ID (defaults to TAILTERM_TASK)")
	hub := fs.String("hub", e.hub, "hub URL (defaults to TAILTERM_HUB)")
	cwd := fs.String("cwd", "", "local project folder (defaults to current directory)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*item, "wi") || *order < 1 || *template != "planned" || !api.ValidID(*task, "tsk") || *hub == "" {
		return errors.New("team launch needs --item wi_ID, --order positive SEQ, a project (--task or TAILTERM_TASK), a hub, and --template planned")
	}
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	absolute, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("project cwd %s is not a directory", absolute)
	}
	e.hub, e.task = *hub, *task
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	detail, err := c.GetTask(ctx, *task)
	if err != nil {
		return err
	}
	if detail.Task.Status != api.TaskOpen || (detail.Task.PauseState != "" && detail.Task.PauseState != api.ProjectPauseActive) {
		return errors.New("team launch requires an open, active project")
	}
	workItem, err := c.GetWorkItem(ctx, *task, *item)
	if err != nil {
		return fmt.Errorf("item %s is missing: %w", *item, err)
	}
	if workItem.Status == "done" || workItem.Status == "dismissed" {
		return errors.New("team launch needs an active bug or feature")
	}
	var handler *api.Agent
	for i := range detail.Agents {
		a := &detail.Agents[i]
		if a.Role == api.AgentRoleDatabaseHandler && a.Status != api.AgentClosed && a.Status != api.AgentExited {
			if handler != nil {
				return errors.New("multiple live database handlers; resolve the project roster first")
			}
			handler = a
		}
	}
	if handler == nil {
		return errors.New("this project has no database handler; use Projects → Set up database handler before team launch")
	}
	if !handler.Online || handler.Status == api.AgentRetired {
		return errors.New("the project's database handler is unavailable; resume or repair it before team launch")
	}
	for _, a := range detail.Agents {
		if a.Name == detail.Task.Orchestrator && a.Status != api.AgentClosed && a.Status != api.AgentExited {
			if a.WorkItem == nil || a.WorkItem.ItemID != *item {
				return fmt.Errorf("project already has a live orchestrator %s for another item; finish or explicitly hand off that work before launch", a.Name)
			}
		}
	}
	input := map[string]any{"hub": *hub, "token": e.token, "task": *task, "item": *item, "revision": workItem.Revision, "order": *order,
		"template": *template, "cwd": absolute, "host": spawn.Host(), "handler": handler}
	var resolved teamLaunchResolved
	if err := teamplan.Run(ctx, input, &resolved); err != nil {
		return err
	}
	if len(resolved.Plan) == 0 || len(resolved.ItemRouting.WorkContextBundle) == 0 {
		return errors.New("team plan is empty or lacks prepared item context")
	}
	if *dryRun {
		fmt.Printf("Planned delivery for %s@%d, order #%d; database handler %s reused\n", *item, workItem.Revision, *order, handler.Name)
		for _, member := range resolved.Plan {
			f := member.Fields
			fmt.Printf("%s runtime=%s model=%s reasoning=%s promptBytes=%d cwd=%s\n", f.Name, f.Runtime, f.Model, f.Reasoning, len([]byte(f.Prompt)), f.Cwd)
		}
		return nil
	}
	currentItem, err := c.GetWorkItem(ctx, *task, *item)
	if err != nil {
		return err
	}
	if currentItem.Revision != workItem.Revision || currentItem.Status == "done" || currentItem.Status == "dismissed" {
		return errors.New("item changed while preparing the team; refresh the bounded order before launch")
	}
	// An agent-parented launch needs handler-authored allocation intents for each
	// exact identity. The owner CLI path has no parent and does not consume extras.
	if e.agent != "" {
		return errors.New("team launch from an agent session needs handler-authored allocation intents; run this command from the owner's unbound CLI session")
	}
	path, err := teamJournalPath(*hub, *task, *item, *order)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another launch of this exact item and order is in progress; retry after it finishes")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	journal, err := loadTeamJournal(path)
	if errors.Is(err, os.ErrNotExist) {
		journal = teamLaunchJournal{Version: 1, Hub: *hub, Task: *task, Item: *item, Revision: workItem.Revision, Order: *order, HandlerID: handler.ID, Context: resolved.ItemRouting.WorkContextBundle}
		for _, entry := range resolved.Plan {
			f := entry.Fields
			f.AgentID = api.NewID("agt")
			journal.Members = append(journal.Members, teamLaunchMember{Fields: f, State: "unstarted"})
		}
		if err = saveTeamJournal(path, journal); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if journal.Hub != *hub || journal.Task != *task || journal.Item != *item || journal.Order != *order || journal.Revision != workItem.Revision || journal.HandlerID != handler.ID || len(journal.Members) != len(resolved.Plan) {
		return errors.New("saved launch journal conflicts with current item or plan; reconcile it before retry")
	}
	for i, entry := range resolved.Plan {
		f := journal.Members[i].Fields
		f.AgentID = ""
		if !reflect.DeepEqual(f, entry.Fields) {
			return errors.New("saved launch fields differ from the current template; reconcile the frozen plan before retry")
		}
	}
	// Re-read immediately before the first mutation. A previously selected lead
	// cannot be silently replaced by a new item-scoped lead.
	detail, err = c.GetTask(ctx, *task)
	if err != nil {
		return err
	}
	if err := checkTeamRoster(detail, journal); err != nil {
		return err
	}
	lead := journal.Members[0].Fields.Name
	if detail.Task.Orchestrator != lead {
		var updated api.Task
		if err := teamplan.Run(ctx, map[string]any{"action": "set-orchestrator", "hub": *hub, "token": e.token, "task": *task, "orchestrator": lead}, &updated); err != nil {
			return err
		}
		if updated.Orchestrator != lead {
			return errors.New("hub did not confirm the item-scoped orchestrator")
		}
	}
	contextFile := path + ".context"
	if err := os.WriteFile(contextFile, journal.Context, 0600); err != nil {
		return err
	}
	for i := range journal.Members {
		member := &journal.Members[i]
		currentItem, err = c.GetWorkItem(ctx, *task, *item)
		if err != nil {
			return err
		}
		if currentItem.Revision != journal.Revision || currentItem.Status == "done" || currentItem.Status == "dismissed" {
			return errors.New("item changed during launch; frozen remaining members were not started")
		}
		detail, err = c.GetTask(ctx, *task)
		if err != nil {
			return err
		}
		if err := checkTeamRoster(detail, journal); err != nil {
			return err
		}
		reconciled := false
		for _, a := range detail.Agents {
			if a.ID == member.Fields.AgentID {
				if a.Name != member.Fields.Name || a.WorkItem == nil || a.WorkItem.ItemID != *item || a.WorkItem.ItemRevision != journal.Revision || a.WorkItem.WorkOrderMessage.Seq != *order {
					return fmt.Errorf("saved identity %s has conflicting registration; reconcile before retry", a.ID)
				}
				if a.Status == api.AgentClosed || a.Status == api.AgentExited {
					return fmt.Errorf("saved identity %s is %s; reconcile before retry", a.ID, a.Status)
				}
				owned, verifyErr := handlerOwned(ctx, *hub, *task, a.ID, a.RunID, a.Session, nil)
				if verifyErr != nil {
					return fmt.Errorf("verify saved session for %s: %w", a.ID, verifyErr)
				}
				if owned == nil {
					return fmt.Errorf("saved identity %s is registered without an owned tmux session; reconcile the uncertain launch before continuing", a.ID)
				}
				member.State, member.RunID = "started", a.RunID
				if err := saveTeamJournal(path, journal); err != nil {
					return err
				}
				fmt.Printf("%s agent=%s run=%s (reconciled)\n", a.Name, a.ID, a.RunID)
				reconciled = true
				break
			}
		}
		if reconciled {
			continue
		}
		if member.State == "started" {
			return fmt.Errorf("started identity %s is missing; reconcile before retry", member.Fields.AgentID)
		}
		member.State = "uncertain"
		if err := saveTeamJournal(path, journal); err != nil {
			return err
		}
		f := member.Fields
		allowed, _ := json.Marshal(f.AllowedTools)
		spawnArgs := []string{"--name", f.Name, "--run", f.Run, "--runtime", f.Runtime, "--model", f.Model,
			"--reasoning", f.Reasoning, "--cwd", f.Cwd, "--prompt", f.Prompt, "--agent-id", f.AgentID,
			"--task", *task, "--hub", *hub, "--work-item", *item, "--work-item-revision", strconv.FormatInt(journal.Revision, 10),
			"--work-order-message", strconv.FormatInt(*order, 10), "--work-context-file", contextFile,
			"--planned-team-members", strconv.Itoa(len(journal.Members)), "--allowed-tools-json", string(allowed)}
		if f.PermissionMode != "" {
			spawnArgs = append(spawnArgs, "--permission-mode", f.PermissionMode)
		}
		if f.ApprovalMode != "" {
			spawnArgs = append(spawnArgs, "--approval-mode", f.ApprovalMode)
		}
		if f.SandboxMode != "" {
			spawnArgs = append(spawnArgs, "--sandbox-mode", f.SandboxMode)
		}
		if err := cmdSpawn(e, spawnArgs); err != nil {
			return fmt.Errorf("launch %s (identity %s frozen for retry): %w", f.Name, f.AgentID, err)
		}
		a, err := c.GetAgent(ctx, *task, f.AgentID)
		if err != nil {
			return fmt.Errorf("reconcile spawned %s: %w", f.Name, err)
		}
		member.State, member.RunID = "started", a.RunID
		if err := saveTeamJournal(path, journal); err != nil {
			return err
		}
		fmt.Printf("%s agent=%s run=%s\n", a.Name, a.ID, a.RunID)
	}
	return nil
}

func checkTeamRoster(detail api.TaskDetail, journal teamLaunchJournal) error {
	if detail.Task.Status != api.TaskOpen || (detail.Task.PauseState != "" && detail.Task.PauseState != api.ProjectPauseActive) {
		return errors.New("project is no longer active")
	}
	lead := journal.Members[0].Fields.Name
	handlerAvailable := false
	for _, a := range detail.Agents {
		if a.ID == journal.HandlerID && a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentRetired && a.Status != api.AgentClosed && a.Status != api.AgentExited {
			handlerAvailable = true
		}
		if a.Name == detail.Task.Orchestrator && a.Status != api.AgentClosed && a.Status != api.AgentExited && detail.Task.Orchestrator != lead {
			if a.WorkItem != nil && a.WorkItem.ItemID == journal.Item {
				return fmt.Errorf("project already has a live orchestrator %s for this item; reconcile the existing launch instead of creating a second lead", a.Name)
			}
			return fmt.Errorf("project has a live orchestrator %s for another item", a.Name)
		}
		for _, member := range journal.Members {
			if a.Name == member.Fields.Name && a.ID != member.Fields.AgentID && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				return fmt.Errorf("name %s is already registered with another identity", a.Name)
			}
		}
	}
	if !handlerAvailable {
		return errors.New("saved database handler is unavailable; restore it before continuing the team launch")
	}
	return nil
}

func teamJournalPath(hub, task, item string, order int64) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".local", "state", "tt", "team-launch")
	key := sha256.Sum256([]byte(hub + "\x00" + task + "\x00" + item + "\x00" + strconv.FormatInt(order, 10)))
	return filepath.Join(dir, hex.EncodeToString(key[:])+".json"), nil
}
func loadTeamJournal(path string) (teamLaunchJournal, error) {
	var journal teamLaunchJournal
	data, err := os.ReadFile(path)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		return journal, err
	}
	if journal.Version != 1 || len(journal.Members) == 0 {
		return journal, errors.New("invalid saved team launch journal")
	}
	if !api.ValidID(journal.Task, "tsk") || !api.ValidID(journal.Item, "wi") || journal.Revision < 1 || journal.Order < 1 || !api.ValidID(journal.HandlerID, "agt") || !json.Valid(journal.Context) {
		return journal, errors.New("invalid saved team launch journal identity or context")
	}
	for _, member := range journal.Members {
		if !api.ValidID(member.Fields.AgentID, "agt") || !api.ValidName(member.Fields.Name) || (member.State != "unstarted" && member.State != "uncertain" && member.State != "started") {
			return journal, errors.New("invalid saved team launch member")
		}
	}
	return journal, nil
}
func saveTeamJournal(path string, journal teamLaunchJournal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
