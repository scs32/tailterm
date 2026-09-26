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
type activityCursor struct {
	Run               string                         `json:"run"`
	Thread            string                         `json:"thread"`
	Path              string                         `json:"path"`
	LastDiscovery     time.Time                      `json:"lastDiscovery,omitempty"`
	FileID            uint64                         `json:"fileId"`
	Offset            int64                          `json:"offset"`
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
	Transition        int64                          `json:"transition,omitempty"`
	PendingReport     *api.ActivityReport            `json:"pendingReport,omitempty"`
	LastReportAttempt time.Time                      `json:"lastReportAttempt,omitempty"`
	RejectedState     string                         `json:"rejectedState,omitempty"`
	Ineligible        bool                           `json:"ineligible,omitempty"`
}

type activityThresholds struct {
	Working    time.Duration
	Hung       time.Duration
	Loop       time.Duration
	LoopCalls  int
	CrashProbe time.Duration
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
	return activityThresholds{seconds("TAILTERM_ACTIVITY_WORKING_SECONDS", 120), seconds("TAILTERM_ACTIVITY_HUNG_SECONDS", 600), seconds("TAILTERM_ACTIVITY_LOOP_SECONDS", 300), calls, seconds("TAILTERM_ACTIVITY_CRASH_PROBE_SECONDS", 15)}
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
	id := fileIdentity(info)
	if c.Path != path || c.FileID != id || info.Size() < c.Offset {
		c.Path, c.FileID, c.Offset, c.Partial, c.Skipping = path, id, 0, "", false
		c.Pending = map[string]pendingActivityCall{}
		c.Completed = nil
		c.SeenTurn, c.TurnComplete = false, false
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
	reader := bufio.NewReaderSize(io.LimitReader(f, remaining), 64<<10)
	for {
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
		Info      struct {
			Total json.RawMessage `json:"total_token_usage"`
		} `json:"info"`
	}
	_ = json.Unmarshal(rec.Payload, &p)
	switch rec.Type {
	case "event_msg":
		switch p.Type {
		case "task_started":
			c.SeenTurn, c.TurnComplete = true, false
		case "task_complete":
			c.SeenTurn, c.TurnComplete = true, true
		case "token_count":
			u := p.Info.Total
			c.Tokens = api.TokenTotals{Input: activityInt(u, "input_tokens"), Cached: activityInt(u, "cached_input_tokens"), CacheWrite: activityInt(u, "cache_write_tokens"), Output: activityInt(u, "output_tokens"), Reasoning: activityInt(u, "reasoning_output_tokens"), Total: activityInt(u, "total_tokens")}
			if c.Tokens.Total == 0 {
				c.Tokens.Total = c.Tokens.Input + c.Tokens.Output
			}
		}
	case "response_item":
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
		c.SeenTurn, c.TurnComplete = true, false
	case "task_complete":
		c.SeenTurn, c.TurnComplete = true, true
	case "session_meta", "turn_context":
	default:
		return fmt.Errorf("unknown Codex record type %q", rec.Type)
	}
	return nil
}

func parseClaudeActivity(line []byte, c *activityCursor) error {
	if len(line) == 0 {
		return nil
	}
	var rec activityRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return err
	}
	now := activityTime(rec.Timestamp, time.Now().UTC())
	c.LastEventAt = now
	var msg struct {
		ID         string `json:"id"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"content"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(rec.Message, &msg)
	switch rec.Type {
	case "assistant":
		c.SeenTurn, c.TurnComplete = true, false
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
		if msg.StopReason == "end_turn" {
			c.TurnComplete = true
		}
	case "user":
		for _, part := range msg.Content {
			if part.Type == "tool_result" {
				finishActivityCall(c, part.ToolUseID, now)
			}
		}
	case "result":
		c.SeenTurn, c.TurnComplete = true, true
	case "system", "summary", "progress", "file-history-snapshot":
	default:
		return fmt.Errorf("unknown Claude record type %q", rec.Type)
	}
	return nil
}
