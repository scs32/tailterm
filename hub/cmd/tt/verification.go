package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func cmdVerification(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt verification plan|receipt|history|enrollment --item ID [--file PATH --request-id KEY --generation N]")
	}
	fs := flag.NewFlagSet("verification "+args[0], flag.ContinueOnError)
	item := fs.String("item", "", "exact work item")
	file := fs.String("file", "", "plan or receipt JSON file")
	key := fs.String("request-id", "", "stable retry identity")
	generation := fs.Int64("generation", 0, "expected record generation")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*item, "wi") || e.agent == "" || e.runID == "" {
		return errors.New("exact handler agent/run and item required")
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	if args[0] == "enrollment" {
		out, err := c.VerificationEnrollment(ctx, e.task, *item, e.agent, e.runID)
		if err != nil {
			return err
		}
		printJSON(out)
		return nil
	}
	if args[0] == "history" {
		out, err := c.VerificationHistory(ctx, e.task, *item, e.agent, e.runID)
		if err != nil {
			return err
		}
		printJSON(out)
		return nil
	}
	if *key == "" || *file == "" {
		return errors.New("file and request-id required")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	req := api.VerificationRequest{RequestID: *key, AgentID: e.agent, RunID: e.runID, ExpectedGeneration: *generation}
	switch args[0] {
	case "plan":
		req.Plan = &api.VerificationPlan{}
		err = json.Unmarshal(data, req.Plan)
	case "receipt":
		req.Receipt = &api.VerificationReceipt{}
		err = json.Unmarshal(data, req.Receipt)
	default:
		return errors.New("unknown verification operation")
	}
	if err != nil {
		return err
	}
	if req.Receipt != nil {
		if err := verifyReceiptLogs(*req.Receipt); err != nil {
			return err
		}
	}
	out, err := c.SaveVerification(ctx, e.task, *item, req)
	if err != nil {
		return err
	}
	printJSON(out)
	return nil
}

func verifyReceiptLogs(r api.VerificationReceipt) error {
	for _, check := range r.Checks {
		logs := []api.VerificationAttempt{{LogURI: check.LogURI, LogDigest: check.LogDigest}}
		logs = append(logs, check.Attempts...)
		for _, check := range logs {
			file, err := os.Open(check.LogURI)
			if err != nil {
				return fmt.Errorf("verification log unavailable: %w", err)
			}
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
			if hex.EncodeToString(hash.Sum(nil)) != check.LogDigest {
				return errors.New("verification log digest mismatch")
			}
		}
	}
	return nil
}
