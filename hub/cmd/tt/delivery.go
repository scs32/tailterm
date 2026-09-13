package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func requireReliableDelivery(ctx context.Context, c *api.Client) error {
	return requireReliableDeliveryVersion(ctx, c, api.ReliableDeliveryCapabilityVersion)
}

func requireReliableDeliveryVersion(ctx context.Context, c *api.Client, required int) error {
	caps, err := c.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("reliable directive core is unavailable; upgrade the hub and retry: %w", err)
	}
	if !caps.ReliableDelivery.Supported {
		return errors.New("reliable directive core is unsupported by this hub; upgrade the hub before using current-assignment or delivery commands")
	}
	for _, version := range caps.ReliableDelivery.Versions {
		if version == required {
			return nil
		}
	}
	return fmt.Errorf("reliable directive core version %d is required; upgrade the hub before retrying", required)
}

func cmdCurrentAssignment(e env, args []string) error {
	fs := flag.NewFlagSet("current-assignment", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	itemID := fs.String("item", "", "exact work-item id for a mandatory action")
	actionKey := fs.String("action-key", "", "exact mandatory action key")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *task == "" || e.agent == "" || e.runID == "" || ((*itemID == "") != (*actionKey == "")) {
		return errors.New("usage: tt current-assignment [--task ID] [--item ID --action-key KEY] [--json]; exact TAILTERM_AGENT and TAILTERM_RUN are required")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if err = requireReliableDelivery(ctx, c); err != nil {
		return err
	}
	if *actionKey != "" {
		if err = requireReliableDeliveryVersion(ctx, c, api.ReliableMandatoryActionCapabilityVersion); err != nil {
			return err
		}
	}
	out, err := c.CurrentAssignmentForResponsibility(ctx, *task, e.agent, e.runID, *itemID, *actionKey)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
	} else {
		fmt.Printf("delivery %s\ngeneration %d\nkind %s\nrecipient %s\naction %s (%s)\nphase %s\nexecution epoch %d\nmessage %d\nitem %s@%d\nbinding order %d\ngoverning order %d\ninstruction bytes %d\ninstruction sha256 %s\nenrollment version %d\n", out.ID, out.Generation, out.Kind, out.RecipientKind, out.ActionKey, out.ActionClass, out.Phase, out.ExecutionEpoch, out.MessageSeq, out.ItemID, out.ItemRevision, out.WorkOrderMessage.Seq, out.GoverningOrderMessage.Seq, out.InstructionBytes, out.InstructionSHA256, out.EnrollmentVersion)
		if out.Message != nil {
			fmt.Printf("directive (stored task data):\n%s\n", out.Message.Text)
		}
	}
	return nil
}

func cmdDelivery(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt delivery create|coverage|incident|ack|progress|block|resolve|resume|result")
	}
	sub := args[0]
	fs := flag.NewFlagSet("delivery "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	requestID := fs.String("request-id", "", "stable retry identity")
	file := fs.String("file", "", "JSON request file, or - for stdin (create/incident)")
	text := fs.String("text", "", "bounded progress, resolution, or result text")
	reason := fs.String("reason", "", "block reason class")
	epoch := fs.Int64("expected-epoch", 0, "current execution epoch")
	blockID := fs.String("block-id", "", "current block id")
	resolutionID := fs.String("resolution-id", "", "current resolution id")
	asJSON := fs.Bool("json", false, "JSON output")
	itemID := fs.String("item", "", "exact work-item id (coverage)")
	actionKey := fs.String("action-key", "", "exact mandatory action key (coverage)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *task == "" {
		return errors.New("project id is required")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if err = requireReliableDelivery(ctx, c); err != nil {
		return err
	}
	if sub == "coverage" {
		if fs.NArg() != 0 || e.agent == "" || e.runID == "" || ((*itemID == "") != (*actionKey == "")) {
			return errors.New("usage: tt delivery coverage [--task ID] [--item ID --action-key KEY] [--json]; exact TAILTERM_AGENT and TAILTERM_RUN are required")
		}
		if err = requireReliableDeliveryVersion(ctx, c, api.ReliableFollowThroughCapabilityVersion); err != nil {
			return err
		}
		if *actionKey != "" {
			if err = requireReliableDeliveryVersion(ctx, c, api.ReliableMandatoryActionCapabilityVersion); err != nil {
				return err
			}
		}
		coverage, coverageErr := c.DeliveryCoverageForResponsibility(ctx, *task, e.agent, e.runID, *itemID, *actionKey)
		if coverageErr != nil {
			return coverageErr
		}
		if *asJSON {
			printJSON(coverage)
		} else {
			fmt.Printf("%s: %s\n", coverage.Status, coverage.Reason)
		}
		return nil
	}
	var out api.DeliveryMutation
	if sub == "create" {
		if fs.NArg() != 0 || *file == "" {
			return errors.New("usage: tt delivery create --file PATH [--task ID] [--json]")
		}
		var req api.CreateRequiredDeliveryRequest
		if err = readAuditJSON(*file, &req); err != nil {
			return err
		}
		if req.EnrollmentVersion >= api.ReliableFollowThroughCapabilityVersion {
			if err = requireReliableDeliveryVersion(ctx, c, req.EnrollmentVersion); err != nil {
				return err
			}
		}
		if req.ProducerAgentID == "" && e.agent != "" {
			req.ProducerAgentID, req.ProducerRunID = e.agent, e.runID
		} else if req.ProducerAgentID != e.agent || req.ProducerRunID != e.runID {
			return errors.New("producer identity must match this exact agent/run")
		}
		out, err = c.CreateRequiredDelivery(ctx, *task, req)
	} else if sub == "incident" {
		if fs.NArg() != 1 || *file == "" || e.agent == "" || e.runID == "" {
			return errors.New("usage: tt delivery incident --file PATH DELIVERY_ID [--task ID] [--json]; exact TAILTERM_AGENT and TAILTERM_RUN are required")
		}
		if err = requireReliableDeliveryVersion(ctx, c, api.ReliableMandatoryActionCapabilityVersion); err != nil {
			return err
		}
		var req api.DeliveryRecoveryIncidentRequest
		if err = readAuditJSON(*file, &req); err != nil {
			return err
		}
		if req.AgentID == "" {
			req.AgentID, req.RunID = e.agent, e.runID
		} else if req.AgentID != e.agent || req.RunID != e.runID {
			return errors.New("incident recorder identity must match this exact agent/run")
		}
		incident, incidentErr := c.RecordDeliveryRecoveryIncident(ctx, *task, fs.Arg(0), req)
		if incidentErr != nil {
			return incidentErr
		}
		if *asJSON {
			printJSON(incident)
		} else {
			fmt.Printf("incident %s delivery=%s cause=%s receipt=%s replay=%t\n", incident.Incident.ID, incident.Delivery.ID, incident.Incident.CauseStatus, incident.Receipt.ID, incident.Replay)
		}
		return nil
	} else {
		if fs.NArg() != 1 || *requestID == "" {
			return errors.New("delivery action requires one DELIVERY_ID and --request-id")
		}
		if sub != "resolve" && (e.agent == "" || e.runID == "") {
			return errors.New("delivery worker action requires exact TAILTERM_AGENT/TAILTERM_RUN")
		}
		deliveryID := fs.Arg(0)
		switch sub {
		case "ack":
			out, err = c.DeliveryAction(ctx, *task, deliveryID, "ack", api.DeliveryActionRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch})
		case "progress":
			out, err = c.DeliveryAction(ctx, *task, deliveryID, "progress", api.DeliveryActionRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch, Text: *text})
		case "block":
			out, err = c.DeliveryAction(ctx, *task, deliveryID, "blocks", api.DeliveryBlockRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch, ReasonClass: *reason, Text: *text})
		case "resolve":
			if *blockID == "" {
				return errors.New("delivery resolve requires --block-id")
			}
			out, err = c.ResolveDeliveryBlock(ctx, *task, deliveryID, *blockID, api.DeliveryResolutionRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch, Text: *text})
		case "resume":
			out, err = c.DeliveryAction(ctx, *task, deliveryID, "resume", api.DeliveryResumeRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch, ResolutionID: *resolutionID})
		case "result":
			out, err = c.DeliveryAction(ctx, *task, deliveryID, "result", api.DeliveryActionRequest{RequestID: *requestID, AgentID: e.agent, RunID: e.runID, ExpectedEpoch: *epoch, Text: *text})
		default:
			return errors.New("usage: tt delivery create|coverage|incident|ack|progress|block|resolve|resume|result")
		}
	}
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
	} else {
		fmt.Printf("%s %s generation=%d phase=%s receipt=%s replay=%t\n", out.Event.Kind, out.Delivery.ID, out.Delivery.Generation, out.Delivery.Phase, out.Receipt.ID, out.Replay)
	}
	return nil
}
