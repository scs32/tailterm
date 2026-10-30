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
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func deploymentBriefing() string {
	return "\nYou are the persistent project deployment agent. Consume only handler-saved release jobs with tt deployment list. Never read or mutate work-item APIs. Automatically execute accepted exact-SHA independently verified candidates under the saved release policy, in the dedicated tasks-hub checkout, with private host credential references. Claim the project fence before any effect; keep a durable host journal and lock. Revalidate exact agent/run/project generation before every effect. A stale, paused, retired or rotated run cannot execute. Never take over ambiguous claimed or blocked jobs; ask the handler to reconcile them. Integrate the full candidate series; refuse conflicts, ref races and SHA mismatches. Import changed integrated-SHA matrix evidence through the handler before publication. Keep target backup pins and rollback artifacts; preserve database writes on rollback. Escalate failed release once. Write the immutable final receipt through tt deployment finish. Remain available after item-team cleanup; provisioning and live activation require their own recorded order."
}
func cmdDeployment(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt deployment setup|list|get|compat-list|compat-get|handler|progress|enqueue|claim|unclaim|check|verification|merged|batch-open|batch-add|batch-drop|finish|block|inputs|reconcile|set-aside|hold|unhold|refuse|supersede|retry|hand-release|hand-releases|window")
	}
	if args[0] == "hand-release" || args[0] == "hand-releases" {
		return cmdHandRelease(e, args)
	}
	if args[0] == "window" {
		return cmdReleaseWindow(e, args[1:])
	}
	if args[0] == "serve" {
		if len(args) != 3 || args[1] != "--config" {
			return errors.New("usage: tt deployment serve --config PRIVATE_PATH")
		}
		return serveRunner("", args[2])
	}
	if args[0] == "template" {
		printJSON(map[string]any{"role": api.AgentRoleDeployment, "runtime": "generic", "name": "deployer", "command": "tt deployment serve --config /private/deployment.json", "instructions": deploymentBriefing()})
		return nil
	}
	if args[0] == "setup" {
		return deploymentSetup(e, args[1:])
	}
	fs := flag.NewFlagSet("deployment "+args[0], flag.ContinueOnError)
	view := fs.String("view", "active", "active or settled release summaries")
	limit := fs.Int("limit", 50, "release page size (1-200)")
	after := fs.String("after", "", "opaque release continuation cursor")
	snapshot := fs.String("snapshot", "", "release ledger snapshot token")
	entry := fs.String("entry", "", "accepted queue entry (handler enqueue, retry), or the queue entry an entry hold is saved on (hold, unhold)")
	job := fs.String("job", "", "saved release job")
	key := fs.String("request-id", "", "stable retry identity")
	generation := fs.Int64("generation", 0, "expected job generation")
	commit := fs.String("commit", "", "exact integrated commit")
	planFile := fs.String("plan-file", "", "integrated matrix plan JSON (handler import)")
	file := fs.String("file", "", "receipt, reconcile or set-aside record JSON file")
	batchDropped := fs.String("batch-dropped", "", "JSON file {\"dropped\":[{\"jobId\",\"reason\"}],\"omitted\":N}: the jobs the lead's batch passed over (finish)")
	released := fs.String("released-commit", "", "tasks-hub commit that carried a hand release (supersede)")
	releaseName := fs.String("release", "", "optional release name; must equal the hand release record's (supersede)")
	handRelease := fs.String("hand-release", "", "recorded hand release ID from tt deployment hand-release (supersede)")
	repo := fs.String("repo", ".", "repository whose tasks-hub must contain the hand release (supersede)")
	reason := fs.String("reason", "", "why a new job is expected to pass (retry), why the job is held or its hold released (hold, unhold), or the runner's stable failure tag (refuse)")
	untilItems := fs.String("until-items", "", "comma-separated work items whose release lifts the hold (hold)")
	notBefore := fs.String("not-before", "", "RFC 3339 time the hub lifts the hold at (hold; with --job or --entry)")
	phase := fs.String("phase", "", "runner phase to report (progress)")
	step := fs.String("step", "", "runner step within the phase (progress), or the step that failed (refuse)")
	target := fs.String("target", "", "the target of the step that failed (refuse)")
	revertOutcome := fs.String("revert-outcome", "", "committed or failed: the tasks-hub revert of a job blocked after publication with no receipt (block)")
	revertCommit := fs.String("revert-commit", "", "the revert commit the runner made, required when committed (block)")
	revertBug := fs.String("revert-bug-request", "", "the request ID of the handler request filing the revert's bug (block)")
	wait := fs.String("wait", "", "the wait the poll ended in: matrix, inputs, host_lock, fence, owner_hold or transport (progress)")
	pid := fs.Int64("pid", 0, "process the wait names, such as the matrix run (progress)")
	withdraw := fs.Bool("withdraw", false, "withdraw this run's progress report (progress)")
	codeState := fs.String("code-state", "", "the runner's code state: current, draining, restart or refused (progress)")
	codeReason := fs.String("code-reason", "", "the refused code state's reason (progress)")
	codeCheckout := fs.String("code-checkout", "", "restored or not-restored, for code reason load-failed (progress)")
	codeLoaded := fs.String("code-loaded", "", "12-hex prefix of the loaded scripts digest (progress)")
	codePublished := fs.String("code-published", "", "12-hex prefix of the published scripts digest (progress)")
	codeSince := fs.String("code-since", "", "RFC 3339 time the runner first saw this code state (progress)")
	var restored stringListFlag
	fs.Var(&restored, "restored", "TARGET=RELEASE the refused job's target runs again; one per target in its receipt (retry, repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Checked before any hub call: a revert belongs to a block only.
	hasRevert := *revertOutcome != "" || *revertCommit != "" || *revertBug != ""
	if hasRevert && args[0] != "block" {
		return errors.New("--revert-outcome, --revert-commit and --revert-bug-request are block flags")
	}
	if *batchDropped != "" && args[0] != "finish" {
		return errors.New("--batch-dropped is a finish flag")
	}
	var retry *api.ReleaseRetry
	if args[0] == "retry" {
		// Checked before any hub call, like supersede's coverage.
		var err error
		if retry, err = deploymentRetry(*entry, *job, *reason, restored); err != nil {
			return err
		}
	}
	var hold *api.ReleaseHold
	if args[0] == "hold" || args[0] == "unhold" {
		// Checked before any hub call, like retry's record.
		var err error
		if hold, err = deploymentHold(args[0], *entry, *job, *reason, *untilItems, *notBefore, *generation); err != nil {
			return err
		}
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	if args[0] == "compat-list" {
		// Bare compatibility output is the historical FULL array. An explicit
		// paging flag opts into the strict envelope used by current consumers.
		paged := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "view" || f.Name == "limit" || f.Name == "after" || f.Name == "snapshot" {
				paged = true
			}
		})
		if *job != "" {
			return errors.New("compat-list does not accept --job")
		}
		if paged {
			out, err := c.CompatibilityReleasesPage(ctx, e.task, api.ReleaseListOptions{View: *view, Limit: *limit, After: *after, Snapshot: *snapshot})
			if err == nil {
				printJSON(out)
			}
			return err
		}
		out, err := c.CompatibilityReleases(ctx, e.task)
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if args[0] == "compat-get" {
		if *job == "" {
			return errors.New("compat-get needs --job")
		}
		// Native detail+snapshot is unsupported; never silently discard it.
		paging := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "view" || f.Name == "limit" || f.Name == "after" || f.Name == "snapshot" {
				paging = true
			}
		})
		if paging {
			return errors.New("compat-get does not accept paging or snapshot flags")
		}
		out, err := c.CompatibilityRelease(ctx, e.task, *job)
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if args[0] == "list" {
		out, err := c.ReleasesPage(ctx, e.task, api.ReleaseListOptions{View: *view, Limit: *limit, After: *after, Snapshot: *snapshot})
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if args[0] == "get" {
		if *job == "" {
			return errors.New("get needs --job")
		}
		out, err := c.Release(ctx, e.task, *job)
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if args[0] == "handler" {
		// The deployer addresses its handler requests to this exact agent.
		out, err := c.ReleaseHandler(ctx, e.task, e.agent, e.runID)
		if err == nil {
			printJSON(map[string]string{"id": out.ID, "name": out.Name})
		}
		return err
	}
	if args[0] == "progress" {
		// The release runner's own progress, which the hub's hung-runner check
		// reads: --phase reports, --withdraw (or a bare --request-id, the form
		// a tt from before this command sends) withdraws, neither reads.
		req := api.ReleaseRequest{AgentID: e.agent, RunID: e.runID}
		// The code state rides on a report only: the hub checks its values.
		code := *codeState != "" || *codeReason != "" || *codeCheckout != "" || *codeLoaded != "" || *codePublished != "" || *codeSince != ""
		if code && (*codeState == "" || *phase == "" || *withdraw) {
			return errors.New("progress --code-* flags need --code-state and --phase, without --withdraw")
		}
		switch {
		case *phase != "":
			if *withdraw {
				return errors.New("progress takes --phase or --withdraw, not both")
			}
			req.Progress = &api.ReleaseProgress{JobID: *job, Phase: *phase, Step: *step, Wait: api.ReleaseProgressWait{Kind: *wait, PID: *pid}}
			if code {
				req.Progress.Code = &api.ReleaseProgressCode{State: *codeState, Reason: *codeReason, Checkout: *codeCheckout, Loaded: *codeLoaded, Published: *codePublished, Since: *codeSince}
			}
		case *withdraw || *key != "":
			if req.RequestID = *key; req.RequestID == "" {
				req.RequestID = "progress-withdraw-" + e.runID
			}
		case *job != "" || *step != "" || *wait != "" || *pid != 0:
			return errors.New("progress needs --phase to report")
		}
		if (req.Progress != nil || req.RequestID != "") && (e.agent == "" || e.runID == "") {
			return errors.New("exact agent/run required")
		}
		out, err := c.ReleaseProgress(ctx, e.task, req)
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if *key == "" || e.agent == "" || e.runID == "" {
		return errors.New("exact agent/run and request-id required")
	}
	req := api.ReleaseRequest{RequestID: *key, Operation: args[0], AgentID: e.agent, RunID: e.runID, EntryID: *entry, JobID: *job, ExpectedGeneration: *generation, IntegratedCommit: *commit, Retry: retry, Hold: hold}
	if args[0] == "refuse" && (*reason != "" || *step != "" || *target != "") {
		// The hub checks the refusal; without these flags the refuse is the
		// bare one an older runner sends.
		req.Refusal = &api.ReleaseRefusal{Step: *step, Target: *target, Reason: *reason}
	}
	if hasRevert {
		// The hub checks the revert; without these flags the block is the
		// bare one an older runner sends, and its revert stays unknown.
		req.Revert = &api.ReleaseRevert{Outcome: *revertOutcome, Commit: *revertCommit, BugRequestID: *revertBug}
	}
	if args[0] == "supersede" {
		j, err := c.Release(ctx, e.task, *job)
		if err != nil {
			return err
		}
		if *handRelease == "" {
			return errors.New("supersede needs --hand-release naming a recorded hand release")
		}
		if err = reportedCoverage(*repo, j, *released); err != nil {
			return err
		}
		req.Supersession = &api.ReleaseSupersession{ReleasedCommit: *released, Release: *releaseName, HandReleaseID: *handRelease}
	}
	if args[0] == "finish" || args[0] == "verification" || args[0] == "inputs" || args[0] == "reconcile" || args[0] == "set-aside" {
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		if args[0] == "inputs" {
			var binding struct {
				Version int    `json:"version"`
				JobID   string `json:"jobId"`
				Commit  string `json:"commit"`
			}
			if err = json.Unmarshal(b, &binding); err != nil {
				return err
			}
			if binding.Version != 1 || binding.JobID != *job || binding.Commit != *commit {
				return errors.New("exact job manifest binding required")
			}
			req.InputsDigest = fmt.Sprintf("%x", sha256.Sum256(b))
		} else if args[0] == "reconcile" || args[0] == "set-aside" {
			// set-aside carries the same typed incident, disposition set_aside.
			req.Reconciliation = &api.ReleaseReconciliation{}
			err = json.Unmarshal(b, req.Reconciliation)
		} else if args[0] == "finish" {
			req.Receipt = &api.ReleaseReceipt{}
			err = json.Unmarshal(b, req.Receipt)
			if err == nil && *batchDropped != "" {
				req.BatchDropped, req.BatchDroppedOmitted, err = deploymentBatchDropped(*batchDropped)
			}
		} else {
			planBytes, readErr := os.ReadFile(*planFile)
			if readErr != nil {
				return readErr
			}
			req.Plan = &api.VerificationPlan{}
			if readErr = json.Unmarshal(planBytes, req.Plan); readErr != nil {
				return readErr
			}
			req.Verification = &api.VerificationReceipt{}
			err = json.Unmarshal(b, req.Verification)
			if err == nil {
				err = verifyReceiptLogs(*req.Verification)
			}
		}
		if err != nil {
			return err
		}
	}
	out, err := c.ReleaseAction(ctx, e.task, req)
	if err == nil {
		printJSON(out)
	}
	if err == nil && hold != nil {
		// The deployer reads no board messages: the hub enforces the hold, and
		// this notice only tells the team.
		notice := holdNotice(args[0], out, hold.Reason)
		postKey := fmt.Sprintf("%s-%s-g%d", out.ID, args[0], out.Generation)
		if *job == "" {
			// An entry hold has no job or generation yet.
			postKey = fmt.Sprintf("%s-%s-%s", out.EntryID, args[0], *key)
		}
		if postErr := postHoldNotice(ctx, c, e, out, postKey, notice); postErr != nil {
			if *job == "" {
				saved := "has an entry hold"
				if args[0] == "unhold" {
					saved = "is released from its entry hold"
				}
				return fmt.Errorf("entry %s %s, but its board notice was not posted: %v; post this notice linked to item %s:\n%s", out.EntryID, saved, postErr, out.ItemID, api.RenderText(notice))
			}
			saved := "is on hold"
			if args[0] == "unhold" {
				saved = "is released from its hold"
			}
			return fmt.Errorf("job %s %s at generation %d, but its board notice was not posted: %v; post this notice linked to item %s:\n%s", out.ID, saved, out.Generation, postErr, out.ItemID, api.RenderText(notice))
		}
	}
	return err
}

// deploymentBatchDropped reads a finish's batch exclusions file (wi_861f):
// {"dropped": [{"jobId", "reason"}], "omitted": N}. Unknown fields are refused;
// the hub checks the ids, reasons and bounds.
func deploymentBatchDropped(path string) ([]api.ReleaseBatchExclusion, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var in struct {
		Dropped []api.ReleaseBatchExclusion `json:"dropped"`
		Omitted int                         `json:"omitted"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err != nil {
		return nil, 0, fmt.Errorf("--batch-dropped: %w", err)
	}
	if len(in.Dropped) == 0 && in.Omitted == 0 {
		return nil, 0, errors.New("--batch-dropped names no job")
	}
	return in.Dropped, in.Omitted, nil
}

// deploymentHold builds the handler's hold or unhold record: a one-line
// reason, and for a hold the items whose release lifts it or the time the
// hub lifts it at. The hub checks that the items exist and are not yet all
// released, and that the time is still ahead. With --entry instead of --job
// it is an entry hold, saved before the entry's next job exists so that job
// is created held.
func deploymentHold(operation, entry, job, reason, untilItems, notBefore string, generation int64) (*api.ReleaseHold, error) {
	if (job == "") == (entry == "") {
		return nil, fmt.Errorf("%s needs exactly one of --job naming the release job or --entry naming the queue entry whose next job is held", operation)
	}
	if entry != "" && untilItems != "" {
		return nil, fmt.Errorf("%s --entry does not accept --until-items: an entry hold is lifted by unhold or its --not-before time", operation)
	}
	if entry != "" && generation != 0 {
		return nil, fmt.Errorf("%s --entry does not accept --generation: the entry has no job yet", operation)
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 512 || strings.ContainsAny(reason, "\x00\n\r") {
		return nil, fmt.Errorf("%s needs --reason: one line, at most 512 characters", operation)
	}
	out := &api.ReleaseHold{Reason: reason}
	if notBefore != "" {
		if operation == "unhold" {
			return nil, errors.New("unhold does not accept --not-before: it releases the hold now")
		}
		if untilItems != "" {
			return nil, errors.New("hold takes --until-items or --not-before, not both")
		}
		if _, err := time.Parse(time.RFC3339, notBefore); err != nil {
			return nil, fmt.Errorf("hold --not-before needs an RFC 3339 time such as 2026-10-29T07:00:00-07:00, got %q", notBefore)
		}
		out.NotBefore = notBefore
	}
	if untilItems == "" {
		return out, nil
	}
	if operation == "unhold" {
		return nil, errors.New("unhold does not accept --until-items: it releases the hold now")
	}
	seen := map[string]bool{}
	for _, id := range strings.Split(untilItems, ",") {
		id = strings.TrimSpace(id)
		if !api.ValidID(id, "wi") {
			return nil, fmt.Errorf("hold --until-items needs comma-separated work item IDs, got %q", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("hold --until-items names item %s twice", id)
		}
		seen[id] = true
		out.UntilItems = append(out.UntilItems, id)
	}
	if len(out.UntilItems) > 16 {
		return nil, errors.New("hold --until-items accepts at most 16 items")
	}
	return out, nil
}

// holdNotice is the board notice for a saved hold or unhold of job j, or of
// an entry hold when j has no ID.
func holdNotice(operation string, j api.ReleaseJob, reason string) api.Envelope {
	if j.ID == "" {
		return entryHoldNotice(operation, j, reason)
	}
	commit := j.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	what := fmt.Sprintf("Release job %s (item %s, commit %s)", j.ID, j.ItemID, commit)
	refs := map[string]string{"release-job": j.ID, "item": j.ItemID}
	if operation == "unhold" {
		return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A release job hold was released", Refs: refs,
			Body: api.EnvelopeBody{Text: fmt.Sprintf("%s is verified again in its original queue position; the deployer can claim it. Reason: %s", what, reason)}}
	}
	condition := "It stays on hold until tt deployment unhold."
	holder := ""
	if j.Hold != nil {
		if len(j.Hold.UntilItems) > 0 {
			condition = "The hub lifts the hold when every one of these items is released: " + strings.Join(j.Hold.UntilItems, ", ") + "; tt deployment unhold releases it earlier."
		}
		if j.Hold.NotBefore != "" {
			condition = "The hub lifts the hold at " + j.Hold.NotBefore + "; tt deployment unhold releases it earlier."
		}
		if j.Hold.AgentID != "" {
			holder = " Held by " + j.Hold.AgentID + "."
		}
	}
	return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A release job is on hold", Refs: refs,
		Body: api.EnvelopeBody{Text: fmt.Sprintf("%s is on an owner hold: the deployer will not claim it and goes on to the next verified job. Reason: %s.%s %s", what, strings.TrimRight(reason, "."), holder, condition)}}
}

// entryHoldNotice is the board notice for a saved entry hold or its release.
func entryHoldNotice(operation string, j api.ReleaseJob, reason string) api.Envelope {
	what := fmt.Sprintf("Release of item %s (entry %s)", j.ItemID, j.EntryID)
	refs := map[string]string{"entry": j.EntryID, "item": j.ItemID}
	if operation == "unhold" {
		return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "An entry release hold was released", Refs: refs,
			Body: api.EnvelopeBody{Text: fmt.Sprintf("%s is no longer on an entry hold: the job acceptance creates is created verified. Reason: %s", what, reason)}}
	}
	condition := "It stays on hold until tt deployment unhold."
	holder := ""
	if j.Hold != nil {
		if j.Hold.NotBefore != "" {
			condition = "The hub lifts it at " + j.Hold.NotBefore + "."
		}
		if j.Hold.AgentID != "" {
			holder = " Held by " + j.Hold.AgentID + "."
		}
	}
	return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A release is on hold before acceptance", Refs: refs,
		Body: api.EnvelopeBody{Text: fmt.Sprintf("%s is on an owner hold before acceptance: the job acceptance creates is created held. Reason: %s.%s %s", what, strings.TrimRight(reason, "."), holder, condition)}}
}

// postHoldNotice posts the notice once per saved transition, under key,
// linked to the job's item at its current revision and to the item's work
// order.
func postHoldNotice(ctx context.Context, c *api.Client, e env, j api.ReleaseJob, key string, notice api.Envelope) error {
	item, err := c.GetWorkItem(ctx, e.task, j.ItemID)
	if err != nil {
		return err
	}
	post := api.PostMessageRequest{Envelope: &notice, Text: api.RenderText(notice), AgentID: e.agent, RunID: e.runID, RequestID: key,
		WorkItems: []api.MessageWorkItem{{ItemTaskID: e.task, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
	if j.OrderMessageSeq > 0 {
		post.WorkOrderMessage = &api.MessageReference{TaskID: e.task, Seq: j.OrderMessageSeq}
	}
	_, err = c.PostMessage(ctx, e.task, post)
	return err
}

// deploymentRetry builds the handler's retry of a refused or rolled-back job:
// a one-line reason, and for each target in that job's receipt the release it
// runs again. The hub checks the list against the receipt.
func deploymentRetry(entry, job, reason string, restored []string) (*api.ReleaseRetry, error) {
	if entry == "" || job == "" {
		return nil, errors.New("retry needs --entry and --job naming the entry's refused release job")
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 512 || strings.ContainsAny(reason, "\x00\n\r") {
		return nil, errors.New("retry needs --reason: one line, at most 512 characters, saying why a new job is expected to pass")
	}
	out := &api.ReleaseRetry{Reason: reason}
	seen := map[string]bool{}
	for _, value := range restored {
		target, release, ok := strings.Cut(value, "=")
		if !ok || release == "" || len(release) > 512 || strings.ContainsAny(release, "\x00\n\r") {
			return nil, fmt.Errorf("retry needs --restored as TARGET=RELEASE, got %q", value)
		}
		if target != "hub" && target != "bridge" && target != "mini" && target != "tailos" {
			return nil, fmt.Errorf("retry --restored names unknown target %q: use hub, bridge, mini or tailos", target)
		}
		if seen[target] {
			return nil, fmt.Errorf("retry --restored names target %s twice", target)
		}
		seen[target] = true
		out.Restored = append(out.Restored, api.ReleaseRestoredTarget{Target: target, Release: release})
	}
	return out, nil
}

// Seams for tests: provisioning runs the checkout's own runner, and setup
// spawns the role only after provisioning succeeded.
var (
	provisionDeploymentPrerequisites = provisionPrerequisites
	spawnDeploymentAgent             = cmdSpawn
)

// deploymentSetup provisions the matrix prerequisites the runner needs to
// verify an integrated commit in the deployer's checkout (--cwd), copying
// them from --prerequisites-from, before it spawns the role. Nothing is
// spawned when either flag is missing or provisioning fails.
func deploymentSetup(e env, args []string) error {
	var source, checkout string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch strings.TrimLeft(name, "-") {
		case "prerequisites-from", "cwd":
			if !strings.HasPrefix(name, "-") {
				break
			}
			if !inline {
				if i+1 >= len(args) {
					return fmt.Errorf("%s needs a value", name)
				}
				i++
				value = args[i]
			}
			if strings.TrimLeft(name, "-") == "cwd" {
				checkout = value
				rest = append(rest, "--cwd", value)
			} else {
				source = value
			}
			continue
		}
		rest = append(rest, args[i])
	}
	if checkout == "" || source == "" {
		return errors.New("usage: tt deployment setup --cwd DEPLOYER_CHECKOUT --prerequisites-from SOURCE_CHECKOUT [spawn flags]")
	}
	if err := provisionDeploymentPrerequisites(checkout, source); err != nil {
		return err
	}
	return spawnDeploymentAgent(e, append([]string{"--role", api.AgentRoleDeployment}, rest...))
}

// serveRunner runs the checkout's release runner and passes SIGTERM and
// SIGINT on to it, so a stop through tt runs the runner's clean-stop unclaim.
// It exits with the runner's status: its exit code, or 128 plus the signal
// that killed it.
func serveRunner(checkout, config string) error {
	cmd := exec.Command("node", "scripts/release-runner.mjs", "--config", config)
	cmd.Dir = checkout
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Registered before Start, so no signal arrives while tt would still die of it.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-signals:
			// The runner may have just exited; its Wait result follows.
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return err
			}
			code := exit.ExitCode()
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				code = 128 + int(status.Signal())
			}
			return &exitError{code: code, err: err}
		}
	}
}

// provisionStderrLimit bounds the runner's stderr that a successful setup
// echoes: notes such as the go-licenses skip reason are a few lines, so 64 KiB
// keeps every real note whole and a runaway runner from flooding the terminal.
const provisionStderrLimit = 64 << 10

// provisionPrerequisites runs the checkout's release runner, which prints the
// provisioned files as JSON, or only a named reason on failure. On success its
// stderr notes follow the JSON on tt's stderr, unchanged up to
// provisionStderrLimit, then one line saying how much was left out.
func provisionPrerequisites(checkout, source string) error {
	cmd := exec.Command("node", "scripts/release-runner.mjs", "--provision-prerequisites", "--from", source)
	cmd.Dir = checkout
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		if reason := strings.TrimSpace(lines[len(lines)-1]); reason != "" {
			return fmt.Errorf("deployment prerequisites: %s", reason)
		}
		return fmt.Errorf("deployment prerequisites: %w", err)
	}
	notes := stderr.Bytes()
	if len(notes) <= provisionStderrLimit {
		_, _ = os.Stderr.Write(notes)
		return nil
	}
	// Cut at the last whole line inside the bound, so no note is half printed.
	shown := notes[:provisionStderrLimit]
	if cut := bytes.LastIndexByte(shown, '\n'); cut >= 0 {
		shown = shown[:cut+1]
	}
	_, _ = os.Stderr.Write(shown)
	fmt.Fprintf(os.Stderr, "deployment prerequisites: %d more bytes of runner stderr not shown (limit %d)\n", len(notes)-len(shown), provisionStderrLimit)
	return nil
}

// cmdHandRelease records the owner's hand release, or lists the records.
// Recording proves every covered job in a local checkout first; the hub
// refuses a request that carries an agent identity. A tailos target is recorded
// only with a verified retained rollback copy (retainedTailOSCopy).
func cmdHandRelease(e env, args []string) error {
	fs := flag.NewFlagSet("deployment "+args[0], flag.ContinueOnError)
	intervention := fs.Int64("intervention", 0, "owner release intervention message sequence")
	released := fs.String("released-commit", "", "tasks-hub commit that carried the hand release")
	releaseName := fs.String("release", "", "release name, such as the hand release's release ID")
	repo := fs.String("repo", ".", "repository whose tasks-hub must contain the hand release")
	key := fs.String("request-id", "", "stable retry identity")
	deployConfig := fs.String("deploy-config", "", "the deployer's private config; required with a tailos target")
	dist := fs.String("dist", "", "built dist-static of the released commit, to verify and retain as the TailOS rollback copy")
	var targets, jobIDs stringListFlag
	fs.Var(&targets, "target", "target the hand release shipped: hub, bridge, mini or tailos (repeatable)")
	fs.Var(&jobIDs, "job", "release job whose accepted commit the hand release carries (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(handReleaseTimeout)
	defer cancel()
	if args[0] == "hand-releases" {
		out, err := c.HandReleases(ctx, e.task)
		if err == nil {
			printJSON(out)
		}
		return err
	}
	if *key == "" || len(jobIDs) == 0 {
		return errors.New("hand-release needs --request-id and at least one --job")
	}
	tailos := false
	for _, t := range targets {
		tailos = tailos || t == "tailos"
	}
	if !tailos && *dist != "" {
		return errors.New("--dist applies only to a hand release with --target tailos")
	}
	commits := []string{}
	for _, id := range jobIDs {
		j, err := c.Release(ctx, e.task, id)
		if err != nil {
			return err
		}
		if err = reportedCoverage(*repo, j, *released); err != nil {
			return err
		}
		commits = append(commits, j.Commit)
	}
	if err = handReleaseNotBehindLedger(ctx, c, e.task, *repo, *released, targets); err != nil {
		return err
	}
	if tailos {
		if err = retainedTailOSCopy(*deployConfig, *released, *dist); err != nil {
			return err
		}
	}
	record := &api.HandRelease{InterventionSeq: *intervention, ReleasedCommit: *released, Release: *releaseName, Targets: targets, Commits: commits}
	// The record gets its own deadline: the job reads, the ledger read, the
	// release.json read and the copy above must not use up its time.
	recordCtx, recordCancel := ctxTimeout(handReleaseTimeout)
	defer recordCancel()
	out, err := c.RecordHandRelease(recordCtx, e.task, api.ReleaseRequest{RequestID: *key, AgentID: e.agent, RunID: e.runID, HandRelease: record})
	if err == nil {
		printJSON(out)
	} else if e.agent != "" || e.runID != "" {
		// The identity is still sent so the hub, not the CLI, decides.
		err = fmt.Errorf("%w; this shell carries an agent identity: record the hand release as the owner from a shell without it, e.g. env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment hand-release ...", err)
	}
	return err
}

// handReleaseTimeout bounds the reads and checks before a hand release is
// recorded, and separately the record request itself. A test shortens it.
var handReleaseTimeout = 20 * time.Second

// handReleaseNotBehindLedger refuses a hand release that the ledger has
// already moved past for any target it names: when the target's latest
// released or superseded job (the order scripts/release-targets.mjs
// releaseBaselines uses: settledAt as time, untimed first, then list order)
// carries a different commit that descends from the hand release, recording
// it would settle later and move that target's baseline back to an older
// commit. A hand release equal to or newer than the ledger's commit, or one
// git shows is not its ancestor, is recorded as before.
func handReleaseNotBehindLedger(ctx context.Context, c *api.Client, task, repo, released string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	if !fullCommit(released) {
		return errors.New("hand-release needs --released-commit as a full 40-hex commit")
	}
	type event struct {
		job, commit, at string
		time            time.Time
		timed           bool
		index           int
	}
	latest := map[string]event{}
	later := func(a, b event) bool { // b applies after a
		if a.timed != b.timed {
			return b.timed
		}
		if a.timed && !a.time.Equal(b.time) {
			return b.time.After(a.time)
		}
		return b.index > a.index
	}
	opts := api.ReleaseListOptions{View: "settled", Limit: 200}
	index := 0
	for {
		page, err := c.ReleasesPage(ctx, task, opts)
		if err != nil {
			return fmt.Errorf("cannot read the settled release ledger to check the hand release against it: %w; nothing was retained or recorded", err)
		}
		for _, j := range page.Jobs {
			index++
			e := event{job: j.ID, at: j.SettledAt, index: index}
			if j.SettledAt != "" {
				t, err := time.Parse(time.RFC3339Nano, j.SettledAt)
				if err != nil {
					return fmt.Errorf("release %s has an invalid settledAt; nothing was retained or recorded", j.ID)
				}
				e.time, e.timed = t, true
			}
			var shipped []string
			switch {
			case j.Receipt != nil && j.Receipt.Outcome == "released":
				e.commit = j.Receipt.Commit
				for _, t := range j.Receipt.Targets {
					if t.Outcome == "released" {
						shipped = append(shipped, t.Target)
					}
				}
			case j.State == "superseded" && j.Supersession != nil:
				e.commit, shipped = j.Supersession.ReleasedCommit, j.Supersession.Targets
			}
			for _, t := range shipped {
				if prev, ok := latest[t]; !ok || later(prev, e) {
					latest[t] = e
				}
			}
		}
		if page.Page.NextAfter == "" {
			break
		}
		opts.After, opts.Snapshot = page.Page.NextAfter, page.Page.Snapshot
	}
	var behind, refused []string
	for _, t := range targets {
		e, ok := latest[t]
		if !ok || e.commit == released {
			continue
		}
		if !fullCommit(e.commit) {
			return fmt.Errorf("release %s carries no full commit for %s; nothing was retained or recorded", e.job, t)
		}
		cmd := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", released, e.commit)
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot compare hand release %s with %s that release %s shipped to %s in %s: fetch tasks-hub and rerun; nothing was retained or recorded", released, e.commit, e.job, t, repo)
		}
		at := e.at
		if at == "" {
			at = "an unrecorded time"
		}
		behind = append(behind, fmt.Sprintf("%s: release %s shipped %s at %s", t, e.job, e.commit, at))
		refused = append(refused, t)
	}
	if len(behind) == 0 {
		return nil
	}
	msg := fmt.Sprintf("hand release %s is older than what the ledger already released (%s); recording it would move those baselines back. Record it without --target %s; nothing was retained or recorded", released, strings.Join(behind, "; "), strings.Join(refused, ", --target "))
	if len(refused) == len(targets) {
		msg += ". Every target is newer in the ledger: the covered jobs' change already shipped there, so ask the database handler to settle those jobs another way"
	}
	return errors.New(msg)
}

const releaseWindowUsage = "usage: tt deployment window [set --zone IANA_ZONE --start HH:MM --end HH:MM --targets hub,bridge,mini|hub,bridge,mini,tailos|tailos --reason TEXT --request-id KEY | clear --reason TEXT --request-id KEY]"

// cmdReleaseWindow reads, sets or clears the project's release quiet window
// (docs/project-deployment.md "Release quiet window"). With no operation it
// prints the window, or null. Set and clear are the owner's: the flags are
// checked before any hub call, and the identity is still sent so the hub, not
// the CLI, decides. Both print the window read back.
func cmdReleaseWindow(e env, args []string) error {
	op := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		op, args = args[0], args[1:]
	}
	if op != "" && op != "set" && op != "clear" {
		return errors.New(releaseWindowUsage)
	}
	fs := flag.NewFlagSet("deployment window "+op, flag.ContinueOnError)
	zone := fs.String("zone", "", "IANA time zone of the window's bounds, such as America/Los_Angeles (set)")
	start := fs.String("start", "", "24-hour HH:MM the window opens, inside it (set)")
	end := fs.String("end", "", "24-hour HH:MM the window ends, outside it; before start crosses midnight (set)")
	targets := fs.String("targets", "", "covered targets: hub,bridge,mini, hub,bridge,mini,tailos or tailos (set)")
	reason := fs.String("reason", "", "why the window is set or cleared, one line (set, clear)")
	key := fs.String("request-id", "", "stable retry identity (set, clear)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(releaseWindowUsage)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var w *api.ReleaseWindow
	switch op {
	case "set":
		var missing []string
		for _, name := range []string{"zone", "start", "end", "targets", "reason", "request-id"} {
			if fs.Lookup(name).Value.String() == "" {
				missing = append(missing, "--"+name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("window set needs %s", strings.Join(missing, ", "))
		}
		w = &api.ReleaseWindow{Zone: *zone, Start: *start, End: *end, Reason: *reason}
		for _, t := range strings.Split(*targets, ",") {
			if t = strings.TrimSpace(t); t != "" {
				w.Targets = append(w.Targets, t)
			}
		}
	case "clear":
		if *reason == "" || *key == "" {
			return errors.New("window clear needs --reason and --request-id")
		}
		if given["zone"] || given["start"] || given["end"] || given["targets"] {
			return errors.New("window clear takes only --reason and --request-id")
		}
		w = &api.ReleaseWindow{Reason: *reason}
	default:
		if len(given) > 0 {
			return errors.New("tt deployment window reads the window and takes no flags; use window set or window clear")
		}
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	if w != nil {
		if _, err = c.ReleaseAction(ctx, e.task, api.ReleaseRequest{RequestID: *key, Operation: "window-" + op, AgentID: e.agent, RunID: e.runID, Window: w}); err != nil {
			if e.agent != "" || e.runID != "" {
				err = fmt.Errorf("%w; this shell carries an agent identity: set the window as the owner from a shell without it, e.g. env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment window %s ...", err, op)
			}
			return err
		}
	}
	page, err := c.ReleasesPage(ctx, e.task, api.ReleaseListOptions{Limit: 1})
	if err == nil {
		printJSON(page.Window)
	}
	return err
}

// tailosReleaseURL is the default of scripts/release-probe.mjs (tailosURL).
const tailosReleaseURL = "https://tailos.tailarr.com/release.json"

// retainedTailOSCopy lets a TailOS hand release be recorded only when
// journalDirectory/tailos-dist-COMMIT holds the build TailOS serves: the next
// job's rollback redeploys that directory (scripts/release-inputs.mjs), and
// only the runner's retain step otherwise writes it. With dist it verifies
// that build and retains it as the runner does: copy to .tmp, 0700, rename.
// Errors name the config's keys, never its values.
func retainedTailOSCopy(configPath, released, dist string) error {
	if !fullCommit(released) {
		return errors.New("a tailos hand release needs --released-commit as a full 40-character commit")
	}
	if configPath == "" {
		return errors.New("a tailos hand release needs --deploy-config PRIVATE_PATH: its journalDirectory must hold the retained rollback copy tailos-dist-" + released)
	}
	var config struct {
		JournalDirectory string `json:"journalDirectory"`
		Targets          struct {
			TailOS struct {
				URL string `json:"url"`
			} `json:"tailos"`
		} `json:"targets"`
	}
	if b, err := os.ReadFile(configPath); err != nil {
		return errors.New("deploy config not readable: check the --deploy-config path")
	} else if json.Unmarshal(b, &config) != nil || config.JournalDirectory == "" {
		return errors.New("deploy config needs a journalDirectory")
	}
	url := config.Targets.TailOS.URL
	if url == "" {
		url = tailosReleaseURL
	}
	live, err := liveTailOSFiles(url, released)
	if err != nil {
		return err
	}
	retained := filepath.Join(config.JournalDirectory, "tailos-dist-"+released)
	const name = "journalDirectory/tailos-dist-"
	if dist != "" {
		if err = matchesTailOSRelease(dist, released, live); err != nil {
			return fmt.Errorf("--dist does not match the live TailOS release.json: %w; nothing was retained or recorded", err)
		}
	}
	switch _, statErr := os.Lstat(retained); {
	case statErr == nil:
		// An existing copy is kept, as in the runner, but only a valid one.
		if err = matchesTailOSRelease(retained, released, live); err != nil {
			return fmt.Errorf("the retained TailOS rollback copy %s%s does not match the live release.json: %w; remove it, build that commit's dist-static and rerun with --dist PATH", name, released, err)
		}
		return nil
	case !errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("the retained TailOS rollback copy %s%s cannot be read", name, released)
	case dist == "":
		return fmt.Errorf("no retained TailOS rollback copy %s%s: the next release could not roll TailOS back; build that commit's dist-static and rerun with --dist PATH", name, released)
	}
	tmp := retained + ".tmp"
	step := "copy"
	if err = os.RemoveAll(tmp); err == nil {
		err = copyTree(dist, tmp)
	}
	if err == nil {
		step, err = "permissions", os.Chmod(tmp, 0o700)
	}
	if err == nil {
		// The copy, not the source, is what a rollback deploys.
		step, err = "verify", matchesTailOSRelease(tmp, released, live)
	}
	if err == nil {
		step, err = "rename", os.Rename(tmp, retained)
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return retainError(released, step, err)
	}
	return nil
}

// retainError names the failed retain step and the copy by its config key,
// never by path: the path a *fs.PathError or *os.LinkError carries is under
// the private journal directory, so only its cause is kept.
func retainError(released, step string, err error) error {
	reason := err.Error()
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) {
		reason = pathErr.Err.Error()
	} else if errors.As(err, &linkErr) {
		reason = linkErr.Err.Error()
	}
	return fmt.Errorf("retaining --dist as journalDirectory/tailos-dist-%s failed at %s: %s; nothing was recorded", released, step, reason)
}

type tailosFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// liveTailOSFiles reads the live release.json once and returns its file map;
// the release must name the hand-released commit.
func liveTailOSFiles(url, released string) (map[string]tailosFile, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("deploy config targets.tailos.url is not a valid URL")
	}
	req.Header.Set("Cache-Control", "no-store")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("the live TailOS release.json could not be read")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	var manifest struct {
		Commit string                `json:"commit"`
		Files  map[string]tailosFile `json:"files"`
	}
	if err != nil || res.StatusCode != http.StatusOK || json.Unmarshal(body, &manifest) != nil || !fullCommit(manifest.Commit) || len(manifest.Files) == 0 {
		return nil, errors.New("the live TailOS release.json could not be read")
	}
	if manifest.Commit != released {
		return nil, fmt.Errorf("TailOS serves %s, not the hand-released commit %s", manifest.Commit, released)
	}
	return manifest.Files, nil
}

// matchesTailOSRelease compares a build directory with the live file map as
// scripts/release-manifest.mjs inventories it: every regular file but the
// top-level release.json, by size and sha256, none missing and none extra.
// The directory's own release.json must name the commit, because the rollback
// probe reads it after a redeploy.
func matchesTailOSRelease(dir, released string, live map[string]tailosFile) error {
	found := map[string]tailosFile{}
	var walk func(prefix string) error
	walk = func(prefix string) error {
		entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(prefix)))
		if err != nil {
			return errors.New("the directory cannot be read")
		}
		for _, entry := range entries {
			name := prefix + entry.Name()
			if entry.IsDir() {
				if err = walk(name + "/"); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("%s is not a regular file", name)
			}
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil {
				return fmt.Errorf("%s cannot be read", name)
			}
			if name == "release.json" {
				var own struct {
					Commit string `json:"commit"`
				}
				if json.Unmarshal(b, &own) != nil || own.Commit != released {
					return errors.New("its release.json does not name the released commit")
				}
				continue
			}
			sum := sha256.Sum256(b)
			found[name] = tailosFile{Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "release.json")); err != nil {
		return errors.New("it has no release.json")
	}
	var missing, extra, differ []string
	for name, want := range live {
		if got, ok := found[name]; !ok {
			missing = append(missing, name)
		} else if got != want {
			differ = append(differ, name)
		}
	}
	for name := range found {
		if _, ok := live[name]; !ok {
			extra = append(extra, name)
		}
	}
	if len(missing)+len(extra)+len(differ) == 0 {
		return nil
	}
	parts := []string{}
	for _, group := range []struct {
		label string
		names []string
	}{{"missing", missing}, {"extra", extra}, {"different", differ}} {
		if len(group.names) == 0 {
			continue
		}
		sort.Strings(group.names)
		more := ""
		if len(group.names) > 3 {
			more = fmt.Sprintf(" and %d more", len(group.names)-3)
			group.names = group.names[:3]
		}
		parts = append(parts, fmt.Sprintf("%s: %s%s", group.label, strings.Join(group.names, ", "), more))
	}
	return errors.New(strings.Join(parts, "; "))
}

// copyTree copies a directory of regular files, as the runner's cpSync does
// for dist-static.
func copyTree(from, to string) error {
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		src, dst := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		if entry.IsDir() {
			if err = copyTree(src, dst); err != nil {
				return err
			}
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", entry.Name())
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err = os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func findReleaseJob(jobs []api.ReleaseJob, id string) (api.ReleaseJob, error) {
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return api.ReleaseJob{}, errors.New("release job not found")
}

// reportedCoverage runs supersedeCoverage and names on stderr each empty
// commit the check ignored, whatever its outcome.
func reportedCoverage(repo string, job api.ReleaseJob, released string) error {
	ignored, err := supersedeCoverage(repo, job, released)
	for _, c := range ignored {
		fmt.Fprintf(os.Stderr, "coverage of %s: ignored empty commit %s\n", job.ID, c)
	}
	return err
}

// supersedeCoverage proves the job's accepted change shipped by hand. The
// released commit must be on the current local tasks-hub, and every patch of
// the job's range base..commit must be in it: by ancestry, or else by patch
// equivalence (git cherry), which a merge in the range cannot use. A commit
// with an empty diff carries no patch, and a cherry-pick drops it, so it is
// covered; it is returned as "SHORT SUBJECT" in ignored, and never missing.
func supersedeCoverage(repo string, job api.ReleaseJob, released string) (ignored []string, err error) {
	// Validated before any git call: an argument such as --output=PATH must
	// never reach git as an option.
	if !fullCommit(released) {
		return nil, errors.New("supersede needs --released-commit as a full 40-hex commit")
	}
	if !fullCommit(job.Commit) || !fullCommit(job.BaseCommit) {
		return nil, errors.New("release job has no full commit and base commit")
	}
	git := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	}
	if _, err := git("merge-base", "--is-ancestor", released, "refs/heads/tasks-hub"); err != nil {
		return nil, fmt.Errorf("supersede needs %s to be an ancestor of refs/heads/tasks-hub in %s", released, repo)
	}
	if _, err := git("merge-base", "--is-ancestor", job.Commit, released); err == nil {
		return nil, nil
	}
	merges, err := git("rev-list", "--merges", job.BaseCommit+".."+job.Commit)
	if err != nil {
		return nil, fmt.Errorf("cannot list %s..%s in %s", job.BaseCommit, job.Commit, repo)
	}
	if len(strings.TrimSpace(string(merges))) > 0 {
		return nil, fmt.Errorf("supersede needs %s to be an ancestor of %s: its range has a merge, which patch equivalence cannot check", job.Commit, released)
	}
	out, err := git("cherry", released, job.Commit, job.BaseCommit)
	if err != nil {
		return nil, fmt.Errorf("git cherry %s %s %s failed in %s", released, job.Commit, job.BaseCommit, repo)
	}
	missing := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "+ ") {
			continue
		}
		c := strings.TrimPrefix(line, "+ ")
		if !fullCommit(c) {
			return ignored, fmt.Errorf("git cherry printed an unexpected line in %s", repo)
		}
		// The range has no merges, so every commit in it has one parent.
		if _, err := git("diff", "--quiet", c+"^", c); err == nil {
			named, err := git("log", "-1", "--format=%h %s", c)
			if err != nil {
				return ignored, fmt.Errorf("cannot read commit %s in %s", c, repo)
			}
			ignored = append(ignored, strings.TrimSpace(string(named)))
			continue
		} else if exit := (*exec.ExitError)(nil); !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return ignored, fmt.Errorf("cannot diff commit %s in %s", c, repo)
		}
		missing = append(missing, c)
	}
	if len(missing) > 0 {
		return ignored, fmt.Errorf("hand release %s is missing patches of %s: %s", released, job.ID, strings.Join(missing, ", "))
	}
	return ignored, nil
}

func fullCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
