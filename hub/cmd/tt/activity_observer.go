package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const maxActivityLine = 1 << 20
const maxActivityPass = 4 << 20

var activityToolName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)

func safeActivityToolName(name string) string {
	if activityToolName.MatchString(name) {
		return name
	}
	return "unknown_tool"
}

type pendingActivityCall struct {
	Name      string    `json:"name"`
	Since     time.Time `json:"since"`
	Signature string    `json:"signature"`
	WaitUntil time.Time `json:"waitUntil,omitempty"`
}
type completedActivityCall struct {
	Signature   string    `json:"signature"`
	At          time.Time `json:"at"`
	Tokens      int64     `json:"tokens"`
	Worktree    string    `json:"worktree"`
	PlannedWait bool      `json:"plannedWait,omitempty"`
}

// A separately frozen snapshot prevents a later partial backlog from replacing
// the last values verified at a complete observed EOF.
type verifiedActivitySnapshot struct {
	Path        string          `json:"path"`
	FileID      uint64          `json:"fileId"`
	Offset      int64           `json:"offset"`
	LastEventAt time.Time       `json:"lastEventAt"`
	Tokens      api.TokenTotals `json:"tokens"`
}

type activityCursor struct {
	Run               string                         `json:"run"`
	Thread            string                         `json:"thread"`
	Path              string                         `json:"path"`
	LastDiscovery     time.Time                      `json:"lastDiscovery,omitempty"`
	FileID            uint64                         `json:"fileId"`
	Offset            int64                          `json:"offset"`
	Ready             bool                           `json:"ready,omitempty"`
	Verified          *verifiedActivitySnapshot      `json:"verified,omitempty"`
	TokensVerified    bool                           `json:"tokensVerified,omitempty"`
	Partial           string                         `json:"partial,omitempty"`
	Skipping          bool                           `json:"skipping,omitempty"`
	LastEventAt       time.Time                      `json:"lastEventAt"`
	TurnComplete      bool                           `json:"turnComplete"`
	SeenTurn          bool                           `json:"seenTurn"`
	Pending           map[string]pendingActivityCall `json:"pending,omitempty"`
	Completed         []completedActivityCall        `json:"completed,omitempty"`
	Tokens            api.TokenTotals                `json:"tokens"`
	ClaudeUsage       map[string]api.TokenTotals     `json:"claudeUsage,omitempty"`
	Unknown           bool                           `json:"unknown,omitempty"`
	MissingTranscript bool                           `json:"missingTranscript,omitempty"`
	Worktree          string                         `json:"worktree,omitempty"`
	WorktreeChangedAt time.Time                      `json:"worktreeChangedAt,omitempty"`
	MissingSince      time.Time                      `json:"missingSince,omitempty"`
	LastCheck         time.Time                      `json:"lastCheck,omitempty"`
	LastState         string                         `json:"lastState,omitempty"`
	LastWakeKey       string                         `json:"lastWakeKey,omitempty"`
	Transition        int64                          `json:"transition,omitempty"`
	PendingReport     *api.ActivityReport            `json:"pendingReport,omitempty"`
	LastReportAttempt time.Time                      `json:"lastReportAttempt,omitempty"`
	RejectedState     string                         `json:"rejectedState,omitempty"`
	RejectedWakeKey   string                         `json:"rejectedWakeKey,omitempty"`
	Ineligible        bool                           `json:"ineligible,omitempty"`
	// StuckUnsupported records that this hub rejected the stuck state; later
	// stuck observations are reported as unknown with a "stuck:" reason.
	StuckUnsupported bool `json:"stuckUnsupported,omitempty"`
	// ClaudeQueued counts input Claude Code queued while busy (queue-operation
	// enqueue) that no dequeue or remove has taken yet. ClaudeQueuedAt is the
	// latest enqueue. A real user prompt resets both, which bounds drift from
	// unmatched removes and sessions that ended with input still queued.
	ClaudeQueued   int       `json:"claudeQueued,omitempty"`
	ClaudeQueuedAt time.Time `json:"claudeQueuedAt,omitempty"`
	// TurnEndReason explains an unusual completed turn, such as one Claude Code
	// ended with an API error. It is empty after a normal end_turn and is
	// cleared when a new turn starts.
	TurnEndReason string `json:"turnEndReason,omitempty"`
	// Model is the last real model the transcript named. ProviderBlock is the
	// current run of consecutive provider failures (docs/provider-blocked.md).
	// ProviderUnsupported records that this hub rejected the provider_blocked
	// state; later blocks are reported in the legacy form.
	Model               string               `json:"model,omitempty"`
	ProviderBlock       *providerBlockCursor `json:"providerBlock,omitempty"`
	ProviderUnsupported bool                 `json:"providerUnsupported,omitempty"`
}

// providerBlockCursor holds typed fields only: no provider or transcript text.
type providerBlockCursor struct {
	Runtime string    `json:"runtime"`
	Class   string    `json:"class"`
	Code    string    `json:"code,omitempty"`
	Status  int       `json:"status,omitempty"`
	Since   time.Time `json:"since"`
	Count   int       `json:"count"`
}

type activityThresholds struct {
	Working    time.Duration
	Hung       time.Duration
	Loop       time.Duration
	LoopCalls  int
	CrashProbe time.Duration
	// WakeStuck is how long a Claude wake may stay unconfirmed, with unread
	// input, before the agent is reported stuck.
	WakeStuck time.Duration
	// ProviderRepeat is how many failed turns in a row make a rate-limited or
	// server-error provider failure a block. Values below 2 mean 2.
	ProviderRepeat int
}

func activityDefaults() activityThresholds {
	seconds := func(name string, fallback int) time.Duration {
		v, err := strconv.Atoi(os.Getenv(name))
		if err != nil || v < 1 || v > 86400 {
			v = fallback
		}
		return time.Duration(v) * time.Second
	}
	calls, err := strconv.Atoi(os.Getenv("TAILTERM_ACTIVITY_LOOP_CALLS"))
	if err != nil || calls < 2 || calls > 100 {
		calls = 5
	}
	repeat, err := strconv.Atoi(os.Getenv("TAILTERM_ACTIVITY_PROVIDER_REPEAT"))
	if err != nil || repeat < 2 || repeat > 20 {
		repeat = 2
	}
	return activityThresholds{seconds("TAILTERM_ACTIVITY_WORKING_SECONDS", 120), seconds("TAILTERM_ACTIVITY_HUNG_SECONDS", 600), seconds("TAILTERM_ACTIVITY_LOOP_SECONDS", 300), calls, seconds("TAILTERM_ACTIVITY_CRASH_PROBE_SECONDS", 15), seconds("TAILTERM_ACTIVITY_WAKE_STUCK_SECONDS", 180), repeat}
}

func activityTranscript(b runtimeBinding) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var pattern string
	if b.Runtime == "claude" {
		pattern = filepath.Join(home, ".claude", "projects", "*", b.Thread+".jsonl")
	} else {
		base := b.CodexHome
		if base == "" {
			base = filepath.Join(home, ".codex")
		}
		pattern = filepath.Join(base, "sessions", "*", "*", "*", "rollout-*-"+b.Thread+".jsonl")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) == 0 {
		return "", err
	}
	// The exact session UUID is required in every candidate basename. Prefer
	// the newest when a runtime has copied or rolled a transcript.
	newest := paths[0]
	var newestTime time.Time
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = path, info.ModTime()
		}
	}
	return newest, nil
}

func fileIdentity(info os.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino
	}
	return 0
}

// readActivityAppend advances only across complete lines. A partial final
// record is bounded and retained locally until the next pass.
func readActivityAppend(path string, c *activityCursor, parse func([]byte, *activityCursor) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	c.Ready = false
	id := fileIdentity(info)
	if c.Path != path || c.FileID != id || info.Size() < c.Offset {
		c.Path, c.FileID, c.Offset, c.Partial, c.Skipping = path, id, 0, "", false
		c.Verified = nil
		// Preserve existing cumulative parser semantics, but do not certify retained
		// totals as belonging to a replacement file. Codex's explicit cumulative
		// usage can establish provenance again; retained Claude usage cannot.
		c.TokensVerified = c.Tokens == (api.TokenTotals{}) && len(c.ClaudeUsage) == 0
		c.Pending = map[string]pendingActivityCall{}
		c.Completed = nil
		c.SeenTurn, c.TurnComplete, c.TurnEndReason = false, false, ""
		c.ClaudeQueued, c.ClaudeQueuedAt = 0, time.Time{}
		c.Model, c.ProviderBlock = "", nil
		if c.ClaudeUsage == nil {
			c.ClaudeUsage = map[string]api.TokenTotals{}
		}
		c.Unknown = false
	}
	if _, err = f.Seek(c.Offset, io.SeekStart); err != nil {
		return err
	}
	remaining := min(info.Size()-c.Offset, maxActivityPass)
	if remaining < 0 {
		return errors.New("negative transcript offset")
	}
	// Readiness is relative to this pass's observed EOF. An incomplete final
	// record (including an oversized one being skipped) delays publication.
	defer func() { c.Ready = c.Offset == info.Size() && c.Partial == "" && !c.Skipping }()
	reader := bufio.NewReaderSize(io.LimitReader(f, remaining), 64<<10)
	for records := 0; ; records++ {
		if records >= 256 {
			return nil
		}
		line, readErr := reader.ReadBytes('\n')
		c.Offset += int64(len(line))
		if c.Skipping {
			if len(line) > 0 && line[len(line)-1] == '\n' {
				c.Skipping = false
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			continue
		}
		if len(c.Partial)+len(line) > maxActivityLine {
			if !c.SeenTurn {
				c.Unknown = true
			}
			c.Partial = ""
			c.Skipping = len(line) == 0 || line[len(line)-1] != '\n'
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			continue
		}
		c.Partial += string(line)
		if len(line) > 0 && line[len(line)-1] == '\n' {
			if err := parse([]byte(strings.TrimSpace(c.Partial)), c); err != nil {
				if !c.SeenTurn {
					c.Unknown = true
				}
			} else if c.SeenTurn {
				c.Unknown = false
			}
			c.Partial = ""
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

type activityRecord struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
	Message   json.RawMessage `json:"message"`
}

func activityTime(value string, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	return fallback
}

func activityInt(raw json.RawMessage, names ...string) int64 {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return 0
	}
	for _, name := range names {
		if v, ok := fields[name]; ok {
			var n int64
			if json.Unmarshal(v, &n) == nil {
				return n
			}
		}
	}
	return 0
}

func activitySignature(name string, args json.RawMessage) string {
	digest := sha256.Sum256(append([]byte(name+"\x00"), args...))
	return name + ":" + hex.EncodeToString(digest[:8])
}

func explicitWait(args json.RawMessage, now time.Time) time.Time {
	var encoded string
	if json.Unmarshal(args, &encoded) == nil {
		args = json.RawMessage(encoded)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return time.Time{}
	}
	var maxMS int64
	for _, name := range []string{"yield_time_ms", "timeout_ms", "duration_ms", "timeout"} {
		if n := activityInt(args, name); n > maxMS {
			maxMS = n
		}
	}
	if maxMS < 1 || maxMS > 86400000 {
		return time.Time{}
	}
	return now.Add(time.Duration(maxMS) * time.Millisecond)
}

func finishActivityCall(c *activityCursor, id string, now time.Time) {
	call, ok := c.Pending[id]
	if !ok {
		return
	}
	delete(c.Pending, id)
	c.Completed = append(c.Completed, completedActivityCall{Signature: call.Signature, At: now, Tokens: c.Tokens.Total, Worktree: c.Worktree, PlannedWait: call.WaitUntil.Sub(call.Since) >= time.Minute || now.Sub(call.Since) >= time.Minute})
	if len(c.Completed) > 32 {
		c.Completed = c.Completed[len(c.Completed)-32:]
	}
}

var (
	providerModelName   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	codexProviderStatus = regexp.MustCompile(`^unexpected status ([0-9]{3})\b`)
)

// noteProviderModel keeps the last well-formed model name. Claude Code's
// "<synthetic>" API-error records and anything malformed are ignored.
func noteProviderModel(c *activityCursor, model string) {
	if providerModelName.MatchString(model) {
		c.Model = model
	}
}

// providerStatusClass maps an HTTP status to a provider block class, or "".
func providerStatusClass(status int) string {
	switch {
	case status == 401 || status == 403:
		return api.ProviderBlockAuth
	case status == 429:
		return api.ProviderBlockRateLimited
	case status >= 500 && status <= 599:
		return api.ProviderBlockServerError
	}
	return ""
}

// claudeProviderClass classifies a Claude Code API-error record from its typed
// fields. The text is only compared with fixed prefixes: a rate_limit error
// that starts with one of them is an account usage limit, not a passing 429.
func claudeProviderClass(code string, status int, text string) string {
	switch code {
	case "authentication_failed":
		return api.ProviderBlockAuth
	case "rate_limit":
		for _, prefix := range []string{"You've reached your", "You've hit your", "You\u2019ve reached your", "You\u2019ve hit your"} {
			if strings.HasPrefix(text, prefix) {
				return api.ProviderBlockUsageLimit
			}
		}
		return api.ProviderBlockRateLimited
	case "server_error", "overloaded", "overloaded_error":
		return api.ProviderBlockServerError
	}
	return providerStatusClass(status)
}

// codexProviderClass classifies a Codex task_complete error message of the
// form "unexpected status NNN ...". Only the status is taken from it.
func codexProviderClass(message string) (string, int) {
	m := codexProviderStatus.FindStringSubmatch(message)
	if m == nil {
		return "", 0
	}
	status, _ := strconv.Atoi(m[1])
	return providerStatusClass(status), status
}

// noteProviderFailure records one failed turn. The same class again extends
// the block and keeps its since; a different class starts a new one.
func noteProviderFailure(c *activityCursor, runtime, class, code string, status int, at time.Time) {
	if !claudeAPIErrorCode.MatchString(code) {
		code = ""
	}
	if status < 400 || status > 599 {
		status = 0
	}
	if b := c.ProviderBlock; b != nil && b.Class == class {
		b.Count++
		b.Code, b.Status = code, status
		return
	}
	c.ProviderBlock = &providerBlockCursor{Runtime: runtime, Class: class, Code: code, Status: status, Since: at, Count: 1}
}

// reportableProviderBlock is the block to report, or nil. A usage limit or an
// authentication failure blocks at once; a rate limit or server error only
// after the repeat threshold of failed turns in a row.
func reportableProviderBlock(c *activityCursor, threshold activityThresholds) *api.ProviderBlock {
	b := c.ProviderBlock
	if b == nil {
		return nil
	}
	if b.Class != api.ProviderBlockUsageLimit && b.Class != api.ProviderBlockAuth && b.Count < max(threshold.ProviderRepeat, 2) {
		return nil
	}
	model := c.Model
	if model == "" {
		model = "unknown"
	}
	return &api.ProviderBlock{Provider: api.ProviderForRuntime(b.Runtime), Runtime: b.Runtime, Model: model, Class: b.Class, Code: b.Code, Status: b.Status, Since: b.Since}
}

func parseCodexActivity(line []byte, c *activityCursor) error {
	if len(line) == 0 {
		return nil
	}
	var rec activityRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return err
	}
	now := activityTime(rec.Timestamp, time.Now().UTC())
	c.LastEventAt = now
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Role      string          `json:"role"`
		Model     string          `json:"model"`
		Info      struct {
			Total json.RawMessage `json:"total_token_usage"`
		} `json:"info"`
		Error *struct {
			Info    json.RawMessage `json:"codex_error_info"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Payload, &p)
	switch rec.Type {
	case "event_msg":
		switch p.Type {
		case "task_started":
			c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, false, ""
		case "task_complete":
			c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, true, ""
			if p.Error == nil {
				c.ProviderBlock = nil
			} else if class, status := codexProviderClass(p.Error.Message); class != "" {
				var code string
				_ = json.Unmarshal(p.Error.Info, &code)
				noteProviderFailure(c, "codex", class, code, status, now)
				c.TurnEndReason = "turn ended by provider error (" + class + ")"
			}
		case "token_count":
			u := p.Info.Total
			c.TokensVerified = true
			c.Tokens = api.TokenTotals{Input: activityInt(u, "input_tokens"), Cached: activityInt(u, "cached_input_tokens"), CacheWrite: activityInt(u, "cache_write_tokens"), Output: activityInt(u, "output_tokens"), Reasoning: activityInt(u, "reasoning_output_tokens"), Total: activityInt(u, "total_tokens")}
			if c.Tokens.Total == 0 {
				c.Tokens.Total = c.Tokens.Input + c.Tokens.Output
			}
		}
	case "response_item":
		// Model output is evidence the provider answered. Prompts are recorded
		// as response items too, and a new prompt alone clears nothing.
		if p.Role != "user" && p.Role != "developer" && p.Role != "system" {
			c.ProviderBlock = nil
		}
		switch p.Type {
		case "function_call", "custom_tool_call":
			if p.CallID == "" {
				return errors.New("tool call without call id")
			}
			if c.Pending == nil {
				c.Pending = map[string]pendingActivityCall{}
			}
			args := p.Arguments
			if p.Type == "custom_tool_call" {
				args = p.Input
			}
			c.Pending[p.CallID] = pendingActivityCall{Name: safeActivityToolName(p.Name), Since: now, Signature: activitySignature(p.Name, args), WaitUntil: explicitWait(args, now)}
		case "function_call_output", "custom_tool_call_output":
			finishActivityCall(c, p.CallID, now)
		}
	case "task_started":
		c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, false, ""
	case "task_complete":
		c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, true, ""
		c.ProviderBlock = nil
	case "turn_context":
		noteProviderModel(c, p.Model)
	case "session_meta":
	default:
		return fmt.Errorf("unknown Codex record type %q", rec.Type)
	}
	return nil
}

// unknownClaudeRecordError names a transcript record type (or queue-operation
// operation) the parser does not know. The wake check tolerates it; any other
// parse error is still strict.
type unknownClaudeRecordError struct{ Type string }

func (e unknownClaudeRecordError) Error() string {
	return fmt.Sprintf("unknown Claude record type %q", e.Type)
}

var claudeAPIErrorCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

// claudeTurnEnd reports whether a Claude transcript record ends a turn, and
// the reason for an unusual end. A turn ends at assistant end_turn, a result
// record, a system turn_duration record, or the synthetic assistant record
// Claude Code writes when an API error ends the response (isApiErrorMessage,
// stop_reason stop_sequence). Before bug wi_132c8895adfe0886 the last two
// left the turn "in progress" and every wake was skipped. The reason carries
// only a fixed phrase and a short error code, never transcript text.
func claudeTurnEnd(line []byte) (ended bool, reason string) {
	var rec struct {
		Type       string `json:"type"`
		Subtype    string `json:"subtype"`
		IsAPIError bool   `json:"isApiErrorMessage"`
		Error      string `json:"error"`
		Message    struct {
			StopReason string `json:"stop_reason"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return false, ""
	}
	switch rec.Type {
	case "assistant":
		if rec.IsAPIError {
			reason = "turn ended by API error"
			if claudeAPIErrorCode.MatchString(rec.Error) {
				reason += " (" + rec.Error + ")"
			}
			return true, reason
		}
		return rec.Message.StopReason == "end_turn", ""
	case "result":
		return true, ""
	case "system":
		return rec.Subtype == "turn_duration", ""
	}
	return false, ""
}

func parseClaudeActivity(line []byte, c *activityCursor) error {
	if len(line) == 0 {
		return nil
	}
	var rec activityRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return err
	}
	ended, endReason := claudeTurnEnd(line)
	now := activityTime(rec.Timestamp, time.Now().UTC())
	c.LastEventAt = now
	var msg struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Text      string          `json:"text"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"content"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(rec.Message, &msg)
	switch rec.Type {
	case "assistant":
		c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, false, ""
		var apiError struct {
			IsAPIError bool   `json:"isApiErrorMessage"`
			Error      string `json:"error"`
			Status     int    `json:"apiErrorStatus"`
		}
		_ = json.Unmarshal(line, &apiError)
		if !apiError.IsAPIError {
			// An ordinary assistant record is evidence the provider answered.
			c.ProviderBlock = nil
			noteProviderModel(c, msg.Model)
		} else {
			var text string
			for _, part := range msg.Content {
				if part.Type == "text" {
					text = part.Text
					break
				}
			}
			if class := claudeProviderClass(apiError.Error, apiError.Status, text); class != "" {
				noteProviderFailure(c, "claude", class, apiError.Error, apiError.Status, now)
			}
		}
		if msg.ID != "" && len(msg.Usage) > 0 {
			if c.ClaudeUsage == nil {
				c.ClaudeUsage = map[string]api.TokenTotals{}
			}
			u := api.TokenTotals{Input: activityInt(msg.Usage, "input_tokens"), Cached: activityInt(msg.Usage, "cache_read_input_tokens"), CacheWrite: activityInt(msg.Usage, "cache_creation_input_tokens"), Output: activityInt(msg.Usage, "output_tokens")}
			old := c.ClaudeUsage[msg.ID]
			u.Input = max(u.Input, old.Input)
			u.Cached = max(u.Cached, old.Cached)
			u.CacheWrite = max(u.CacheWrite, old.CacheWrite)
			u.Output = max(u.Output, old.Output)
			u.Total = u.Input + u.Cached + u.CacheWrite + u.Output
			c.Tokens.Input += u.Input - old.Input
			c.Tokens.Cached += u.Cached - old.Cached
			c.Tokens.CacheWrite += u.CacheWrite - old.CacheWrite
			c.Tokens.Output += u.Output - old.Output
			c.Tokens.Total += u.Total - old.Total
			c.ClaudeUsage[msg.ID] = u
		}
		for _, part := range msg.Content {
			if part.Type == "tool_use" && part.ID != "" {
				if c.Pending == nil {
					c.Pending = map[string]pendingActivityCall{}
				}
				c.Pending[part.ID] = pendingActivityCall{Name: safeActivityToolName(part.Name), Since: now, Signature: activitySignature(part.Name, part.Input), WaitUntil: explicitWait(part.Input, now)}
			}
		}
		if ended {
			c.TurnComplete, c.TurnEndReason = true, endReason
			if endReason != "" {
				// A response cut short by an API error runs no tool, so a
				// tool_use it recorded would otherwise stay pending forever.
				clear(c.Pending)
			}
		}
	case "user":
		// A real user prompt starts a new turn. Tool results are also encoded as
		// user records, but they continue the assistant's existing turn.
		if claudeUserText(line) != "" {
			c.SeenTurn, c.TurnComplete, c.TurnEndReason = true, false, ""
			c.ClaudeQueued, c.ClaudeQueuedAt = 0, time.Time{}
		}
		for _, part := range msg.Content {
			if part.Type == "tool_result" {
				finishActivityCall(c, part.ToolUseID, now)
			}
		}
	case "result":
		c.SeenTurn, c.TurnComplete = true, true
	case "queue-operation":
		// Claude Code queues input typed or delivered while it is busy. The
		// record never changes turn state: a dequeued prompt arrives as its own
		// user record, and a removed one was absorbed into the running turn or
		// discarded.
		var q struct {
			Operation string `json:"operation"`
		}
		_ = json.Unmarshal(line, &q)
		switch q.Operation {
		case "enqueue":
			c.ClaudeQueued++
			c.ClaudeQueuedAt = now
		case "dequeue", "remove":
			c.ClaudeQueued = max(0, c.ClaudeQueued-1)
			if c.ClaudeQueued == 0 {
				c.ClaudeQueuedAt = time.Time{}
			}
		default:
			return unknownClaudeRecordError{Type: "queue-operation/" + q.Operation}
		}
	case "system":
		// turn_duration closes every turn, including one an API error ended.
		// No tool can still run after it. It keeps the reason set by the
		// record that ended the turn.
		if ended {
			c.SeenTurn, c.TurnComplete = true, true
			clear(c.Pending)
		}
	case "summary", "progress", "file-history-snapshot", "mode", "permission-mode", "ai-title", "atis-latch", "cost-state", "last-prompt", "attachment":
	default:
		return unknownClaudeRecordError{Type: rec.Type}
	}
	return nil
}
