package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/teamplan"
)

// The supervised relay ticks this runner on the launch host. Hub rows are the
// journal: a local file may hold the frozen context only during one spawn call.
// Dependencies are injectable so tests never start a real runtime or tmux.
type teamRunner struct {
	plan    func(context.Context, map[string]any, *teamLaunchResolved) error
	spawn   func(env, []string) error
	owned   func(context.Context, env, api.Agent) error
	cleanup func(context.Context, env, string, string) error
}

func productionTeamRunner() teamRunner {
	return teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			return teamplan.Run(ctx, in, out)
		},
		spawn: cmdSpawn,
		owned: func(ctx context.Context, e env, a api.Agent) error {
			owned, err := handlerOwned(ctx, e.hub, a.TaskID, a.ID, a.RunID, a.Session, nil)
			if err != nil {
				return err
			}
			if owned == nil {
				return errors.New("exact owned session missing")
			}
			return nil
		},
		cleanup: func(ctx context.Context, e env, task, id string) error {
			result, err := cleanupSessions(ctx, e, task, []string{id})
			if err != nil {
				return err
			}
			if len(result.Errors) > 0 {
				return errors.New(result.Errors[0])
			}
			return nil
		},
	}
}

func (r teamRunner) tick(ctx context.Context, e env, c *api.Client, host string) error {
	list, err := c.TeamQueueByHost(ctx, host)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, entry := range list.Entries {
		if seen[entry.TaskID] {
			continue
		}
		seen[entry.TaskID] = true
		// A failed entry freezes its project even when it is not returned by
		// the host query. The first entry in position order is authoritative.
		queue, err := c.ListTeamQueue(ctx, entry.TaskID)
		if err != nil {
			return err
		}
		var first *api.TeamQueueEntry
		for i := range queue.Entries {
			q := &queue.Entries[i]
			if q.State != "finished" && !(q.State == "failed" && q.ReleasedAt != "") {
				first = q
				break
			}
		}
		if first == nil || first.State == "failed" || first.ID != entry.ID {
			continue
		}
		if err := r.advance(ctx, e, c, *first, host); err != nil {
			return fmt.Errorf("team queue %s: %w", first.ID, err)
		}
	}
	return nil
}

func (r teamRunner) advance(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if detail.Task.Status != api.TaskOpen || detail.Task.PauseState != api.ProjectPauseActive {
		return nil
	}
	if q.State == "queued" {
		if detail.Task.Orchestrator != "" {
			return nil
		}
		item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if item.Revision != q.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("queued item changed before launch"))
		}
		handlers := 0
		for _, a := range detail.Agents {
			if a.Role == api.AgentRoleDatabaseHandler && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired && a.Online {
				handlers++
			}
		}
		if handlers != 1 {
			return nil
		}
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-claim-" + q.ID, Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: host, PauseGeneration: detail.Task.PauseGeneration})
		if err != nil {
			var response *api.HTTPError
			if errors.As(err, &response) && response.Status == 409 {
				return nil
			}
			return err
		} // another runner/manual launch won; retry the next tick
	}
	if q.State == "launching" {
		return r.launch(ctx, e, c, q, host)
	}
	if q.State == "running" {
		return r.finish(ctx, e, c, q, host)
	}
	return nil
}

func (r teamRunner) fail(ctx context.Context, c *api.Client, q api.TeamQueueEntry, cause error) error {
	_, err := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-fail-" + q.ID, Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: cause.Error()})
	if err != nil {
		return fmt.Errorf("%v; record failure: %w", cause, err)
	}
	return cause
}

func (r teamRunner) verifyMember(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, m teamLaunchMember) (api.Agent, error) {
	a, err := c.GetAgent(ctx, q.TaskID, m.Fields.AgentID)
	if err != nil {
		return a, err
	}
	if a.Name != m.Fields.Name || a.Host != q.Host || a.WorkItem == nil || a.WorkItem.ItemID != q.ItemID || a.WorkItem.ItemRevision != q.ItemRevision || a.WorkItem.WorkOrderMessage.Seq != q.OrderMessageSeq || a.Status == api.AgentClosed || a.Status == api.AgentExited || (m.RunID != "" && a.RunID != m.RunID) {
		return a, errors.New("exact agent registration differs from frozen member")
	}
	if err := r.owned(ctx, e, a); err != nil {
		return a, err
	}
	return a, nil
}

func (r teamRunner) launch(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if detail.Task.PauseGeneration != q.PauseGeneration {
		return r.fail(ctx, c, q, errors.New("project pause generation changed during launch"))
	}
	if q.Host != host {
		return nil
	}
	var journal teamLaunchJournal
	if len(q.LaunchJSON) == 0 {
		item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if item.Revision != q.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("item changed before frozen launch"))
		}
		var handler *api.Agent
		for i := range detail.Agents {
			a := &detail.Agents[i]
			if a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentRetired && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				if handler != nil {
					return nil
				}
				handler = a
			}
		}
		if handler == nil || detail.Task.Orchestrator != "" {
			return nil
		}
		var resolved teamLaunchResolved
		input := map[string]any{"hub": e.hub, "token": e.token, "task": q.TaskID, "item": q.ItemID, "revision": q.ItemRevision, "order": q.OrderMessageSeq, "template": q.Template, "cwd": q.Cwd, "host": host, "handler": handler}
		if err := r.plan(ctx, input, &resolved); err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("prepare team plan: %w", err))
		}
		if len(resolved.Plan) == 0 || !json.Valid(resolved.ItemRouting.WorkContextBundle) {
			return r.fail(ctx, c, q, errors.New("team plan is incomplete"))
		}
		journal = teamLaunchJournal{Version: 1, Hub: e.hub, Task: q.TaskID, Item: q.ItemID, Revision: q.ItemRevision, Order: q.OrderMessageSeq, HandlerID: handler.ID, Context: resolved.ItemRouting.WorkContextBundle}
		for _, member := range resolved.Plan {
			f := member.Fields
			f.AgentID = api.NewID("agt")
			journal.Members = append(journal.Members, teamLaunchMember{Fields: f, State: "unstarted", RunID: api.NewID("run")})
		}
		frozen, _ := json.Marshal(journal)
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-freeze-" + q.ID, Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: frozen})
		if err != nil {
			return err
		}
	} else if err := json.Unmarshal(q.LaunchJSON, &journal); err != nil {
		return r.fail(ctx, c, q, err)
	}
	if len(journal.Members) == 0 || journal.Task != q.TaskID || journal.Item != q.ItemID || journal.Revision != q.ItemRevision || journal.Order != q.OrderMessageSeq {
		return r.fail(ctx, c, q, errors.New("frozen team identity conflicts with queue entry"))
	}
	lead := journal.Members[0].Fields.Name
	if detail.Task.Orchestrator != lead {
		if detail.Task.Orchestrator != "" {
			return r.fail(ctx, c, q, errors.New("another orchestrator took this project"))
		}
		if _, err := c.UpdateTask(ctx, q.TaskID, api.UpdateTaskRequest{Orchestrator: &lead, TeamLaunchToken: "queue-claim-" + q.ID}); err != nil {
			return r.fail(ctx, c, q, err)
		}
	}
	for i := range journal.Members {
		member := &journal.Members[i]
		fresh, err := c.GetTask(ctx, q.TaskID)
		if err != nil {
			return err
		}
		if fresh.Task.PauseState != api.ProjectPauseActive {
			return nil
		}
		if fresh.Task.PauseGeneration != q.PauseGeneration {
			return r.fail(ctx, c, q, errors.New("project pause generation changed during launch"))
		}
		available := false
		for _, a := range fresh.Agents {
			if a.ID == journal.HandlerID && a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentRetired && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				available = true
			}
		}
		if !available {
			return nil
		}
		if fresh.Task.Orchestrator != lead {
			return r.fail(ctx, c, q, errors.New("lead changed during frozen launch"))
		}
		current, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if current.Revision != q.ItemRevision || current.Status == "done" || current.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("item changed during frozen launch"))
		}
		if member.State == "started" {
			if _, err := r.verifyMember(ctx, e, c, q, *member); err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("started member %s changed: %w", member.Fields.AgentID, err))
			}
			continue
		}
		if member.State == "uncertain" {
			a, err := r.verifyMember(ctx, e, c, q, *member)
			if err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("uncertain spawn %s has no exact registration; never respawn: %w", member.Fields.AgentID, err))
			}
			q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-started-%s-%d", q.ID, i), Operation: "started", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i, MemberRunID: a.RunID})
			if err != nil {
				return err
			}
			continue
		}
		if member.State != "unstarted" {
			return r.fail(ctx, c, q, errors.New("invalid frozen member state"))
		}
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-attempt-%s-%d", q.ID, i), Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i})
		if err != nil {
			return err
		}
		contextFile, err := os.CreateTemp("", "tt-team-context-*.json")
		if err != nil {
			return r.fail(ctx, c, q, err)
		}
		if _, err = contextFile.Write(journal.Context); err != nil {
			contextFile.Close()
			os.Remove(contextFile.Name())
			return r.fail(ctx, c, q, err)
		}
		contextFile.Close()
		f := member.Fields
		allowed, _ := json.Marshal(f.AllowedTools)
		args := []string{"--name", f.Name, "--run", f.Run, "--runtime", f.Runtime, "--model", f.Model, "--reasoning", f.Reasoning, "--cwd", f.Cwd, "--prompt", f.Prompt, "--agent-id", f.AgentID, "--expected-run-id", member.RunID, "--task", q.TaskID, "--hub", e.hub, "--work-item", q.ItemID, "--work-item-revision", strconv.FormatInt(q.ItemRevision, 10), "--work-order-message", strconv.FormatInt(q.OrderMessageSeq, 10), "--work-context-file", contextFile.Name(), "--planned-team-members", strconv.Itoa(len(journal.Members)), "--allowed-tools-json", string(allowed)}
		if f.PermissionMode != "" {
			args = append(args, "--permission-mode", f.PermissionMode)
		}
		if f.ApprovalMode != "" {
			args = append(args, "--approval-mode", f.ApprovalMode)
		}
		if f.SandboxMode != "" {
			args = append(args, "--sandbox-mode", f.SandboxMode)
		}
		spawnErr := r.spawn(e, args)
		os.Remove(contextFile.Name())
		if spawnErr != nil {
			return r.fail(ctx, c, q, fmt.Errorf("spawn %s: %w", f.Name, spawnErr))
		}
		a, err := r.verifyMember(ctx, e, c, q, *member)
		if err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("spawn registration %s: %w", f.Name, err))
		}
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-started-%s-%d", q.ID, i), Operation: "started", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i, MemberRunID: a.RunID})
		if err != nil {
			return err
		}
	}
	_, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-running-" + q.ID, Operation: "running", EntryID: q.ID, ExpectedRevision: q.Revision})
	return err
}

func (r teamRunner) finish(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
	if err != nil {
		return err
	}
	if item.Status != "done" && item.Status != "dismissed" {
		return nil
	}
	var closeReq api.TeamCloseRequest
	if len(q.CloseJSON) == 0 {
		detail, err := c.GetTask(ctx, q.TaskID)
		if err != nil {
			return err
		}
		if detail.Task.Orchestrator == "" {
			var journal teamLaunchJournal
			if json.Unmarshal(q.LaunchJSON, &journal) != nil || len(journal.Members) == 0 || journal.Members[0].RunID == "" {
				return r.fail(ctx, c, q, errors.New("closed lead has no frozen exact run"))
			}
			// The owner may have handed the lead to another member of this item.
			// Locate the exact saved receipt by its lead run, including the
			// replacement, instead of assuming the originally spawned lead.
			for _, a := range detail.Agents {
				if a.WorkItem == nil || a.WorkItem.ItemID != q.ItemID || a.WorkItem.ItemTaskID != q.TaskID {
					continue
				}
				candidate := api.TeamCloseRequest{RequestID: "team-close-" + q.TaskID + "-" + a.RunID, ItemID: q.ItemID, ItemRevision: q.ItemRevision}
				result, lookupErr := c.GetTeamCloseReceipt(ctx, q.TaskID, candidate.RequestID)
				if lookupErr == nil && result.TaskID == q.TaskID && result.ItemID == q.ItemID && result.LeadAgentID == a.ID {
					closeReq = candidate
					break
				}
				if lookupErr != nil {
					var response *api.HTTPError
					if !errors.As(lookupErr, &response) || response.Status != 404 {
						return lookupErr
					}
				}
			}
			if closeReq.RequestID == "" {
				return r.fail(ctx, c, q, errors.New("closed lead has no exact team close receipt"))
			}
		} else {
			closeReq, err = closeTeamSnapshot(detail.Task, detail.Agents, "", "")
			if err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("close gate: %w", err))
			}
			// The close request identity is frozen with its exact team snapshot.
			// A definite refusal can refresh the snapshot without reusing a key
			// that might already identify a different receipt.
			closeReq.RequestID = ""
			identity, _ := json.Marshal(closeReq)
			sum := sha256.Sum256(identity)
			closeReq.RequestID = fmt.Sprintf("queue-close-%s-%x", q.ID, sum[:12])
		}
		if closeReq.ItemID != q.ItemID {
			return r.fail(ctx, c, q, errors.New("close snapshot selected another item"))
		}
		data, _ := json.Marshal(closeReq)
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-close-freeze-%s-%d", q.ID, q.Revision), Operation: "close", EntryID: q.ID, ExpectedRevision: q.Revision, CloseJSON: data})
		if err != nil {
			return err
		}
	} else if err := json.Unmarshal(q.CloseJSON, &closeReq); err != nil {
		return r.fail(ctx, c, q, err)
	}
	result, err := c.GetTeamCloseReceipt(ctx, q.TaskID, closeReq.RequestID)
	if err != nil {
		var response *api.HTTPError
		if !errors.As(err, &response) || response.Status != 404 {
			return err
		}
		result, err = c.CloseItemTeam(ctx, q.TaskID, closeReq)
		if err != nil {
			var response *api.HTTPError
			if errors.As(err, &response) && response.Status == 409 && (response.Code == "team-close-obligations" || response.Code == "team-close-snapshot") {
				_, refreshErr := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-close-refresh-%s-%d", q.ID, q.Revision), Operation: "close_refresh", EntryID: q.ID, ExpectedRevision: q.Revision, CloseRequestID: closeReq.RequestID})
				return refreshErr
			}
			return r.fail(ctx, c, q, fmt.Errorf("team close: %w", err))
		}
	}
	for _, m := range result.Members {
		if m.Host != host {
			return r.fail(ctx, c, q, fmt.Errorf("member %s cleanup is on another host", m.AgentID))
		}
		if err := r.cleanup(ctx, e, q.TaskID, m.AgentID); err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("member %s cleanup: %w", m.AgentID, err))
		}
	}
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	for _, m := range result.Members {
		found := false
		for _, a := range detail.Agents {
			if a.ID == m.AgentID && a.RunID == m.RunID && a.CleanupDone {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	_, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-finish-" + q.ID, Operation: "finish", EntryID: q.ID, ExpectedRevision: q.Revision})
	return err
}

func relayTeamQueueTick(ctx context.Context) error {
	e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	e.loadConfig()
	if e.hub == "" {
		return nil
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	return productionTeamRunner().tick(ctx, e, c, spawn.Host())
}
