package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 2a (docs/broker-phase-2a.md): obligations, ack and progress.

// cmdObligations lists the caller's open obligations. Listing counts as
// delivery to this run, never as acknowledgement. --overdue lists every
// overdue obligation in the project, for the lead or owner.
func cmdObligations(e env, args []string) error {
	fs := flag.NewFlagSet("obligations", flag.ContinueOnError)
	overdue := fs.Bool("overdue", false, "every overdue obligation in the project")
	all := fs.Bool("all", false, "include closed obligations")
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{2, err}
	}
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	agent, run := e.agent, e.runID
	if *overdue {
		agent, run = "", ""
	}
	list, err := c.ListObligations(ctx, task, agent, run, !*all, *overdue)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(list)
		return nil
	}
	names := map[string]string{}
	if agents, err := c.ListAgents(ctx, task); err == nil {
		for _, a := range agents {
			names[a.ID] = a.Name
		}
	}
	for _, o := range list {
		line := fmt.Sprintf("#%d %s %s → %s: %s", o.MessageSeq, o.SourceKind, o.State, or(names[o.AgentID], o.AgentID), o.Subject)
		if o.Overdue != "" {
			line += " [overdue: " + o.Overdue + "]"
		}
		if o.State != api.ObligationClosed && o.Needs != api.ObligationNeedsDelivery {
			line += " (due " + o.DueAt.Local().Format("15:04") + ")"
		}
		if o.Outcome != "" {
			line += " outcome=" + o.Outcome
		}
		fmt.Println(line)
	}
	if len(list) == 0 {
		fmt.Println("(no obligations)")
	} else if !*overdue {
		fmt.Println("Acknowledge with `tt ack SEQ`, record progress with `tt progress SEQ --text ...`, and finish with `tt send --reply-to SEQ` (result, answer, decline, or block).")
	}
	return nil
}

// cmdObligationAction implements tt ack SEQ and tt progress SEQ.
func cmdObligationAction(e env, action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	text := fs.String("text", "", "progress note")
	// Accept "tt progress 12 --text ..." as well as flags first.
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{2, err}
	}
	if fs.NArg() != 1 {
		return &exitError{2, fmt.Errorf("usage: tt %s SEQ", action)}
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(fs.Arg(0), "#"), 10, 64)
	if err != nil || seq < 1 {
		return &exitError{2, fmt.Errorf("SEQ must be a message number")}
	}
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	if e.agent == "" || e.runID == "" {
		return errors.New("tt " + action + " needs an agent session (TAILTERM_AGENT and TAILTERM_RUN)")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	o, err := c.ObligationAction(ctx, task, seq, action, api.ObligationActionRequest{AgentID: e.agent, RunID: e.runID, Text: *text})
	if err != nil {
		return err
	}
	fmt.Printf("#%d %s\n", o.MessageSeq, o.State)
	return nil
}

// cmdReassign moves an open obligation to another agent (lead or owner use).
func cmdReassign(e env, args []string) error {
	fs := flag.NewFlagSet("reassign", flag.ContinueOnError)
	to := fs.String("to", "", "agent name or id")
	reason := fs.String("reason", "", "why it moves")
	if len(args) > 1 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string{}, args[1:]...), args[0])
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &exitError{2, err}
	}
	if fs.NArg() != 1 || *to == "" {
		return &exitError{2, errors.New("usage: tt reassign OBLIGATION_ID --to AGENT [--reason TEXT]")}
	}
	task, err := e.requireTask()
	if err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	target, err := resolveAgent(ctx, c, task, *to)
	if err != nil {
		return err
	}
	// From an agent session this acts as that agent (only the lead may);
	// without one it is the owner.
	m, err := c.ReassignObligation(ctx, task, fs.Arg(0), api.ObligationReassignRequest{ToAgentID: target, Reason: *reason, ActorAgentID: e.agent, ActorRunID: e.runID})
	if err != nil {
		return err
	}
	fmt.Printf("reassigned as #%d\n", m.Seq)
	return nil
}

// unackedObligations counts this run's obligations that still need an ack.
func unackedObligations(e env) (int, string) {
	c, err := e.client(2 * time.Second)
	if err != nil || e.runID == "" {
		return 0, ""
	}
	ctx, cancel := ctxTimeout(2 * time.Second)
	defer cancel()
	list, err := c.ListObligations(ctx, e.task, e.agent, e.runID, true, false)
	if err != nil {
		return 0, ""
	}
	var seqs []string
	for _, o := range list {
		if o.Needs != api.ObligationNeedsDelivery && (o.State == api.ObligationQueued || o.State == api.ObligationDelivered) {
			seqs = append(seqs, fmt.Sprintf("#%d", o.MessageSeq))
		}
	}
	return len(seqs), strings.Join(seqs, ", ")
}

// unreadDirectedFreeText counts unread free-text messages another agent sent
// directly to this one. Typed messages and directed owner posts are covered by
// obligations instead, so acknowledging one never leaves an agent stuck on it.
func unreadDirectedFreeText(e env) int {
	c, err := e.client(2 * time.Second)
	if err != nil {
		return 0
	}
	ctx, cancel := ctxTimeout(2 * time.Second)
	defer cancel()
	after, err := readCursor(ctx, c, e.task, e.agent)
	if err != nil {
		return 0
	}
	msgs, err := c.ListMessages(ctx, e.task, after, e.agent, 200)
	if err != nil {
		return 0
	}
	n := 0
	for _, m := range msgs {
		if m.To == e.agent && m.From.AgentID != "" && m.From.AgentID != e.agent && m.Envelope == nil {
			n++
		}
	}
	return n
}
