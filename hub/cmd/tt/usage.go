package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func cmdUsage(e env, args []string) error {
	if len(args) > 0 && args[0] == "prices" {
		return cmdUsagePrices(e, args[1:])
	}
	if len(args) > 0 && args[0] == "warning" {
		return cmdUsageWarning(e, args[1:])
	}
	if len(args) > 0 && args[0] == "budget" {
		return cmdUsageBudget(e, args[1:])
	}
	if len(args) > 0 && args[0] == "defaults" {
		return cmdUsageDefaults(e, args[1:])
	}
	if len(args) > 0 && args[0] == "hold" {
		return cmdUsageHold(e, args[1:])
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	item := fs.String("item", "", "item ID")
	from := fs.String("from", "", "inclusive RFC3339 request time")
	to := fs.String("to", "", "exclusive RFC3339 request time")
	asJSON := fs.Bool("json", false, "versioned JSON report")
	calibration := fs.Bool("calibration", false, "list done items with actual tokens, lane, owned paths, template and estimate")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || !api.ValidID(*project, "tsk") || (*calibration && (*item != "" || *from != "" || *to != "")) {
		return errors.New("usage: tt usage [--project ID] [--item ID] [--from RFC3339] [--to RFC3339] [--json] | tt usage --calibration [--project ID] [--json]")
	}
	if *calibration {
		return usageCalibration(e, *project, *asJSON)
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
		if text := formatTokenBudget(row.Budget); text != "" {
			fmt.Println("  budget: " + text)
		}
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
	fmt.Println(formatUsageRow(label, s))
}

var usageClassLabels = map[string]string{"input": "input", "cached": "cached", "cacheWrite": "cache write", "output": "output", "reasoning": "reasoning"}

// formatUsageRow renders one summary for people. The report keeps exact
// rational strings; text rounds them for display and --json is unchanged.
func formatUsageRow(label string, s api.UsageSummary) string {
	total, classes := new(big.Rat), []string{}
	reported := false
	for _, class := range api.UsageClasses {
		value := "unavailable" // an absent class was not measured; it is not zero
		if raw, ok := s.Tokens[class]; ok {
			if n, valid := new(big.Rat).SetString(raw); valid {
				total.Add(total, n)
				reported = true
				value = formatTokenCount(raw)
			}
		}
		classes = append(classes, usageClassLabels[class]+" "+value)
	}
	tokens := "unavailable"
	if reported {
		tokens = formatTokenCount(total.RatString())
	}
	row := fmt.Sprintf("%s: %s · requests %d · allocated %s · tokens %s (%s)", label, s.State, s.Requests, formatRatDecimal(s.AllocatedTurns, 1), tokens, strings.Join(classes, ", "))
	if s.AverageContext != nil {
		row += " · average context " + formatTokenCount(*s.AverageContext) + " tokens"
	} else {
		row += " · average context unavailable"
	}
	if s.CachedShare != nil {
		row += " · cached-input share " + formatPercent(*s.CachedShare)
	}
	if len(s.PricedSubtotal) > 0 {
		currencies := make([]string, 0, len(s.PricedSubtotal))
		for currency := range s.PricedSubtotal {
			currencies = append(currencies, currency)
		}
		sort.Strings(currencies)
		for i, currency := range currencies {
			amount := "unavailable"
			if n, ok := new(big.Rat).SetString(s.PricedSubtotal[currency]); ok {
				amount = n.FloatString(2)
			}
			currencies[i] = currency + " " + amount
		}
		row += " · estimated subtotal " + strings.Join(currencies, ", ")
		if !s.CostComplete {
			row += " (partial)"
		}
	}
	return row
}

// formatTokenCount prints an exact count ("4207361/31", "6710000") in units:
// plain below 1,000, then 12.1k, 6.71M, 1.23B.
func formatTokenCount(exact string) string {
	n, ok := new(big.Rat).SetString(exact)
	if !ok {
		return "unavailable"
	}
	v, _ := n.Float64()
	units := []struct {
		size   float64
		suffix string
		digits int
	}{{1e3, "k", 1}, {1e6, "M", 2}, {1e9, "B", 2}}
	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		// Rounding may reach the next unit: 999,960 is 1.00M, not 1000.0k.
		if math.Abs(v) < u.size {
			continue
		}
		text := strconv.FormatFloat(v/u.size, 'f', u.digits, 64)
		if i < len(units)-1 && strings.HasPrefix(strings.TrimPrefix(text, "-"), "1000") {
			next := units[i+1]
			text, u = strconv.FormatFloat(v/next.size, 'f', next.digits, 64), next
		}
		return text + u.suffix
	}
	return formatRatDecimal(exact, 1)
}

// formatRatDecimal prints an exact quantity as a decimal with at most digits
// fraction digits and no trailing zeros: "11/2" is 5.5.
func formatRatDecimal(exact string, digits int) string {
	n, ok := new(big.Rat).SetString(exact)
	if !ok {
		return "unavailable"
	}
	text := n.FloatString(digits)
	if strings.Contains(text, ".") {
		text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	}
	return text
}

// formatPercent prints an exact share ("39/40") as a percentage with one decimal.
func formatPercent(exact string) string {
	n, ok := new(big.Rat).SetString(exact)
	if !ok {
		return "unavailable"
	}
	return n.Mul(n, big.NewRat(100, 1)).FloatString(1) + "%"
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

// cmdUsageWarning reads or sets the multiple of an item's saved estimate past
// which the hub warns the item's lead and the owner helper.
func cmdUsageWarning(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New("usage: tt usage warning get [--project ID] [--json] | set --threshold N [--project ID] [--json]")
	}
	fs := flag.NewFlagSet("usage warning", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	threshold := fs.String("threshold", "", "multiple of the estimate, 1 to 100 with at most three decimals")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: tt usage warning get [--project ID] [--json] | set --threshold N [--project ID] [--json]")
	}
	if args[0] == "get" && *threshold != "" {
		return errors.New("get takes no threshold; use tt usage warning set --threshold N")
	}
	if args[0] == "set" && *threshold == "" {
		return errors.New("set requires --threshold")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	var out api.UsageWarnings
	if args[0] == "get" {
		out, err = c.UsageWarnings(ctx, *project)
	} else {
		// An agent names itself; the hub accepts only the owner helper.
		out, err = c.SetUsageWarning(ctx, *project, api.UsageWarningRequest{Threshold: *threshold, AgentID: e.agent})
	}
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Print(formatUsageWarnings(out))
	return nil
}

// formatUsageWarnings is the text form of the warning level and the warnings
// posted, one line each. A warning with a queue entry compared that entry's
// team figure and shows the item's lifetime figure beside it; one without an
// entry was recorded when the lifetime figure itself was compared.
func formatUsageWarnings(out api.UsageWarnings) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Warning level: %s× estimate", out.Threshold)
	if out.Default {
		b.WriteString(" (default)")
	}
	b.WriteString("\n")
	if len(out.Warnings) == 0 {
		b.WriteString("No warnings posted.\n")
	}
	for _, w := range out.Warnings {
		bound := ""
		if w.ActualState == "partial" {
			bound = "at least "
		}
		estimate, compared := formatTokenCount(strconv.FormatInt(w.EstimateTokens, 10)), "lifetime actual"
		if w.EntryID != "" {
			compared = "team actual"
			fmt.Fprintf(&b, "  %s: entry %s · estimate %s", w.ItemID, w.EntryID, estimate)
		} else {
			fmt.Fprintf(&b, "  %s: estimate %s", w.ItemID, estimate)
		}
		fmt.Fprintf(&b, " · %s %s%s", compared, bound, formatTokenCount(w.ActualTokens))
		if ratio, ok := new(big.Rat).SetString(w.Ratio); ok {
			fmt.Fprintf(&b, " · %s%s×", bound, ratio.FloatString(2))
		}
		if w.EntryID != "" {
			b.WriteString(" · lifetime ")
			if w.LifetimeState == "partial" {
				b.WriteString("at least ")
			}
			b.WriteString(formatTokenCount(w.LifetimeTokens))
		}
		fmt.Fprintf(&b, " · level %s×", w.Threshold)
		if w.LeadMessageSeq > 0 {
			fmt.Fprintf(&b, " · lead #%d", w.LeadMessageSeq)
		} else {
			b.WriteString(" · no lead")
		}
		if w.HelperMessageSeq > 0 {
			fmt.Fprintf(&b, " · owner helper #%d", w.HelperMessageSeq)
		} else {
			b.WriteString(" · no owner helper")
		}
		fmt.Fprintf(&b, " · %s\n", w.At.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// formatTokenBudget renders an item's estimate against its lifetime actual:
// "estimate 12.00M · lifetime actual 15.30M · 1.28×". A partial actual is a
// lower bound, so it and its ratio say "at least". While the item has a
// running queue entry whose team has measured tokens on it, the figure the
// warning compares follows: " · this team 3.10M · 0.26×". A nil budget (an
// older hub) prints nothing.
func formatTokenBudget(b *api.TokenBudget) string {
	if b == nil {
		return ""
	}
	parts := []string{"no estimate"}
	if b.Estimate != nil {
		parts[0] = "estimate " + formatTokenCount(strconv.FormatInt(b.Estimate.Tokens, 10))
	}
	bound := ""
	switch b.ActualState {
	case "measured":
		parts = append(parts, "lifetime actual "+formatTokenCount(b.ActualTokens))
	case "partial":
		bound = "at least "
		parts = append(parts, "lifetime actual at least "+formatTokenCount(b.ActualTokens)+" (partial)")
	default:
		parts = append(parts, "lifetime actual not measured")
	}
	if ratio := formatBudgetRatio(b); ratio != "" {
		parts = append(parts, bound+ratio)
	}
	if t := b.Team; t != nil && (t.ActualState == "measured" || t.ActualState == "partial") {
		bound = ""
		if t.ActualState == "partial" {
			bound = "at least "
		}
		parts = append(parts, "this team "+bound+formatTokenCount(t.ActualTokens))
		if ratio, ok := new(big.Rat).SetString(t.Ratio); ok {
			parts = append(parts, bound+ratio.FloatString(2)+"×")
		}
	}
	return strings.Join(parts, " · ")
}

// formatBudgetRatio prints actual/estimate as "1.28×", or nothing when the
// budget has no ratio.
func formatBudgetRatio(b *api.TokenBudget) string {
	if b == nil || b.Ratio == "" {
		return ""
	}
	n, ok := new(big.Rat).SetString(b.Ratio)
	if !ok {
		return ""
	}
	return n.FloatString(2) + "×"
}

// usageCalibrationRow is one done item for estimating from history.
type usageCalibrationRow struct {
	ItemID string `json:"itemId"`
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	// Lane is the template of the item's latest team queue entry, small or
	// planned, or "none" when the item was never queued.
	Lane       string `json:"lane"`
	OwnedPaths int    `json:"ownedPaths"`
	// Template is that entry's team shape, plan-review or plan-only.
	Template       string `json:"template,omitempty"`
	ActualTokens   string `json:"actualTokens"`
	ActualState    string `json:"actualState"`
	EstimateTokens int64  `json:"estimateTokens,omitempty"`
	EstimateBasis  string `json:"estimateBasis,omitempty"`
	Ratio          string `json:"ratio,omitempty"`
}

type usageCalibrationReport struct {
	ProjectID string                `json:"projectId"`
	Items     []usageCalibrationRow `json:"items"`
}

// launchTeamShape mirrors the hub's team shape for an entry listed in full,
// which carries its launch instead of the summary's teamShape.
func launchTeamShape(q api.TeamQueueEntry) string {
	if q.TeamShape != "" || len(q.LaunchJSON) == 0 {
		return q.TeamShape
	}
	var launch struct {
		Members []struct {
			Fields struct {
				Role string `json:"role"`
			} `json:"fields"`
		} `json:"members"`
	}
	if json.Unmarshal(q.LaunchJSON, &launch) != nil || len(launch.Members) == 0 {
		return ""
	}
	for _, m := range launch.Members {
		if m.Fields.Role == "Plan review" {
			return "plan-review"
		}
	}
	return "plan-only"
}

// collectUsageCalibration reads every done item and every team queue entry of
// the project to their last pages, so no item is cut off at a page limit.
func collectUsageCalibration(ctx context.Context, c *api.Client, project string) (usageCalibrationReport, error) {
	out := usageCalibrationReport{ProjectID: project, Items: []usageCalibrationRow{}}
	latest := map[string]api.TeamQueueEntry{}
	keep := func(entries []api.TeamQueueEntry) {
		for _, q := range entries {
			if prior, ok := latest[q.ItemID]; !ok || q.Position > prior.Position {
				latest[q.ItemID] = q
			}
		}
	}
	after := int64(0)
	for {
		page, err := c.ListTeamQueuePage(ctx, project, api.TeamQueueListOptions{Limit: api.MaxLimit, After: after})
		if err != nil {
			return out, err
		}
		// Every page repeats the active entries; history is cut to the page.
		keep(page.Entries)
		if page.History == nil || page.History.NextAfter == 0 {
			break
		}
		after = page.History.NextAfter
	}
	seq := int64(0)
	for {
		list, err := c.ListWorkItems(ctx, project, "", "done", seq, api.MaxLimit)
		if err != nil {
			return out, err
		}
		if len(list.Items) == 0 {
			break
		}
		for _, item := range list.Items {
			row := usageCalibrationRow{ItemID: item.ID, Seq: item.Seq, Kind: item.Kind, Title: item.Title, Lane: "none", ActualTokens: "0", ActualState: "not measured"}
			if q, ok := latest[item.ID]; ok {
				row.Lane, row.OwnedPaths, row.Template = q.Template, len(q.Ownership), launchTeamShape(q)
				if row.Lane == "" {
					row.Lane = "planned" // an entry saved before templates were recorded
				}
			}
			if b := item.Budget; b != nil {
				row.ActualTokens, row.ActualState, row.Ratio = b.ActualTokens, b.ActualState, b.Ratio
				if b.Estimate != nil {
					row.EstimateTokens, row.EstimateBasis = b.Estimate.Tokens, b.Estimate.Basis
				}
			}
			out.Items = append(out.Items, row)
			seq = item.Seq
		}
	}
	return out, nil
}

func usageCalibration(e env, project string, asJSON bool) error {
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(2 * time.Minute)
	defer cancel()
	out, err := collectUsageCalibration(ctx, c, project)
	if err != nil {
		return err
	}
	if asJSON {
		printJSON(out)
		return nil
	}
	for _, row := range out.Items {
		fmt.Println(formatCalibrationRow(row))
	}
	return nil
}

// formatCalibrationRow prints one done item on one line.
func formatCalibrationRow(row usageCalibrationRow) string {
	budget := &api.TokenBudget{ActualTokens: row.ActualTokens, ActualState: row.ActualState, Ratio: row.Ratio}
	if row.EstimateTokens > 0 {
		budget.Estimate = &api.WorkItemEstimate{Tokens: row.EstimateTokens, Basis: row.EstimateBasis}
	}
	template := row.Template
	if template == "" {
		template = "none"
	}
	return fmt.Sprintf("#%d %s %s lane=%s paths=%d template=%s · %s · %s", row.Seq, row.ItemID, row.Kind, row.Lane, row.OwnedPaths, template, formatTokenBudget(budget), row.Title)
}

// usageBudgetHost names this host as the queue runner and the relay do, so
// tt usage budget shows the reading admission here would use.
var usageBudgetHost = spawn.Host

const usageBudgetUsage = "usage: tt usage budget get | set --runtime codex|claude --window five_hour|seven_day --allowance TOKENS [--reserve PCT] [--reset-at RFC3339] [--stale 15m] | clear --runtime R --window W, each with [--host H] [--project ID] [--json]"

// cmdUsageBudget reads, sets or clears the project's token budget rows. With
// no row the queue ignores budgets; with any row a queued team is admitted
// only when its estimate fits what remains after the reserve.
func cmdUsageBudget(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set" && args[0] != "clear") {
		return errors.New(usageBudgetUsage)
	}
	fs := flag.NewFlagSet("usage budget", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	runtime := fs.String("runtime", "", "codex or claude")
	window := fs.String("window", "", "five_hour or seven_day")
	allowance := fs.Int64("allowance", 0, "tokens that 100 percent of the window represents")
	reserve := fs.Int("reserve", 0, "percent of the allowance kept back from admission, 0 to 90")
	resetAt := fs.String("reset-at", "", "RFC3339 reset instant, used when no provider reading gives one")
	stale := fs.Duration("stale", 0, "how old a provider reading may be and still decide (default 15m)")
	host := fs.String("host", "", "the host whose provider reading is shown (default: this host)")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || !api.ValidID(*project, "tsk") {
		return errors.New(usageBudgetUsage)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch args[0] {
	case "get":
		if set["runtime"] || set["window"] || set["allowance"] || set["reserve"] || set["reset-at"] || set["stale"] {
			return errors.New("get takes no budget values; use tt usage budget set")
		}
	case "set":
		if *runtime == "" || *window == "" || *allowance < 1 {
			return errors.New("set requires --runtime, --window and --allowance: the tokens that 100 percent of the window represents")
		}
		if *stale < 0 || *stale%time.Second != 0 {
			return errors.New("--stale is a whole number of seconds, such as 15m")
		}
	case "clear":
		if *runtime == "" || *window == "" || set["allowance"] || set["reserve"] || set["reset-at"] || set["stale"] {
			return errors.New("clear requires --runtime and --window and takes no other budget value")
		}
	}
	if *host == "" {
		*host = usageBudgetHost()
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	var out api.UsageBudgets
	// An agent names itself; the hub accepts the owner helper or a database handler.
	req := api.UsageBudgetRequest{Runtime: *runtime, Window: *window, AllowanceTokens: *allowance, ReservePercent: *reserve, ResetAt: *resetAt, StaleSeconds: int(*stale / time.Second), AgentID: e.agent}
	switch args[0] {
	case "set":
		if _, err = c.SetUsageBudget(ctx, *project, req); err != nil {
			return err
		}
	case "clear":
		if _, err = c.DeleteUsageBudget(ctx, *project, api.UsageBudgetRequest{Runtime: *runtime, Window: *window, AgentID: e.agent}); err != nil {
			return err
		}
	}
	// Every form prints the rows with the source this host's admission would use.
	if out, err = c.UsageBudgets(ctx, *project, *host); err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Print(formatUsageBudgets(out))
	return nil
}

// formatUsageBudgets is the text form of the budget rows: "not configured",
// or one line per row with the source in use, the remaining tokens, the reset
// time and, when the provider reading is not the source, why.
func formatUsageBudgets(out api.UsageBudgets) string {
	if !out.Configured || len(out.Budgets) == 0 {
		return "Token budget: not configured\n"
	}
	var b strings.Builder
	for _, row := range out.Budgets {
		fmt.Fprintf(&b, "%s %s: allowance %s · reserve %d%% · reading stale after %s", row.Runtime, row.Window, formatTokenCount(strconv.FormatInt(row.AllowanceTokens, 10)), row.ReservePercent, time.Duration(row.StaleSeconds)*time.Second)
		if row.ResetAt != "" {
			b.WriteString(" · owner reset " + row.ResetAt)
		}
		if status := row.Status; status != nil {
			host := status.Host
			if host == "" {
				host = "this host"
			}
			switch status.Source {
			case api.UsageBudgetSourceProvider:
				fmt.Fprintf(&b, " · source provider reading for host %s · remaining %s · resets %s", host, formatTokenCount(status.RemainingTokens), status.ResetAt)
			case api.UsageBudgetSourceAllowance:
				fmt.Fprintf(&b, " · source allowance minus reported usage · remaining %s · resets %s · provider reading for host %s %s", formatTokenCount(status.RemainingTokens), status.ResetAt, host, status.Reading)
			default:
				fmt.Fprintf(&b, " · no usable source: provider reading for host %s %s and no reset time is set; admission is held", host, status.Reading)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

const usageDefaultsUsage = "usage: tt usage defaults get | set --small N --small-race N --planned N --planned-race N, each with [--project ID] [--json]"

// cmdUsageDefaults reads or sets the lane default estimates: what admission
// compares with the budget for an item that has no saved estimate. They are
// data the backlog steward recomputes; they never warn and never pause a team.
func cmdUsageDefaults(e env, args []string) error {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return errors.New(usageDefaultsUsage)
	}
	fs := flag.NewFlagSet("usage defaults", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	small := fs.Int64("small", 0, "tokens for a Small item without the Go race check")
	smallRace := fs.Int64("small-race", 0, "tokens for a Small item with the Go race check")
	planned := fs.Int64("planned", 0, "tokens for a Planned item without the Go race check")
	plannedRace := fs.Int64("planned-race", 0, "tokens for a Planned item with the Go race check")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || !api.ValidID(*project, "tsk") {
		return errors.New(usageDefaultsUsage)
	}
	values := *small != 0 || *smallRace != 0 || *planned != 0 || *plannedRace != 0
	if args[0] == "get" && values {
		return errors.New("get takes no values; use tt usage defaults set")
	}
	if args[0] == "set" && (*small < 1 || *smallRace < 1 || *planned < 1 || *plannedRace < 1) {
		return errors.New("set requires all four: --small, --small-race, --planned and --planned-race")
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	var out api.UsageEstimateDefaults
	if args[0] == "get" {
		out, err = c.UsageEstimateDefaults(ctx, *project)
	} else {
		out, err = c.SetUsageEstimateDefaults(ctx, *project, api.UsageEstimateDefaultsRequest{SmallTokens: *small, SmallRaceTokens: *smallRace, PlannedTokens: *planned, PlannedRaceTokens: *plannedRace, AgentID: e.agent})
	}
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	fmt.Print(formatUsageEstimateDefaults(out))
	return nil
}

func formatUsageEstimateDefaults(out api.UsageEstimateDefaults) string {
	if !out.Configured {
		return "Lane default estimates: not set (tt usage defaults set)\n"
	}
	count := func(n int64) string { return formatTokenCount(strconv.FormatInt(n, 10)) }
	return fmt.Sprintf("Lane default estimates: small %s · small with Go race %s · planned %s · planned with Go race %s · set %s\n",
		count(out.SmallTokens), count(out.SmallRaceTokens), count(out.PlannedTokens), count(out.PlannedRaceTokens), out.UpdatedAt)
}

// formatEstimateDefault is the queue list's label for an entry admitted on a
// lane default: " · default estimate 70.00M (planned, Go race)".
func formatEstimateDefault(d *api.TeamQueueEstimateDefault) string {
	if d == nil {
		return ""
	}
	lane := d.Lane
	if d.GoRace {
		lane += ", Go race"
	}
	return " · default estimate " + formatTokenCount(strconv.FormatInt(d.Tokens, 10)) + " (" + lane + ")"
}

// formatBudgetHold is the queue list's line for an entry whose team is held
// past the hold multiple of its saved estimate, with the two ways out.
func formatBudgetHold(task string, h *api.BudgetHold) string {
	if h == nil {
		return ""
	}
	bound := ""
	if h.TeamState == "partial" {
		bound = "at least "
	}
	return fmt.Sprintf("held: past %d times its token estimate since %s (team %s%s, estimate %s); no new turns start. Continue: tt usage hold continue --project %s --entry %s. Stop, from an unbound owner shell: tt team queue fail --task %s --entry %s --reason TEXT",
		api.BudgetHoldMultiple, h.CreatedAt, bound, formatTokenCount(h.TeamTokens), formatTokenCount(strconv.FormatInt(h.EstimateTokens, 10)), task, h.EntryID, task, h.EntryID)
}

const usageHoldUsage = "usage: tt usage hold list | continue --entry tqe_ID, each with [--project ID] [--json]"

// cmdUsageHold lists the project's token budget holds, or continues one: the
// owner's or the owner helper's answer that lifts it. Stopping a held team is
// tt team queue fail.
func cmdUsageHold(e env, args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "continue") {
		return errors.New(usageHoldUsage)
	}
	fs := flag.NewFlagSet("usage hold", flag.ContinueOnError)
	project := fs.String("project", e.task, "project ID")
	entry := fs.String("entry", "", "continue: the held queue entry")
	asJSON := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 || !api.ValidID(*project, "tsk") || (args[0] == "list") != (*entry == "") {
		return errors.New(usageHoldUsage)
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(15 * time.Second)
	defer cancel()
	if args[0] == "continue" {
		q, err := c.GetTeamQueueEntry(ctx, *project, *entry)
		if err != nil {
			return err
		}
		// An agent names itself; the hub accepts only the owner helper. The
		// request id names the hold, so a retry returns the first result.
		id := fmt.Sprintf("usage-hold-continue-%s-%d", q.ID, q.Revision)
		if _, err = c.TeamQueueAction(ctx, *project, api.TeamQueueRequest{RequestID: id, Operation: "budget_continue", EntryID: q.ID, ExpectedRevision: q.Revision, AgentID: e.agent}); err != nil {
			return err
		}
		if !*asJSON {
			fmt.Printf("Continued entry %s; its team is woken again.\n", q.ID)
		}
	}
	out, err := c.UsageHolds(ctx, *project, "", "")
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(out)
		return nil
	}
	if len(out.Holds) == 0 {
		fmt.Println("No token budget holds.")
		return nil
	}
	for _, h := range out.Holds {
		if h.State == api.BudgetHoldHeld {
			fmt.Printf("%s %s %s\n", h.EntryID, h.ItemID, formatBudgetHold(*project, &h))
			continue
		}
		fmt.Printf("%s %s stopped at %s; its %d runs stay unwoken until the failed entry is released\n", h.EntryID, h.ItemID, h.ResolvedAt, len(h.Runs))
	}
	return nil
}
