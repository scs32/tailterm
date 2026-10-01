package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"math/big"
	"os"
	"time"
)

func cmdUsage(e env, args []string) error {
	if len(args) > 0 && args[0] == "prices" {
		return cmdUsagePrices(e, args[1:])
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	item := fs.String("item", "", "item ID")
	from := fs.String("from", "", "inclusive RFC3339 request time")
	to := fs.String("to", "", "exclusive RFC3339 request time")
	asJSON := fs.Bool("json", false, "versioned JSON report")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*project, "tsk") {
		return errors.New("usage: tt usage [--project ID] [--item ID] [--from RFC3339] [--to RFC3339] [--json]")
	}
	q := api.UsageQuery{Item: *item}
	var err error
	if *from != "" {
		q.From, err = time.Parse(time.RFC3339Nano, *from)
		if err != nil {
			return err
		}
	}
	if *to != "" {
		q.To, err = time.Parse(time.RFC3339Nano, *to)
		if err != nil {
			return err
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return errors.New("from must precede to")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	out, err := c.Usage(ctx, *project, q)
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("Usage %s · prices revision %d · UTC request times\n", out.ProjectID, out.PriceRevision)
	for _, row := range append(out.Items, out.Overhead) {
		printUsageRow(row.Title, row.Summary)
		if out.TimeVersion > 0 {
			printUsageTime(row.Time)
		}
		for _, group := range row.Phases {
			printUsageRow("  "+group.Label, group.Summary)
		}
		for _, group := range row.Roles {
			printUsageRow("  role "+group.Label, group.Summary)
		}
		for _, group := range row.Models {
			printUsageRow("  model "+group.Label, group.Summary)
		}
		for _, group := range row.PhaseRoles {
			printUsageRow("  phase/role "+group.Label, group.Summary)
		}
	}
	return nil
}
func printUsageRow(label string, s api.UsageSummary) {
	fmt.Printf("%s: %s · requests %d · allocated %s · tokens %v", label, s.State, s.Requests, s.AllocatedTurns, s.Tokens)
	if s.AverageContext != nil {
		fmt.Printf(" · average context %s", *s.AverageContext)
	} else {
		fmt.Print(" · average context unavailable")
	}
	if s.CachedShare != nil {
		fmt.Printf(" · cached-input share %s", *s.CachedShare)
	}
	if len(s.PricedSubtotal) > 0 {
		fmt.Printf(" · estimated subtotal %v (complete=%t)", s.PricedSubtotal, s.CostComplete)
	}
	fmt.Println()
}

// usageMillis reads an exact millisecond quantity ("1500" or "3001/2").
func usageMillis(value string) float64 {
	r, ok := new(big.Rat).SetString(value)
	if !ok {
		return 0
	}
	ms, _ := r.Float64()
	return ms
}

// usageDuration shows whole seconds, or milliseconds under one second.
func usageDuration(value string) string {
	d := time.Duration(usageMillis(value) * float64(time.Millisecond))
	if d >= time.Second {
		d = d.Round(time.Second)
	} else {
		d = d.Round(time.Millisecond)
	}
	return d.String()
}
func usageShare(part, whole float64) string {
	if whole <= 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", 100*part/whole)
}

// printUsageTime prints where an item's wall time went: one line for the
// item, then one per agent, phase and role, and its longest waits.
func printUsageTime(t *api.UsageTime) {
	if t == nil {
		fmt.Println("  time: not measured")
		return
	}
	model, tool, waiting, unmeasured := usageMillis(t.ModelMs), usageMillis(t.ToolMs), usageMillis(t.WaitingMs), usageMillis(t.UnmeasuredMs)
	whole := model + tool + waiting + unmeasured
	fmt.Printf("  time: wall %s · model %s · tool %s · waiting %s", usageDuration(t.WallMs), usageShare(model, whole), usageShare(tool, whole), usageShare(waiting, whole))
	if unmeasured > 0 {
		fmt.Printf(" · unmeasured %s", usageShare(unmeasured, whole))
	}
	fmt.Printf(" · polls %d", t.Polls)
	if t.Polls > 0 {
		fmt.Printf(" (%s)", usageDuration(t.PollMs))
	}
	fmt.Printf(" · %s to %s\n", t.From.UTC().Format(time.RFC3339), t.To.UTC().Format(time.RFC3339))
	wall := usageMillis(t.WallMs)
	fmt.Printf("  timeline: some model working %s · only tools running %s · nobody active %s", usageShare(usageMillis(t.Timeline.ModelMs), wall), usageShare(usageMillis(t.Timeline.ToolsOnlyMs), wall), usageShare(usageMillis(t.Timeline.IdleMs), wall))
	if usageMillis(t.Timeline.UnmeasuredMs) > 0 {
		fmt.Printf(" · unmeasured %s", usageShare(usageMillis(t.Timeline.UnmeasuredMs), wall))
	}
	fmt.Println()
	for _, a := range t.Agents {
		fmt.Printf("  time agent %s (%s): model %s · tool %s · waiting %s", a.Name, a.Role, usageDuration(a.ModelMs), usageDuration(a.ToolMs), usageDuration(a.WaitingMs))
		if usageMillis(a.UnmeasuredMs) > 0 {
			fmt.Printf(" · unmeasured %s", usageDuration(a.UnmeasuredMs))
		}
		fmt.Printf(" · polls %d\n", a.Polls)
	}
	for _, group := range []struct {
		label string
		rows  []api.UsageTimeSplit
	}{{"time phase", t.Phases}, {"time role", t.Roles}, {"time phase/role", t.PhaseRoles}} {
		for _, row := range group.rows {
			fmt.Printf("  %s %s: model %s · tool %s · waiting %s\n", group.label, row.Key, usageDuration(row.ModelMs), usageDuration(row.ToolMs), usageDuration(row.WaitingMs))
		}
	}
	for _, w := range t.Waits {
		target := "nothing the Board shows"
		if w.MessageSeq > 0 {
			target = fmt.Sprintf("#%d %q", w.MessageSeq, w.Subject)
		}
		fmt.Printf("  wait %s: %s · %s · awaited by %s\n", w.Cause, target, usageDuration(w.Ms), w.AwaitedBy)
	}
}
func cmdUsagePrices(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New("usage: tt usage prices get|set [--project ID] [--file PATH --expected-revision N --request-id KEY]")
	}
	fs := flag.NewFlagSet("usage prices", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	file := fs.String("file", "", "JSON array of dated model prices")
	revision := fs.Int64("expected-revision", -1, "current table revision")
	key := fs.String("request-id", "", "stable retry identity")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	if args[0] == "get" {
		out, err := c.UsagePrices(ctx, *project)
		if err != nil {
			return err
		}
		printJSON(out)
		return nil
	}
	if *revision < 0 || *key == "" || *file == "" {
		return errors.New("set requires file, expected-revision and request-id")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var rows []api.UsagePrice
	if err = json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	out, err := c.SetUsagePrices(ctx, *project, api.UsagePriceRequest{RequestID: *key, ExpectedRevision: *revision, Rows: rows})
	if err != nil {
		return err
	}
	printJSON(out)
	return nil
}
