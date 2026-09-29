package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// errRetiredWriter answers legacy commands that broker phase 3 retired.
var errRetiredWriter = &exitError{2, errors.New("retired in broker phase 3: legacy directives and operational records no longer accept writes. Send typed messages with tt send; the hub tracks them as obligations (tt obligations, tt ack, tt progress)")}

var obligationIDRE = regexp.MustCompile(`^obl_[0-9a-f]{16}$`)

// cmdOwner is the owner's control over obligations (broker phase 3):
// extend a deadline, answer on the recipient's behalf, or cancel.
func cmdOwner(e env, args []string) error {
	if len(args) > 0 && (args[0] == "intervene" || args[0] == "interventions") {
		// Checked before any request: an agent session never records or reads these.
		if e.agent != "" {
			return errors.New("tt owner is for the owner, not agent sessions")
		}
		if args[0] == "intervene" {
			return cmdOwnerIntervene(e, args[1:])
		}
		return cmdOwnerInterventions(e, args[1:])
	}
	usage := errors.New("usage: tt owner extend OBLIGATION_ID --for 30m [--reason T] | answer OBLIGATION_ID (--text T | --approve) [--session S] | delegate OBLIGATION_ID --session S --authorization REF [--agent ID --run ID] | cancel OBLIGATION_ID --reason T | intervene --task ID --kind KIND --item ID [--product-item ID] --text T | interventions --task ID [--tz ZONE]")
	if len(args) < 2 {
		return usage
	}
	if e.agent != "" && (len(args) < 1 || args[0] != "answer") {
		return errors.New("tt owner is for the owner, not agent sessions")
	}
	sub, obligation := args[0], args[1]
	fs := flag.NewFlagSet("owner "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	forFlag := fs.String("for", "", "extend: new deadline from now, such as 30m or 2h")
	reason := fs.String("reason", "", "why (required for cancel)")
	text := fs.String("text", "", "answer: the answer to post")
	session := fs.String("session", "", "delegate: exact owner-authorized session identity")
	authorization := fs.String("authorization", "", "delegate: owner authorization reference")
	delegateAgent := fs.String("agent", "", "delegate: optional exact agent id")
	delegateRun := fs.String("run", "", "delegate: optional exact run id")
	approve := fs.Bool("approve", false, "answer with server-retained exact expected text")
	requestID := fs.String("request-id", "", "stable retry key (default: derived per minute)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *task == "" || !obligationIDRE.MatchString(obligation) {
		return usage
	}
	if *requestID == "" {
		scope := time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339)
		digest := sha256.Sum256([]byte(sub + "\x00" + obligation + "\x00" + *forFlag + "\x00" + *reason + "\x00" + *text + "\x00" + *session + "\x00" + *authorization + "\x00" + *delegateAgent + "\x00" + *delegateRun + fmt.Sprint(*approve) + "\x00" + scope))
		*requestID = fmt.Sprintf("owner-%s-%x", sub, digest[:12])
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	var out api.OwnerActionResult
	switch sub {
	case "extend":
		out, err = c.ExtendObligation(ctx, *task, obligation, api.ObligationExtendRequest{For: *forFlag, Reason: *reason, RequestID: *requestID})
	case "delegate":
		out, err = c.DelegateOwnerObligation(ctx, *task, obligation, api.OwnerDelegationRequest{Session: *session, AgentID: *delegateAgent, RunID: *delegateRun, AuthorizationRef: *authorization, RequestID: *requestID})
	case "answer":
		if e.agent != "" && *session == "" {
			return errors.New("an agent answer requires an owner-authorized delegate session")
		}
		out, err = c.AnswerObligation(ctx, *task, obligation, api.ObligationAnswerRequest{Text: *text, RequestID: *requestID, Approve: *approve, DelegateSession: *session, AgentID: e.agent, RunID: e.runID})
	case "cancel":
		out, err = c.CancelObligation(ctx, *task, obligation, api.ObligationCancelRequest{Reason: *reason, RequestID: *requestID})
	default:
		return usage
	}
	if err != nil {
		return err
	}
	if o := out.Obligation; o != nil {
		fmt.Printf("%s #%d: %s", sub, o.MessageSeq, o.State)
		if o.Outcome != "" {
			fmt.Printf(" (%s)", o.Outcome)
		}
		if sub == "extend" {
			fmt.Printf(", due %s", o.DueAt.Local().Format("15:04"))
		}
		fmt.Println()
	}
	return nil
}

const interventionUsage = "usage: tt owner intervene --task ID --kind KIND --item ID [--product-item ID] --text T [--request-id R] [--json]\n  --task is the project ID; required unless TAILTERM_TASK is set\n  KIND is one of "

// cmdOwnerIntervene records one owner-side intervention on the Board.
func cmdOwnerIntervene(e env, args []string) error {
	usage := errors.New(interventionUsage + strings.Join(api.InterventionKinds, ", "))
	fs := flag.NewFlagSet("owner intervene", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	kind := fs.String("kind", "", "one of "+strings.Join(api.InterventionKinds, ", "))
	item := fs.String("item", "", "the work item the intervention concerns (this project)")
	product := fs.String("product-item", "", "the product item expected to remove this intervention (any project)")
	text := fs.String("text", "", "what the owner did and why")
	requestID := fs.String("request-id", "", "stable retry key (default: derived per minute)")
	asJSON := fs.Bool("json", false, "print the recorded message as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *task == "" {
		return usage
	}
	req := api.CreateInterventionRequest{Kind: *kind, ItemID: *item, ProductItemID: *product, Text: *text, RequestID: *requestID}
	if err := api.ValidateIntervention(req); err != nil {
		return fmt.Errorf("%v\n%v", err, usage)
	}
	if req.RequestID == "" {
		scope := time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339)
		digest := sha256.Sum256([]byte(req.Kind + "\x00" + req.ItemID + "\x00" + req.ProductItemID + "\x00" + req.Text + "\x00" + scope))
		req.RequestID = fmt.Sprintf("owner-intervene-%x", digest[:12])
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	m, err := c.CreateIntervention(ctx, *task, req)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(m)
		return nil
	}
	link := "no product item"
	if m.Intervention != nil && m.Intervention.ProductItemID != "" {
		link = "product fix " + m.Intervention.ProductItemID
	}
	fmt.Printf("intervention #%d recorded: %s on %s (%s)\n", m.Seq, req.Kind, req.ItemID, link)
	return nil
}

// cmdOwnerInterventions prints the project's per-day and per-kind counts.
func cmdOwnerInterventions(e env, args []string) error {
	fs := flag.NewFlagSet("owner interventions", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	zone := fs.String("tz", localTimeZoneName(), "IANA time zone for day buckets")
	asJSON := fs.Bool("json", false, "print every intervention and the summary as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *task == "" {
		return errors.New("usage: tt owner interventions --task ID [--tz ZONE] [--json]\n  --task is the project ID; required unless TAILTERM_TASK is set")
	}
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	var all api.InterventionList
	for after := int64(0); ; {
		page, err := c.ListInterventions(ctx, *task, after, api.MaxInterventionPage, *zone)
		if err != nil {
			return err
		}
		all.Summary = page.Summary
		all.Interventions = append(all.Interventions, page.Interventions...)
		if page.NextAfter == 0 {
			break
		}
		after = page.NextAfter
	}
	if *asJSON {
		printJSON(all)
		return nil
	}
	s := all.Summary
	fmt.Printf("%d interventions (%s); %d of %d linked to a product item\n", s.Total, s.TimeZone, s.Linked, s.Total)
	if s.Total == 0 {
		return nil
	}
	fmt.Printf("by kind: %s\n", formatKindCounts(s.ByKind))
	for _, day := range s.Days {
		fmt.Printf("%s  %3d  linked %d  %s\n", day.Day, day.Total, day.Linked, formatKindCounts(day.ByKind))
	}
	if len(s.Unlinked) > 0 {
		fmt.Println("unlinked:")
		for _, ref := range s.Unlinked {
			fmt.Printf("  #%d  %s  %s  %s\n", ref.Seq, ref.Day, ref.Kind, ref.ItemID)
		}
	}
	return nil
}

// formatKindCounts lists kinds in the fixed vocabulary order.
func formatKindCounts(counts map[string]int) string {
	order := map[string]int{}
	for i, kind := range api.InterventionKinds {
		order[kind] = i
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return order[kinds[i]] < order[kinds[j]] })
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		parts = append(parts, fmt.Sprintf("%s %d", kind, counts[kind]))
	}
	return strings.Join(parts, ", ")
}

// localTimeZoneName returns this machine's IANA zone so day buckets match the
// owner's calendar; the hub rejects anything it cannot load.
func localTimeZoneName() string {
	if tz := os.Getenv("TZ"); tz != "" && !strings.HasPrefix(tz, ":") && !strings.HasPrefix(tz, "/") {
		if _, err := time.LoadLocation(tz); err == nil && tz != "Local" {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.LastIndex(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}
