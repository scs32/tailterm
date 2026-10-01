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
	"os/exec"
	"path/filepath"
	"strconv"
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

const teamQueueUsage = "usage: tt team queue add|list|policy|limit|scope|rebind|requeue|fail|accept|integrated|replace-lead|remove|reorder|release|abandon"

// parseQueueLimit reads --limit: none (no fixed cap, stored as 0) or N >= 1.
func parseQueueLimit(raw string) (int, bool) {
	if raw == "none" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 1
}

func queueLimitText(limit int) string {
	if limit == 0 {
		return "none"
	}
	return strconv.Itoa(limit)
}

// queueWorktreePath is an entry's own worktree path: the repository root's
// .build/worktrees/queue-<item8>. checkout must be a worktree root.
func queueWorktreePath(checkout, item string) (string, error) {
	top, err := exec.Command("git", "-C", checkout, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("--new-worktree needs a Git worktree root; %s is not one", checkout)
	}
	realTop, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil {
		return "", err
	}
	realCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil || realCheckout != realTop {
		return "", fmt.Errorf("--new-worktree needs a Git worktree root; %s is not one", checkout)
	}
	common, err := exec.Command("git", "-C", checkout, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(strings.TrimSpace(string(common)))
	return filepath.Join(root, ".build", "worktrees", "queue-"+strings.TrimPrefix(item, "wi_")[:8]), nil
}

// addQueueWorktree gives an entry its own detached worktree at commit, under
// the repository root's .build/worktrees, named for the item.
func addQueueWorktree(checkout, item, commit string) (string, error) {
	path, err := queueWorktreePath(checkout, item)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("queue worktree already exists: %s; queue from it with --cwd %s", path, path)
	}
	if output, err := exec.Command("git", "-C", checkout, "worktree", "add", "--detach", path, commit).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return path, nil
}

// repairQueueWorktree recreates an entry's missing queue worktree, detached at
// its frozen base, when the entry's cwd is exactly that worktree's path.
func repairQueueWorktree(q api.TeamQueueEntry, ownership []string) (string, error) {
	root := filepath.Dir(q.Repository)
	target := filepath.Join(root, ".build", "worktrees", "queue-"+strings.TrimPrefix(q.ItemID, "wi_")[:8])
	if filepath.Clean(q.Cwd) != target {
		return "", fmt.Errorf("entry %s names a missing checkout %s that is not its queue worktree %s", q.ID, q.Cwd, target)
	}
	if output, err := exec.Command("git", "-C", root, "worktree", "add", "--detach", target, q.BaseCommit).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(output)))
	}
	repository, err := queueRepositoryScope(target, ownership)
	if err == nil && repository != q.Repository {
		err = fmt.Errorf("the recreated worktree is in repository %s, not the entry's %s", repository, q.Repository)
	}
	if err != nil {
		_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", target).Run()
		return "", err
	}
	return target, nil
}

// sameQueueDir reports whether two paths name one directory.
func sameQueueDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	realA, errA := filepath.EvalSymlinks(a)
	realB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && realA == realB
}

func cmdTeamQueue(e env, args []string) error {
	if len(args) == 0 {
		return errors.New(teamQueueUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("team queue "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	hub := fs.String("hub", e.hub, "hub URL")
	item := fs.String("item", "", "work item ID")
	order := fs.Int64("order", 0, "recorded work-order message sequence")
	template := fs.String("template", "planned", "team template: planned, or small for an eligible small bug")
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
	limit := fs.String("limit", "", "limit: project concurrency limit, none (no fixed cap), 1 (serial) or N; list: history page size 1..200")
	after := fs.Int64("after", 0, "list: history entries older than this position (the previous page's older cursor)")
	activeOnly := fs.Bool("active", false, "list: active entries only, no history")
	minFreeDisk := fs.Int64("min-free-disk-mib", 0, "free-disk reserve for new parallel teams in MiB (0: default 8192)")
	newWorktree := fs.Bool("new-worktree", false, "add, scope: create the entry's own detached worktree under .build/worktrees (scope: at its frozen base)")
	noNewWorktree := fs.Bool("no-new-worktree", false, "add: use the current checkout in a parallel project")
	serial := fs.Bool("serial", false, "add: declare no ownership; the entry runs alone")
	reason := fs.String("reason", "", "fail: why the owner is failing this entry")
	source := fs.Int64("source", 0, "rebind: the amendment's message sequence")
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
		opts := api.TeamQueueListOptions{After: *after, Item: *item}
		if *item != "" && (!api.ValidID(*item, "wi") || *activeOnly || *after != 0 || *limit != "") {
			return errors.New("usage: tt team queue list --item wi_ID prints that item's entry in full")
		}
		if *activeOnly {
			opts.View = api.TeamQueueViewActive
		}
		if *limit != "" {
			n, convErr := strconv.Atoi(*limit)
			if convErr != nil || n < 1 || n > api.MaxLimit {
				return fmt.Errorf("usage: tt team queue list [--active | --item wi_ID] [--limit 1..%d] [--after POSITION] [--json]", api.MaxLimit)
			}
			opts.Limit = n
		}
		if *after < 0 || (*activeOnly && (*after != 0 || *limit != "")) {
			return fmt.Errorf("usage: tt team queue list [--active | --item wi_ID] [--limit 1..%d] [--after POSITION] [--json]", api.MaxLimit)
		}
		list, err := c.ListTeamQueuePage(ctx, *task, opts)
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
		fmt.Printf("concurrency limit=%s\n", queueLimitText(list.ConcurrencyLimit))
		matrix, matrixNotice := readMatrixWaitlist()
		if matrixNotice != "" {
			fmt.Println(matrixNotice)
		}
		for _, q := range list.Entries {
			state := q.State
			owns := strings.Join(q.Ownership, ",")
			if owns == "" && q.Serial {
				owns = "serial (runs alone)"
			} else if owns == "" {
				owns = "unscoped (legacy)"
			}
			if q.OwnerIntegration != nil {
				state += " (owner-integrated)"
			} else if q.State == "failed" && q.ReleasedAt != "" {
				state += " (released)"
			}
			fmt.Printf("%d %s %s %s order=#%d template=%s revision=%d repository=%s cwd=%s owns=%s blocked-by=%s reason=%s handler=%s/%s lease=%d%s%s\n", q.Position, state, q.ID, q.ItemID, q.OrderMessageSeq, q.Template, q.Revision, q.Repository, q.Cwd, owns, strings.Join(q.BlockedBy, ","), q.BlockReason, q.HandlerID, q.HandlerRunID, q.HandlerLeaseGeneration, queueArmText(q.HandlerArm), queueAttemptText(q))
			fmt.Printf("  team last-transition tokens=%d\n", q.Tokens.Total)
			for _, member := range q.Activities {
				state := "unknown"
				if member.Activity != nil {
					state = member.Activity.State
				}
				fmt.Printf("  %s activity=%s\n", member.Name, state)
			}
			if q.OwnerIntegration != nil {
				fmt.Printf("  Owner-integrated at %s: commit=%s changed=%s; slot, lease and ownership released, team closes when the item is terminal\n", q.OwnerIntegration.At, q.OwnerIntegration.Commit, strings.Join(q.OwnerIntegration.ChangedFiles, ","))
			}
			if q.Integration != nil {
				fmt.Printf("  Ready to integrate: base=%s worktree=%s branch=%s commit=%s evidence=%s\n", q.Integration.BaseCommit, q.Integration.Worktree, q.Integration.Branch, q.Integration.Commit, q.Integration.Evidence)
			} else if q.Acceptance != nil {
				fmt.Printf("  Accepted for integration: worktree=%s branch=%s commit=%s; waiting for exact cleanup\n", q.Acceptance.Worktree, q.Acceptance.Branch, q.Acceptance.Commit)
			}
			for _, line := range matrix.waitLines(q) {
				fmt.Println(line)
			}
		}
		if h := list.History; h != nil {
			shown := 0
			for _, q := range list.Entries {
				if q.Summary {
					shown++
				}
			}
			fmt.Printf("history: showing %d of %d (newest first; full entry: tt team queue list --task %s --item wi_ID)\n", shown, h.Total, *task)
			if h.NextAfter > 0 {
				fmt.Printf("older: tt team queue list --task %s --limit %d --after %d\n", *task, h.Limit, h.NextAfter)
			}
		}
		return nil
	}
	if e.agent != "" && sub != "accept" && sub != "scope" && sub != "rebind" && sub != "requeue" {
		return errors.New("owner-side team queue changes require an unbound CLI session")
	}
	req := api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: sub}
	createdWorktree := ""
	switch sub {
	case "policy":
		if *policyVersion < 1 || *policyExpires == "" || *policySessions < 1 || *policyPolling < 1 || *policyBindings < 1 || *policyRate < 1 || *policyBurst < 1 || *policyHeadroom < 1 || *policyHeadroom >= 100 || *minFreeDisk < 0 {
			return errors.New("usage: tt team queue policy --policy-version N --expires RFC3339 --sessions N --polling N --bindings N --requests-per-minute N --burst N --headroom-percent N [--min-free-disk-mib N]")
		}
		req.HostMinFreeDiskMiB = *minFreeDisk
		req.Operation, req.Host, req.HostPolicyVersion, req.HostPolicyExpires, req.HostMaxSessions, req.HostMaxPolling = "set_host_policy", spawn.Host(), *policyVersion, *policyExpires, *policySessions, *policyPolling
		req.LimiterDomain, err = canonicalLimiterDomain(e.hub)
		if err != nil {
			return err
		}
		req.HostMaxRelayBindings, req.HostMaxRequestsPerMinute, req.HostMaxBurst, req.HostHeadroomPercent = *policyBindings, *policyRate, *policyBurst, *policyHeadroom
	case "limit":
		value, ok := parseQueueLimit(*limit)
		if !ok {
			return errors.New("usage: tt team queue limit --limit none|N (none: no fixed cap; 1: serial)")
		}
		req.Operation, req.ConcurrencyLimit, req.Host = "set_limit", value, spawn.Host()
		if queueParallel(value) {
			list, listErr := c.TeamQueueByHost(ctx, req.Host)
			if listErr != nil {
				return listErr
			}
			if list.HostPolicy != nil {
				if err := saveHostRelayCensus(ctx, c, *task, req.Host, *list.HostPolicy, list.HostUsage, queueCwds(list.Entries), time.Now()); err != nil {
					return err
				}
			}
		}
	case "add":
		if !api.ValidID(*item, "wi") || *order < 1 || (*template != "planned" && *template != "small") || (*newWorktree && (*noNewWorktree || *cwd != "")) {
			return errors.New("usage: tt team queue add --item wi_ID --order SEQ [--template planned|small] [--owns PATH... | --serial] [--cwd DIR | --new-worktree | --no-new-worktree]")
		}
		if *serial && len(ownership) > 0 {
			return errors.New("--serial declares no ownership; pass either --owns or --serial")
		}
		list, listErr := c.ListTeamQueuePage(ctx, *task, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
		if listErr != nil {
			return listErr
		}
		parallel := queueParallel(list.ConcurrencyLimit)
		if len(ownership) == 0 && !*serial {
			// Ownership comes from the handler's intake record for the
			// item's current revision and this order.
			intake, intakeErr := intakeOwnership(ctx, c, *task, *item, *order)
			if intakeErr != nil {
				return intakeErr
			}
			ownership = intake
			if len(ownership) > 0 {
				fmt.Printf("owns %s (from the scope confirmation)\n", strings.Join(ownership, ","))
			}
		}
		if parallel && len(ownership) == 0 && !*serial {
			return errors.New("a parallel queue entry needs ownership: pass --owns PATH, ask the database handler to record it with tt work-items scope confirm --owns PATH, or mark the entry --serial to run it alone")
		}
		if *cwd == "" && !*noNewWorktree && !*newWorktree {
			// A parallel project gives each entry its own worktree by default.
			*newWorktree = parallel
		}
		if *newWorktree {
			checkout, wdErr := os.Getwd()
			if wdErr != nil {
				return wdErr
			}
			// Refuse bad ownership before any worktree exists; the new
			// worktree is the same repository at the checkout's HEAD.
			if _, scopeErr := queueRepositoryScope(checkout, ownership); scopeErr != nil {
				return scopeErr
			}
			*cwd, err = addQueueWorktree(checkout, *item, "HEAD")
			if err != nil {
				return err
			}
			createdWorktree = *cwd
			// Until the hub confirms the entry, any failure removes the new
			// worktree so a retry can create it again.
			defer func() {
				if createdWorktree != "" {
					_ = exec.Command("git", "-C", checkout, "worktree", "remove", "--force", createdWorktree).Run()
				}
			}()
			fmt.Printf("worktree %s\n", *cwd)
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
		req.ItemID, req.OrderMessageSeq, req.Template, req.Host, req.Cwd, req.Repository, req.Ownership, req.Serial = *item, *order, *template, spawn.Host(), *cwd, repository, ownership, *serial
		if repository != "" {
			req.BaseCommit, err = queueGitCommit(*cwd)
			if err != nil {
				return err
			}
		}
	case "remove", "reorder", "release", "replace-lead", "accept", "scope", "fail", "integrated", "rebind", "requeue":
		if !validTeamQueueEntryID(*entry) {
			return errors.New(sub + " requires --entry tqe_ID")
		}
		if sub == "scope" && *newWorktree && (*cwd != "" || *noNewWorktree) {
			return errors.New("usage: tt team queue scope --entry tqe_ID [--owns PATH...] --new-worktree")
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
			realWorktree, err := acceptedWorktree(*worktree, q.Repository)
			if err != nil {
				return err
			}
			item, err := c.GetWorkItem(ctx, *task, q.ItemID)
			if err != nil {
				return err
			}
			if item.Status != "done" {
				return errors.New("handler acceptance requires the saved done item")
			}
			base, err := acceptanceBase(ctx, c, *task, q.ItemID, e.agent, e.runID, q.BaseCommit, *commit)
			if err != nil {
				return err
			}
			req.Operation, req.RequestID = "accept", "queue-accept-"+q.ID
			req.HandlerAgentID, req.HandlerRunID = e.agent, e.runID
			req.Acceptance = &api.TeamIntegrationAcceptance{Repository: q.Repository, BaseCommit: base, Worktree: realWorktree, Branch: *branch, Commit: *commit, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: *acceptanceEvidence}
			if q.Acceptance == nil {
				ready, err := queueIntegrationSnapshot(ctx, api.TeamQueueEntry{Repository: q.Repository, BaseCommit: base, Acceptance: req.Acceptance}, item, api.TeamCloseRequest{})
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
		if sub == "scope" {
			ownsGiven := len(ownership) > 0
			if *newWorktree && !ownsGiven {
				// Moving keeps the entry's declaration unless --owns replaces it.
				ownership = q.Ownership
				if len(ownership) == 0 {
					return fmt.Errorf("entry %s declares no ownership; pass --owns PATH to move it to its own worktree", q.ID)
				}
			}
			if len(ownership) == 0 {
				return errors.New("usage: tt team queue scope --entry tqe_ID --owns PATH [--owns PATH...] [--new-worktree]")
			}
			req.Ownership = ownership
			if q.Repository != "" && q.Cwd != "" {
				if _, statErr := os.Stat(q.Cwd); statErr == nil {
					if _, scopeErr := queueRepositoryScope(q.Cwd, ownership); scopeErr != nil {
						return scopeErr
					}
				}
			}
			if e.agent != "" {
				// An agent names its exact run; the hub checks it is an
				// available handler, the leased handler or the item lead.
				if e.runID == "" {
					return errors.New("scope from an agent session needs its exact run")
				}
				self, selfErr := c.GetAgent(ctx, *task, e.agent)
				if selfErr != nil {
					return selfErr
				}
				if self.Role == api.AgentRoleDatabaseHandler {
					req.HandlerAgentID, req.HandlerRunID = e.agent, e.runID
				} else {
					req.LeadAgentID, req.LeadRunID = e.agent, e.runID
				}
			}
			if *newWorktree {
				// A queued entry moves off a shared checkout into its own
				// worktree at its frozen base, keeping its place and history.
				if q.State != "queued" {
					return fmt.Errorf("only a queued entry can move to its own worktree; %s is %s", q.ID, q.State)
				}
				if q.Repository == "" || q.BaseCommit == "" {
					return fmt.Errorf("entry %s has no frozen repository and base; queue a new entry with tt team queue add --new-worktree", q.ID)
				}
				if q.Host != spawn.Host() {
					return fmt.Errorf("entry %s launches on %s; move it from that host", q.ID, q.Host)
				}
				if _, statErr := os.Stat(q.Cwd); errors.Is(statErr, os.ErrNotExist) {
					// The hub saved a move whose worktree the CLI then removed:
					// recreate it; the entry already names it.
					path, repairErr := repairQueueWorktree(q, ownership)
					if repairErr != nil {
						return repairErr
					}
					fmt.Printf("worktree %s recreated at %s\n", path, q.BaseCommit)
					if !ownsGiven {
						return nil
					}
				} else if target, pathErr := queueWorktreePath(q.Cwd, q.ItemID); pathErr != nil {
					return pathErr
				} else if sameQueueDir(q.Cwd, target) {
					fmt.Printf("entry already uses its own worktree %s\n", q.Cwd)
					if !ownsGiven {
						return nil
					}
				} else {
					if _, statErr := os.Lstat(target); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
						return fmt.Errorf("queue worktree already exists: %s; remove it with git worktree remove %s, then rerun", target, target)
					}
					checkout := q.Cwd
					path, addErr := addQueueWorktree(checkout, q.ItemID, q.BaseCommit)
					if addErr != nil {
						return addErr
					}
					createdWorktree = path
					// Until the hub saves the move, any failure removes the
					// new worktree so a retry can create it again.
					defer func() {
						if createdWorktree != "" {
							_ = exec.Command("git", "-C", checkout, "worktree", "remove", "--force", createdWorktree).Run()
						}
					}()
					fmt.Printf("worktree %s\n", path)
					repository, scopeErr := queueRepositoryScope(path, ownership)
					if scopeErr != nil {
						return scopeErr
					}
					if repository != q.Repository {
						return fmt.Errorf("the new worktree is in repository %s, not the entry's %s", repository, q.Repository)
					}
					req.Cwd, req.Repository = path, repository
				}
			}
		}
		if sub == "rebind" || sub == "requeue" {
			if e.agent != "" {
				// An agent names its exact run; the hub checks it is an
				// available database handler (the leased one for a running
				// entry). A lead or team member may not move its own entry.
				self, selfErr := c.GetAgent(ctx, *task, e.agent)
				if selfErr != nil {
					return selfErr
				}
				if e.runID == "" || self.Role != api.AgentRoleDatabaseHandler {
					return fmt.Errorf("only the owner or a database handler may %s a team queue entry", sub)
				}
				req.HandlerAgentID, req.HandlerRunID = e.agent, e.runID
			}
		}
		if sub == "rebind" {
			// After a scope amendment: move the queued or running entry, and
			// a running team's bindings, to the item's current revision.
			if *source < 1 {
				return errors.New("usage: tt team queue rebind --entry tqe_ID --source SEQ (SEQ: the amendment's message)")
			}
			current, itemErr := c.GetWorkItem(ctx, *task, q.ItemID)
			if itemErr != nil {
				return itemErr
			}
			if current.Revision == q.ItemRevision {
				return fmt.Errorf("entry %s is already bound to item revision %d", q.ID, q.ItemRevision)
			}
			req.ItemRevision, req.SourceMessageSeq = current.Revision, *source
		}
		if sub == "requeue" {
			// Retry a released failed entry as a new attempt of its item. The
			// attempt copies the failed entry unless a flag overrides it.
			templateGiven := false
			fs.Visit(func(f *flag.Flag) { templateGiven = templateGiven || f.Name == "template" })
			if templateGiven {
				if *template != "planned" && *template != "small" {
					return errors.New("usage: tt team queue requeue --entry tqe_ID [--order SEQ] [--template planned|small] [--owns PATH...] [--cwd DIR]")
				}
				req.Template = *template
			}
			if *order < 0 {
				return errors.New("usage: tt team queue requeue --entry tqe_ID [--order SEQ] [--template planned|small] [--owns PATH...] [--cwd DIR]")
			}
			req.OrderMessageSeq, req.Ownership = *order, ownership
			if *cwd != "" {
				// A new checkout of the same repository, at its HEAD.
				if q.Host != spawn.Host() {
					return fmt.Errorf("entry %s launches on %s; requeue it with --cwd from that host", q.ID, q.Host)
				}
				abs, absErr := filepath.Abs(*cwd)
				if absErr != nil {
					return absErr
				}
				if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
					return fmt.Errorf("project cwd is not a directory: %s", abs)
				}
				owned := ownership
				if len(owned) == 0 {
					owned = q.Ownership
				}
				repository, scopeErr := queueRepositoryScope(abs, owned)
				if scopeErr != nil {
					return scopeErr
				}
				if repository != q.Repository {
					return fmt.Errorf("%s is in repository %q, not the entry's %q", abs, repository, q.Repository)
				}
				req.Cwd = abs
				if repository != "" {
					if req.BaseCommit, err = queueGitCommit(abs); err != nil {
						return err
					}
				}
			} else if len(ownership) > 0 && q.Repository != "" && q.Cwd != "" {
				if _, statErr := os.Stat(q.Cwd); statErr == nil {
					if _, scopeErr := queueRepositoryScope(q.Cwd, ownership); scopeErr != nil {
						return scopeErr
					}
				}
			}
		}
		if sub == "fail" {
			if strings.TrimSpace(*reason) == "" {
				return errors.New("usage: tt team queue fail --entry tqe_ID --reason TEXT")
			}
			req.Failure = "Owner failed this entry: " + strings.TrimSpace(*reason)
			req.RequestID = fmt.Sprintf("queue-owner-fail-%s-%d", q.ID, q.Revision)
		}
		if sub == "integrated" {
			if *commit == "" {
				return errors.New("usage: tt team queue integrated --entry tqe_ID --commit SHA [--evidence TEXT]")
			}
			base := q.BaseCommit
			if q.Acceptance != nil {
				base = q.Acceptance.BaseCommit
			}
			changed, err := ownerIntegratedChanges(ctx, q.Repository, base, *commit)
			if err != nil {
				return err
			}
			req.Operation, req.RequestID = "owner_integrated", "queue-integrated-"+q.ID
			req.OwnerIntegrationCommit, req.OwnerIntegrationEvidence = *commit, *acceptanceEvidence
			req.ChangedFiles = ownedChanges(changed, q.Ownership)
		}
		if sub == "release" {
			var unlock func()
			req, unlock, err = queueReleaseRequest(ctx, c, *hub, *task, q, false)
			if err != nil {
				return err
			}
			defer unlock()
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
		return errors.New(teamQueueUsage)
	}
	result, err := c.TeamQueueAction(ctx, *task, req)
	if err != nil && sub == "scope" && req.Cwd != "" {
		// A lost response may follow a saved move: keep the worktree the
		// entry now names.
		if saved, getErr := c.GetTeamQueueEntry(ctx, *task, req.EntryID); getErr == nil && saved.Cwd == req.Cwd {
			result, err = saved, nil
		}
	}
	if err != nil {
		return err
	}
	if sub == "scope" && req.Cwd != "" && result.Cwd != req.Cwd {
		return errors.New("the hub did not save the new worktree; update the hub before moving entries")
	}
	createdWorktree = "" // The saved entry now names it.
	if *jsonOut {
		printJSON(result)
	} else {
		if sub == "limit" {
			fmt.Printf("concurrency limit=%s\n", queueLimitText(int(result.Revision)))
		} else if sub == "rebind" {
			fmt.Printf("rebind %s %s%s (%s)\n", result.ID, result.ItemID, queueAttemptText(result), result.State)
		} else if sub == "requeue" {
			fmt.Printf("requeue %s %s at %d item-revision=%d%s\n", result.ID, result.ItemID, result.Position, result.ItemRevision, queueAttemptText(result))
		} else {
			fmt.Printf("%s %s %s at %d\n", sub, result.ID, result.ItemID, result.Position)
		}
	}
	return nil
}

// ownerIntegratedChanges checks that an owner-integrated commit is in the
// entry's frozen repository and descends from its base, then lists the files
// it changed against their merge base.
func ownerIntegratedChanges(ctx context.Context, repository, base, commit string) ([]string, error) {
	if repository == "" || base == "" {
		return nil, errors.New("the entry has no frozen repository and base; release it with tt team queue release instead")
	}
	if len(commit) != 40 && len(commit) != 64 {
		return nil, errors.New("--commit must be a full commit SHA")
	}
	if err := exec.CommandContext(ctx, "git", "--git-dir="+repository, "cat-file", "-e", commit+"^{commit}").Run(); err != nil {
		return nil, fmt.Errorf("commit %s is not in the entry's repository %s", commit, repository)
	}
	if err := exec.CommandContext(ctx, "git", "--git-dir="+repository, "merge-base", "--is-ancestor", base, commit).Run(); err != nil {
		return nil, fmt.Errorf("commit %s does not descend from the entry base %s", commit, base)
	}
	return queueChangedFiles(ctx, repository, base, commit)
}

// intakeOwnership reads the ownership the database handler recorded at scope
// confirmation for the item's current revision and the order. None is nil.
func intakeOwnership(ctx context.Context, c *api.Client, task, item string, order int64) ([]string, error) {
	current, err := c.GetWorkItem(ctx, task, item)
	if err != nil {
		return nil, err
	}
	confirmation, err := c.GetWorkOrderScopeConfirmation(ctx, task, item, current.Revision, order)
	var response *api.HTTPError
	if errors.As(err, &response) && response.Status == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return confirmation.Ownership, nil
}

// queueReleaseRequest builds the release of a failed entry for the owner's
// command and the runner. An uncertain spawn needs this host's launch lock,
// an unchanged entry and proof that no session or registration exists for
// it. When locked is false it takes the lock and returns its unlock, which
// the caller holds until the release is sent.
func queueReleaseRequest(ctx context.Context, c *api.Client, hub, task string, q api.TeamQueueEntry, locked bool) (api.TeamQueueRequest, func(), error) {
	req := api.TeamQueueRequest{RequestID: "queue-release-" + q.ID, Operation: "release", EntryID: q.ID, ExpectedRevision: q.Revision}
	unlock := func() {}
	var journal teamLaunchJournal
	if len(q.LaunchJSON) > 0 && json.Unmarshal(q.LaunchJSON, &journal) != nil {
		return req, unlock, errors.New("failed queue launch journal is invalid")
	}
	uncertain := false
	for _, m := range journal.Members {
		if m.State == "uncertain" {
			uncertain = true
		}
	}
	if !uncertain {
		return req, unlock, nil
	}
	if q.Host != spawn.Host() {
		return req, unlock, errors.New("uncertain queue release must run on the saved launch host")
	}
	if !locked {
		lock, lockErr := queueLaunchLock(hub, task, q.ID)
		if lockErr != nil {
			return req, unlock, lockErr
		}
		unlock = func() { unlockQueueLaunch(lock) }
	}
	fail := func(err error) (api.TeamQueueRequest, func(), error) {
		unlock()
		return req, func() {}, err
	}
	fresh, fetchErr := c.GetTeamQueueEntry(ctx, task, q.ID)
	if fetchErr != nil {
		return fail(fetchErr)
	}
	if fresh.Revision != q.Revision || fresh.State != q.State || !bytes.Equal(fresh.LaunchJSON, q.LaunchJSON) {
		return fail(errors.New("queue entry changed while acquiring the host launch lock"))
	}
	sessions, sessionErr := localSessions(ctx)
	if sessionErr != nil {
		return fail(sessionErr)
	}
	proof := &api.TeamQueueReleaseProof{TaskID: task, EntryID: q.ID, ItemID: q.ItemID, Host: q.Host}
	digest := sha256.Sum256(q.LaunchJSON)
	proof.LaunchDigest = hex.EncodeToString(digest[:])
	for _, member := range journal.Members {
		if member.State != "uncertain" {
			continue
		}
		for _, session := range sessions {
			if (session.Hub == hub && session.Task == task && session.Agent == member.Fields.AgentID) || session.Name == member.Fields.Name {
				return fail(fmt.Errorf("uncertain member %s still has an owned or name-conflicting session", member.Fields.AgentID))
			}
		}
		_, agentErr := c.GetAgent(ctx, task, member.Fields.AgentID)
		if agentErr == nil {
			continue
		}
		var response *api.HTTPError
		if !errors.As(agentErr, &response) || response.Status != 404 {
			return fail(agentErr)
		}
		proof.Members = append(proof.Members, api.TeamQueueReleaseMember{AgentID: member.Fields.AgentID, RunID: member.RunID, Name: member.Fields.Name})
	}
	if len(proof.Members) > 0 {
		req.ReleaseProof = proof
		req.SessionsChecked = true
		req.Host = q.Host
	}
	return req, unlock, nil
}

// queueAttemptText is the list suffix for an entry that is a later attempt of
// its item or was rebound to an amended item revision.
// The verification host's lock and waitlist: the JSON file
// scripts/verify-matrix-host-lock.mjs keeps on the machine that runs the
// matrix (docs/objective-verification.md, "Host lock and waitlist"). The list
// reads it without the file's mutex; updates land by rename.
type matrixHostEntry struct {
	PID         int64  `json:"pid"`
	Kind        string `json:"kind"`
	Item        string `json:"item"`
	Agent       string `json:"agent"`
	Priority    string `json:"priority"`
	RequestedAt string `json:"requestedAt"`
}

type matrixWaitlist struct {
	Version int               `json:"version"`
	Host    string            `json:"host"`
	Holder  *matrixHostEntry  `json:"holder"`
	Waiters []matrixHostEntry `json:"waiters"`
}

var matrixLocalHost = os.Hostname

// matrixHostLabel is the first DNS label in lower case, so Stephens-Mini,
// stephens-mini and Stephens-Mini.local name one machine.
func matrixHostLabel(host string) string {
	return strings.ToLower(strings.SplitN(strings.TrimSpace(host), ".", 2)[0])
}

// matrixName keeps a value from the file on one line.
func matrixName(value string) string {
	if value == "" {
		return "unknown"
	}
	return strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return '_'
		}
		return r
	}, value)
}

// readMatrixWaitlist returns this machine's waitlist, or nil with one line
// saying why none is shown. An absent file is a free host: nil and no line.
func readMatrixWaitlist() (*matrixWaitlist, string) {
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if path != "" && !filepath.IsAbs(path) {
		return nil, "matrix host: TAILTERM_MATRIX_HOST_LOCK must be an absolute path; waitlist not shown"
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, ""
		}
		path = filepath.Join(home, ".local/state/tailterm-matrix/host.json")
	}
	unusable := func(reason string) (*matrixWaitlist, string) {
		return nil, fmt.Sprintf("matrix host: lock file %s unusable (%s); waitlist not shown", path, reason)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return unusable("unreadable")
	}
	var list matrixWaitlist
	if json.Unmarshal(raw, &list) != nil {
		return unusable("not JSON")
	}
	if list.Version != 1 {
		return unusable("unknown version")
	}
	local, _ := matrixLocalHost()
	if matrixHostLabel(list.Host) != matrixHostLabel(local) {
		return nil, fmt.Sprintf("matrix host: lock file %s belongs to host %s; waitlist not shown", path, matrixName(list.Host))
	}
	return &list, ""
}

// waitLines is one line per run of the entry's item that waits for the
// verification host, with its place in the file's order.
func (m *matrixWaitlist) waitLines(q api.TeamQueueEntry) []string {
	if m == nil || q.State == "failed" || q.ItemID == "" || matrixHostLabel(q.Host) != matrixHostLabel(m.Host) {
		return nil
	}
	holder := "none"
	if h := m.Holder; h != nil {
		holder = fmt.Sprintf("%s/%s/pid %d", matrixName(h.Item), matrixName(h.Agent), h.PID)
	}
	var lines []string
	for i, w := range m.Waiters {
		if w.Item == q.ItemID {
			lines = append(lines, fmt.Sprintf("  waiting-for-matrix position=%d of %d holder=%s priority=%s kind=%s agent=%s since=%s", i+1, len(m.Waiters), holder, matrixName(w.Priority), matrixName(w.Kind), matrixName(w.Agent), matrixName(w.RequestedAt)))
		}
	}
	return lines
}

func queueAttemptText(q api.TeamQueueEntry) string {
	text := ""
	if q.Attempt > 1 {
		text += fmt.Sprintf(" attempt %d", q.Attempt)
		if q.RetryOf != "" {
			text += " retry-of=" + q.RetryOf
		}
	}
	if n := len(q.Rebinds); n > 0 {
		text += fmt.Sprintf(" rebound %d→%d", q.Rebinds[0].FromItemRevision, q.Rebinds[n-1].ToItemRevision)
	}
	return text
}

// queueArmText is the list suffix for an entry leased under a handler arm
// policy (docs/handler-ab.md).
func queueArmText(a *api.TeamQueueHandlerArm) string {
	if a == nil {
		return ""
	}
	fallback := "no"
	if a.Fallback {
		fallback = or(a.FallbackReason, "yes")
	}
	return fmt.Sprintf(" arm=%s drawn=%s fallback=%s", a.Arm, a.DrawnArm, fallback)
}
