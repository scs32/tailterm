package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// A wake is uncertain from the moment its intent is durable. A crash between
// that write and terminal input cannot safely be distinguished from a crash
// after input, so no later relay pass types another prompt until the exact new
// user record proves delivery. The file is private host state, scoped to a run.
type claudeWakeIntent struct {
	Run         string    `json:"run"`
	Thread      string    `json:"thread"`
	Session     string    `json:"session"`
	PromptSHA   string    `json:"promptSha"`
	Nonce       string    `json:"nonce"`
	Prompt      string    `json:"prompt"`
	Path        string    `json:"path"`
	FileID      uint64    `json:"fileId"`
	Offset      int64     `json:"offset"`
	Pane        string    `json:"pane"`
	Phase       string    `json:"phase"`
	At          time.Time `json:"at"`
	TextAt      time.Time `json:"textAt,omitempty"`
	EnterAt     time.Time `json:"enterAt,omitempty"`
	ConfirmedAt time.Time `json:"confirmedAt,omitempty"`
}

type claudeWakeSnapshot struct {
	Pane      string
	SessionID string
	Created   string
	PanePID   int
	Path      string
	FileID    uint64
	Offset    int64
	Screen    string
	Cursor    activityCursor
}

type claudeWakeOps struct {
	inspect func(context.Context, runtimeBinding, string) (claudeWakeSnapshot, error)
	send    func(context.Context, string, string, bool) error
	sleep   func(time.Duration)
	now     func() time.Time
}

var errClaudeWakeUnsafe = errors.New("Claude is not safely idle")

func claudeWakePath(b runtimeBinding) string {
	return filepath.Join(relayDir(), bindingKey(b)+"-"+b.Run+".claude-wake.json")
}

func claudeWakeHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func claudeWakeNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// A captured pane is accepted only with a single, empty Claude input line.
// Unknown footers, choices, a cursor in an editor, and multiline input fail
// closed. ANSI escapes are not requested from capture-pane.
func emptyClaudeInput(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) == 0 {
		return false
	}
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "allow this") || strings.Contains(lower, "do you want to proceed") || strings.Contains(lower, "esc to cancel") || strings.Contains(lower, "select an option") || strings.Contains(lower, "(y/n)") || strings.Contains(lower, "[y/n]") {
			return false
		}
	}
	input := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			placeholder := strings.TrimPrefix(trimmed, "❯")
			placeholder = strings.TrimSpace(placeholder)
			if (placeholder != "" && !(strings.HasPrefix(placeholder, "Try \"") && strings.HasSuffix(placeholder, "\"") && len(placeholder) <= 100)) || input >= 0 {
				return false
			}
			input = i
		}
	}
	if input < 0 || len(lines)-input > 5 {
		return false
	}
	for _, line := range lines[input+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "? for shortcuts") || strings.HasPrefix(trimmed, "⏵⏵") || strings.HasPrefix(trimmed, "──") {
			continue
		}
		return false
	}
	return true
}

func claudeBlockedScreen(screen string) bool {
	lower := strings.ToLower(screen)
	return strings.Contains(lower, "allow this") || strings.Contains(lower, "do you want to proceed") ||
		strings.Contains(lower, "esc to cancel") || strings.Contains(lower, "select an option") ||
		strings.Contains(lower, "(y/n)") || strings.Contains(lower, "[y/n]")
}

func exactClaudeInput(screen, expected string) bool {
	if expected == "" {
		return emptyClaudeInput(screen)
	}
	if claudeBlockedScreen(screen) {
		return false
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	input := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			input = i // the final prompt is the active input, not scrollback
		}
	}
	if input < 0 || len(lines)-input > 10 {
		return false
	}
	parts := []string{strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[input]), "❯"))}
	border := false
	for _, line := range lines[input+1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "──") {
			border = true
			continue
		}
		if border && (trimmed == "" || strings.HasPrefix(trimmed, "⏵⏵") || strings.HasPrefix(trimmed, "? for shortcuts")) {
			continue
		}
		if !border && trimmed != "" && strings.HasPrefix(line, "  ") && !strings.HasPrefix(trimmed, "⏵⏵") {
			parts = append(parts, trimmed) // Claude wraps editor text at words.
			continue
		}
		if trimmed != "" {
			return false
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ") == strings.Join(strings.Fields(expected), " ")
}

func claudeTranscriptSnapshot(b runtimeBinding) (claudeWakeSnapshot, error) {
	path, err := activityTranscript(b)
	if err != nil || path == "" {
		return claudeWakeSnapshot{}, errors.New("Claude transcript unavailable")
	}
	var cursor activityCursor
	strictUnknown := false
	unknownReason := ""
	for i := 0; i < 128; i++ {
		if err := readActivityAppend(path, &cursor, func(line []byte, cursor *activityCursor) error {
			err := parseClaudeActivity(line, cursor)
			if err != nil {
				strictUnknown = true
				if unknownReason == "" {
					unknownReason = err.Error()
				}
			} else if cursor.TurnComplete && claudeCompletedRecord(line) {
				// A later fully completed turn supersedes an older malformed
				// record. Unknown data after that boundary still blocks input.
				strictUnknown, unknownReason = false, ""
			}
			return err
		}); err != nil {
			return claudeWakeSnapshot{}, fmt.Errorf("Claude transcript unavailable: %w", err)
		}
		if cursor.Ready {
			break
		}
	}
	if !cursor.Ready || cursor.Unknown || strictUnknown || !cursor.SeenTurn || !cursor.TurnComplete || len(cursor.Pending) != 0 {
		return claudeWakeSnapshot{}, fmt.Errorf("Claude transcript busy, incomplete, or unknown: %s", unknownReason)
	}
	return claudeWakeSnapshot{Path: path, FileID: cursor.FileID, Offset: cursor.Offset, Cursor: cursor}, nil
}

func claudeCompletedRecord(line []byte) bool {
	var rec struct {
		Type    string `json:"type"`
		Message struct {
			StopReason string `json:"stop_reason"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return false
	}
	return rec.Type == "result" || rec.Type == "assistant" && rec.Message.StopReason == "end_turn"
}

func nativeClaudeInspect(ctx context.Context, b runtimeBinding, expected string) (claudeWakeSnapshot, error) {
	// Discovery requires one pane, the exact tmux run environment and one live
	// Claude process descended from that pane. It also checks creation identity.
	receipt, err := nativeRuntimeDiscovery(ctx, b, api.Agent{Runtime: "claude"})
	if err != nil {
		return claudeWakeSnapshot{}, fmt.Errorf("Claude pane identity unavailable: %w", err)
	}
	fields := []string{"session_id", "session_created", "session_name", "TAILTERM_HUB", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "pane_id", "pane_pid", "pane_in_mode", "cursor_x", "cursor_y"}
	for i, field := range fields {
		fields[i] = `"#{q/e:` + field + `}"`
	}
	raw, err := startupTmux(ctx, "list-panes", "-s", "-t", b.Session, "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		return claudeWakeSnapshot{}, fmt.Errorf("Claude pane unavailable: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row []string
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &row) != nil || len(row) != len(fields) ||
		row[0] != receipt.SessionID || row[1] != receipt.SessionCreated || row[2] != b.Session ||
		row[3] != b.Hub || row[4] != b.Task || row[5] != b.Agent || row[6] != b.Run ||
		row[8] != fmt.Sprint(receipt.PanePID) || row[9] != "0" || row[7] == "" {
		return claudeWakeSnapshot{}, errors.New("Claude pane identity changed or pane is in a mode")
	}
	cursorX, xerr := strconv.Atoi(row[10])
	cursorY, yerr := strconv.Atoi(row[11])
	if xerr != nil || yerr != nil || cursorY < 0 || cursorX < 0 {
		return claudeWakeSnapshot{}, errors.New("Claude cursor identity unavailable")
	}
	args := []string{"capture-pane", "-p", "-t", row[7]}
	if expected != "" {
		args = append(args, "-J")
	}
	screen, err := startupTmux(ctx, args...)
	if err != nil {
		return claudeWakeSnapshot{}, fmt.Errorf("Claude pane capture unavailable: %w", err)
	}
	visible := string(screen)
	if claudeBlockedScreen(visible) {
		return claudeWakeSnapshot{}, errors.New("Claude pane has a permission or selection prompt")
	}
	if expected == "" {
		lines := strings.Split(visible, "\n")
		if cursorX != 2 || cursorY >= len(lines) {
			return claudeWakeSnapshot{}, errors.New("Claude cursor is not at an empty input")
		}
		visible = strings.Join(lines[cursorY:], "\n")
	}
	if !exactClaudeInput(visible, expected) {
		return claudeWakeSnapshot{}, errors.New("Claude input is occupied, prompting, or unknown")
	}
	transcript, err := claudeTranscriptSnapshot(b)
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	transcript.Pane, transcript.SessionID, transcript.Created, transcript.PanePID, transcript.Screen = row[7], row[0], row[1], receipt.PanePID, visible
	return transcript, nil
}

func nativeClaudeSend(ctx context.Context, pane, value string, literal bool) error {
	args := []string{"send-keys", "-t", pane}
	if literal {
		args = append(args, "-l", "--", value)
	} else {
		args = append(args, "Enter")
	}
	_, err := startupTmux(ctx, args...)
	return err
}

func claudeUserText(line []byte) string {
	var record struct {
		Type             string `json:"type"`
		IsMeta           bool   `json:"isMeta"`
		IsCompactSummary bool   `json:"isCompactSummary"`
		Message          struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &record) != nil || record.Type != "user" || record.IsMeta || record.IsCompactSummary {
		return ""
	}
	var direct string
	if json.Unmarshal(record.Message.Content, &direct) == nil {
		for _, prefix := range []string{"<command-name>", "<local-command-stdout>", "<local-command-caveat>"} {
			if strings.HasPrefix(strings.TrimSpace(direct), prefix) {
				return ""
			}
		}
		return direct
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(record.Message.Content, &parts) != nil {
		return ""
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Type != "text" {
			return ""
		}
		text.WriteString(part.Text)
	}
	return text.String()
}

func claudeWakeConfirmed(intent claudeWakeIntent) (bool, error) {
	f, err := os.Open(intent.Path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || fileIdentity(info) != intent.FileID || info.Size() < intent.Offset {
		return false, errors.New("Claude transcript identity changed")
	}
	if info.Size()-intent.Offset > 4<<20 {
		return false, errors.New("Claude confirmation scan budget exceeded")
	}
	if _, err := f.Seek(intent.Offset, io.SeekStart); err != nil {
		return false, err
	}
	reader := bufio.NewReader(io.LimitReader(f, 4<<20))
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			return false, nil // a partial tail cannot confirm a submitted turn
		}
		if err != nil {
			return false, err
		}
		if len(line) > maxActivityLine {
			return false, errors.New("Claude confirmation record too large")
		}
		if claudeUserText(line) == intent.Prompt {
			return true, nil
		}
	}
}

func sameClaudeSnapshot(a, b claudeWakeSnapshot) bool {
	return a.Pane == b.Pane && a.SessionID == b.SessionID && a.Created == b.Created && a.PanePID == b.PanePID &&
		a.Path == b.Path && a.FileID == b.FileID && a.Offset == b.Offset
}

func claudeWakeWith(ctx context.Context, b runtimeBinding, prompt string, ops claudeWakeOps) error {
	if b.Runtime != "claude" || !validBinding(b) || prompt == "" || strings.ContainsAny(prompt, "\r\n") {
		return errors.New("invalid Claude wake binding or prompt")
	}
	path := claudeWakePath(b)
	var previous claudeWakeIntent
	if data, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(data, &previous) != nil {
			return errors.New("Claude wake guard unreadable; did not confirm")
		}
		if previous.Run != b.Run || previous.Thread != b.Thread || previous.Session != b.Session {
			return errors.New("Claude wake guard identity mismatch; did not confirm")
		}
		if previous.Phase != "confirmed" {
			if confirmed, _ := claudeWakeConfirmed(previous); !confirmed {
				return errors.New("prior Claude wake did not confirm; no resend")
			}
			previous.Phase = "confirmed"
			if err := writePrivateJSON(path, previous); err != nil {
				return err
			}
		}
		if previous.PromptSHA == claudeWakeHash(prompt) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	first, err := ops.inspect(ctx, b, "")
	if err != nil {
		return fmt.Errorf("%w: %v", errClaudeWakeUnsafe, err)
	}
	if !emptyClaudeInput(first.Screen) || !first.Cursor.TurnComplete || !first.Cursor.Ready || first.Cursor.Unknown || len(first.Cursor.Pending) != 0 {
		return errClaudeWakeUnsafe
	}
	second, err := ops.inspect(ctx, b, "")
	if err != nil {
		return fmt.Errorf("%w: Claude input recheck: %v", errClaudeWakeUnsafe, err)
	}
	if !sameClaudeSnapshot(first, second) {
		return fmt.Errorf("%w: Claude identity or input changed before wake", errClaudeWakeUnsafe)
	}
	nonce, err := claudeWakeNonce()
	if err != nil {
		return fmt.Errorf("Claude wake nonce unavailable: %w", err)
	}
	intent := claudeWakeIntent{Run: b.Run, Thread: b.Thread, Session: b.Session, PromptSHA: claudeWakeHash(prompt), Nonce: nonce, Prompt: prompt, Path: first.Path, FileID: first.FileID, Offset: first.Offset, Pane: first.Pane, Phase: "uncertain", At: ops.now().UTC()}
	if err := writePrivateJSON(path, intent); err != nil {
		return err
	}
	if err := ops.send(ctx, first.Pane, prompt, true); err != nil {
		return fmt.Errorf("Claude literal text failed; did not confirm: %w", err)
	}
	intent.TextAt = ops.now().UTC()
	if err := writePrivateJSON(path, intent); err != nil {
		return fmt.Errorf("Claude text receipt save failed; did not confirm: %w", err)
	}
	ops.sleep(100 * time.Millisecond)
	third, err := ops.inspect(ctx, b, prompt)
	if err != nil {
		return fmt.Errorf("Claude input verification before Enter failed; did not confirm: %w", err)
	}
	if !sameClaudeSnapshot(first, third) {
		return errors.New("Claude identity or transcript changed before Enter; did not confirm")
	}
	if err := ops.send(ctx, first.Pane, "", false); err != nil {
		return fmt.Errorf("Claude Enter failed; did not confirm: %w", err)
	}
	intent.EnterAt = ops.now().UTC()
	if err := writePrivateJSON(path, intent); err != nil {
		return fmt.Errorf("Claude Enter receipt save failed; did not confirm: %w", err)
	}
	deadline := ops.now().Add(5 * time.Second)
	for {
		if confirmed, err := claudeWakeConfirmed(intent); confirmed {
			intent.Phase = "confirmed"
			intent.ConfirmedAt = ops.now().UTC()
			if err := writePrivateJSON(path, intent); err != nil {
				return fmt.Errorf("Claude confirmation save failed; did not confirm: %w", err)
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("Claude confirmation unavailable; did not confirm: %w", err)
		}
		if !ops.now().Before(deadline) || ctx.Err() != nil {
			return errors.New("Claude new user turn did not confirm within five seconds")
		}
		ops.sleep(50 * time.Millisecond)
	}
}

func claudeQueue(ctx context.Context, b runtimeBinding, prompt string) error {
	return claudeWakeWith(ctx, b, prompt, claudeWakeOps{inspect: nativeClaudeInspect, send: nativeClaudeSend, sleep: time.Sleep, now: time.Now})
}
