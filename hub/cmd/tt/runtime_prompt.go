package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Runtime prompt detection (docs/runtime-prompts.md). A runtime prompt is a
// modal menu or dialog the agent's own runtime draws in its pane, such as
// Codex's rate-limit model menu or a Claude permission dialog. While one is
// up the runtime is not at its input, so wakes queue behind it unseen.
//
// Classification reads only the prompt area at the bottom of a plain pane
// capture, never the transcript above it, and needs a prompt's exact title
// and option labels for a known kind. Anything else that looks like a prompt
// is unknown and escalates. Screen text never leaves the host: reports carry
// the kind, its fixed label and a hash of the prompt area.

type runtimePromptOption struct {
	Label    string
	Selected bool
	Row      int // row in the classified lines
}

type runtimePromptMatch struct {
	Kind        string
	Fingerprint string
	Options     []runtimePromptOption
}

// selected returns the index of the option under the selection marker.
func (m runtimePromptMatch) selected() int {
	for i, o := range m.Options {
		if o.Selected {
			return i
		}
	}
	return -1
}

func (m runtimePromptMatch) option(label string) int {
	for i, o := range m.Options {
		if o.Label == label {
			return i
		}
	}
	return -1
}

var (
	runtimePromptOptionRow = regexp.MustCompile(`^\s*([›❯>]\s*)?(\d{1,2})\.\s+(\S.*?)\s*$`)
	// Codex draws a key hint as the last row of every menu: "enter continue ·
	// esc quit", "enter/esc confirm · ctrl+c quit", "Press enter to continue".
	codexPromptFooter = regexp.MustCompile(`^(press enter\b|enter(/esc)?\s+[a-z]|esc\s+[a-z])`)
	// Two or more spaces separate an option label from its description column.
	runtimePromptColumn = regexp.MustCompile(`\s{2,}`)
)

// claudePromptPhrases are generic prompt signatures in Claude's prompt area.
var claudePromptPhrases = append([]string{"enter to confirm", "enter to select", "press enter to"}, claudeDialogPhrases...)

// plainRuntimeScreen removes terminal escapes from a `capture-pane -p -e`
// capture and normalizes no-break spaces, leaving one string per row.
func plainRuntimeScreen(raw string) []string {
	var out strings.Builder
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == '[':
			j := i + 2
			for j < len(raw) && (raw[j] < 0x40 || raw[j] > 0x7e) {
				j++
			}
			i = j + 1
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == ']':
			j := i + 2
			for j < len(raw) && raw[j] != 0x07 && !(raw[j] == 0x1b && j+1 < len(raw) && raw[j+1] == '\\') {
				j++
			}
			if j < len(raw) && raw[j] == 0x1b {
				j++
			}
			i = j + 1
		case c == 0x1b:
			i += 2
		case c == '\n' || c == '\t' || c >= 0x20:
			out.WriteByte(c)
			i++
		default:
			i++
		}
	}
	text := strings.ReplaceAll(out.String(), " ", " ")
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// classifyRuntimePrompt returns the prompt on a plain capture, if any.
func classifyRuntimePrompt(runtime string, lines []string) (runtimePromptMatch, bool) {
	if runtime == "claude" {
		return classifyClaudePrompt(lines)
	}
	return classifyCodexPrompt(lines)
}

func runtimePromptOptions(lines []string, from int) []runtimePromptOption {
	var out []runtimePromptOption
	for i := from; i < len(lines); i++ {
		m := runtimePromptOptionRow.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		label := strings.TrimSpace(runtimePromptColumn.Split(m[3], 2)[0])
		out = append(out, runtimePromptOption{Label: label, Selected: m[1] != "", Row: i})
	}
	return out
}

// runtimePromptPrint hashes the prompt rows with selection markers removed,
// so moving the selection does not change the prompt's identity.
func runtimePromptPrint(runtime, kind string, rows []string) string {
	h := sha256.New()
	h.Write([]byte(runtime + "\x00" + kind + "\x00"))
	for _, row := range rows {
		if m := runtimePromptOptionRow.FindStringSubmatch(row); m != nil && m[1] != "" {
			row = strings.Replace(row, strings.TrimSpace(m[1]), " ", 1)
		}
		if fields := strings.Fields(row); len(fields) > 0 {
			h.Write([]byte(strings.Join(fields, " ") + "\n"))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func blankRow(line string) bool { return strings.TrimSpace(line) == "" }

// codexPromptArea returns the rows of a Codex menu: a key-hint footer as the
// last row on screen, the options above it and the title block above those.
// The block ends at a run of two blank rows or at eight rows above the first
// option, so transcript text above a menu is not part of it. An idle composer
// ends with its status line, not a key hint, and has no area.
func codexPromptArea(lines []string) ([]string, int) {
	last := len(lines) - 1
	for last >= 0 && blankRow(lines[last]) {
		last--
	}
	if last < 0 || !codexPromptFooter.MatchString(strings.ToLower(strings.TrimSpace(lines[last]))) {
		return nil, -1
	}
	first := last
	for i := last - 1; i >= 0 && i >= last-24; i-- {
		if m := runtimePromptOptionRow.FindStringSubmatch(lines[i]); m != nil && m[2] == "1" {
			first = i
			break
		}
	}
	top, blanks, seen := first, 0, false
	for i := first - 1; i >= 0 && i >= first-8; i-- {
		if blankRow(lines[i]) {
			blanks++
			if seen && blanks >= 2 {
				break
			}
			continue
		}
		blanks, seen, top = 0, true, i
	}
	return lines[top : last+1], first - top
}

func classifyCodexPrompt(lines []string) (runtimePromptMatch, bool) {
	area, optionsAt := codexPromptArea(lines)
	if area == nil {
		return runtimePromptMatch{}, false
	}
	options := runtimePromptOptions(area, optionsAt)
	header := strings.Join(area[:optionsAt], "\n")
	labels := map[string]bool{}
	switchOption := false
	for _, o := range options {
		labels[o.Label] = true
		switchOption = switchOption || strings.HasPrefix(o.Label, "Switch to ")
	}
	kind := api.RuntimePromptUnknown
	switch {
	case strings.Contains(header, "Approaching rate limits") && labels["Keep current model"] && labels["Keep current model (never show again)"] && switchOption:
		kind = api.RuntimePromptCodexRateLimit
	case labels["Try new model"] && labels["Use existing model"]:
		kind = api.RuntimePromptCodexModelMigration
	case strings.Contains(header, "Usage limit reached") || strings.Contains(header, "You've reached your workspace credit limit"):
		kind = api.RuntimePromptCodexUsageLimit
	case strings.Contains(header, "Trust this folder?") && labels["Trust and continue"] && labels["Quit"],
		strings.Contains(header, "Do you trust the contents of this directory?") && labels["Yes, continue"] && labels["No, quit"]:
		kind = api.RuntimePromptCodexTrust
	}
	return runtimePromptMatch{Kind: kind, Fingerprint: runtimePromptPrint("codex", kind, area), Options: options}, true
}

// classifyClaudePrompt reads Claude's active prompt area. An idle input box
// (claudePromptArea reports it boxed) is never a dialog: its rows are the
// agent's draft or Claude's suggestion, whatever they say. With no input box,
// as while a dialog is up, the dialog starts at the rule Claude draws above
// it: the nearest rule above its first numbered option, or the last rule on
// screen. The transcript above that rule is never read.
func classifyClaudePrompt(lines []string) (runtimePromptMatch, bool) {
	area, boxed := claudePromptArea(lines)
	if boxed {
		return runtimePromptMatch{}, false
	}
	first := -1
	for i := len(area) - 1; i >= 0 && i >= len(area)-30; i-- {
		if m := runtimePromptOptionRow.FindStringSubmatch(area[i]); m != nil && m[2] == "1" {
			first = i
			break
		}
	}
	if first < 0 {
		first = len(area) - 1
	}
	top := max(0, len(area)-12)
	for i := first; i >= 0 && i >= first-20; i-- {
		if claudeRuleLine(area[i]) {
			top = i
			break
		}
	}
	area = area[top:]
	var rows []string
	for _, row := range area {
		if !blankRow(row) {
			rows = append(rows, row)
		}
	}
	options := runtimePromptOptions(rows, 0)
	marker := false
	for _, o := range options {
		marker = marker || o.Selected
	}
	lower := strings.ToLower(strings.Join(rows, "\n"))
	phrase := false
	for _, p := range claudePromptPhrases {
		phrase = phrase || strings.Contains(lower, p)
	}
	if !marker && !phrase {
		return runtimePromptMatch{}, false
	}
	labels := map[string]bool{}
	for _, o := range options {
		labels[o.Label] = true
	}
	proceed, trust := false, false
	for _, row := range rows {
		trimmed := strings.TrimSpace(row)
		proceed = proceed || strings.HasPrefix(strings.ToLower(trimmed), "do you want to ")
		// Claude's trust choices are not numbered.
		trust = trust || strings.TrimSpace(strings.TrimPrefix(trimmed, "❯")) == "Yes, I trust this folder"
	}
	kind := api.RuntimePromptUnknown
	switch {
	case trust && strings.Contains(lower, "enter to confirm"):
		kind = api.RuntimePromptClaudeTrust
	case proceed && labels["Yes"] && strings.Contains(lower, "esc to cancel"):
		kind = api.RuntimePromptClaudePermission
	case strings.Contains(lower, "enter to select") && len(options) > 0:
		kind = api.RuntimePromptClaudeSelection
	}
	return runtimePromptMatch{Kind: kind, Fingerprint: runtimePromptPrint("claude", kind, rows), Options: options}, true
}

// runtimePromptQuiet is how long a runtime's transcript must be quiet before
// the relay captures its pane. TAILTERM_ACTIVITY_PROMPT_SECONDS overrides it.
func runtimePromptQuiet() time.Duration {
	v, err := strconv.Atoi(os.Getenv("TAILTERM_ACTIVITY_PROMPT_SECONDS"))
	if err != nil || v < 1 || v > 86400 {
		v = 30
	}
	return time.Duration(v) * time.Second
}

const (
	runtimePromptAnswerLimit  = 3
	runtimePromptAnswerWindow = 5 * time.Minute
	runtimePromptConfirmWait  = 5 * time.Second
	runtimePromptSeenCap      = 64
)

// runtimePromptTargets names the option each answering action selects.
var runtimePromptTargets = map[string]map[string]string{
	api.RuntimePromptCodexRateLimit: {
		api.RuntimePromptKeepCurrentNeverShow: "Keep current model (never show again)",
		api.RuntimePromptKeepCurrent:          "Keep current model",
	},
	api.RuntimePromptCodexModelMigration: {api.RuntimePromptUseExisting: "Use existing model"},
}

// runtimePromptDeps are the host operations detection and answers use. The
// relay loop installs the native ones in the activity context; without them
// detection is off, which keeps other activity callers unchanged.
type runtimePromptDeps struct {
	inspect func(context.Context, runtimeBinding) (runtimePane, error)
	send    func(ctx context.Context, pane string, keys ...string) error
	sleep   func(time.Duration)
	now     func() time.Time
}

type runtimePromptDepsKey struct{}

func nativeRuntimePromptDeps() *runtimePromptDeps {
	return &runtimePromptDeps{
		inspect: func(ctx context.Context, b runtimeBinding) (runtimePane, error) {
			name := "Codex"
			if b.Runtime == "claude" {
				name = "Claude"
			}
			return inspectRuntimePane(ctx, b, name, false)
		},
		send: func(ctx context.Context, pane string, keys ...string) error {
			_, err := startupTmux(ctx, append([]string{"send-keys", "-t", pane}, keys...)...)
			return err
		},
		sleep: time.Sleep,
		now:   time.Now,
	}
}

// runtimePromptSeen is the decision for one prompt fingerprint in one run. An
// open Claude dialog keeps the fingerprint it was first seen with.
// Attempted is saved before the first key, so a relay that stops mid-answer
// never types into that prompt again.
type runtimePromptSeen struct {
	Kind      string    `json:"kind"`
	Since     time.Time `json:"since"`
	Action    string    `json:"action,omitempty"`
	Outcome   string    `json:"outcome,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	At        time.Time `json:"at,omitempty"`
	Attempted bool      `json:"attempted,omitempty"`
}

// runtimePromptIntent is the answer in progress, written before any key.
type runtimePromptIntent struct {
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	Action      string    `json:"action"`
	Target      string    `json:"target"`
	Keys        []string  `json:"keys"`
	Pane        string    `json:"pane"`
	Phase       string    `json:"phase"`
	At          time.Time `json:"at"`
}

// runtimePromptLocal is private host state for one binding run. It holds no
// screen text: only kinds, hashes, option labels from the fixed target table
// and outcomes.
type runtimePromptLocal struct {
	Run     string                       `json:"run"`
	Thread  string                       `json:"thread"`
	Prompt  *api.RuntimePrompt           `json:"prompt,omitempty"`
	Seen    map[string]runtimePromptSeen `json:"seen,omitempty"`
	Answers []time.Time                  `json:"answers,omitempty"`
	Intent  *runtimePromptIntent         `json:"intent,omitempty"`
	Logged  string                       `json:"logged,omitempty"`
}

func runtimePromptPath(b runtimeBinding) string {
	return filepath.Join(relayDir(), bindingKey(b)+"-"+b.Run+".runtime-prompt.json")
}

func loadRuntimePromptLocal(b runtimeBinding) runtimePromptLocal {
	var l runtimePromptLocal
	if data, err := os.ReadFile(runtimePromptPath(b)); err == nil {
		_ = json.Unmarshal(data, &l)
	}
	if l.Run != b.Run || l.Thread != b.Thread {
		l = runtimePromptLocal{Run: b.Run, Thread: b.Thread}
	}
	if l.Seen == nil {
		l.Seen = map[string]runtimePromptSeen{}
	}
	return l
}

// runtimePromptPolicies caches each project's policy for a minute per relay
// process, inside the host's request budget. An older hub's missing policy is
// cached the same way; a failed read is not, so the next tick reads again.
var runtimePromptPolicies struct {
	sync.Mutex
	entries map[string]runtimePromptPolicyEntry
}

type runtimePromptPolicyEntry struct {
	actions     map[string]string
	unsupported bool
	at          time.Time
}

func resetRuntimePromptPolicies() {
	runtimePromptPolicies.Lock()
	runtimePromptPolicies.entries = nil
	runtimePromptPolicies.Unlock()
}

var errRuntimePromptUnsupported = errors.New("hub has no runtime prompt policy")

func runtimePromptActions(ctx context.Context, client *api.Client, b runtimeBinding, now time.Time) (map[string]string, error) {
	key := b.Hub + "\x00" + b.Task
	runtimePromptPolicies.Lock()
	entry, ok := runtimePromptPolicies.entries[key]
	runtimePromptPolicies.Unlock()
	if ok && now.Sub(entry.at) < time.Minute && now.Sub(entry.at) >= 0 {
		if entry.unsupported {
			return nil, errRuntimePromptUnsupported
		}
		return entry.actions, nil
	}
	p, err := client.RuntimePromptPolicy(ctx, b.Task)
	entry = runtimePromptPolicyEntry{actions: p.Actions, at: now}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusMethodNotAllowed) {
		// An older hub cannot store runtime_prompt either; stay silent.
		entry = runtimePromptPolicyEntry{unsupported: true, at: now}
	} else if err != nil {
		return nil, err
	}
	runtimePromptPolicies.Lock()
	if runtimePromptPolicies.entries == nil {
		runtimePromptPolicies.entries = map[string]runtimePromptPolicyEntry{}
	}
	runtimePromptPolicies.entries[key] = entry
	runtimePromptPolicies.Unlock()
	if entry.unsupported {
		return nil, errRuntimePromptUnsupported
	}
	return entry.actions, nil
}

func samePane(a, b runtimePane) bool {
	return a.Pane == b.Pane && a.SessionID == b.SessionID && a.Created == b.Created && a.PanePID == b.PanePID
}

// logRuntimePrompt writes one relay line when the prompt, its action or its
// outcome changes, and one when it clears.
func logRuntimePrompt(l *runtimePromptLocal, agent string, now time.Time) {
	key := ""
	line := ""
	if p := l.Prompt; p != nil {
		key = p.Kind + "/" + p.Fingerprint + "/" + p.Action + "/" + p.Outcome
		line = fmt.Sprintf("runtime prompt %s: %s %s", p.Kind, p.Action, p.Outcome)
		if p.Reason != "" {
			line += " (" + p.Reason + ")"
		}
	} else if l.Logged != "" {
		line = "runtime prompt cleared"
	}
	if key == l.Logged {
		return
	}
	l.Logged = key
	fmt.Fprintf(os.Stderr, "[tt relay] %s %s %s\n", now.UTC().Format(time.RFC3339), agent, line)
}

// observeRuntimePrompt runs inside the activity tick for a live runtime. It
// captures the pane only after the transcript has been quiet, classifies the
// prompt area, applies the project's policy once per prompt, and returns the
// prompt to report, or nil when the runtime is not on one. The prompt clears
// when the pane shows none, when the transcript has an event inside the quiet
// window, or when the binding has no session.
func observeRuntimePrompt(ctx context.Context, deps *runtimePromptDeps, b runtimeBinding, client *api.Client, c *activityCursor, now time.Time) (*api.RuntimePrompt, error) {
	l := loadRuntimePromptLocal(b)
	save := func() error { return writePrivateJSON(runtimePromptPath(b), l) }
	clear := func() (*api.RuntimePrompt, error) {
		if l.Prompt == nil && l.Logged == "" {
			return nil, nil
		}
		l.Prompt = nil
		logRuntimePrompt(&l, b.Agent, now)
		return nil, save()
	}
	if b.Session == "" || (c.SeenTurn && now.Sub(c.LastEventAt) < runtimePromptQuiet()) {
		return clear()
	}
	runtime := b.Runtime
	if runtime == "" {
		runtime = "codex"
	}
	first, err := deps.inspect(ctx, b)
	if err != nil {
		// An unreadable pane is not evidence of a prompt; keep the last report.
		if l.Prompt != nil {
			return l.Prompt, nil
		}
		return nil, nil
	}
	match, ok := classifyRuntimePrompt(runtime, plainRuntimeScreen(first.Raw))
	if !ok {
		return clear()
	}
	// A Claude dialog redraws rows while it waits, so its hash changes from
	// one capture to the next. While a prompt of the same kind stays open it
	// keeps its first identity; clear() ends that, and the next dialog is
	// hashed afresh. A transcript event after the prompt was first seen means
	// the agent moved on, so a dialog found then is a new one even when no
	// tick cleared the old (relay stopped, host asleep, skipped ticks). Codex
	// menus are static and keep their content hash, which the answer checks
	// rely on.
	if p := l.Prompt; runtime == "claude" && p != nil && p.Runtime == "claude" && p.Kind == match.Kind && !c.LastEventAt.After(p.Since) {
		match.Fingerprint = p.Fingerprint
	}
	actions, err := runtimePromptActions(ctx, client, b, now)
	if errors.Is(err, errRuntimePromptUnsupported) {
		return nil, nil
	}
	kind, _ := api.LookupRuntimePromptKind(match.Kind)
	action := api.RuntimePromptEscalate
	if err == nil && api.RuntimePromptActionAllowed(match.Kind, actions[match.Kind]) {
		action = actions[match.Kind]
	}
	// A failed policy read decides nothing for a kind the policy can change.
	// A kind whose only action is escalate needs no policy.
	undecided := err != nil && len(kind.Actions) > 1
	seen, known := l.Seen[match.Fingerprint]
	// A confirmed answer saw this prompt gone, so the same fingerprint has
	// come back: it is decided afresh, inside the answer limit.
	if returned := known && seen.Outcome == api.RuntimePromptConfirmed; !known || returned {
		seen = runtimePromptSeen{Kind: match.Kind, Since: now}
		if !returned && len(l.Seen) >= runtimePromptSeenCap {
			oldest := ""
			for fp, s := range l.Seen {
				if oldest == "" || s.Since.Before(l.Seen[oldest].Since) {
					oldest = fp
				}
			}
			delete(l.Seen, oldest)
		}
	}
	switch {
	case seen.Attempted && seen.Outcome == "":
		// A relay stopped between intent and outcome. Never type again.
		seen.Outcome, seen.Reason, seen.At = api.RuntimePromptAmbiguous, "relay stopped during an answer", now
	case seen.Outcome != "" && seen.Outcome != api.RuntimePromptSkipped:
		// Decided once per prompt and run.
	case undecided:
		// Skipped is decided again on the next tick, when the read may work.
		const reason = "policy unavailable; retrying"
		if seen.Reason != reason {
			fmt.Fprintf(os.Stderr, "[tt relay] %s runtime prompt policy unavailable; retrying: %v\n", b.Agent, err)
		}
		seen.Action, seen.Outcome, seen.Reason, seen.At = api.RuntimePromptEscalate, api.RuntimePromptSkipped, reason, now
	case api.RuntimePromptAnswers(action) && runtime == "codex" && runtimePromptTargets[match.Kind][action] != "":
		seen.Action = action
		l.Seen[match.Fingerprint] = seen
		// An answer outlives the tick's short budget: its confirmation alone
		// may take five seconds.
		answerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		seen.Outcome, seen.Reason = answerRuntimePrompt(answerCtx, deps, b, client, &l, first, match, action, save)
		cancel()
		seen.At = deps.now().UTC()
		if seen.Outcome != api.RuntimePromptSkipped {
			seen.Attempted = true
		}
	case action == api.RuntimePromptReport:
		seen.Action, seen.Outcome, seen.Reason, seen.At = action, api.RuntimePromptReported, "", now
	default:
		seen.Action, seen.Outcome, seen.Reason, seen.At = api.RuntimePromptEscalate, api.RuntimePromptEscalated, "", now
	}
	if seen.Action == "" {
		seen.Action = action
	}
	l.Seen[match.Fingerprint] = seen
	l.Prompt = &api.RuntimePrompt{Kind: match.Kind, Runtime: runtime, Label: kind.Label, Fingerprint: match.Fingerprint, Since: seen.Since.UTC(), Action: seen.Action, Outcome: seen.Outcome, Reason: seen.Reason, At: seen.At.UTC()}
	logRuntimePrompt(&l, b.Agent, now)
	if err := save(); err != nil {
		return nil, err
	}
	return l.Prompt, nil
}

// answerRuntimePrompt selects the policy's option with the same safety rules
// as a Claude wake: an eligible agent in an active project, two identical
// inspections, a durable intent before any key, the selection verified on
// the exact target label before a separate Enter, confirmation that the
// prompt is gone, and no resend. It returns the outcome and a bounded reason.
func answerRuntimePrompt(ctx context.Context, deps *runtimePromptDeps, b runtimeBinding, client *api.Client, l *runtimePromptLocal, first runtimePane, match runtimePromptMatch, action string, save func() error) (string, string) {
	now := deps.now()
	recent := l.Answers[:0]
	for _, at := range l.Answers {
		if now.Sub(at) < runtimePromptAnswerWindow {
			recent = append(recent, at)
		}
	}
	l.Answers = recent
	if len(l.Answers) >= runtimePromptAnswerLimit {
		return api.RuntimePromptSkipped, fmt.Sprintf("answer limit of %d per %s reached", runtimePromptAnswerLimit, runtimePromptAnswerWindow)
	}
	if active, err := relayProjectActive(ctx, client, b); err != nil || !active {
		return api.RuntimePromptSkipped, "project not active"
	}
	a, err := client.GetAgent(ctx, b.Task, b.Agent)
	if err != nil {
		return api.RuntimePromptSkipped, "agent unavailable"
	}
	if reason := relayAgentSkipReason(a, b); reason != "" {
		return api.RuntimePromptSkipped, reason
	}
	target := runtimePromptTargets[match.Kind][action]
	second, err := deps.inspect(ctx, b)
	if err != nil || !samePane(first, second) {
		return api.RuntimePromptSkipped, "pane changed before the answer"
	}
	again, ok := classifyRuntimePrompt("codex", plainRuntimeScreen(second.Raw))
	if !ok || again.Fingerprint != match.Fingerprint || again.Kind != match.Kind {
		return api.RuntimePromptSkipped, "prompt changed before the answer"
	}
	from, to := again.selected(), again.option(target)
	if from < 0 || to < 0 {
		return api.RuntimePromptFailed, "target option not found"
	}
	key, steps := "Down", to-from
	if steps < 0 {
		key, steps = "Up", -steps
	}
	keys := make([]string, steps)
	for i := range keys {
		keys[i] = key
	}
	l.Intent = &runtimePromptIntent{Fingerprint: match.Fingerprint, Kind: match.Kind, Action: action, Target: target, Keys: append(keys, "Enter"), Pane: second.Pane, Phase: "uncertain", At: now.UTC()}
	seen := l.Seen[match.Fingerprint]
	seen.Attempted = true
	l.Seen[match.Fingerprint] = seen
	l.Answers = append(l.Answers, now.UTC())
	if err := save(); err != nil {
		return api.RuntimePromptFailed, "answer intent could not be saved"
	}
	finish := func(outcome, reason string) (string, string) {
		l.Intent.Phase = outcome
		_ = save()
		return outcome, reason
	}
	if len(keys) > 0 {
		if err := deps.send(ctx, second.Pane, keys...); err != nil {
			return finish(api.RuntimePromptAmbiguous, "navigation keys failed")
		}
		deps.sleep(150 * time.Millisecond)
	}
	third, err := deps.inspect(ctx, b)
	if err != nil || !samePane(first, third) {
		return finish(api.RuntimePromptFailed, "pane changed before Enter")
	}
	moved, ok := classifyRuntimePrompt("codex", plainRuntimeScreen(third.Raw))
	if !ok || moved.Fingerprint != match.Fingerprint {
		return finish(api.RuntimePromptFailed, "prompt changed before Enter")
	}
	if at := moved.selected(); at < 0 || moved.Options[at].Label != target {
		return finish(api.RuntimePromptFailed, "selection is not on the policy option")
	}
	l.Intent.Phase = "entered"
	if err := save(); err != nil {
		return api.RuntimePromptFailed, "answer intent could not be saved"
	}
	if err := deps.send(ctx, third.Pane, "Enter"); err != nil {
		return finish(api.RuntimePromptAmbiguous, "Enter failed")
	}
	deadline := deps.now().Add(runtimePromptConfirmWait)
	for {
		deps.sleep(100 * time.Millisecond)
		if pane, err := deps.inspect(ctx, b); err == nil && samePane(first, pane) {
			if after, ok := classifyRuntimePrompt("codex", plainRuntimeScreen(pane.Raw)); !ok || after.Fingerprint != match.Fingerprint {
				return finish(api.RuntimePromptConfirmed, "selected the policy option")
			}
		}
		if !deps.now().Before(deadline) || ctx.Err() != nil {
			return finish(api.RuntimePromptAmbiguous, "prompt still shown after Enter")
		}
	}
}

// runtimePromptKey is the prompt part of the activity tick's change key. It
// is empty without a prompt, so existing cursors keep their keys. Since is
// part of it: a prompt that returns with the same outcome is a new report.
func runtimePromptKey(p *api.RuntimePrompt) string {
	if p == nil {
		return ""
	}
	return "|prompt:" + p.Kind + "/" + p.Fingerprint + "/" + p.Action + "/" + p.Outcome + "/" + p.Reason + "/" + p.Since.UTC().Format(time.RFC3339Nano)
}

// runtimePromptStatus is the prompt part of a tt relay --status line.
func runtimePromptStatus(b runtimeBinding) string {
	p := loadRuntimePromptLocal(b).Prompt
	if p == nil {
		return ""
	}
	return fmt.Sprintf(" prompt=%s action=%s outcome=%s prompt-at=%s", p.Kind, p.Action, p.Outcome, p.At.Format(time.RFC3339))
}
