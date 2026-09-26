package main

import (
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// cmdMessageChecks reports broker phase-1 shadow checks (docs/broker-phase-1.md).
func cmdMessageChecks(e env, args []string) error {
	fs := flag.NewFlagSet("message-checks", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "only checks newer than this; 0 means all")
	summary := fs.Bool("summary", false, "print adoption and Jev totals instead of one line per post")
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
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(60 * time.Second)
	defer cancel()
	var checks []api.MessageCheck
	for after := int64(0); ; {
		page, err := c.ListMessageChecks(ctx, task, after, 500)
		if err != nil {
			return err
		}
		checks = append(checks, page...)
		if len(page) < 500 {
			break
		}
		after = page[len(page)-1].Seq
	}
	if *since > 0 {
		cutoff := time.Now().Add(-*since)
		kept := checks[:0]
		for _, check := range checks {
			if !check.CreatedAt.Before(cutoff) {
				kept = append(kept, check)
			}
		}
		checks = kept
	}
	if *asJSON {
		printJSON(checks)
		return nil
	}
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	if *summary {
		fmt.Print(summarizeChecks(checks, names))
		reviews, err := c.ListReviewConvergence(ctx, task)
		if err != nil {
			fmt.Printf("Review history unavailable (unknown): %v\n", err)
		}
		if err == nil {
			fmt.Print(summarizeReviews(reviews))
		}
		if obligations, err := c.ListObligations(ctx, task, "", "", false, false); err == nil {
			cutoff := time.Time{}
			if *since > 0 {
				cutoff = time.Now().Add(-*since)
			}
			fmt.Print(summarizeAcks(obligations, names, cutoff, time.Now()))
			fmt.Print(summarizeHandlerResponses(obligations, cutoff, time.Now()))
		}
		return nil
	}
	for _, check := range checks {
		sender := or(names[check.AgentID], or(check.AgentID, "human"))
		line := fmt.Sprintf("#%d %s %s jev:%s", check.Seq, sender, check.Form, check.JevStatus)
		if flags := jevConcerns(check); len(flags) > 0 {
			line += " [" + strings.Join(flags, ", ") + "]"
		}
		fmt.Println(line)
		for _, p := range check.Problems {
			fmt.Println("    " + p.String())
		}
	}
	if len(checks) == 0 {
		fmt.Println("(no checks)")
	}
	return nil
}

// summarizeHandlerResponses uses immutable request creation and typed outcome
// timestamps. Delivery, acknowledgement and agent lifecycle do not affect it.
func summarizeHandlerResponses(list []api.Obligation, cutoff, now time.Time) string {
	var completed []time.Duration
	var worst, oldest api.Obligation
	var worstDuration, oldestAge time.Duration
	var pending, blocked, declined, cancelled, withdrawn int
	for _, o := range list {
		if !o.HandlerRequest || o.CreatedAt.Before(cutoff) {
			continue
		}
		switch o.Outcome {
		case api.OutcomeCancelled:
			cancelled++
			continue
		case api.OutcomeWithdrawn:
			withdrawn++
			continue
		}
		if o.State != api.ObligationClosed {
			pending++
			if o.State == api.ObligationBlocked {
				blocked++
			}
			age := now.Sub(o.CreatedAt)
			if age > oldestAge {
				oldest, oldestAge = o, age
			}
			continue
		}
		if o.Outcome != api.OutcomeResult && o.Outcome != api.OutcomeDeclined {
			continue
		}
		if o.Outcome == api.OutcomeDeclined {
			declined++
		}
		if o.ClosedAt == nil {
			continue
		}
		d := o.ClosedAt.Sub(o.CreatedAt)
		if d < 0 {
			d = 0
		}
		completed = append(completed, d)
		if d > worstDuration {
			worst, worstDuration = o, d
		}
	}
	out := fmt.Sprintf("Handler requests: %d pending (%d blocked), oldest %s", pending, blocked, oldestAge)
	if pending > 0 {
		out += fmt.Sprintf(" (#%d)", oldest.MessageSeq)
	}
	out += "\n"
	if len(completed) > 0 {
		sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
		median := completed[len(completed)/2]
		if len(completed)%2 == 0 {
			median = (completed[len(completed)/2-1] + median) / 2
		}
		out += fmt.Sprintf("Handler responses: %d result/decline (%d declined), median %s, max %s (#%d→#%d)\n", len(completed), declined, median, worstDuration, worst.MessageSeq, worst.OutcomeSeq)
	} else {
		out += "Handler responses: no result/decline in this window\n"
	}
	out += fmt.Sprintf("Handler requests cancelled: %d, withdrawn: %d\n", cancelled, withdrawn)
	return out
}

// Nouls where a high score is a concern, and nouls where a low score is.
var (
	jevHighConcerns = []string{"ack_only", "cut_off", "manipulation"}
	jevLowConcerns  = []string{"readable", "subject_plain"}
)

func jevConcerns(check api.MessageCheck) []string {
	if check.Jev == nil {
		return nil
	}
	var out []string
	for _, k := range jevHighConcerns {
		if p, ok := check.Jev.Nouls[k]; ok && p >= 0.5 {
			out = append(out, k)
		}
	}
	for _, k := range jevLowConcerns {
		if p, ok := check.Jev.Nouls[k]; ok && p < 0.5 {
			out = append(out, "not "+k)
		}
	}
	return out
}

func summarizeChecks(checks []api.MessageCheck, names map[string]string) string {
	var b strings.Builder
	forms := map[string]int{}
	jevStatus := map[string]int{}
	concerns := map[string]int{}
	problems := map[string]int{}
	type tally struct{ valid, total int }
	senders := map[string]*tally{}
	agentPosts := 0
	for _, check := range checks {
		if check.Form == api.CheckFormHuman {
			continue
		}
		agentPosts++
		forms[check.Form]++
		jevStatus[check.JevStatus]++
		for _, c := range jevConcerns(check) {
			concerns[c]++
		}
		for _, p := range check.Problems {
			problems[p.String()]++
		}
		sender := or(names[check.AgentID], check.AgentID)
		if senders[sender] == nil {
			senders[sender] = &tally{}
		}
		senders[sender].total++
		if check.Form == api.CheckFormTyped || check.Form == api.CheckFormTextConvention {
			senders[sender].valid++
		}
	}
	fmt.Fprintf(&b, "Agent posts: %d\n", agentPosts)
	if agentPosts == 0 {
		return b.String()
	}
	for _, form := range []string{api.CheckFormTyped, api.CheckFormTextConvention, api.CheckFormTextConventionInvalid, api.CheckFormFreeText} {
		fmt.Fprintf(&b, "  %-24s %d\n", form, forms[form])
	}
	valid := forms[api.CheckFormTyped] + forms[api.CheckFormTextConvention]
	fmt.Fprintf(&b, "Valid (typed + convention): %d%% (%d of %d; phase-1 exit needs 90%%)\n", valid*100/agentPosts, valid, agentPosts)
	if len(problems) > 0 {
		b.WriteString("Top problems:\n")
		for i, p := range topCounts(problems) {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "  %3d  %s\n", problems[p], p)
		}
	}
	fmt.Fprintf(&b, "Jev: %d scored, %d unavailable, %d pending, %d disabled\n",
		jevStatus[api.JevStatusScored], jevStatus[api.JevStatusUnavailable], jevStatus[api.JevStatusPending], jevStatus[api.JevStatusDisabled])
	if jevStatus[api.JevStatusScored] > 0 {
		var parts []string
		for _, k := range jevHighConcerns {
			parts = append(parts, fmt.Sprintf("%s %d", k, concerns[k]))
		}
		for _, k := range jevLowConcerns {
			parts = append(parts, fmt.Sprintf("not %s %d", k, concerns["not "+k]))
		}
		fmt.Fprintf(&b, "Jev concerns (of scored): %s\n", strings.Join(parts, ", "))
	}
	b.WriteString("By sender (valid of total):\n")
	order := make([]string, 0, len(senders))
	for s := range senders {
		order = append(order, s)
	}
	sort.Slice(order, func(i, j int) bool {
		if senders[order[i]].total != senders[order[j]].total {
			return senders[order[i]].total > senders[order[j]].total
		}
		return order[i] < order[j]
	})
	for _, s := range order {
		t := senders[s]
		fmt.Fprintf(&b, "  %-24s %d/%d\n", s, t.valid, t.total)
	}
	return b.String()
}

func topCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// summarizeAcks reports how long agents take to acknowledge work (broker
// phase 3.1): median and worst latency from delivery to acknowledgement, and
// work still unacknowledged.
func summarizeAcks(list []api.Obligation, names map[string]string, cutoff, now time.Time) string {
	var latencies []time.Duration
	var worst api.Obligation
	var worstD time.Duration
	gating := 0
	withdrawn := 0
	for _, o := range list {
		if o.Needs == api.ObligationNeedsDelivery || o.CreatedAt.Before(cutoff) {
			continue
		}
		if o.Outcome == api.OutcomeWithdrawn {
			withdrawn++
		}
		if gatesPosts(o, now) {
			gating++
		}
		if o.AckedAt == nil {
			continue
		}
		d := ackLatency(o)
		if len(latencies) == 0 || d > worstD {
			worst, worstD = o, d
		}
		latencies = append(latencies, d)
	}
	if len(latencies) == 0 && gating == 0 {
		return fmt.Sprintf("Acknowledgement: no acknowledged work in this window\nWithdrawn: %d\n", withdrawn)
	}
	out := ""
	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		median := latencies[len(latencies)/2]
		if len(latencies)%2 == 0 {
			median = (latencies[len(latencies)/2-1] + median) / 2
		}
		out = fmt.Sprintf("Acknowledgement (from delivery): %d acknowledged, median %s, worst %s (#%d by %s)\n", len(latencies),
			median.Round(time.Second), worstD.Round(time.Second), worst.MessageSeq, or(names[worst.AgentID], worst.AgentID))
	}
	if gating > 0 {
		out += fmt.Sprintf("Unacknowledged past the %s grace: %d (their recipients' posts are refused)\n", api.ObligationAckGrace, gating)
	}
	out += fmt.Sprintf("Withdrawn: %d\n", withdrawn)
	return out
}

// ackLatency is the time from delivery to acknowledgement. Work acknowledged
// straight from the queue counts from creation: acknowledging it (by tt ack
// or a reply) stamps delivered_at with the same instant as acked_at.
func ackLatency(o api.Obligation) time.Duration {
	from := o.CreatedAt
	if o.DeliveredAt != nil && o.DeliveredAt.Before(*o.AckedAt) {
		from = *o.DeliveredAt
	}
	return o.AckedAt.Sub(from)
}

func summarizeReviews(list []api.ReviewConvergence) string {
	var out strings.Builder
	for _, item := range list {
		if item.History == "unknown" {
			fmt.Fprintf(&out, "%s reviews: unknown; follow-ups: unknown\n", item.ItemID)
		} else {
			fmt.Fprintf(&out, "%s reviews: %d/2; follow-ups: %d\n", item.ItemID, len(item.Rounds), len(item.FollowUps))
		}
	}
	return out.String()
}
