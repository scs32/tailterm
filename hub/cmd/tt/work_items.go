package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func workItemProject(e env, project string) (string, error) {
	if project == "" {
		project = e.task
	}
	if project == "" {
		return "", errors.New("project is required (--project or TAILTERM_TASK)")
	}
	if !api.ValidID(project, "tsk") {
		return "", errors.New("project must be a valid tsk_ id")
	}
	if e.agent != "" && e.task != project {
		return "", errors.New("an agent may manage work items only in its own project")
	}
	return project, nil
}

func bodyFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	var r io.Reader
	var closeFile func() error
	if path == "-" {
		r = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		r, closeFile = file, file.Close
	}
	if closeFile != nil {
		defer closeFile()
	}
	data, err := io.ReadAll(io.LimitReader(r, int64(api.MaxTextLen+1)))
	if err != nil {
		return "", err
	}
	if len(data) > api.MaxTextLen {
		return "", fmt.Errorf("body exceeds %d bytes", api.MaxTextLen)
	}
	return string(data), nil
}

func cmdWorkItems(e env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tt work-items <list|get|create|update|receipt|dispatch|revisions|messages>")
	}
	switch args[0] {
	case "list":
		return cmdWorkItemList(e, args[1:])
	case "get":
		return cmdWorkItemGet(e, args[1:])
	case "create":
		return cmdWorkItemCreate(e, args[1:])
	case "update":
		return cmdWorkItemUpdate(e, args[1:])
	case "receipt":
		return cmdWorkItemReceipt(e, args[1:])
	case "dispatch":
		return cmdWorkItemDispatch(e, args[1:])
	case "revisions":
		return cmdWorkItemRevisions(e, args[1:])
	case "messages":
		return cmdWorkItemMessages(e, args[1:])
	default:
		return fmt.Errorf("unknown work-items command %q", args[0])
	}
}

func cmdWorkItemList(e env, args []string) error {
	fs := flag.NewFlagSet("work-items list", flag.ContinueOnError)
	project := fs.String("project", e.task, "owning project id (empty lists all projects)")
	kind := fs.String("kind", "", "bug or feature")
	status := fs.String("status", "", "open, in_progress, blocked, done or dismissed")
	after := fs.Int64("after", 0, "return items after this sequence")
	limit := fs.Int("limit", 50, "maximum items")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if e.agent != "" && *project != e.task {
		return errors.New("an agent may list work items only in its own project")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	list, err := c.ListWorkItems(ctx, *project, *kind, *status, *after, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(list)
		return nil
	}
	for _, item := range list.Items {
		fmt.Printf("%s  %-7s %-11s %-6s %s  %s\n", item.ID, item.Kind, item.Status, item.Priority, item.TaskID, item.Title)
	}
	return nil
}

func cmdWorkItemGet(e env, args []string) error {
	fs := flag.NewFlagSet("work-items get", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	revision := fs.Int64("revision", 0, "exact historical revision")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *revision < 0 {
		return errors.New("usage: tt work-items get [--project ID] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if *revision > 0 {
		item, err := c.GetWorkItemRevision(ctx, project, fs.Arg(0), *revision)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(item)
			return nil
		}
		fmt.Printf("%s %s revision %d %s [%s]\n%s\n", strings.ToUpper(item.Kind), item.ItemID, item.Revision, item.Title, item.Status, item.Description)
		return nil
	}
	item, err := c.GetWorkItem(ctx, project, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(item)
		return nil
	}
	fmt.Printf("%s %s %s [%s]\n%s\n", strings.ToUpper(item.Kind), item.ID, item.Title, item.Status, item.Description)
	return nil
}

func cmdWorkItemCreate(e env, args []string) error {
	fs := flag.NewFlagSet("work-items create", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	kind := fs.String("kind", "", "bug or feature (required)")
	title := fs.String("title", "", "title (required)")
	body := fs.String("body-file", "", "description file, or - for stdin")
	priority := fs.String("priority", "", "low, normal, high or urgent")
	source := fs.Int64("source-seq", 0, "source board message sequence")
	requestID := fs.String("request-id", "", "stable retry key (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	description, err := bodyFile(*body)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	item, err := c.CreateWorkItem(ctx, project, api.CreateWorkItemRequest{Kind: *kind, Title: *title, Description: description, Priority: *priority, AgentID: e.agent, SourceMessageSeq: *source, RequestID: *requestID})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(item)
	} else {
		fmt.Println(item.ID)
	}
	return nil
}

func cmdWorkItemUpdate(e env, args []string) error {
	fs := flag.NewFlagSet("work-items update", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	revision := fs.Int64("revision", 0, "expected revision (required)")
	title := fs.String("title", "", "new title")
	body := fs.String("body-file", "", "new description file, or - for stdin")
	status := fs.String("status", "", "new status")
	priority := fs.String("priority", "", "new priority")
	requestID := fs.String("request-id", "", "stable retry key (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *revision < 1 {
		return errors.New("usage: tt work-items update --revision N --request-id KEY [fields] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	req := api.CreateWorkItemUpdate{ExpectedRevision: *revision, AgentID: e.agent, RunID: e.runID, RequestID: *requestID}
	if visited["title"] {
		req.Title = title
	}
	if visited["status"] {
		req.Status = status
	}
	if visited["priority"] {
		req.Priority = priority
	}
	if visited["body-file"] {
		description, err := bodyFile(*body)
		if err != nil {
			return err
		}
		req.Description = &description
	}
	if req.Title == nil && req.Description == nil && req.Status == nil && req.Priority == nil {
		return errors.New("at least one update field is required")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if *requestID == "" {
		return errors.New("--request-id is required")
	}
	result, err := c.CreateWorkItemUpdate(ctx, project, fs.Arg(0), req)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(result)
	} else {
		fmt.Printf("%s revision %d receipt %s\n", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID)
	}
	return nil
}

func cmdWorkItemReceipt(e env, args []string) error {
	fs := flag.NewFlagSet("work-items receipt", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	requestID := fs.String("request-id", "", "stable update retry key (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *requestID == "" {
		return errors.New("usage: tt work-items receipt --request-id KEY WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	result, err := c.GetWorkItemUpdateReceipt(ctx, project, fs.Arg(0), *requestID, e.agent)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(result)
	} else {
		fmt.Printf("%s revision %d receipt %s\n", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID)
	}
	return nil
}

func cmdWorkItemRevisions(e env, args []string) error {
	fs := flag.NewFlagSet("work-items revisions", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	after := fs.Int64("after", 0, "return revisions after this revision")
	limit := fs.Int("limit", api.DefaultWorkItemHistoryPage, "maximum revisions")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") {
		return errors.New("usage: tt work-items revisions [--after N] [--limit N] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	list, err := c.ListWorkItemRevisions(ctx, project, fs.Arg(0), *after, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(list)
		return nil
	}
	for _, revision := range list.Revisions {
		fmt.Printf("%d  %-10s %-11s %s\n", revision.Revision, revision.Provenance, revision.Status, revision.Title)
	}
	if list.NextAfter > 0 {
		fmt.Printf("next after %d\n", list.NextAfter)
	}
	return nil
}

func cmdWorkItemMessages(e env, args []string) error {
	fs := flag.NewFlagSet("work-items messages", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	revision := fs.Int64("revision", 0, "only messages explicitly linked to this revision")
	after := fs.Int64("after", 0, "return messages after this sequence")
	limit := fs.Int("limit", api.DefaultWorkItemHistoryPage, "maximum messages")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *revision < 0 {
		return errors.New("usage: tt work-items messages [--revision N] [--after N] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	list, err := c.ListWorkItemMessages(ctx, project, fs.Arg(0), *revision, *after, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(list)
		return nil
	}
	for _, link := range list.Links {
		fmt.Printf("#%d  revision %d %-10s %s\n", link.Message.Seq, link.ItemRevision, link.RevisionCoverage, link.Message.Text)
	}
	if list.NextAfter > 0 {
		fmt.Printf("next after %d\n", list.NextAfter)
	}
	return nil
}

func cmdWorkItemDispatch(e env, args []string) error {
	fs := flag.NewFlagSet("work-items dispatch", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	target := fs.String("target-project", "", "destination project (default owning project)")
	revision := fs.Int64("revision", 0, "expected revision (required)")
	requestID := fs.String("request-id", "", "stable retry key (required)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *revision < 1 {
		return errors.New("usage: tt work-items dispatch --revision N --request-id KEY [--target-project ID] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	result, err := c.DispatchWorkItem(ctx, project, fs.Arg(0), api.DispatchWorkItemRequest{Revision: *revision, TargetTaskID: *target, AgentID: e.agent, RequestID: *requestID})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(result)
	} else {
		fmt.Printf("dispatched %s as %s in message #%d\n", result.Item.ID, result.Dispatch.ID, result.Dispatch.MessageSeq)
	}
	return nil
}
