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
