package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Owner delegation windows (docs/owner-delegation-windows.md).

const delegationUsage = "usage: tt owner delegation open --delegate NAME (--for 3h | --until RFC3339) [--scope decisions|decisions-merges-deploys] [--reason T] [--request-id K] [--json]\n" +
	"       tt owner delegation close WINDOW_ID [--reason T] [--request-id K] [--json]\n" +
	"       tt owner delegation list [--json]"

// delegationNow is the clock --for is measured from; tests move it.
var delegationNow = time.Now

// derivedKey is a stable per-minute retry key, like the other owner commands.
func derivedKey(prefix string, parts ...string) string {
	scope := delegationNow().UTC().Truncate(time.Minute).Format(time.RFC3339)
	digest := sha256.Sum256([]byte(strings.Join(append(parts, scope), "\x00")))
	return fmt.Sprintf("%s-%x", prefix, digest[:12])
}

func cmdOwnerDelegation(e env, args []string) error {
	// Checked before any request: an agent session never opens, closes or lists windows.
	if e.agent != "" {
		return errors.New("tt owner delegation is for the owner, not agent sessions")
	}
	if len(args) == 0 {
		return errors.New(delegationUsage)
	}
	sub, rest := args[0], args[1:]
	window := ""
	if sub == "close" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(delegationUsage)
		}
		window, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("owner delegation "+sub, flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	delegate := fs.String("delegate", "", "open: the delegate agent's name or id")
	forFlag := fs.Duration("for", 0, "open: window length, such as 3h (1m to 7 days)")
	until := fs.String("until", "", "open: end time as RFC3339")
	scope := fs.String("scope", api.DelegationScopeDecisions, "open: decisions or decisions-merges-deploys")
	reason := fs.String("reason", "", "why (optional)")
	requestID := fs.String("request-id", "", "stable retry key (default: derived per minute)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *task == "" || fs.NArg() != 0 {
		return errors.New(delegationUsage)
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	switch sub {
	case "list":
		list, err := c.ListDelegationWindows(ctx, *task)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(list)
			return nil
		}
		printDelegationWindows(list.Windows)
		return nil
	case "open":
		if (*forFlag == 0) == (*until == "") {
			return errors.New("give exactly one of --for or --until")
		}
		ends := delegationNow().Add(*forFlag)
		if *until != "" {
			if ends, err = time.Parse(time.RFC3339, *until); err != nil {
				return fmt.Errorf("--until must be RFC3339, such as 2026-09-30T18:00:00-07:00: %w", err)
			}
		}
		normalized := api.NormalizeDelegationScope(*scope)
		if !api.ValidDelegationScope(normalized) {
			return errors.New("--scope must be decisions or decisions-merges-deploys")
		}
		explicit := *requestID != ""
		if !explicit {
			*requestID = derivedKey("owner-delegation-open", *delegate, forFlag.String(), *until, normalized, *reason)
		}
		req := api.OpenDelegationWindowRequest{Delegate: *delegate, EndsAt: ends.UTC().Truncate(time.Second), Scope: normalized, Reason: *reason,
			Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: *requestID}
		out, err := c.OpenDelegationWindow(ctx, *task, req)
		if *forFlag != 0 && requestIDReused(err) {
			out, err = replayDelegationOpen(ctx, c, *task, req, *forFlag, explicit, err)
		} else if err == nil && out.Replay && out.Window != nil {
			err = replayedWindowOpen(ctx, c, *task, out.Window.ID)
		}
		return printDelegationResult(out, err, *asJSON)
	case "close":
		if *requestID == "" {
			*requestID = derivedKey("owner-delegation-close", window, *reason)
		}
		out, err := c.CloseDelegationWindow(ctx, *task, window, api.CloseDelegationWindowRequest{Reason: *reason, Source: &api.DelegationSource{Kind: api.DelegationSourceTT}, RequestID: *requestID})
		return printDelegationResult(out, err, *asJSON)
	}
	return errors.New(delegationUsage)
}

// delegationReplaySlack is how far a window's stored length may differ from
// --for and still be the window an earlier call with the same derived key
// opened. The derived key already holds --for, so the slack only absorbs the
// clocks: the hub stamps the start up to the 10 second request timeout after
// the CLI read its clock, the end time loses up to a second to truncation,
// and the rest allows for the two hosts' clocks differing.
const delegationReplaySlack = 15 * time.Second

// An explicit --request-id does not hold --for, so its window may be at most
// explicitReplayShorter shorter (a second of end-time truncation plus request
// latency) or explicitReplayLonger longer (clock jitter) than --for. A smaller
// difference in --for cannot be told apart without the hub storing it.
const (
	explicitReplayShorter = 3 * time.Second
	explicitReplayLonger  = time.Second
)

// requestIDReused reports the hub's refusal of a request ID that is already
// stored with a different payload.
func requestIDReused(err error) bool {
	var he *api.HTTPError
	return errors.As(err, &he) && he.Status == http.StatusConflict && strings.Contains(he.Msg, "request ID was already used")
}

// replayDelegationOpen retries a refused --for open. A repeated call measures
// --for from a later moment, so its end time differs from the one stored
// under the request ID. Windows carry no request ID, so this picks the newest
// window of the same delegate, scope, reason and length and, only while that
// window is open, resends the request once with its end time. The request ID
// is already taken, so the hub can only replay the stored result or refuse
// again: nothing new is opened.
func replayDelegationOpen(ctx context.Context, c *api.Client, task string, req api.OpenDelegationWindowRequest, length time.Duration, explicit bool, refused error) (api.OwnerActionResult, error) {
	list, err := c.ListDelegationWindows(ctx, task)
	if err != nil {
		return api.OwnerActionResult{}, refused
	}
	shorter, longer := delegationReplaySlack, delegationReplaySlack
	if explicit {
		shorter, longer = explicitReplayShorter, explicitReplayLonger
	}
	windows := list.Windows
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].CreatedAt.After(windows[j].CreatedAt) })
	var newest *api.DelegationWindow
	for i, w := range windows {
		if (w.DelegateName != req.Delegate && w.DelegateAgentID != req.Delegate) || w.Scope != req.Scope || w.Reason != req.Reason || w.EndsAt.Equal(req.EndsAt) {
			continue
		}
		if newest == nil {
			newest = &windows[i]
		}
		if off := w.EndsAt.Sub(w.CreatedAt) - length; off < -shorter || off > longer {
			continue
		}
		if err := windowStillOpen(w, windows); err != nil {
			return api.OwnerActionResult{}, err
		}
		req.EndsAt = w.EndsAt
		out, err := c.OpenDelegationWindow(ctx, task, req)
		if requestIDReused(err) {
			// The request ID holds another window. Only one window is open per
			// project and w is open, so the request's own window has ended.
			return api.OwnerActionResult{}, fmt.Errorf("this request's window has ended (closed or expired), so the repeated open was not replayed; the open delegation window is %s to %s until %s",
				w.ID, w.DelegateName, w.EndsAt.Local().Format("Jan 2 15:04"))
		}
		if err == nil && out.Window != nil && out.Window.ID != w.ID {
			// Another window with the same end time was stored under the request ID.
			err = listedWindowOpen(out.Window.ID, windows)
		}
		return out, err
	}
	if newest != nil {
		return api.OwnerActionResult{}, fmt.Errorf("%w; window %s to %s lasts %s, not --for %s, so it was not replayed",
			refused, newest.ID, newest.DelegateName, newest.EndsAt.Sub(newest.CreatedAt).Round(time.Second), length)
	}
	return api.OwnerActionResult{}, refused
}

// replayedWindowOpen checks the current state of a window the hub replayed:
// the replay is the open-time snapshot, which still says open.
func replayedWindowOpen(ctx context.Context, c *api.Client, task, id string) error {
	list, err := c.ListDelegationWindows(ctx, task)
	if err != nil {
		return fmt.Errorf("the hub replayed window %s but its current state could not be read: %w", id, err)
	}
	return listedWindowOpen(id, list.Windows)
}

func listedWindowOpen(id string, windows []api.DelegationWindow) error {
	for _, w := range windows {
		if w.ID == id {
			return windowStillOpen(w, windows)
		}
	}
	return fmt.Errorf("the hub replayed window %s but it is not listed", id)
}

// windowStillOpen refuses a closed or expired window, or one past its end
// that the broker has not expired yet, and names the project's open window.
func windowStillOpen(w api.DelegationWindow, windows []api.DelegationWindow) error {
	now := delegationNow()
	if w.Active(now) {
		return nil
	}
	open := "no delegation window is open"
	for _, o := range windows {
		if o.Active(now) {
			open = fmt.Sprintf("the open delegation window is %s to %s", o.ID, o.DelegateName)
		}
	}
	state, at := w.State, w.EndsAt
	if w.EndedAt != nil {
		at = *w.EndedAt
	}
	if state == api.DelegationOpen {
		state = api.DelegationExpired
	}
	return fmt.Errorf("window %s to %s %s at %s, so the repeated open was not replayed; %s",
		w.ID, w.DelegateName, state, at.Local().Format("Jan 2 15:04"), open)
}

func printDelegationResult(out api.OwnerActionResult, err error, asJSON bool) error {
	if err != nil {
		return err
	}
	if asJSON {
		printJSON(out)
		return nil
	}
	if out.Window != nil {
		printDelegationWindows([]api.DelegationWindow{*out.Window})
	}
	return nil
}

func printDelegationWindows(windows []api.DelegationWindow) {
	if len(windows) == 0 {
		fmt.Println("(no delegation windows)")
		return
	}
	for _, w := range windows {
		when := "until " + w.EndsAt.Local().Format("Jan 2 15:04")
		if w.EndedAt != nil {
			when = "ended " + w.EndedAt.Local().Format("Jan 2 15:04")
		}
		answered, returned := 0, 0
		for _, r := range w.Routes {
			if r.AnswerSeq > 0 {
				answered++
			}
			if r.ReturnedAt != nil {
				returned++
			}
		}
		fmt.Printf("%s  %s  → %s  %s  %s  routed %d, answered %d, returned %d\n", w.ID, w.State, w.DelegateName, strings.ReplaceAll(w.Scope, "_", "-"), when, len(w.Routes), answered, returned)
		for _, r := range w.Routes {
			if r.AnswerSeq == 0 {
				continue
			}
			fmt.Printf("  #%d %s — delegated answer #%d by %s at %s\n    answer: %s\n    rationale: %s\n", r.RequestSeq, r.Subject, r.AnswerSeq, r.AnsweredByName,
				r.AnsweredAt.Local().Format("Jan 2 15:04"), oneLine(r.Answer), oneLine(r.Rationale))
		}
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// cmdAskAnswer answers a Board decision: a window delegate with a required
// rationale, or the owner without one.
func cmdAskAnswer(e env, args []string) error {
	usage := errors.New("usage: tt ask answer SEQ (--option ID | --text T) [--rationale R] [--request-id K] [--json]; a delegate must give --rationale")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usage
	}
	seq, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
	if err != nil || seq < 1 {
		return usage
	}
	fs := flag.NewFlagSet("ask answer", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	option := fs.String("option", "", "chosen option id")
	text := fs.String("text", "", "custom answer, or an explanation with --option")
	rationale := fs.String("rationale", "", "why (required for a delegate)")
	requestID := fs.String("request-id", "", "stable retry key (default: derived per minute)")
	asJSON := fs.Bool("json", false, "print the stored answer as JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *task == "" || fs.NArg() != 0 || (*option == "" && strings.TrimSpace(*text) == "") {
		return usage
	}
	if e.agent != "" && strings.TrimSpace(*rationale) == "" {
		return errors.New("a delegated answer needs --rationale")
	}
	if e.agent == "" && *rationale != "" {
		return errors.New("--rationale is for a delegate's answer")
	}
	if *requestID == "" {
		*requestID = derivedKey("ask-answer", fmt.Sprint(seq), e.agent, *option, *text, *rationale)
	}
	c, err := e.client(10 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(10 * time.Second)
	defer cancel()
	m, err := c.AnswerDecision(ctx, *task, seq, api.AnswerDecisionRequest{RequestID: *requestID, OptionID: *option, Text: *text, AgentID: e.agent, RunID: e.runID, Rationale: *rationale})
	if err != nil {
		return err
	}
	if *asJSON {
		printJSON(m)
	} else {
		fmt.Printf("Decision #%d answered in #%d.\n", seq, m.Seq)
	}
	return nil
}
