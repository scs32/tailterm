package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"os/exec"
	"strings"
	"time"
)

func deploymentBriefing() string {
	return "\nYou are the persistent project deployment agent. Consume only handler-saved release jobs with tt deployment list. Never read or mutate work-item APIs. Automatically execute accepted exact-SHA independently verified candidates under the saved release policy, in the dedicated tasks-hub checkout, with private host credential references. Claim the project fence before any effect; keep a durable host journal and lock. Revalidate exact agent/run/project generation before every effect. A stale, paused, retired or rotated run cannot execute. Never take over ambiguous claimed or blocked jobs; ask the handler to reconcile them. Integrate the full candidate series; refuse conflicts, ref races and SHA mismatches. Import changed integrated-SHA matrix evidence through the handler before publication. Keep target backup pins and rollback artifacts; preserve database writes on rollback. Escalate failed release once. Write the immutable final receipt through tt deployment finish. Remain available after item-team cleanup; provisioning and live activation require their own recorded order."
}
func cmdDeployment(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt deployment setup|list|handler|enqueue|claim|check|verification|merged|finish|block|inputs|reconcile|set-aside|refuse|supersede|hand-release|hand-releases")
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
	entry := fs.String("entry", "", "accepted queue entry (handler enqueue)")
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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	if args[0] == "list" {
		out, err := c.Releases(ctx, e.task)
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
	req := api.ReleaseRequest{RequestID: *key, Operation: args[0], AgentID: e.agent, RunID: e.runID, EntryID: *entry, JobID: *job, ExpectedGeneration: *generation, IntegratedCommit: *commit}
	if args[0] == "supersede" {
		jobs, err := c.Releases(ctx, e.task)
		if err != nil {
			return err
		}
		if *handRelease == "" {
			return errors.New("supersede needs --hand-release naming a recorded hand release")
		}
		j, err := findReleaseJob(jobs, *job)
		if err != nil {
			return err
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
	return err
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
// refuses a request that carries an agent identity.
func cmdHandRelease(e env, args []string) error {
	fs := flag.NewFlagSet("deployment "+args[0], flag.ContinueOnError)
	intervention := fs.Int64("intervention", 0, "owner release intervention message sequence")
	released := fs.String("released-commit", "", "tasks-hub commit that carried the hand release")
	releaseName := fs.String("release", "", "release name, such as the hand release's release ID")
	repo := fs.String("repo", ".", "repository whose tasks-hub must contain the hand release")
	key := fs.String("request-id", "", "stable retry identity")
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
	jobs, err := c.Releases(ctx, e.task)
	if err != nil {
		return err
	}
	commits := []string{}
	for _, id := range jobIDs {
		j, err := findReleaseJob(jobs, id)
		if err != nil {
			return err
		}
		if err = supersedeCoverage(*repo, j, *released); err != nil {
			return err
		}
		commits = append(commits, j.Commit)
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
