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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func deploymentBriefing() string {
	return "\nYou are the persistent project deployment agent. Consume only handler-saved release jobs with tt deployment list. Never read or mutate work-item APIs. Automatically execute accepted exact-SHA independently verified candidates under the saved release policy, in the dedicated tasks-hub checkout, with private host credential references. Claim the project fence before any effect; keep a durable host journal and lock. Revalidate exact agent/run/project generation before every effect. A stale, paused, retired or rotated run cannot execute. Never take over ambiguous claimed or blocked jobs; ask the handler to reconcile them. Integrate the full candidate series; refuse conflicts, ref races and SHA mismatches. Import changed integrated-SHA matrix evidence through the handler before publication. Keep target backup pins and rollback artifacts; preserve database writes on rollback. Escalate failed release once. Write the immutable final receipt through tt deployment finish. Remain available after item-team cleanup; provisioning and live activation require their own recorded order."
}
func cmdDeployment(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt deployment setup|list|get|compat-list|compat-get|handler|enqueue|claim|check|verification|merged|finish|block|inputs|reconcile|set-aside|hold|unhold|refuse|supersede|retry|hand-release|hand-releases")
	}
	if args[0] == "hand-release" || args[0] == "hand-releases" {
		return cmdHandRelease(e, args)
	}
	if args[0] == "serve" {
		if len(args) != 3 || args[1] != "--config" {
			return errors.New("usage: tt deployment serve --config PRIVATE_PATH")
		}
		cmd := exec.Command("node", "scripts/release-runner.mjs", "--config", args[2])
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
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
	entry := fs.String("entry", "", "accepted queue entry (handler enqueue, retry)")
	job := fs.String("job", "", "saved release job")
	key := fs.String("request-id", "", "stable retry identity")
	generation := fs.Int64("generation", 0, "expected job generation")
	commit := fs.String("commit", "", "exact integrated commit")
	planFile := fs.String("plan-file", "", "integrated matrix plan JSON (handler import)")
	file := fs.String("file", "", "receipt, reconcile or set-aside record JSON file")
	released := fs.String("released-commit", "", "tasks-hub commit that carried a hand release (supersede)")
	releaseName := fs.String("release", "", "optional release name; must equal the hand release record's (supersede)")
	handRelease := fs.String("hand-release", "", "recorded hand release ID from tt deployment hand-release (supersede)")
	repo := fs.String("repo", ".", "repository whose tasks-hub must contain the hand release (supersede)")
	reason := fs.String("reason", "", "why a new job is expected to pass (retry), or why the job is held or its hold released (hold, unhold)")
	untilItems := fs.String("until-items", "", "comma-separated work items whose release lifts the hold (hold)")
	var restored stringListFlag
	fs.Var(&restored, "restored", "TARGET=RELEASE the refused job's target runs again; one per target in its receipt (retry, repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
		if hold, err = deploymentHold(args[0], *job, *reason, *untilItems); err != nil {
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
	if *key == "" || e.agent == "" || e.runID == "" {
		return errors.New("exact agent/run and request-id required")
	}
	req := api.ReleaseRequest{RequestID: *key, Operation: args[0], AgentID: e.agent, RunID: e.runID, EntryID: *entry, JobID: *job, ExpectedGeneration: *generation, IntegratedCommit: *commit, Retry: retry, Hold: hold}
	if args[0] == "supersede" {
		j, err := c.Release(ctx, e.task, *job)
		if err != nil {
			return err
		}
		if *handRelease == "" {
			return errors.New("supersede needs --hand-release naming a recorded hand release")
		}
		if err = supersedeCoverage(*repo, j, *released); err != nil {
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
		if postErr := postHoldNotice(ctx, c, e, args[0], out, notice); postErr != nil {
			saved := "is on hold"
			if args[0] == "unhold" {
				saved = "is released from its hold"
			}
			return fmt.Errorf("job %s %s at generation %d, but its board notice was not posted: %v; post this notice linked to item %s:\n%s", out.ID, saved, out.Generation, postErr, out.ItemID, api.RenderText(notice))
		}
	}
	return err
}

// deploymentHold builds the handler's hold or unhold record: a one-line
// reason, and for a hold the items whose release lifts it. The hub checks
// that the items exist and are not yet all released.
func deploymentHold(operation, job, reason, untilItems string) (*api.ReleaseHold, error) {
	if job == "" {
		return nil, fmt.Errorf("%s needs --job naming the release job", operation)
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 512 || strings.ContainsAny(reason, "\x00\n\r") {
		return nil, fmt.Errorf("%s needs --reason: one line, at most 512 characters", operation)
	}
	out := &api.ReleaseHold{Reason: reason}
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

// holdNotice is the board notice for a saved hold or unhold of job j.
func holdNotice(operation string, j api.ReleaseJob, reason string) api.Envelope {
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
		if j.Hold.AgentID != "" {
			holder = " Held by " + j.Hold.AgentID + "."
		}
	}
	return api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "A release job is on hold", Refs: refs,
		Body: api.EnvelopeBody{Text: fmt.Sprintf("%s is on an owner hold: the deployer will not claim it and goes on to the next verified job. Reason: %s.%s %s", what, strings.TrimRight(reason, "."), holder, condition)}}
}

// postHoldNotice posts the notice once per saved transition, linked to the
// job's item at its current revision and to the item's work order.
func postHoldNotice(ctx context.Context, c *api.Client, e env, operation string, j api.ReleaseJob, notice api.Envelope) error {
	item, err := c.GetWorkItem(ctx, e.task, j.ItemID)
	if err != nil {
		return err
	}
	post := api.PostMessageRequest{Envelope: &notice, Text: api.RenderText(notice), AgentID: e.agent, RunID: e.runID, RequestID: fmt.Sprintf("%s-%s-g%d", j.ID, operation, j.Generation),
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

// provisionPrerequisites runs the checkout's release runner, which prints the
// provisioned files as JSON, or only a named reason on failure.
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
	ctx, cancel := ctxTimeout(20 * time.Second)
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
		if err = supersedeCoverage(*repo, j, *released); err != nil {
			return err
		}
		commits = append(commits, j.Commit)
	}
	if tailos {
		if err = retainedTailOSCopy(*deployConfig, *released, *dist); err != nil {
			return err
		}
	}
	record := &api.HandRelease{InterventionSeq: *intervention, ReleasedCommit: *released, Release: *releaseName, Targets: targets, Commits: commits}
	out, err := c.RecordHandRelease(ctx, e.task, api.ReleaseRequest{RequestID: *key, AgentID: e.agent, RunID: e.runID, HandRelease: record})
	if err == nil {
		printJSON(out)
	} else if e.agent != "" || e.runID != "" {
		// The identity is still sent so the hub, not the CLI, decides.
		err = fmt.Errorf("%w; this shell carries an agent identity: record the hand release as the owner from a shell without it, e.g. env -u TAILTERM_AGENT -u TAILTERM_RUN tt deployment hand-release ...", err)
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
	if err = os.RemoveAll(tmp); err == nil {
		err = copyTree(dist, tmp)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o700)
	}
	if err == nil {
		// The copy, not the source, is what a rollback deploys.
		err = matchesTailOSRelease(tmp, released, live)
	}
	if err == nil {
		err = os.Rename(tmp, retained)
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("retaining --dist as %s%s failed: %w; nothing was recorded", name, released, err)
	}
	return nil
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

// supersedeCoverage proves the job's accepted change shipped by hand. The
// released commit must be on the current local tasks-hub, and every patch of
// the job's range base..commit must be in it: by ancestry, or else by patch
// equivalence (git cherry), which a merge in the range cannot use.
func supersedeCoverage(repo string, job api.ReleaseJob, released string) error {
	// Validated before any git call: an argument such as --output=PATH must
	// never reach git as an option.
	if !fullCommit(released) {
		return errors.New("supersede needs --released-commit as a full 40-hex commit")
	}
	if !fullCommit(job.Commit) || !fullCommit(job.BaseCommit) {
		return errors.New("release job has no full commit and base commit")
	}
	git := func(args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	}
	if _, err := git("merge-base", "--is-ancestor", released, "refs/heads/tasks-hub"); err != nil {
		return fmt.Errorf("supersede needs %s to be an ancestor of refs/heads/tasks-hub in %s", released, repo)
	}
	if _, err := git("merge-base", "--is-ancestor", job.Commit, released); err == nil {
		return nil
	}
	merges, err := git("rev-list", "--merges", job.BaseCommit+".."+job.Commit)
	if err != nil {
		return fmt.Errorf("cannot list %s..%s in %s", job.BaseCommit, job.Commit, repo)
	}
	if len(strings.TrimSpace(string(merges))) > 0 {
		return fmt.Errorf("supersede needs %s to be an ancestor of %s: its range has a merge, which patch equivalence cannot check", job.Commit, released)
	}
	out, err := git("cherry", released, job.Commit, job.BaseCommit)
	if err != nil {
		return fmt.Errorf("git cherry %s %s %s failed in %s", released, job.Commit, job.BaseCommit, repo)
	}
	missing := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "+ ") {
			missing = append(missing, strings.TrimPrefix(line, "+ "))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("hand release %s is missing patches of %s: %s", released, job.ID, strings.Join(missing, ", "))
	}
	return nil
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
