package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"os/exec"
	"time"
)

func deploymentBriefing() string {
	return "\nYou are the persistent project deployment agent. Consume only handler-saved release jobs with tt deployment list. Never read or mutate work-item APIs. Automatically execute accepted exact-SHA independently verified candidates under the saved release policy, in the dedicated tasks-hub checkout, with private host credential references. Claim the project fence before any effect; keep a durable host journal and lock. Revalidate exact agent/run/project generation before every effect. A stale, paused, retired or rotated run cannot execute. Never take over ambiguous claimed or blocked jobs; ask the handler to reconcile them. Integrate the full candidate series; refuse conflicts, ref races and SHA mismatches. Import changed integrated-SHA matrix evidence through the handler before publication. Keep target backup pins and rollback artifacts; preserve database writes on rollback. Escalate failed release once. Write the immutable final receipt through tt deployment finish. Remain available after item-team cleanup; provisioning and live activation require their own recorded order."
}
func cmdDeployment(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt deployment setup|list|handler|enqueue|claim|check|verification|merged|finish|block|inputs|reconcile|refuse|supersede")
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
		return cmdSpawn(e, append([]string{"--role", api.AgentRoleDeployment}, args[1:]...))
	}
	fs := flag.NewFlagSet("deployment "+args[0], flag.ContinueOnError)
	entry := fs.String("entry", "", "accepted queue entry (handler enqueue)")
	job := fs.String("job", "", "saved release job")
	key := fs.String("request-id", "", "stable retry identity")
	generation := fs.Int64("generation", 0, "expected job generation")
	commit := fs.String("commit", "", "exact integrated commit")
	planFile := fs.String("plan-file", "", "integrated matrix plan JSON (handler import)")
	file := fs.String("file", "", "receipt JSON file")
	released := fs.String("released-commit", "", "tasks-hub commit that carried a hand release (supersede)")
	releaseName := fs.String("release", "", "hand release record, such as its release name (supersede)")
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
		if err = supersedeAncestry(*repo, jobs, *job, *released); err != nil {
			return err
		}
		req.Supersession = &api.ReleaseSupersession{ReleasedCommit: *released, Release: *releaseName}
	}
	if args[0] == "finish" || args[0] == "verification" || args[0] == "inputs" || args[0] == "reconcile" {
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
		} else if args[0] == "reconcile" {
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

// supersedeAncestry proves the job's accepted commit shipped by hand: it is
// an ancestor of the released commit, which is on the current local tasks-hub.
func supersedeAncestry(repo string, jobs []api.ReleaseJob, id, released string) error {
	// Validated before any git call: an argument such as --output=PATH must
	// never reach git as an option.
	if !fullCommit(released) {
		return errors.New("supersede needs --released-commit as a full 40-hex commit")
	}
	for _, j := range jobs {
		if j.ID != id {
			continue
		}
		if !fullCommit(j.Commit) {
			return errors.New("release job has no full commit")
		}
		for _, pair := range [][2]string{{j.Commit, released}, {released, "refs/heads/tasks-hub"}} {
			if err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", pair[0], pair[1]).Run(); err != nil {
				return fmt.Errorf("supersede needs %s to be an ancestor of %s in %s", pair[0], pair[1], repo)
			}
		}
		return nil
	}
	return errors.New("release job not found")
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
