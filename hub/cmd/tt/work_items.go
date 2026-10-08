package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/triage"
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
		return errors.New("usage: tt work-items <list|get|create|update|receipt|dispatch|revisions|messages|scope|evidence|narrative|triage>")
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
	case "scope":
		return cmdWorkOrderScope(e, args[1:])
	case "evidence":
		return cmdWorkItemEvidence(e, args[1:])
	case "narrative":
		return cmdWorkItemNarrative(e, args[1:])
	case "triage":
		return cmdWorkItemTriage(e, args[1:])
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
	fmt.Printf("%s %s %s [%s]\n", strings.ToUpper(item.Kind), item.ID, item.Title, item.Status)
	for _, line := range workItemBudgetLines(item.Budget) {
		fmt.Println(line)
	}
	fmt.Println(item.Description)
	return nil
}

// workItemBudgetLines is the budget block of tt work-items get: the estimate
// against the lifetime actual, then the estimate's basis and who set it. An
// older hub sends no budget and gets no lines.
func workItemBudgetLines(b *api.TokenBudget) []string {
	if b == nil {
		return nil
	}
	lines := []string{"Budget: " + formatTokenBudget(b)}
	if est := b.Estimate; est != nil {
		setter := est.SetBy.AgentID
		if setter == "" {
			setter = est.SetBy.User
		}
		lines = append(lines, fmt.Sprintf("Estimate basis: %s (set %s by %s)", est.Basis, est.SetAt.UTC().Format(time.RFC3339), setter))
	}
	return lines
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
	reportID := fs.String("report-id", "", "completion report id (required when marking a feature done)")
	reportVersion := fs.Int64("report-version", 0, "completion report version")
	reportDigest := fs.String("report-digest", "", "completion report SHA-256")
	reportScope := fs.Int64("report-scope-revision", 0, "completion report feature scope revision")
	worktree := fs.String("worktree", "", "accepted builder worktree root (records team queue acceptance with a done save)")
	branch := fs.String("branch", "", "accepted branch")
	commit := fs.String("commit", "", "accepted commit SHA")
	acceptanceEvidence := fs.String("evidence", "", "queue acceptance evidence (default: the saved completion receipt)")
	estimateTokens := fs.Int64("estimate-tokens", 0, "token estimate, saved without a new revision (database handler or owner; 0 clears it)")
	estimateBasis := fs.String("estimate-basis", "", "one-line basis of the estimate, such as \"Small, 2 paths, median of 8 Small items\"")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *revision < 1 {
		return errors.New("usage: tt work-items update --revision N --request-id KEY [fields] [--worktree DIR --branch B --commit SHA] WI_ID | tt work-items update --revision N --request-id KEY --estimate-tokens N --estimate-basis TEXT WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	req := api.CreateWorkItemUpdate{ExpectedRevision: *revision, AgentID: e.agent, RunID: e.runID, RequestID: *requestID}
	if *reportID != "" || *reportVersion != 0 || *reportDigest != "" || *reportScope != 0 {
		req.CompletionReport = &api.NarrativeReportPin{ReportID: *reportID, Version: *reportVersion, Digest: *reportDigest, ScopeRevision: *reportScope}
	}
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
	if visited["estimate-tokens"] || visited["estimate-basis"] {
		// An estimate is saved alone and leaves the item revision unchanged.
		if !visited["estimate-tokens"] {
			return errors.New("--estimate-basis needs --estimate-tokens")
		}
		for _, name := range []string{"title", "body-file", "status", "priority", "report-id", "report-version", "report-digest", "report-scope-revision", "worktree", "branch", "commit", "evidence"} {
			if visited[name] {
				return fmt.Errorf("--estimate-tokens is saved alone; drop --%s", name)
			}
		}
		if *estimateTokens < 0 || *estimateTokens > api.MaxEstimateTokens {
			return fmt.Errorf("--estimate-tokens must be between 0 and %d", api.MaxEstimateTokens)
		}
		if *estimateTokens == 0 && *estimateBasis != "" {
			return errors.New("--estimate-tokens 0 clears the estimate and takes no --estimate-basis")
		}
		if *estimateTokens > 0 && (*estimateBasis == "" || len(*estimateBasis) > api.MaxEstimateBasisBytes || strings.ContainsAny(*estimateBasis, "\r\n") || strings.TrimSpace(*estimateBasis) != *estimateBasis) {
			return fmt.Errorf("--estimate-basis is required: one line of at most %d bytes", api.MaxEstimateBasisBytes)
		}
		if *requestID == "" {
			return errors.New("--request-id is required")
		}
		req.EstimateTokens = estimateTokens
		if *estimateTokens > 0 {
			req.EstimateBasis = estimateBasis
		}
		c, err := e.client(10 * time.Second)
		if err != nil {
			return err
		}
		ctx, cancel := ctxTimeout(30 * time.Second)
		defer cancel()
		result, err := c.CreateWorkItemUpdate(ctx, project, fs.Arg(0), req)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(result)
			return nil
		}
		fmt.Println(estimateSaveLine(result))
		return nil
	}
	if req.Title == nil && req.Description == nil && req.Status == nil && req.Priority == nil {
		return errors.New("at least one update field is required")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	if *requestID == "" {
		return errors.New("--request-id is required")
	}
	acceptFlags := *worktree != "" || *branch != "" || *commit != "" || *acceptanceEvidence != ""
	var entry *api.TeamQueueEntry
	if req.Status != nil && *req.Status == "done" && e.agent != "" {
		// The item lead's or leased handler's done save carries the queue
		// acceptance, so the team never waits on a separate remembered
		// accept. The hub decides who may record it.
		entry, err = pendingQueueAcceptance(ctx, c, project, fs.Arg(0), e.agent, e.runID)
		if err != nil {
			return err
		}
	}
	if entry != nil && entry.Acceptance != nil && !acceptFlags {
		entry = nil // already accepted; nothing to carry
	}
	if entry == nil && acceptFlags {
		return errors.New("--worktree, --branch, --commit and --evidence apply only to a done save, by the item lead or the leased handler, of an item whose running team waits on acceptance")
	}
	if entry != nil {
		if *worktree == "" || *branch == "" || *commit == "" {
			return fmt.Errorf("team queue entry %s waits on acceptance: save done with the accepted --worktree, --branch and --commit", entry.ID)
		}
		realWorktree, err := acceptedWorktree(*worktree, entry.Repository)
		if err != nil {
			return err
		}
		// A retry after a lost response finds the entry already accepted: send
		// the identical request so the hub replays the saved receipt.
		if entry.Acceptance == nil {
			base, err := acceptanceBase(ctx, c, project, entry.ItemID, e.agent, e.runID, entry.BaseCommit, *commit)
			if err != nil {
				return err
			}
			if err = verifyAcceptedGit(ctx, entry.Repository, base, realWorktree, *branch, *commit); err != nil {
				return fmt.Errorf("accepted Git tuple failed worktree verification: %v", err)
			}
		}
		req.QueueAcceptance = &api.WorkItemQueueAcceptance{EntryID: entry.ID, Worktree: realWorktree, Branch: *branch, Commit: *commit, Evidence: *acceptanceEvidence}
	}
	result, err := c.CreateWorkItemUpdate(ctx, project, fs.Arg(0), req)
	if err != nil {
		return err
	}
	var accepted api.TeamQueueEntry
	if entry != nil {
		accepted, err = c.GetTeamQueueEntry(ctx, project, entry.ID)
		if err != nil {
			return fmt.Errorf("saved %s revision %d receipt %s; queue acceptance readback failed: %w", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID, err)
		}
		if accepted.Acceptance == nil || accepted.Acceptance.Commit != *commit || accepted.Acceptance.ItemRevision != result.Revision.Revision {
			return fmt.Errorf("saved %s revision %d receipt %s but queue entry %s shows no matching acceptance", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID, entry.ID)
		}
	}
	if *asJSON {
		if entry != nil {
			printJSON(struct {
				api.WorkItemUpdateResult
				QueueEntry api.TeamQueueEntry `json:"queueEntry"`
			}{result, accepted})
		} else {
			printJSON(result)
		}
	} else {
		fmt.Printf("%s revision %d receipt %s\n", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID)
		if entry != nil {
			fmt.Printf("queue acceptance recorded: entry %s revision %d commit %s\n", accepted.ID, accepted.Revision, accepted.Acceptance.Commit)
		}
	}
	return nil
}

// pendingQueueAcceptance returns the item's repository-backed team queue
// entry: the running entry that waits on its acceptance, else one already
// accepted (a retried save), else nil. The calling agent and run no longer
// select the entry: any agent caller gets it, and the hub admits only the
// item lead or the leased handler.
func pendingQueueAcceptance(ctx context.Context, c *api.Client, project, item, _, _ string) (*api.TeamQueueEntry, error) {
	// The item's own entry in any state; an older hub returns every entry.
	list, err := c.ListTeamQueuePage(ctx, project, api.TeamQueueListOptions{Item: item})
	if err != nil {
		return nil, err
	}
	var accepted *api.TeamQueueEntry
	for i := range list.Entries {
		q := list.Entries[i]
		if q.ItemID != item || q.Repository == "" {
			continue
		}
		if q.State == "running" && q.Acceptance == nil {
			return &q, nil
		}
		if q.Acceptance != nil && accepted == nil {
			accepted = &q
		}
	}
	return accepted, nil
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
	} else if result.Estimate != nil {
		fmt.Println(estimateSaveLine(result))
	} else {
		fmt.Printf("%s revision %d receipt %s\n", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID)
	}
	return nil
}

// estimateSaveLine reports the estimate one request saved. The revision is
// the item's unchanged revision.
func estimateSaveLine(result api.WorkItemUpdateResult) string {
	line := fmt.Sprintf("%s revision %d (unchanged) receipt %s", result.Revision.ItemID, result.Revision.Revision, result.Receipt.ID)
	est := result.Estimate
	if est == nil {
		return line // an older hub ignores the estimate fields
	}
	if est.Tokens == 0 {
		return line + " estimate cleared"
	}
	return line + fmt.Sprintf(" estimate %d tokens: %s", est.Tokens, est.Basis)
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

// cmdWorkItemTriage prints backlog suggestions for the owner to confirm. It
// changes nothing. --release-record adds open items that a markdown release
// record names or describes.
func cmdWorkItemTriage(e env, args []string) error {
	fs := flag.NewFlagSet("work-items triage", flag.ContinueOnError)
	project := fs.String("project", e.task, "project id")
	staleDays := fs.Int("stale-days", triage.DefaultStaleDays, "days without activity before an open item is listed as stale")
	var records stringListFlag
	fs.Var(&records, "release-record", "markdown release record to match open items against (repeatable)")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *staleDays < 1 {
		return errors.New("usage: tt work-items triage --project tsk_ID [--stale-days N] [--release-record FILE ...] [--json]")
	}
	task, err := workItemProject(e, *project)
	if err != nil {
		return err
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	out, err := c.WorkItemTriage(ctx, task, *staleDays, e.agent, e.runID)
	if err != nil {
		return err
	}
	if len(records) > 0 {
		var open []api.WorkItem
		var after int64
		for {
			page, err := c.ListWorkItems(ctx, task, "", "", after, api.MaxLimit)
			if err != nil {
				return err
			}
			for _, item := range page.Items {
				if item.Status != "done" && item.Status != "dismissed" {
					open = append(open, item)
				}
			}
			if page.Next == 0 || len(page.Items) == 0 {
				break
			}
			after = page.Next
		}
		for _, path := range records {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out.AlreadyReleased = append(out.AlreadyReleased, releaseRecordMatches(open, triage.ReleaseRows(string(data)))...)
		}
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Println("Suggestions only — nothing was changed; confirm with the owner.")
	fmt.Printf("Likely duplicates (%d):\n", len(out.Duplicates))
	for _, d := range out.Duplicates {
		fmt.Printf("  %s %q ~ %s %q (title %.2f, criteria %.2f)\n", d.Items[0].ID, d.Items[0].Title, d.Items[1].ID, d.Items[1].Title, d.TitleSimilarity, d.CriteriaSimilarity)
	}
	fmt.Printf("Possibly already delivered (%d):\n", len(out.AlreadyReleased))
	for _, r := range out.AlreadyReleased {
		switch {
		case r.Done != nil:
			fmt.Printf("  %s %q ~ done %s %q (title %.2f, criteria %.2f)\n", r.Item.ID, r.Item.Title, r.Done.ID, r.Done.Title, r.TitleSimilarity, r.CriteriaSimilarity)
		case r.NamedInRelease:
			fmt.Printf("  %s %q is named in release row: %s\n", r.Item.ID, r.Item.Title, r.Release)
		default:
			fmt.Printf("  %s %q ~ release row (title %.2f): %s\n", r.Item.ID, r.Item.Title, r.TitleSimilarity, r.Release)
		}
	}
	fmt.Printf("No activity for %d days (%d):\n", out.StaleDays, len(out.Stale))
	for _, st := range out.Stale {
		fmt.Printf("  %s %q last activity %s (%d days)\n", st.Item.ID, st.Item.Title, st.LastActivity, st.IdleDays)
	}
	if len(out.HeldForTriage) > 0 {
		fmt.Printf("Review follow-ups held for triage (%d):\n", len(out.HeldForTriage))
	}
	for _, h := range out.HeldForTriage {
		fmt.Printf("  %s %q filed from review message #%d\n", h.Item.ID, h.Item.Title, h.SourceMessageSeq)
	}
	return nil
}

// releaseRecordMatches lists open items a release row names by ID, or whose
// title matches one of the row's cells.
func releaseRecordMatches(open []api.WorkItem, rows []triage.ReleaseRow) []api.TriageAlreadyDone {
	var out []api.TriageAlreadyDone
	for _, item := range open {
		ref := api.TriageItem{ID: item.ID, Title: item.Title, Status: item.Status}
		for _, row := range rows {
			named := false
			for _, id := range row.ItemIDs {
				if id == item.ID {
					named = true
				}
			}
			best := 0.0
			for _, cell := range row.Cells {
				if score := triage.Similarity(item.Title, strings.ReplaceAll(cell, "`", "")); score > best {
					best = score
				}
			}
			if named || best >= triage.TitleThreshold {
				out = append(out, api.TriageAlreadyDone{Item: ref, Release: row.Text, TitleSimilarity: best, NamedInRelease: named})
			}
		}
	}
	return out
}
