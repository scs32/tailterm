package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// A stalled turn is a Claude turn that is open, has no pending tool call and
// has stopped producing output. On 2026-10-01 a verifier sat in one turn for
// 17.5 minutes with its token counter frozen; its nudge was held as "turn in
// progress" and a person had to press Escape and wake it by hand (bug
// wi_03ce50892a559767, order #27407, docs/claude-wake.md "Stalled turns").
//
// Claude Code writes a transcript record only when a block of a response is
// complete, so a healthy long thinking block is transcript-quiet too. A stall
// therefore needs two signals: no transcript record and no worktree change for
// the threshold, and the same token counter in two captures of the pane's
// status line at least claudeStallProbeGap apart. A counter that cannot be
// read still makes a stall, but one the relay only reports.
//
// Every stall is recorded, logged, shown as the stuck activity state and
// reported once. Only a host whose setting says "interrupt" presses Escape,
// once per stall and never twice in a run, and then lets the normal wake path
// deliver the unread input, or types one fixed resume line when there is none.

const (
	// claudeStallProbeGap is the least time between the two pane captures.
	claudeStallProbeGap = 60 * time.Second
	// claudeStallCursorAge is how old the activity cursor may be. An older
	// one means the relay has not read the transcript lately, so nothing is
	// known about the turn.
	claudeStallCursorAge = 2 * time.Minute
	// claudeStallEndWait is how long the pass that sent the interrupt waits
	// for the transcript to show the turn ended; later passes keep looking
	// until claudeStallEndLimit.
	claudeStallEndWait  = 5 * time.Second
	claudeStallEndLimit = 2 * time.Minute
	// claudeStallResumeGrace gives the normal wake path the first turn after
	// an interrupt, so unread input or a broker nudge is delivered instead of
	// the resume line.
	claudeStallResumeGrace = 30 * time.Second
	// claudeStallResumeLimit bounds how long an unconfirmed resume line is
	// followed through the wake path's own retries.
	claudeStallResumeLimit = 10 * time.Minute
	// claudeStallNoticeRetry spaces attempts to post a notice after a failed one.
	claudeStallNoticeRetry = time.Minute

	claudeStallReport    = "report"
	claudeStallInterrupt = "interrupt"

	// claudeStallResumeLine is typed, through the wake path, into an agent the
	// relay interrupted when nothing else is waiting for it.
	claudeStallResumeLine = "Your previous turn stopped producing output and the relay interrupted it. Continue the work you were doing."

	claudeStallSubject = "A Claude agent's turn has stopped producing output"
)

// claudeStall is one run's stall record: private host state beside the wake
// intent. It holds the reading being watched, the current stall and how many
// stalls the run has had.
type claudeStall struct {
	Run string `json:"run"`
	// Count is the number of stalls detected in this run.
	Count int `json:"count,omitempty"`
	// Since is when the turn last wrote a transcript record or changed the
	// worktree. It identifies the stall.
	Since time.Time `json:"since,omitempty"`
	// Probe is the first pane reading of a quiet turn.
	Probe *claudeStallReading `json:"probe,omitempty"`
	// CheckedAt is the last time the hub was asked about this agent for the
	// stall rule. It spaces those reads by claudeStallProbeGap, so a turn that
	// stays quiet (a stalled agent, or one whose status is not running) costs
	// one read a minute, not one per relay pass.
	CheckedAt time.Time `json:"checkedAt,omitempty"`

	DetectedAt time.Time `json:"detectedAt,omitempty"`
	// Mode is the host setting read at detection. It is fixed for this stall.
	Mode string `json:"mode,omitempty"`
	// Counter is the token counter both captures showed. Unreadable says why
	// there is none; such a stall is never interrupted.
	Counter    string `json:"counter,omitempty"`
	Unreadable string `json:"unreadable,omitempty"`
	// Unread and WakeSeqs are the input waiting for the agent at detection.
	Unread   int     `json:"unread,omitempty"`
	WakeSeqs []int64 `json:"wakeSeqs,omitempty"`
	// NotInterrupted says why a host set to interrupt did not.
	NotInterrupted string `json:"notInterrupted,omitempty"`

	InterruptedAt time.Time `json:"interruptedAt,omitempty"`
	// EndedAt is when the transcript showed the interrupted turn had ended.
	EndedAt time.Time `json:"endedAt,omitempty"`
	// RewakeAt is when the normal wake path confirmed a wake after the
	// interrupt. ResumeAt is when the resume line was handed to the wake
	// path; ResumeConfirmedAt is when the transcript confirmed it.
	RewakeAt          time.Time `json:"rewakeAt,omitempty"`
	ResumeAt          time.Time `json:"resumeAt,omitempty"`
	ResumeConfirmedAt time.Time `json:"resumeConfirmedAt,omitempty"`
	// Outcome is the latest action or refusal, in one bounded line.
	Outcome string `json:"outcome,omitempty"`

	// The notice is written once and sent unchanged, so a retry or a restarted
	// relay replays the same request. EscalatedAt is set with a repeat's notice.
	Notice        string    `json:"notice,omitempty"`
	NoticedAt     time.Time `json:"noticedAt,omitempty"`
	NoticeTriedAt time.Time `json:"noticeTriedAt,omitempty"`
	EscalatedAt   time.Time `json:"escalatedAt,omitempty"`
	// ClearedAt ends the stall: the turn produced output again, ended, or was
	// woken after the interrupt.
	ClearedAt time.Time `json:"clearedAt,omitempty"`
}

// claudeStallReading is one capture of the pane's status line.
type claudeStallReading struct {
	At   time.Time `json:"at"`
	Pane string    `json:"pane,omitempty"`
	// Counter is the token counter as drawn ("75", "9.5k"); empty with Note.
	Counter string `json:"counter,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (s claudeStall) active() bool { return !s.DetectedAt.IsZero() && s.ClearedAt.IsZero() }

type claudeStallOps struct {
	now   func() time.Time
	sleep func(time.Duration)
	// cursor is the activity observer's place in the agent's transcript.
	cursor func(runtimeBinding) (activityCursor, bool)
	agent  func(context.Context, runtimeBinding) (api.Agent, error)
	// capture proves the pane's identity and returns one capture of it.
	capture func(context.Context, runtimeBinding) (runtimePane, error)
	// interrupt sends the runtime's interrupt key to a pane.
	interrupt func(context.Context, string) error
	// ended reports a transcript read to its end at a completed turn.
	ended func(context.Context, runtimeBinding) bool
	// wake types a prompt through the Claude wake path.
	wake func(context.Context, runtimeBinding, string) error
	// notify posts the stall notice. Nil disables it; the stall is still recorded.
	notify func(context.Context, runtimeBinding, claudeStall) error
	// action reads the host setting.
	action func() string
}

func claudeStallPath(b runtimeBinding) string {
	return filepath.Join(relayDir(), bindingKey(b)+"-"+b.Run+".claude-stall.json")
}

func loadClaudeStall(b runtimeBinding) claudeStall {
	var s claudeStall
	if data, err := os.ReadFile(claudeStallPath(b)); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if s.Run != b.Run {
		return claudeStall{Run: b.Run}
	}
	return s
}

// relayStallAction reads the host setting claudeStallAction from
// ~/.config/tailterm/relay.json, beside hub.json. It is read at each
// detection, so a change needs no relay restart. A missing or unreadable
// file, malformed JSON, a missing key or any value other than "interrupt"
// means report: the relay then sends no key and types nothing. The key is
// read by its exact name from a map: encoding/json would match a struct field
// without regard to case, so "claudestallaction" would count, and would even
// override the real key when both are present.
func relayStallAction() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return claudeStallReport
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "tailterm", "relay.json"))
	if err != nil || len(data) > 64<<10 {
		return claudeStallReport
	}
	var config map[string]json.RawMessage
	var action string
	if json.Unmarshal(data, &config) != nil || json.Unmarshal(config["claudeStallAction"], &action) != nil || action != claudeStallInterrupt {
		return claudeStallReport
	}
	return claudeStallInterrupt
}

// claudeStallQuietSince is when a turn last showed progress.
func claudeStallQuietSince(c activityCursor) time.Time {
	if c.WorktreeChangedAt.After(c.LastEventAt) {
		return c.WorktreeChangedAt
	}
	return c.LastEventAt
}

// claudeStallDecision is the stall rule's answer for one observation.
type claudeStallDecision struct {
	// Quiet: an open Claude turn with no pending tool call and nothing new
	// for the threshold. Since is when it last showed progress.
	Quiet bool
	Since time.Time
	// Stalled: quiet, and two readings far enough apart do not show a growing
	// token counter. Interruptible: both readings showed the same counter on
	// the same pane.
	Stalled       bool
	Interruptible bool
	Counter       string
	// Reason says why the turn is not quiet, not yet stalled, or stalled but
	// not interruptible.
	Reason string
}

// claudeStallDecide is the stall rule. It reads only its arguments.
//
// A turn is quiet when the agent is a running Claude agent, its transcript has
// been read to its end at an open turn, no tool call is pending (a pending
// call belongs to the hung-tool rule, however old), and neither a transcript
// record nor a worktree change is newer than the threshold.
//
// A quiet turn is stalled once two pane readings at least claudeStallProbeGap
// apart are in hand, unless both show a token counter and the counters
// differ: a growing counter is a slow turn, not a stalled one. Equal counters
// on one pane make the stall interruptible. A reading with no counter (no
// status line, a dialog, a draft in the input, a changed pane) still makes a
// stall, because the transcript says so, but never an interruptible one.
func claudeStallDecide(runtime, status string, c activityCursor, first, second *claudeStallReading, now time.Time, threshold time.Duration) claudeStallDecision {
	d := claudeStallDecision{Since: claudeStallQuietSince(c)}
	switch {
	case runtime != "claude":
		d.Reason = "not a Claude agent"
	case status != api.AgentRunning:
		d.Reason = "agent status is " + status
	case c.MissingTranscript || c.Unknown || !c.Ready:
		d.Reason = "transcript not read to its end"
	case !c.SeenTurn:
		d.Reason = "no turn seen yet"
	case c.TurnComplete:
		d.Reason = "turn complete"
	case len(c.Pending) != 0:
		d.Reason = "tool call pending: " + oldestPending(&c).Name
	case d.Since.IsZero() || now.Sub(d.Since) < threshold:
		d.Reason = "output newer than the threshold"
	default:
		d.Quiet = true
	}
	if !d.Quiet {
		return d
	}
	if first == nil || second == nil || second.At.Sub(first.At) < claudeStallProbeGap {
		d.Reason = "waiting for two pane readings"
		return d
	}
	switch {
	case first.Counter != "" && second.Counter != "" && first.Counter != second.Counter:
		d.Reason = "token counter grew"
		return d
	case first.Counter == "" || second.Counter == "":
		notes := first.Note
		if second.Note != "" && second.Note != first.Note {
			notes = strings.TrimPrefix(notes+"; "+second.Note, "; ")
		}
		d.Stalled, d.Reason = true, "token counter not readable: "+notes
	case first.Pane != second.Pane:
		d.Stalled, d.Reason = true, "pane changed between the two readings"
	default:
		d.Stalled, d.Interruptible, d.Counter = true, true, second.Counter
	}
	return d
}

var (
	// "✻ Marinating… (7s · ↓ 75 tokens · thinking with high effort)"
	claudeStatusLine = regexp.MustCompile(`^\s*\S+\s+\S.*…\s+\((.+)\)\s*$`)
	claudeTokenPart  = regexp.MustCompile(`^[↓↑]?\s*([0-9]+(?:\.[0-9]+)?[kKmM]?) tokens$`)
)

// claudeStallCounter reads the token counter from one `capture-pane -p -e`
// capture of a busy Claude pane, or says why there is none. The pane must
// pass the checks a wake makes before typing: no dialog in the prompt area
// and an empty input with the cursor at its start. It must also be busy as
// Claude Code draws it (2.1.292, testdata/claude-pane): the footer says "esc
// to interrupt", and the status line sits above the input box, with only blank
// rows and `⎿` rows (a tip, a task list) between them. A status line in the
// scrollback of an idle pane is therefore never read. Early thinking shows no
// counter and streaming text shows no status line; both are unreadable.
func claudeStallCounter(raw string, cursorX, cursorY int) (string, error) {
	if _, err := claudeInputScreen(raw, cursorX, cursorY, ""); err != nil {
		return "", err
	}
	full, _, err := claudePlainScreen(raw)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(full, "\n"), "\n")
	area, boxed := claudePromptArea(lines)
	if !boxed {
		return "", errors.New("no input box")
	}
	busy := false
	for _, line := range area {
		busy = busy || strings.Contains(line, "esc to interrupt")
	}
	if !busy {
		return "", errors.New("footer does not say esc to interrupt")
	}
	for i := len(lines) - len(area) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "⎿") {
			continue
		}
		m := claudeStatusLine.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		for _, part := range strings.Split(m[1], "·") {
			if t := claudeTokenPart.FindStringSubmatch(strings.TrimSpace(part)); t != nil {
				return t[1], nil
			}
		}
		return "", errors.New("status line shows no token counter")
	}
	return "", errors.New("no status line above the input box")
}

// claudeStallRead captures the pane and reads its counter. A failed capture
// or an unreadable status line is a reading with a note, not an error.
func claudeStallRead(ctx context.Context, b runtimeBinding, ops claudeStallOps) claudeStallReading {
	reading := claudeStallReading{At: ops.now().UTC()}
	pane, err := ops.capture(ctx, b)
	if err == nil {
		reading.Pane = pane.Pane
		reading.Counter, err = claudeStallCounter(pane.Raw, pane.CursorX, pane.CursorY)
	}
	if err != nil {
		reading.Counter = ""
		reading.Note = claudeClip(strings.Join(strings.Fields(err.Error()), " "), 160)
	}
	return reading
}

func claudeStallLog(now time.Time, b runtimeBinding, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[tt relay] %s %s Claude %s\n", now.UTC().Format(time.RFC3339), b.Agent, fmt.Sprintf(format, args...))
}

func claudeStallSetOutcome(s *claudeStall, text string) {
	s.Outcome = claudeClip(strings.Join(strings.Fields(text), " "), 240)
}

// claudeStallPass runs the stall rule for one binding and follows a stall it
// already recorded. It never returns an error: a failure is logged and the
// next pass tries again, and nothing here may hold up delivery.
func claudeStallPass(ctx context.Context, b runtimeBinding, ops claudeStallOps) {
	if b.Runtime != "claude" || !validBinding(b) {
		return
	}
	defer func() {
		if recover() != nil {
			claudeStallLog(ops.now(), b, "stall check panicked; skipped")
		}
	}()
	now := ops.now().UTC()
	path := claudeStallPath(b)
	saved, _ := os.ReadFile(path)
	s := loadClaudeStall(b)
	save := func() {
		if len(saved) == 0 && s.Count == 0 && s.Probe == nil && s.CheckedAt.IsZero() {
			return // nothing was ever seen: leave no file behind
		}
		if err := writePrivateJSON(path, s); err != nil {
			claudeStallLog(now, b, "stall record not saved: %v", err)
		}
	}
	c, ok := ops.cursor(b)
	if !ok || c.Run != b.Run || c.Thread != b.Thread || c.LastCheck.IsZero() || now.Sub(c.LastCheck) > claudeStallCursorAge {
		// Nothing current is known about the turn. A recorded stall stays as
		// it is; a reading being watched is too old to compare later.
		if s.Probe != nil && !s.active() {
			s.Probe = nil
			save()
		}
		return
	}
	if s.active() {
		claudeStallFollow(ctx, b, ops, &s, c, now)
		save()
		return
	}
	// The cursor is the relay's last full read. A transcript that has grown
	// since is progress the next activity check will count.
	grown := false
	if info, err := os.Stat(c.Path); err != nil || fileIdentity(info) != c.FileID || info.Size() != c.Offset {
		grown = true
	}
	threshold := activityDefaults().Stalled
	quiet := claudeStallDecide(b.Runtime, api.AgentRunning, c, nil, nil, now, threshold)
	if grown || !quiet.Quiet {
		if s.Probe != nil {
			s.Probe = nil
			save()
		}
		return
	}
	if s.Probe != nil && s.Since.Equal(quiet.Since) && now.Sub(s.Probe.At) < claudeStallProbeGap {
		return // the second reading is not due yet
	}
	if !s.CheckedAt.IsZero() && now.Sub(s.CheckedAt) < claudeStallProbeGap {
		return
	}
	s.CheckedAt = now
	a, err := ops.agent(ctx, b)
	if err != nil || a.RunID != b.Run {
		save()
		return
	}
	if d := claudeStallDecide(b.Runtime, a.Status, c, nil, nil, now, threshold); !d.Quiet {
		s.Probe = nil
		save()
		return
	}
	reading := claudeStallRead(ctx, b, ops)
	if s.Probe == nil || !s.Since.Equal(quiet.Since) {
		s.Since, s.Probe = quiet.Since, &reading
		save()
		return
	}
	d := claudeStallDecide(b.Runtime, a.Status, c, s.Probe, &reading, now, threshold)
	if !d.Stalled {
		// A counter that grew: watch again from this reading.
		s.Probe = &reading
		save()
		return
	}
	first := *s.Probe
	s = claudeStall{Run: b.Run, Count: s.Count + 1, CheckedAt: now, Since: d.Since, DetectedAt: now, Mode: ops.action(), Counter: d.Counter, Unread: a.Unread, WakeSeqs: claudeStallHeldSeqs(b)}
	if s.Mode != claudeStallInterrupt {
		s.Mode = claudeStallReport
	}
	if !d.Interruptible {
		s.Unreadable = claudeClip(d.Reason, 200)
	}
	signals := fmt.Sprintf("token counter %s unchanged from %s to %s", d.Counter, first.At.Format(time.RFC3339), reading.At.Format(time.RFC3339))
	if !d.Interruptible {
		signals = s.Unreadable
	}
	claudeStallLog(now, b, "turn stalled since %s (stall %d of run %s, mode %s): no transcript record or worktree change for %dm, no tool call pending, %s", s.Since.Format(time.RFC3339), s.Count, b.Run, s.Mode, int(now.Sub(s.Since).Minutes()), signals)
	save()
	claudeStallAct(ctx, b, ops, &s, a, c, reading, now)
	claudeStallNotify(ctx, b, ops, &s, ops.now().UTC())
	save()
}

// claudeStallHeldSeqs lists the message sequences of the latest wake the
// relay held back from this run, for the notice.
func claudeStallHeldSeqs(b runtimeBinding) []int64 {
	var p relayProgress
	data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".progress.json"))
	if err != nil || json.Unmarshal(data, &p) != nil || p.Run != b.Run || p.Skip == nil {
		return nil
	}
	seqs := p.Skip.MessageSeqs
	if len(seqs) > 8 {
		seqs = seqs[len(seqs)-8:]
	}
	return append([]int64(nil), seqs...)
}

// claudeStallAct interrupts a newly detected stall when everything allows it.
// Report mode, a repeat stall, an unreadable counter, the owner's own session
// and any change since the second reading all leave the pane untouched. The
// interrupt time is saved before the key is sent, so no later pass or
// restarted relay can send a second one for this stall.
func claudeStallAct(ctx context.Context, b runtimeBinding, ops claudeStallOps, s *claudeStall, a api.Agent, c activityCursor, second claudeStallReading, now time.Time) {
	refuse := func(reason string) {
		s.NotInterrupted = claudeClip(reason, 200)
		claudeStallSetOutcome(s, "no key sent and nothing typed: "+reason)
		if s.Mode == claudeStallInterrupt {
			claudeStallLog(now, b, "stalled turn not interrupted: %s", reason)
		}
	}
	switch {
	case s.Mode != claudeStallInterrupt:
		claudeStallSetOutcome(s, "no key sent and nothing typed: host setting is report")
		return
	case s.Count > 1:
		refuse(fmt.Sprintf("stall %d of this run; a run is interrupted at most once", s.Count))
		return
	case s.Unreadable != "":
		refuse(s.Unreadable)
		return
	case b.Role == api.AgentRoleOwnerHelper || a.Role == api.AgentRoleOwnerHelper:
		refuse("the owner helper's session is never interrupted")
		return
	}
	// One more reading, at the moment of the key: the same pane, the same
	// counter, a pane that still passes the wake's checks, and a transcript
	// that has not grown.
	last := claudeStallRead(ctx, b, ops)
	if last.Counter == "" {
		refuse("pane not safe at the interrupt: " + last.Note)
		return
	}
	if last.Pane != second.Pane || last.Counter != s.Counter {
		refuse("pane or token counter changed before the interrupt")
		return
	}
	if info, err := os.Stat(c.Path); err != nil || fileIdentity(info) != c.FileID || info.Size() != c.Offset {
		refuse("transcript changed before the interrupt")
		return
	}
	s.InterruptedAt = ops.now().UTC()
	claudeStallSetOutcome(s, "interrupt key being sent")
	if err := writePrivateJSON(claudeStallPath(b), s); err != nil {
		s.InterruptedAt = time.Time{}
		refuse(fmt.Sprintf("stall record not saved: %v", err))
		return
	}
	if err := ops.interrupt(ctx, last.Pane); err != nil {
		claudeStallSetOutcome(s, fmt.Sprintf("interrupt key failed: %v; not sent again", err))
		claudeStallLog(now, b, "stalled turn interrupt failed, not sent again: %v", err)
		return
	}
	claudeStallLog(s.InterruptedAt, b, "stalled turn interrupted once (pane %s)", last.Pane)
	claudeStallSetOutcome(s, "interrupted; waiting for the transcript to show the turn ended")
	for deadline := ops.now().Add(claudeStallEndWait); ; ops.sleep(100 * time.Millisecond) {
		if ops.ended(ctx, b) {
			claudeStallEnded(b, s, ops.now().UTC())
			return
		}
		if !ops.now().Before(deadline) || ctx.Err() != nil {
			return
		}
	}
}

func claudeStallEnded(b runtimeBinding, s *claudeStall, now time.Time) {
	s.EndedAt = now
	claudeStallSetOutcome(s, "interrupted; the transcript shows the turn ended")
	claudeStallLog(now, b, "interrupted turn ended; its wake can now be delivered")
}

// claudeStallFollow follows a recorded stall on later passes. A stall the
// relay did not interrupt stays until the turn shows progress. An interrupted
// one waits for the turn to end, then for the normal wake path, and types the
// resume line only when nothing is unread after claudeStallResumeGrace.
func claudeStallFollow(ctx context.Context, b runtimeBinding, ops claudeStallOps, s *claudeStall, c activityCursor, now time.Time) {
	clear := func(why string) {
		s.ClearedAt, s.Probe = now, nil
		claudeStallLog(now, b, "turn stall of %s ended: %s", s.Since.Format(time.RFC3339), why)
	}
	if s.NoticedAt.IsZero() {
		claudeStallNotify(ctx, b, ops, s, now)
	}
	if s.InterruptedAt.IsZero() || strings.HasPrefix(s.Outcome, "interrupt key failed") {
		switch {
		case c.TurnComplete:
			clear("the turn ended")
		case len(c.Pending) != 0:
			clear("a tool call started")
		case claudeStallQuietSince(c).After(s.Since):
			clear("the turn produced output")
		}
		return
	}
	if s.EndedAt.IsZero() {
		switch {
		case ops.ended(ctx, b):
			claudeStallEnded(b, s, now)
		case now.Sub(s.InterruptedAt) > claudeStallEndLimit:
			// Claude Code records nothing for an Escape pressed before a
			// turn's first assistant record. The stall stands, nothing is
			// typed, and only real progress clears it.
			const never = "interrupted, but the transcript never showed the turn ending; nothing typed; needs a person"
			if s.Outcome != never {
				claudeStallSetOutcome(s, never)
				claudeStallLog(now, b, "interrupted turn never ended in the transcript; nothing typed")
			}
			if claudeStallQuietSince(c).After(s.InterruptedAt) {
				clear("the turn moved on")
			}
			return
		default:
			return
		}
	}
	if !s.RewakeAt.IsZero() {
		clear("its wake was delivered")
		return
	}
	if !s.ResumeAt.IsZero() {
		claudeStallFollowResume(ctx, b, ops, s, now, clear)
		return
	}
	switch {
	case !c.TurnComplete && claudeStallQuietSince(c).After(s.EndedAt):
		clear("a new turn started")
		return
	case now.Sub(s.EndedAt) > claudeStallResumeLimit:
		claudeStallSetOutcome(s, "interrupted; no wake followed")
		clear("no wake followed the interrupt")
		return
	case now.Sub(s.EndedAt) < claudeStallResumeGrace:
		return
	case now.Sub(s.CheckedAt) < claudeStallResumeGrace/2:
		return // the hub was asked a moment ago
	}
	s.CheckedAt = now
	a, err := ops.agent(ctx, b)
	if err != nil || a.RunID != b.Run {
		return
	}
	if a.Unread > 0 {
		// The normal wake path owns unread input and reports its own skips.
		return
	}
	s.ResumeAt = now
	claudeStallSetOutcome(s, "resume line handed to the wake path")
	if err := writePrivateJSON(claudeStallPath(b), s); err != nil {
		s.ResumeAt = time.Time{}
		return
	}
	claudeStallLog(now, b, "nothing unread after the interrupt; typing the resume line once")
	claudeStallResumed(b, ops, s, ops.wake(ctx, b, claudeStallResumeLine), clear)
}

func claudeStallResumed(b runtimeBinding, ops claudeStallOps, s *claudeStall, err error, clear func(string)) {
	if err == nil {
		s.ResumeConfirmedAt = ops.now().UTC()
		claudeStallSetOutcome(s, "interrupted; resume line typed and confirmed")
		clear("the resume line was delivered")
		return
	}
	claudeStallSetOutcome(s, "resume line not confirmed: "+err.Error())
}

// claudeStallFollowResume follows an unconfirmed resume line through the wake
// path's own bounded retries. It calls the wake path again only while that
// path's intent still holds the resume line, so the line is never typed as a
// fresh wake a second time.
func claudeStallFollowResume(ctx context.Context, b runtimeBinding, ops claudeStallOps, s *claudeStall, now time.Time, clear func(string)) {
	var intent claudeWakeIntent
	data, err := os.ReadFile(claudeWakePath(b))
	if err != nil || json.Unmarshal(data, &intent) != nil || intent.Run != b.Run || intent.PromptSHA != claudeWakeHash(claudeStallResumeLine) {
		clear("another wake followed the resume line")
		return
	}
	switch intent.Phase {
	case "confirmed":
		claudeStallResumed(b, ops, s, nil, clear)
	case "uncertain", "exhausted":
		if now.Sub(s.ResumeAt) > claudeStallResumeLimit {
			claudeStallSetOutcome(s, "resume line never confirmed; needs a person")
			clear("the resume line never confirmed")
			return
		}
		claudeStallResumed(b, ops, s, ops.wake(ctx, b, claudeStallResumeLine), clear)
	default:
		clear("the transcript moved on without the resume line")
	}
}

// claudeStallWake wraps the Claude wake path for the relay loop: the first
// wake that confirms after an interrupt is the re-delivery, and the stall
// record keeps its time.
func claudeStallWake(queue func(context.Context, runtimeBinding, string) error, now func() time.Time) func(context.Context, runtimeBinding, string) error {
	return func(ctx context.Context, b runtimeBinding, prompt string) error {
		err := queue(ctx, b, prompt)
		if err == nil {
			claudeStallNoteWake(b, now().UTC())
		}
		return err
	}
}

// claudeStallNoteWake records a wake confirmed after this run's interrupt.
func claudeStallNoteWake(b runtimeBinding, now time.Time) {
	data, err := os.ReadFile(claudeStallPath(b))
	if err != nil {
		return
	}
	var s claudeStall
	if json.Unmarshal(data, &s) != nil || s.Run != b.Run || !s.active() || s.InterruptedAt.IsZero() || !s.RewakeAt.IsZero() || !s.ResumeAt.IsZero() {
		return
	}
	// Only a wake the transcript confirmed after the interrupt counts; an
	// earlier confirmed intent with the same prompt is not a re-delivery.
	var intent claudeWakeIntent
	if data, err := os.ReadFile(claudeWakePath(b)); err != nil || json.Unmarshal(data, &intent) != nil || intent.Run != b.Run || intent.Phase != "confirmed" || intent.ConfirmedAt.Before(s.InterruptedAt) {
		return
	}
	s.RewakeAt = now
	if s.EndedAt.IsZero() {
		s.EndedAt = now
	}
	claudeStallSetOutcome(&s, "interrupted; pending wake delivered and confirmed")
	claudeStallLog(now, b, "pending wake delivered after the interrupt")
	if err := writePrivateJSON(claudeStallPath(b), s); err != nil {
		claudeStallLog(now, b, "stall record not saved: %v", err)
	}
}

// claudeStallStuckReason is the stuck reason of a recorded stall that still
// holds: the turn is open, nothing is pending and nothing has been written
// since. It is "" once the turn moves or ends.
func claudeStallStuckReason(b runtimeBinding, c *activityCursor, now time.Time) string {
	if b.Runtime != "claude" || c.TurnComplete || len(c.Pending) != 0 {
		return ""
	}
	s := loadClaudeStall(b)
	if !s.active() || claudeStallQuietSince(*c).After(s.Since) {
		return ""
	}
	return fmt.Sprintf("turn stalled: no output for %dm", int(now.Sub(s.Since).Minutes()))
}

// claudeStallNoticeText writes the notice for a stall. It uses only what the
// record holds, so the text is the same however often it is built.
func claudeStallNoticeText(b runtimeBinding, s claudeStall) string {
	counter := fmt.Sprintf("the pane's token counter read %s in two captures at least %ds apart", s.Counter, int(claudeStallProbeGap.Seconds()))
	if s.Unreadable != "" {
		counter = "the pane's token counter could not be compared (" + s.Unreadable + ")"
	}
	text := fmt.Sprintf("Agent %s (run %s, tmux session %s) has an open turn that stopped producing output. Signals at %s: no transcript record and no worktree change since %s (%dm); no tool call pending; %s. This is stall %d of the run. Host setting claudeStallAction is %s. ",
		b.Agent, b.Run, b.Session, s.DetectedAt.Format(time.RFC3339), s.Since.Format(time.RFC3339), int(s.DetectedAt.Sub(s.Since).Minutes()), counter, s.Count, s.Mode)
	next := "type the resume line once"
	if s.Unread > 0 {
		next = fmt.Sprintf("let the normal wake deliver its %d unread", s.Unread)
		if len(s.WakeSeqs) > 0 {
			next += fmt.Sprintf(" (held wake sequences %v)", s.WakeSeqs)
		}
	}
	switch {
	case s.Count > 1:
		text += "The relay sent no key and typed nothing: it never interrupts a run after its first stall. This is a repeat, so the agent needs a person."
	case !s.InterruptedAt.IsZero() && strings.HasPrefix(s.Outcome, "interrupt key failed"):
		text += "The relay tried to interrupt the turn at " + s.InterruptedAt.Format(time.RFC3339) + " and the key could not be sent. It will not try again for this stall and typed nothing. The agent needs a person."
	case !s.InterruptedAt.IsZero() && !s.EndedAt.IsZero():
		text += "The relay interrupted the turn once at " + s.InterruptedAt.Format(time.RFC3339) + " and the transcript shows it ended. It will now " + next + "."
	case !s.InterruptedAt.IsZero():
		text += "The relay interrupted the turn once at " + s.InterruptedAt.Format(time.RFC3339) + ". The transcript had not shown the turn ending " + fmt.Sprint(int(claudeStallEndWait.Seconds())) + "s later; nothing is typed until it does. If it does, the relay will " + next + "."
	case s.Mode == claudeStallInterrupt:
		text += "The relay sent no key and typed nothing, because " + s.NotInterrupted + ". The agent needs a person."
	case s.Unreadable != "":
		text += "The relay sent no key and typed nothing. With the setting on interrupt it would still not have interrupted, because the counter could not be compared."
	default:
		text += "The relay sent no key and typed nothing. With the setting on interrupt it would have interrupted the turn once and then would " + next + "."
	}
	if s.Unread > 0 {
		text += " The agent has unread input, so an earlier notice about a skipped wake may describe the same episode."
	}
	return text
}

// claudeStallNotify posts one notice for every stall, once. A stall after the
// run's first is marked as a repeat. The text is frozen in the record before
// the first attempt; a failed attempt is tried again at most once a minute.
func claudeStallNotify(ctx context.Context, b runtimeBinding, ops claudeStallOps, s *claudeStall, now time.Time) {
	if ops.notify == nil || !s.NoticedAt.IsZero() || (!s.NoticeTriedAt.IsZero() && now.Sub(s.NoticeTriedAt) < claudeStallNoticeRetry) {
		return
	}
	if s.Notice == "" {
		s.Notice = claudeStallNoticeText(b, *s)
	}
	s.NoticeTriedAt = now
	if err := ops.notify(ctx, b, *s); err != nil {
		claudeStallLog(now, b, "stall notice failed: %v", err)
		return
	}
	s.NoticedAt = now
	if s.Count > 1 {
		s.EscalatedAt = now
	}
	claudeStallLog(now, b, "stall notice posted once (stall %d of the run)", s.Count)
}

// claudeStallRecipient picks who hears about a stalled agent: the newest live
// owner helper, never the stalled agent itself. The zero Agent means the Board.
func claudeStallRecipient(agents []api.Agent, stalled string) api.Agent {
	var found api.Agent
	for _, a := range agents {
		if a.Role != api.AgentRoleOwnerHelper || a.ID == stalled || a.Status == api.AgentClosed || a.Status == api.AgentExited || a.Status == api.AgentRetired {
			continue
		}
		if found.ID == "" || a.CreatedAt.After(found.CreatedAt) {
			found = a
		}
	}
	return found
}

// claudeStallRequestID identifies one stall's notice: the run and when the
// turn went quiet.
func claudeStallRequestID(b runtimeBinding, s claudeStall) string {
	return fmt.Sprintf("claude-stall-%s-%d", strings.TrimPrefix(b.Run, "run_"), s.Since.Unix())
}

// nativeClaudeStallNotify posts the frozen notice as the relay, like the
// wake-skip notice (nativeClaudeEscalate): the relay's configured hub token,
// its shared request budget, and the relay-author mark. A conflict means this
// identity was stored before with other data, which only happens when the
// record was lost after a successful post, so it counts as posted.
func nativeClaudeStallNotify(ctx context.Context, b runtimeBinding, s claudeStall) error {
	e := env{hub: b.Hub}
	e.loadConfig()
	c, err := e.client(3 * time.Second)
	if err != nil {
		return err
	}
	c.HTTP.Transport = relayAuthorTransport{base: c.HTTP.Transport}
	attachRelayBudget(c, activeRelayBudget)
	agents, err := c.ListAgents(ctx, b.Task)
	if err != nil {
		return err
	}
	to := claudeStallRecipient(agents, b.Agent)
	refs := map[string]string{"agent": b.Agent, "run": b.Run, "session": b.Session, "mode": s.Mode, "stall": fmt.Sprint(s.Count)}
	if s.Count > 1 {
		refs["repeat"] = "true"
	}
	env := api.Envelope{Kind: api.EnvelopeKindNotice, To: to.Name, Subject: claudeStallSubject, Refs: refs, Body: api.EnvelopeBody{Text: s.Notice}}
	_, err = c.PostMessage(ctx, b.Task, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: to.ID, RequestID: claudeStallRequestID(b, s)})
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		return nil
	}
	return err
}

// nativeClaudeInterrupt presses Escape, Claude Code's interrupt key, once.
func nativeClaudeInterrupt(ctx context.Context, pane string) error {
	_, err := startupTmux(ctx, "send-keys", "-t", pane, "Escape")
	return err
}

// nativeClaudeStallOps reads this host's state and acts on real panes.
func nativeClaudeStallOps(c *api.Client) claudeStallOps {
	return claudeStallOps{
		now:   time.Now,
		sleep: time.Sleep,
		cursor: func(b runtimeBinding) (activityCursor, bool) {
			var cursor activityCursor
			data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".activity.json"))
			if err != nil || json.Unmarshal(data, &cursor) != nil {
				return activityCursor{}, false
			}
			return cursor, true
		},
		agent: func(ctx context.Context, b runtimeBinding) (api.Agent, error) {
			return c.GetAgent(ctx, b.Task, b.Agent)
		},
		capture: func(ctx context.Context, b runtimeBinding) (runtimePane, error) {
			return inspectRuntimePane(ctx, b, "Claude", false)
		},
		interrupt: nativeClaudeInterrupt,
		ended: func(ctx context.Context, b runtimeBinding) bool {
			_, err := claudeTranscriptSnapshotContext(ctx, b, time.Now())
			return err == nil
		},
		wake:   claudeQueue,
		notify: nativeClaudeStallNotify,
		action: relayStallAction,
	}
}
