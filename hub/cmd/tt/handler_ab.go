package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler arms (docs/handler-ab.md): tt handler arms get|set and
// tt handler ab-report.

// armFlags collects repeated --arm ID=MODEL/RUNTIME/REASONING:WEIGHT values.
type armFlags []string

func (a *armFlags) String() string     { return strings.Join(*a, ",") }
func (a *armFlags) Set(v string) error { *a = append(*a, v); return nil }

// parseArmFlag reads ID=MODEL/RUNTIME/REASONING:WEIGHT. The model may itself
// contain slashes, so runtime and reasoning are the last two segments.
func parseArmFlag(v string) (api.HandlerArm, error) {
	bad := fmt.Errorf("--arm %q must be ID=MODEL/RUNTIME/REASONING:WEIGHT, for example S=claude-sonnet-5-5/claude/high:1", v)
	id, rest, ok := strings.Cut(v, "=")
	if !ok || id == "" {
		return api.HandlerArm{}, bad
	}
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return api.HandlerArm{}, bad
	}
	weight, err := strconv.Atoi(rest[colon+1:])
	if err != nil {
		return api.HandlerArm{}, bad
	}
	parts := strings.Split(rest[:colon], "/")
	if len(parts) < 3 {
		return api.HandlerArm{}, bad
	}
	n := len(parts)
	return api.HandlerArm{ID: id, Model: strings.Join(parts[:n-2], "/"), Runtime: parts[n-2], Reasoning: parts[n-1], Weight: weight}, nil
}

func formatArm(a api.HandlerArm) string {
	return fmt.Sprintf("%s=%s/%s/%s:%d", a.ID, a.Model, a.Runtime, a.Reasoning, a.Weight)
}

// armTemplateDigest is the handler template digest the policy expects: the
// prompt file's exact contents, or the saved handler spec's --prompt, so
// rotation successors started from that spec match.
func armTemplateDigest(e env, task, promptFile string) (string, error) {
	if promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return "", err
		}
		return handlerTemplateDigest(string(data)), nil
	}
	spec, err := loadHandlerSpec(e.hub, task)
	if err != nil {
		return "", err
	}
	if spec == nil {
		return "", errors.New("no handler launch spec is saved on this host for that project; save the Planned database-role spec with tt handler spec, or pass --prompt-file")
	}
	return handlerTemplateDigest(spec.value("prompt")), nil
}

func cmdHandlerArms(e env, args []string) error {
	usage := errors.New("usage: tt handler arms get|set --task ID [--revision N --arm ID=MODEL/RUNTIME/REASONING:WEIGHT ... --seed TEXT --fallback=BOOL --enabled=BOOL --hold-minutes N --prompt-file PATH --request-id KEY] [--json]")
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return usage
	}
	operation := args[0]
	fs := flag.NewFlagSet("handler arms", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	revision := fs.Int64("revision", -1, "expected policy revision (required for set)")
	var arms armFlags
	fs.Var(&arms, "arm", "arm as ID=MODEL/RUNTIME/REASONING:WEIGHT (repeatable; replaces every arm)")
	seed := fs.String("seed", "", "draw seed")
	fallback := fs.Bool("fallback", false, "lease another arm when the drawn arm has no free handler")
	enabled := fs.Bool("enabled", false, "lease by arm")
	hold := fs.Int("hold-minutes", 0, "Claude limit hold in minutes, 5 to 1440")
	promptFile := fs.String("prompt-file", "", "handler assignment prompt file (default: the saved handler spec's --prompt)")
	requestID := fs.String("request-id", "", "stable retry key (default: derived from the change)")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("a valid --task project ID is required")
	}
	if operation == "set" {
		if err := requireOwnerSession(e, "tt handler arms set"); err != nil {
			return err
		}
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	view, err := c.HandlerArmPolicy(ctx, *task)
	if err != nil {
		return err
	}
	if operation == "set" {
		if *revision < 0 {
			return errors.New("tt handler arms set needs --revision, the revision shown by tt handler arms get")
		}
		current := view.Policy
		req := api.HandlerArmPolicyRequest{ExpectedRevision: *revision, Enabled: current.Enabled, Seed: current.Seed, Fallback: current.Fallback,
			LimitHoldMinutes: current.LimitHoldMinutes, Arms: current.Arms}
		if len(arms) > 0 {
			req.Arms = nil
			for _, v := range arms {
				arm, err := parseArmFlag(v)
				if err != nil {
					return err
				}
				req.Arms = append(req.Arms, arm)
			}
		}
		if flagPresent(args, "seed") {
			req.Seed = *seed
		}
		if flagPresent(args, "fallback") {
			req.Fallback = *fallback
		}
		if flagPresent(args, "enabled") {
			req.Enabled = *enabled
		}
		if flagPresent(args, "hold-minutes") {
			req.LimitHoldMinutes = *hold
		}
		// Only enabling needs the reference template; a disabled save keeps
		// the saved digest unless a prompt file is given.
		req.TemplateDigest = current.TemplateDigest
		if req.Enabled || *promptFile != "" {
			if req.TemplateDigest, err = armTemplateDigest(e, *task, *promptFile); err != nil {
				return err
			}
		}
		req.RequestID = *requestID
		if req.RequestID == "" {
			b, _ := json.Marshal(req)
			sum := sha256.Sum256(append([]byte(*task+"\x00"), b...))
			req.RequestID = "handler-arms-" + hex.EncodeToString(sum[:12])
		}
		if _, err = c.SetHandlerArmPolicy(ctx, *task, req); err != nil {
			return err
		}
		if view, err = c.HandlerArmPolicy(ctx, *task); err != nil {
			return err
		}
	}
	if *jsonOut {
		printJSON(view)
		return nil
	}
	printArmPolicy(view)
	return nil
}

func printArmPolicy(view api.HandlerArmPolicyView) {
	p := view.Policy
	state := "disabled"
	if p.Enabled {
		state = "enabled"
	}
	fmt.Printf("Handler arms: %s, revision %d\n", state, p.Revision)
	if p.Revision == 0 {
		fmt.Println("No policy saved; queue entries lease the first free handler.")
	}
	fmt.Printf("Seed: %q  Fallback: %v  Limit hold: %d min\n", p.Seed, p.Fallback, p.LimitHoldMinutes)
	fmt.Printf("Template digest: %s\n", or(p.TemplateDigest, "-"))
	for _, a := range p.Arms {
		fmt.Printf("  arm %s\n", formatArm(a))
	}
	fmt.Println("Handlers:")
	if len(view.Handlers) == 0 {
		fmt.Println("  none open")
	}
	for _, h := range view.Handlers {
		arm := "arm " + h.Arm
		if h.Arm == "" {
			arm = "no arm: " + h.NoArmReason
		}
		digest := or(h.TemplateDigest, "missing")
		if len(digest) > 12 {
			digest = digest[:12]
		}
		fmt.Printf("  %s %s/%s %s %s/%s/%s digest=%s digestMatches=%v %s\n", h.Name, h.AgentID, h.RunID, h.Status, or(h.Model, "-"), h.Runtime, or(h.Reasoning, "-"), digest, h.DigestMatches, arm)
	}
	for _, l := range view.Limits {
		fmt.Printf("Limited: arm %s since %s (%s, %s)\n", l.Arm, l.StartedAt.UTC().Format(time.RFC3339), l.Source, l.AgentID)
	}
}

func cmdHandlerABReport(e env, args []string) error {
	fs := flag.NewFlagSet("handler ab-report", flag.ContinueOnError)
	task := fs.String("task", e.task, "project ID")
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !api.ValidID(*task, "tsk") || fs.NArg() != 0 {
		return errors.New("usage: tt handler ab-report --task ID [--json]")
	}
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := c.HandlerABReport(ctx, *task)
	if err != nil {
		return err
	}
	if *jsonOut {
		printJSON(report)
		return nil
	}
	printABReport(report)
	return nil
}

func abMillis(v *int64) string {
	if v == nil {
		return "-"
	}
	return (time.Duration(*v) * time.Millisecond).Round(time.Second).String()
}

func abInt(v *int64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatInt(*v, 10)
}

func printABReport(r api.HandlerABReport) {
	fmt.Printf("Handler A/B report for %s (policy revision %d, %d finished items)\n", r.TaskID, r.Policy.Revision, len(r.Items))
	for _, a := range r.Arms {
		fmt.Printf("Arm %s %s/%s/%s: n=%d fallbacks=%d requests=%d response median=%s p90=%s launch-to-done median=%s (%d done) tokens median=%s mean input/request=%s rotations=%d digest-flagged=%d\n",
			a.Arm, or(a.Model, "-"), or(a.Runtime, "-"), or(a.Reasoning, "-"), a.N, a.Fallbacks, a.Requests, abMillis(a.ResponseMedianMillis), abMillis(a.ResponseP90Millis),
			abMillis(a.LaunchToDoneMedian), a.LaunchToDoneDone, abInt(a.HandlerTokensMedian), or(derefString(a.MeanInputPerRequest), "-"), a.Rotations, a.DigestFlaggedItems)
		keys := make([]string, 0, len(a.Counts))
		for k := range a.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d (%s/item)", k, a.Counts[k].Total, a.Counts[k].Rate))
		}
		fmt.Printf("  %s\n", strings.Join(parts, " "))
	}
	for _, c := range r.Comparisons {
		fmt.Printf("Compare %s vs %s %s: %s\n", c.A, c.B, c.Metric, c.Flag)
	}
	for _, it := range r.Items {
		fallback := ""
		if it.Fallback {
			fallback = " (fallback from " + it.DrawnArm + ")"
		}
		flags := ""
		if len(it.DigestFlags) > 0 {
			flags = " digest differs: " + strings.Join(it.DigestFlags, ",")
		}
		fmt.Printf("Item %s arm %s%s: tokens=%d requests=%d launch-to-done=%s blocks=%d refused=%d incorrect=%d links=%d limits=%d%s\n",
			it.ItemID, it.Arm, fallback, it.HandlerTokens, len(it.ResponseMillis), abMillis(it.LaunchToDoneMillis), it.HandlerBlocks, it.RefusedSaves, it.IncorrectSaves, it.LinkCorrections, it.LimitEvents.Total, flags)
	}
	fmt.Println("Flags are a heuristic at small n, not a significance test.")
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// checkRotationArm refuses a rotation before any spawn when the project has a
// saved arm policy, the old run belongs to one of its arms, and the saved
// spec's model or reasoning differs from the old run's: rotation stays within
// the arm. Without a policy, or on a hub without handler arms, it passes.
func checkRotationArm(ctx context.Context, c *api.Client, task string, old api.Agent, spec handlerSpec) error {
	view, err := c.HandlerArmPolicy(ctx, task)
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == 404 {
		return nil
	}
	if err != nil {
		return err
	}
	if view.Policy.Revision == 0 {
		return nil
	}
	for _, h := range view.Handlers {
		if h.AgentID != old.ID || h.RunID != old.RunID || h.Arm == "" {
			continue
		}
		if spec.value("model") != h.Model || spec.value("reasoning") != h.Reasoning {
			return fmt.Errorf("handler rotation refused (%s): the saved launch spec (model %q, reasoning %q) differs from handler %s (model %q, reasoning %q); rotation stays within the handler arm, so save matching settings with tt handler spec",
				api.HandlerRotationRefusedArmChanged, spec.value("model"), spec.value("reasoning"), old.Name, h.Model, h.Reasoning)
		}
	}
	return nil
}
