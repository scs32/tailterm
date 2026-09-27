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
		return errors.New("usage: tt deployment setup|list|enqueue|claim|check|verification|merged|finish|block|inputs|reconcile|refuse")
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
	if *key == "" || e.agent == "" || e.runID == "" {
		return errors.New("exact agent/run and request-id required")
	}
	req := api.ReleaseRequest{RequestID: *key, Operation: args[0], AgentID: e.agent, RunID: e.runID, EntryID: *entry, JobID: *job, ExpectedGeneration: *generation, IntegratedCommit: *commit}
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
