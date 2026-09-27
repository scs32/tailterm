package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// errRetiredWriter answers legacy commands that broker phase 3 retired.
var errRetiredWriter = &exitError{2, errors.New("retired in broker phase 3: legacy directives and operational records no longer accept writes. Send typed messages with tt send; the hub tracks them as obligations (tt obligations, tt ack, tt progress)")}

var obligationIDRE = regexp.MustCompile(`^obl_[0-9a-f]{16}$`)

// cmdOwner is the owner's control over obligations (broker phase 3):
// extend a deadline, answer on the recipient's behalf, or cancel.
func cmdOwner(e env, args []string) error {
	usage := errors.New("usage: tt owner extend OBLIGATION_ID --for 30m [--reason T] | answer OBLIGATION_ID (--text T | --approve) [--session S] | delegate OBLIGATION_ID --session S --authorization REF [--agent ID --run ID] | cancel OBLIGATION_ID --reason T")
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
