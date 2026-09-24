package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// errRetiredWriter answers legacy commands that broker phase 3 retired.
var errRetiredWriter = &exitError{2, errors.New("retired in broker phase 3: legacy directives and operational records no longer accept writes. Send typed messages with tt send; the hub tracks them as obligations (tt obligations, tt ack, tt progress)")}

// cmdOwner is the owner's control over obligations (broker phase 3):
// extend a deadline, answer on the recipient's behalf, or cancel.
func cmdOwner(e env, args []string) error {
	usage := errors.New("usage: tt owner extend OBLIGATION_ID --for 30m [--reason T] | answer OBLIGATION_ID --text T | cancel OBLIGATION_ID --reason T")
	if len(args) < 2 {
		return usage
	}
	if e.agent != "" {
		return errors.New("tt owner is for the owner, not agent sessions")
	}
	sub, obligation := args[0], args[1]
	fs := flag.NewFlagSet("owner "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	forFlag := fs.String("for", "", "extend: new deadline from now, such as 30m or 2h")
	reason := fs.String("reason", "", "why (required for cancel)")
	text := fs.String("text", "", "answer: the answer to post")
	requestID := fs.String("request-id", "", "stable retry key (default: derived per minute)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *task == "" || !api.ValidID(obligation, "obl") {
		return usage
	}
	if *requestID == "" {
		scope := time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339)
		digest := sha256.Sum256([]byte(sub + "\x00" + obligation + "\x00" + *forFlag + "\x00" + *reason + "\x00" + *text + "\x00" + scope))
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
	case "answer":
		out, err = c.AnswerObligation(ctx, *task, obligation, api.ObligationAnswerRequest{Text: *text, RequestID: *requestID})
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
