package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func queueProject(e env, value string) (string, error) {
	if value == "" {
		value = e.task
	}
	if !api.ValidID(value, "tsk") {
		return "", errors.New("Queue project is required and must be a tsk_ id")
	}
	if e.agent != "" && value != e.task {
		return "", errors.New("an agent may read or mutate only its own project's Queue")
	}
	return value, nil
}

func cmdQueue(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt queue list|get|history|changes|action|receipt")
	}
	sub := args[0]
	fs := flag.NewFlagSet("queue "+sub, flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "receiving project id")
	cursor := fs.String("cursor", "", "opaque frozen-page cursor")
	limit := fs.Int("limit", api.DefaultQueuePage, "page size")
	includeTerminal := fs.Bool("include-terminal", false, "include completed and cancelled cycles")
	after := fs.Int64("after", 0, "incremental event checkpoint")
	cutoff := fs.Int64("cutoff", 0, "frozen incremental cutoff")
	file := fs.String("file", "", "Queue action JSON file, or - for stdin")
	requestID := fs.String("request-id", "", "stable Queue mutation request identity")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	project, err := queueProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(15 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	if e.agent != "" {
		self, selfErr := c.GetAgent(ctx, project, e.agent)
		if selfErr != nil {
			return selfErr
		}
		if self.Role != api.AgentRoleDatabaseHandler || self.RunID != e.runID || self.Status == api.AgentRetired || self.Status == api.AgentClosed || self.Status == api.AgentExited {
			return errors.New("agent Queue access requires this project's exact active database-handler run")
		}
	}
	switch sub {
	case "list":
		out, e2 := c.ListQueue(ctx, project, *cursor, *limit, *includeTerminal)
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
			return nil
		}
		for _, entry := range out.Entries {
			fmt.Printf("%s  c%-2d r%-3d %-9s %-6s %s/%s@%d  %s\n", entry.ID, entry.Cycle, entry.Revision, entry.State, entry.QueuePriority, entry.SourceTaskID, entry.ItemID, entry.OfferedItemRevision, entry.Item.Title)
		}
		if out.Cursor != "" {
			fmt.Printf("cursor %s\n", out.Cursor)
		}
		return nil
	case "get":
		if fs.NArg() != 1 {
			return errors.New("usage: tt queue get [--project ID] QUEUE_ENTRY")
		}
		out, e2 := c.GetQueueEntry(ctx, project, fs.Arg(0))
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
		} else {
			fmt.Printf("%s cycle %d revision %d %s [%s]\n%s/%s@%d · Queue priority %s\n", out.ID, out.Cycle, out.Revision, out.Item.Title, out.State, out.SourceTaskID, out.ItemID, out.OfferedItemRevision, out.QueuePriority)
		}
		return nil
	case "history":
		if fs.NArg() != 1 {
			return errors.New("usage: tt queue history [--cursor CURSOR] QUEUE_ENTRY")
		}
		out, e2 := c.ListQueueHistory(ctx, project, fs.Arg(0), *cursor, *limit)
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
		} else {
			for _, event := range out.Events {
				fmt.Printf("%d  c%d r%d %-20s %s\n", event.Seq, event.Cycle, event.Revision, event.Kind, event.Reason)
			}
		}
		return nil
	case "changes":
		out, e2 := c.ListQueueChanges(ctx, project, *after, *cutoff, *limit)
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
		} else {
			for _, event := range out.Events {
				fmt.Printf("%d  %s c%d r%d %s\n", event.Seq, event.EntryID, event.Cycle, event.Revision, event.Kind)
			}
			fmt.Printf("checkpoint %d cutoff %d complete=%t\n", out.Checkpoint, out.Cutoff, out.Complete)
		}
		return nil
	case "action":
		if fs.NArg() != 1 {
			return errors.New("usage: tt queue action --file PATH QUEUE_ENTRY")
		}
		var req api.QueueActionRequest
		if err = readAuditJSON(*file, &req); err != nil {
			return err
		}
		if e.agent != "" {
			if req.AgentID != "" && req.AgentID != e.agent {
				return errors.New("Queue action agentId does not match this exact agent")
			}
			if req.RunID != "" && req.RunID != e.runID {
				return errors.New("Queue action runId does not match this exact run")
			}
			req.AgentID, req.RunID = e.agent, e.runID
		}
		out, e2 := c.QueueAction(ctx, project, fs.Arg(0), req)
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
		} else {
			fmt.Printf("%s cycle %d revision %d %s receipt %s\n", out.Entry.ID, out.Entry.Cycle, out.Entry.Revision, out.Entry.State, out.Receipt.ID)
		}
		return nil
	case "receipt":
		if *requestID == "" {
			return errors.New("--request-id is required")
		}
		out, e2 := c.GetQueueReceipt(ctx, project, *requestID, e.agent)
		if e2 != nil {
			return e2
		}
		if *asJSON {
			printJSON(out)
		} else {
			fmt.Printf("%s cycle %d revision %d %s receipt %s\n", out.Entry.ID, out.Entry.Cycle, out.Entry.Revision, out.Entry.State, out.Receipt.ID)
		}
		return nil
	default:
		return fmt.Errorf("unknown queue command %q", sub)
	}
}
