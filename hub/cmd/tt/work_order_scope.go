package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func cmdWorkOrderScope(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt work-items scope confirm|get|save|receipt [flags] ITEM")
	}
	sub := args[0]
	fs := flag.NewFlagSet("work-items scope "+sub, flag.ContinueOnError)
	project := fs.String("project", e.task, "project id")
	revision := fs.Int64("revision", 0, "exact current item revision")
	scope := fs.Int64("scope-revision", 0, "exact narrative scope revision")
	order := fs.Int64("order", 0, "recorded work-order message sequence")
	source := fs.Int64("source", 0, "linked bookkeeping source message sequence")
	kind := fs.String("kind", "", "order, sequencing_note or decision")
	entry := fs.String("queue-entry", "", "exact queued entry id, if present")
	admissionsFile := fs.String("admissions-file", "", "JSON array of exact admitted agent/run/digest bindings")
	requestID := fs.String("request-id", "", "stable retry id")
	complete := fs.Bool("complete", false, "handler verified complete owner-filed scope")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") {
		return errors.New("scope command needs one wi_ID")
	}
	task, err := workItemProject(e, *project)
	if err != nil {
		return err
	}
	item := fs.Arg(0)
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(20 * time.Second)
	defer cancel()
	switch sub {
	case "confirm":
		if e.agent == "" || e.runID == "" || !*complete || !validScopeArgs(*requestID, *revision, *order) || *scope < 1 {
			return errors.New("confirm requires handler agent, --complete, --request-id, --revision, --scope-revision and --order")
		}
		out, err := c.ConfirmWorkOrderScope(ctx, task, item, api.ConfirmWorkOrderScopeRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedRevision: *revision, ScopeRevision: *scope, OrderMessageSeq: *order, Complete: true})
		if err != nil {
			return err
		}
		printJSON(out)
	case "get":
		if *revision < 1 || *order < 1 {
			return errors.New("get requires --revision and --order")
		}
		out, err := c.GetWorkOrderScopeConfirmation(ctx, task, item, *revision, *order)
		if err != nil {
			return err
		}
		printJSON(out)
	case "save":
		if e.agent == "" || e.runID == "" || !validScopeArgs(*requestID, *revision, *order) || *source < 1 || *kind == "" {
			return errors.New("save requires handler agent, --request-id, --revision, --order, --source and --kind")
		}
		admissions := []api.WorkOrderAdmission{}
		if *admissionsFile != "" {
			data, err := os.ReadFile(*admissionsFile)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &admissions); err != nil {
				return fmt.Errorf("admissions file: %w", err)
			}
		}
		out, err := c.SaveWorkOrderBookkeeping(ctx, task, item, api.WorkOrderBookkeepingRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedRevision: *revision, OrderMessageSeq: *order, SourceMessageSeq: *source, Kind: *kind, QueueEntryID: *entry, Admissions: admissions})
		if err != nil {
			return err
		}
		printJSON(out)
	case "receipt":
		if *requestID == "" {
			return errors.New("receipt requires --request-id")
		}
		out, err := c.GetWorkOrderBookkeepingReceipt(ctx, task, item, *requestID)
		if err != nil {
			return err
		}
		printJSON(out)
	default:
		return fmt.Errorf("unknown scope command %q", sub)
	}
	return nil
}

func validScopeArgs(requestID string, revision, order int64) bool {
	return requestID != "" && revision > 0 && order > 0
}
