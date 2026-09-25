package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func flagPresent(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || strings.HasPrefix(a, "--"+name+"=") {
			return true
		}
	}
	return false
}

func teamClosePendingPath(e env, task, item string) string {
	h := sha256.Sum256([]byte(e.hub + "\x00" + task + "\x00" + item + "\x00" + e.agent + "\x00" + e.runID))
	return filepath.Join(relayDir(), "team-close-"+hex.EncodeToString(h[:12])+".json")
}

func closeTeamSnapshot(task api.Task, agents []api.Agent, actorAgent, actorRun string) (api.TeamCloseRequest, error) {
	return closeTeamSnapshotForItem(task, agents, actorAgent, actorRun, "")
}

func closeTeamSnapshotForItem(task api.Task, agents []api.Agent, actorAgent, actorRun, itemID string) (api.TeamCloseRequest, error) {
	var scoped api.Agent
	for _, a := range agents {
		if !a.ItemLead || a.WorkItem == nil || a.Status == api.AgentClosed || (itemID != "" && a.WorkItem.ItemID != itemID) || (actorAgent != "" && (a.ID != actorAgent || a.RunID != actorRun)) {
			continue
		}
		if scoped.ID != "" {
			return api.TeamCloseRequest{}, errors.New("several item leads match; specify the exact item")
		}
		scoped = a
	}
	if scoped.ID != "" {
		if scoped.Status == api.AgentExited && actorAgent != "" {
			return api.TeamCloseRequest{}, errors.New("the item lead has exited; only the owner can close this team")
		}
		return closeTeamSnapshotWithLead(task, agents, actorAgent, actorRun, scoped, scoped.ItemLeadRevision)
	}
	var lead, exited api.Agent
	for _, a := range agents {
		if !strings.EqualFold(a.Name, task.Orchestrator) || a.Status == api.AgentClosed {
			continue
		}
		if a.Status == api.AgentExited {
			if actorAgent != "" {
				continue
			}
			if exited.ID != "" {
				return api.TeamCloseRequest{}, errors.New("multiple exited agents match the project orchestrator")
			}
			exited = a
		} else {
			if lead.ID != "" {
				return api.TeamCloseRequest{}, errors.New("multiple current agents match the project orchestrator")
			}
			lead = a
		}
	}
	if lead.ID == "" {
		lead = exited
	}
	if lead.ID == "" && actorAgent != "" {
		return api.TeamCloseRequest{}, errors.New("the item lead has exited; only the owner can close this team")
	}
	if lead.ID == "" || lead.WorkItem == nil || lead.WorkItem.ItemTaskID != task.ID {
		return api.TeamCloseRequest{}, errors.New("the current project orchestrator has no item binding")
	}
	if itemID != "" && lead.WorkItem.ItemID != itemID {
		return api.TeamCloseRequest{}, errors.New("no current lead for the selected item")
	}
	return closeTeamSnapshotWithLead(task, agents, actorAgent, actorRun, lead, task.LeadRevision)
}

func closeTeamSnapshotWithLead(task api.Task, agents []api.Agent, actorAgent, actorRun string, lead api.Agent, revision int64) (api.TeamCloseRequest, error) {
	if actorAgent != "" && (actorAgent != lead.ID || actorRun != lead.RunID) {
		return api.TeamCloseRequest{}, errors.New("only the exact item lead may run tt close --team")
	}
	req := api.TeamCloseRequest{RequestID: "team-close-" + task.ID + "-" + lead.RunID, ActorAgentID: actorAgent, ActorRunID: actorRun,
		LeadAgentID: lead.ID, LeadRunID: lead.RunID, LeadRevision: revision,
		ItemID: lead.WorkItem.ItemID, ItemRevision: lead.WorkItem.ItemRevision}
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler || a.WorkItem == nil || a.WorkItem.ItemTaskID != task.ID || a.WorkItem.ItemID != req.ItemID || (a.Status == api.AgentClosed && a.CleanupDone) {
			continue
		}
		req.Members = append(req.Members, api.TeamCloseMember{AgentID: a.ID, RunID: a.RunID, Host: a.Host, Status: a.Status})
	}
	slices.SortFunc(req.Members, func(a, b api.TeamCloseMember) int { return strings.Compare(a.AgentID, b.AgentID) })
	return req, nil
}

func cmdCloseTeam(e env, taskID, itemID string, jsonOut bool) error {
	if !api.ValidID(taskID, "tsk") || (e.agent != "" && e.task != taskID) {
		return errors.New("invalid or mismatched team close project")
	}
	if itemID != "" && !api.ValidID(itemID, "wi") {
		return errors.New("invalid team close item")
	}
	c, err := e.client(15 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	detail, err := c.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if e.agent != "" {
		for _, a := range detail.Agents {
			if a.ID == e.agent && a.WorkItem != nil {
				if itemID != "" && itemID != a.WorkItem.ItemID {
					return errors.New("actor cannot close another item's team")
				}
				itemID = a.WorkItem.ItemID
				break
			}
		}
	}
	pending := teamClosePendingPath(e, taskID, itemID)
	var req api.TeamCloseRequest
	hasItemLead := false
	for _, a := range detail.Agents {
		if a.ItemLead && a.WorkItem != nil && a.WorkItem.ItemID == itemID && a.Status != api.AgentClosed {
			hasItemLead = true
			break
		}
	}
	if detail.Task.Orchestrator == "" && !hasItemLead {
		data, readErr := os.ReadFile(pending)
		if readErr != nil {
			return errors.New("project has no current orchestrator or saved team close retry")
		}
		if err := json.Unmarshal(data, &req); err != nil {
			return fmt.Errorf("saved team close retry: %w", err)
		}
		if req.ActorAgentID != e.agent || req.ActorRunID != e.runID {
			return errors.New("saved team close retry belongs to a different exact actor run")
		}
	} else {
		req, err = closeTeamSnapshotForItem(detail.Task, detail.Agents, e.agent, e.runID, itemID)
		if err != nil {
			return err
		}
		if err := writePrivateJSON(pending, req); err != nil {
			return fmt.Errorf("save team close retry: %w", err)
		}
	}
	for _, m := range req.Members {
		if m.Host == spawn.Host() {
			if _, err := rememberSessions(ctx, e.hub); err != nil {
				return fmt.Errorf("verify local session ownership: %w", err)
			}
			break
		}
	}
	result, err := c.CloseItemTeam(ctx, taskID, req)
	if err != nil {
		return err
	}
	var remote []string
	for _, m := range result.Members {
		if m.AgentID == result.LeadAgentID {
			continue
		}
		if m.Host != spawn.Host() {
			remote = append(remote, m.AgentID+" on "+m.Host)
			continue
		}
		cleaned, err := cleanupSessions(ctx, e, taskID, []string{m.AgentID})
		if err != nil {
			return err
		}
		if len(cleaned.Errors) != 0 {
			return errors.New(strings.Join(cleaned.Errors, "; "))
		}
	}
	if jsonOut {
		printJSON(result)
	} else {
		fmt.Printf("Recorded team close for %s; project remains open.\n", result.ItemID)
		if len(remote) != 0 {
			fmt.Printf("Remote cleanup pending: %s\n", strings.Join(remote, ", "))
		}
	}
	for _, m := range result.Members {
		if m.AgentID != result.LeadAgentID {
			continue
		}
		if m.Host != spawn.Host() {
			fmt.Printf("Lead cleanup pending on %s: %s\n", m.Host, m.AgentID)
			return nil
		}
		// Local workers have been attempted before the lead's own tmux session.
		cleaned, err := cleanupSessions(ctx, e, taskID, []string{m.AgentID})
		if err != nil {
			return err
		}
		if len(cleaned.Errors) != 0 {
			return errors.New(strings.Join(cleaned.Errors, "; "))
		}
	}
	return nil
}
