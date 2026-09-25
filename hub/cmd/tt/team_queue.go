package main

import (
	"bytes"
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
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func validTeamQueueEntryID(id string) bool {
	if !strings.HasPrefix(id, "tqe_") || len(id) != 20 {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

func cmdTeamQueue(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt team queue add|list|policy|limit|accept|replace-lead|remove|reorder|release|abandon")
	}
	sub := args[0]
	fs := flag.NewFlagSet("team queue "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	hub := fs.String("hub", e.hub, "hub URL")
	item := fs.String("item", "", "work item ID")
	order := fs.Int64("order", 0, "recorded work-order message sequence")
	template := fs.String("template", "planned", "team template")
	entry := fs.String("entry", "", "queue entry ID")
	leadAgent := fs.String("lead-agent", "", "exact replacement item team member ID")
	worktree := fs.String("worktree", "", "accepted builder worktree root")
	branch := fs.String("branch", "", "accepted branch")
	commit := fs.String("commit", "", "accepted commit SHA")
	acceptanceEvidence := fs.String("evidence", "", "handler-saved terminal acceptance evidence reference")
	before := fs.String("before", "", "place before this queued entry; omit to move to end")
	cwd := fs.String("cwd", "", "absolute project folder for launch host")
	var ownership ownershipFlags
	fs.Var(&ownership, "owns", "repository-relative owned file or directory (repeatable)")
	limit := fs.Int("limit", 0, "project concurrency limit (1 or 2)")
	policyVersion := fs.Int64("policy-version", 0, "owner host policy version")
	policyExpires := fs.String("expires", "", "host policy expiry in RFC3339")
	policySessions := fs.Int("sessions", 0, "host session budget")
	policyPolling := fs.Int("polling", 0, "host polling budget")
	policyBindings := fs.Int("bindings", 0, "maximum host relay bindings")
	policyRate := fs.Int("requests-per-minute", 0, "host relay request-rate budget")
	policyBurst := fs.Int("burst", 0, "host relay burst budget")
	policyHeadroom := fs.Int("headroom-percent", 0, "reserved host limiter headroom percent")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*task, "tsk") || *hub == "" {
		return errors.New("team queue requires a project and hub")
	}
	e.task, e.hub = *task, *hub
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if sub == "list" {
		list, err := c.ListTeamQueue(ctx, *task)
		if err != nil {
			return err
		}
		if *jsonOut {
			printJSON(list)
			return nil
		}
		if len(list.Entries) == 0 {
			fmt.Println("(empty team queue)")
			return nil
		}
		fmt.Printf("concurrency limit=%d\n", list.ConcurrencyLimit)
		for _, q := range list.Entries {
			state := q.State
			owns := strings.Join(q.Ownership, ",")
			if owns == "" {
				owns = "unscoped (conflicts with all)"
			}
			if q.State == "failed" && q.ReleasedAt != "" {
				state += " (released)"
			}
			fmt.Printf("%d %s %s %s order=#%d revision=%d repository=%s owns=%s blocked-by=%s reason=%s handler=%s/%s lease=%d\n", q.Position, state, q.ID, q.ItemID, q.OrderMessageSeq, q.Revision, q.Repository, owns, strings.Join(q.BlockedBy, ","), q.BlockReason, q.HandlerID, q.HandlerRunID, q.HandlerLeaseGeneration)
			if q.Integration != nil {
				fmt.Printf("  Ready to integrate: base=%s worktree=%s branch=%s commit=%s evidence=%s\n", q.Integration.BaseCommit, q.Integration.Worktree, q.Integration.Branch, q.Integration.Commit, q.Integration.Evidence)
			} else if q.Acceptance != nil {
				fmt.Printf("  Accepted for integration: worktree=%s branch=%s commit=%s; waiting for exact cleanup\n", q.Acceptance.Worktree, q.Acceptance.Branch, q.Acceptance.Commit)
			}
		}
		return nil
	}
	if e.agent != "" && sub != "accept" {
		return errors.New("owner-side team queue changes require an unbound CLI session")
	}
	req := api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: sub}
	switch sub {
	case "policy":
		if *policyVersion < 1 || *policyExpires == "" || *policySessions < 1 || *policyPolling < 1 || *policyBindings < 1 || *policyRate < 1 || *policyBurst < 1 || *policyHeadroom < 1 || *policyHeadroom >= 100 {
			return errors.New("usage: tt team queue policy --policy-version N --expires RFC3339 --sessions N --polling N --bindings N --requests-per-minute N --burst N --headroom-percent N")
		}
		req.Operation, req.Host, req.HostPolicyVersion, req.HostPolicyExpires, req.HostMaxSessions, req.HostMaxPolling = "set_host_policy", spawn.Host(), *policyVersion, *policyExpires, *policySessions, *policyPolling
		req.LimiterDomain, err = canonicalLimiterDomain(e.hub)
		if err != nil {
			return err
		}
		req.HostMaxRelayBindings, req.HostMaxRequestsPerMinute, req.HostMaxBurst, req.HostHeadroomPercent = *policyBindings, *policyRate, *policyBurst, *policyHeadroom
	case "limit":
		if *limit < 1 || *limit > 2 {
			return errors.New("usage: tt team queue limit --limit 1|2")
		}
		req.Operation, req.ConcurrencyLimit, req.Host = "set_limit", *limit, spawn.Host()
		if *limit > 1 {
			list, listErr := c.TeamQueueByHost(ctx, req.Host)
			if listErr != nil {
				return listErr
			}
			if list.HostPolicy != nil {
				if err := saveHostRelayCensus(ctx, c, *task, req.Host, list.HostPolicy.LimiterDomain, list.HostPolicy.Version, list.HostUsage, time.Now()); err != nil {
					return err
				}
			}
		}
	case "add":
		if !api.ValidID(*item, "wi") || *order < 1 || *template != "planned" {
			return errors.New("usage: tt team queue add --item wi_ID --order SEQ [--template planned]")
		}
		if *cwd == "" {
			*cwd, err = os.Getwd()
			if err != nil {
				return err
			}
		}
		*cwd, err = filepath.Abs(*cwd)
		if err != nil {
			return err
		}
		info, statErr := os.Stat(*cwd)
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("project cwd is not a directory: %s", *cwd)
		}
		repository, scopeErr := queueRepositoryScope(*cwd, ownership)
		if scopeErr != nil {
			return scopeErr
		}
		req.ItemID, req.OrderMessageSeq, req.Template, req.Host, req.Cwd, req.Repository, req.Ownership = *item, *order, *template, spawn.Host(), *cwd, repository, ownership
		if repository != "" {
			req.BaseCommit, err = queueGitCommit(*cwd)
			if err != nil {
				return err
			}
		}
	case "remove", "reorder", "release", "replace-lead", "accept":
		if !validTeamQueueEntryID(*entry) {
			return errors.New("remove/reorder/release/replace-lead requires --entry tqe_ID")
		}
		q, err := c.GetTeamQueueEntry(ctx, *task, *entry)
		if err != nil {
			return err
		}
		req.EntryID, req.ExpectedRevision, req.BeforeID = q.ID, q.Revision, *before
		if sub == "accept" {
			if e.agent == "" || e.runID == "" || e.agent != q.HandlerID || e.runID != q.HandlerRunID || *worktree == "" || *branch == "" || *commit == "" || *acceptanceEvidence == "" {
				return errors.New("accept requires the exact assigned handler and --worktree, --branch, --commit and --evidence")
			}
			realWorktree, err := filepath.EvalSymlinks(*worktree)
			if err != nil || !filepath.IsAbs(realWorktree) {
				return errors.New("accepted worktree must be an existing absolute path")
			}
			repository, err := queueRepositoryScope(realWorktree, nil)
			if err != nil || repository != q.Repository {
				return errors.New("accepted worktree is outside the frozen repository")
			}
			item, err := c.GetWorkItem(ctx, *task, q.ItemID)
			if err != nil {
				return err
			}
			if item.Status != "done" {
				return errors.New("handler acceptance requires the saved done item")
			}
			req.Operation, req.RequestID = "accept", "queue-accept-"+q.ID
			req.HandlerAgentID, req.HandlerRunID = e.agent, e.runID
			req.Acceptance = &api.TeamIntegrationAcceptance{Repository: q.Repository, BaseCommit: q.BaseCommit, Worktree: realWorktree, Branch: *branch, Commit: *commit, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: *acceptanceEvidence}
			if q.Acceptance == nil {
				ready, err := queueIntegrationSnapshot(ctx, api.TeamQueueEntry{Repository: q.Repository, BaseCommit: q.BaseCommit, Acceptance: req.Acceptance}, item, api.TeamCloseRequest{})
				if err != nil || ready.Commit != *commit {
					return fmt.Errorf("accepted Git tuple failed worktree verification: %v", err)
				}
			}
		}
		if sub == "replace-lead" {
			if !api.ValidID(*leadAgent, "agt") {
				return errors.New("replace-lead requires --lead-agent agt_ID")
			}
			candidate, err := c.GetAgent(ctx, *task, *leadAgent)
			if err != nil {
				return err
			}
			detail, err := c.GetTask(ctx, *task)
			if err != nil {
				return err
			}
			for _, a := range detail.Agents {
				if a.ItemLead && a.WorkItem != nil && a.WorkItem.ItemID == q.ItemID {
					req.ExpectedLeadRevision = a.ItemLeadRevision
					break
				}
			}
			if req.ExpectedLeadRevision == 0 || candidate.WorkItem == nil || candidate.WorkItem.ItemID != q.ItemID {
				return errors.New("exact current and replacement leads of this item are required")
			}
			req.Operation, req.LeadAgentID, req.LeadRunID = "replace_lead", candidate.ID, candidate.RunID
		}
		if sub == "release" {
			req.RequestID = "queue-release-" + q.ID
			var journal teamLaunchJournal
			if len(q.LaunchJSON) > 0 && json.Unmarshal(q.LaunchJSON, &journal) != nil {
				return errors.New("failed queue launch journal is invalid")
			}
			uncertain := false
			for _, m := range journal.Members {
				if m.State == "uncertain" {
					uncertain = true
				}
			}
			if uncertain {
				if q.Host != spawn.Host() {
					return errors.New("uncertain queue release must run on the saved launch host")
				}
				lock, lockErr := queueLaunchLock(*hub, *task, q.ID)
				if lockErr != nil {
					return lockErr
				}
				defer unlockQueueLaunch(lock)
				fresh, fetchErr := c.GetTeamQueueEntry(ctx, *task, q.ID)
				if fetchErr != nil {
					return fetchErr
				}
				if fresh.Revision != q.Revision || fresh.State != q.State || !bytes.Equal(fresh.LaunchJSON, q.LaunchJSON) {
					return errors.New("queue entry changed while acquiring the host launch lock")
				}
				sessions, sessionErr := localSessions(ctx)
				if sessionErr != nil {
					return sessionErr
				}
				proof := &api.TeamQueueReleaseProof{TaskID: *task, EntryID: q.ID, ItemID: q.ItemID, Host: q.Host}
				digest := sha256.Sum256(q.LaunchJSON)
				proof.LaunchDigest = hex.EncodeToString(digest[:])
				for _, member := range journal.Members {
					if member.State != "uncertain" {
						continue
					}
					for _, session := range sessions {
						if (session.Hub == *hub && session.Task == *task && session.Agent == member.Fields.AgentID) || session.Name == member.Fields.Name {
							return fmt.Errorf("uncertain member %s still has an owned or name-conflicting session", member.Fields.AgentID)
						}
					}
					_, agentErr := c.GetAgent(ctx, *task, member.Fields.AgentID)
					if agentErr == nil {
						continue
					}
					var response *api.HTTPError
					if !errors.As(agentErr, &response) || response.Status != 404 {
						return agentErr
					}
					proof.Members = append(proof.Members, api.TeamQueueReleaseMember{AgentID: member.Fields.AgentID, RunID: member.RunID, Name: member.Fields.Name})
				}
				if len(proof.Members) > 0 {
					req.ReleaseProof = proof
					req.SessionsChecked = true
					req.Host = q.Host
				}
			}
		}
	case "abandon":
		if !api.ValidID(*item, "wi") || *order < 1 {
			return errors.New("abandon requires --item wi_ID --order SEQ")
		}
		path, pathErr := teamJournalPath(*hub, *task, *item, *order)
		if pathErr != nil {
			return pathErr
		}
		lock, lockErr := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
		if lockErr == nil {
			defer lock.Close()
			lockErr = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if lockErr == nil {
				defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			}
		} else if errors.Is(lockErr, os.ErrNotExist) {
			// Before the first journal there is no local launch effect.
			lockErr = nil
		}
		if lockErr != nil {
			return fmt.Errorf("manual launch may still be running: %w", lockErr)
		}
		journal, readErr := loadTeamJournal(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if readErr == nil {
			if journal.Hub != *hub || journal.Task != *task || journal.Item != *item || journal.Order != *order {
				return errors.New("manual journal identity differs from reservation")
			}
			sessions, sessionErr := localSessions(ctx)
			if sessionErr != nil {
				return sessionErr
			}
			proof := struct {
				Task    string `json:"task"`
				Item    string `json:"item"`
				Order   int64  `json:"order"`
				Members []struct {
					AgentID string `json:"agentId"`
					State   string `json:"state"`
					RunID   string `json:"runId"`
				} `json:"members"`
			}{Task: *task, Item: *item, Order: *order}
			for _, member := range journal.Members {
				for _, session := range sessions {
					if (session.Hub == *hub && session.Task == *task && session.Agent == member.Fields.AgentID) || session.Name == member.Fields.Name {
						return fmt.Errorf("manual member %s still has an owned or name-conflicting session", member.Fields.AgentID)
					}
				}
				proof.Members = append(proof.Members, struct {
					AgentID string `json:"agentId"`
					State   string `json:"state"`
					RunID   string `json:"runId"`
				}{member.Fields.AgentID, member.State, member.RunID})
			}
			req.ManualJournal, _ = json.Marshal(proof)
			req.SessionsChecked = true
		}
		req.Operation = "manual_release"
		req.ItemID, req.OrderMessageSeq = *item, *order
		req.ReservationToken = fmt.Sprintf("manual-%s-%s-%d", *task, *item, *order)
		req.RequestID = fmt.Sprintf("manual-release-%s-%s-%d", *task, *item, *order)
	default:
		return errors.New("usage: tt team queue add|list|policy|limit|replace-lead|remove|reorder|release|abandon")
	}
	result, err := c.TeamQueueAction(ctx, *task, req)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(result)
	} else {
		if sub == "limit" {
			fmt.Printf("concurrency limit=%d\n", result.Revision)
		} else {
			fmt.Printf("%s %s %s at %d\n", sub, result.ID, result.ItemID, result.Position)
		}
	}
	return nil
}
