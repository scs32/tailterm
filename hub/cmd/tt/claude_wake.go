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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	// UnknownTypes lists transcript record types the idle check ignored.
	UnknownTypes []string
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

// claudeDialogPhrases mark a permission, selection or confirmation prompt.
var claudeDialogPhrases = []string{"allow this", "do you want to proceed", "esc to cancel", "select an option", "(y/n)", "[y/n]"}

// claudePromptArea returns the rows of a plain Claude capture that belong to
// the active prompt, and whether an input box bounds them. The area runs from
// the input box's top rule, the rule directly above the last ❯ row, to the end
// of the capture; the transcript above it (answers that quote a dialog, for
// example) is not part of it. With no ❯ row, or no rule directly above it (a
// dialog's "❯ 1. Yes" row, an unknown layout), the whole capture is the area,
// so anything the rule cannot place is still checked everywhere.
func claudePromptArea(lines []string) ([]string, bool) {
	input := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
			input = i
		}
	}
	if input >= 1 && claudeRuleLine(lines[input-1]) {
		return lines[input-1:], true
	}
	return lines, false
}

func claudeRuleLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && strings.Trim(trimmed, "─") == ""
}

// claudeDialog names the lowest dialog phrase in the active prompt area, the
// one nearest a live dialog footer, and its 1-based row there. The text is
// stable for an unchanged screen, so the relay logs a steady refusal once.
func claudeDialog(screen string) (string, bool) {
	area, boxed := claudePromptArea(strings.Split(strings.TrimRight(screen, "\n"), "\n"))
	for i := len(area) - 1; i >= 0; i-- {
		lower := strings.ToLower(area[i])
		for _, phrase := range claudeDialogPhrases {
			if strings.Contains(lower, phrase) {
				where := fmt.Sprintf("prompt area row %d", i+1)
				if !boxed {
					where = fmt.Sprintf("capture row %d (no input box, whole capture checked)", i+1)
				}
				return fmt.Sprintf("%q in %s", phrase, where), true
			}
		}
	}
	return "", false
}

// A captured pane is accepted only with a single, empty Claude input line in
// the active prompt area. Unknown footers, choices, a cursor in an editor, and
// multiline input fail closed. ANSI escapes are not requested from capture-pane.
func emptyClaudeInput(screen string) bool {
	if claudeBlockedScreen(screen) {
		return false
	}
	lines, _ := claudePromptArea(strings.Split(strings.TrimRight(screen, "\n"), "\n"))
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

// claudeBlockedScreen reports a dialog phrase in the active prompt area.
func claudeBlockedScreen(screen string) bool {
	_, blocked := claudeDialog(screen)
	return blocked
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

// claudeQueueStaleAfter bounds how long queued input can hold a wake after a
// completed turn. Claude Code dequeues queued input within about 50 ms of a
// turn ending, so an older unmatched enqueue is drift (an unrecorded removal,
// for example) and the pane check decides instead.
const claudeQueueStaleAfter = 30 * time.Second

// claudeQueueFresh reports input Claude Code queued recently enough that a new
// turn is about to start from it.
func claudeQueueFresh(c activityCursor, now time.Time) bool {
	return c.ClaudeQueued > 0 && now.Sub(c.ClaudeQueuedAt) < claudeQueueStaleAfter
}

// claudeRecordLog remembers which unrecognized transcript record types this
// relay process has already logged, so each is reported once.
var claudeRecordLog struct {
	sync.Mutex
	seen map[string]bool
}

const claudeRecordLogCap = 64

// logClaudeRecordOnce writes one relay log line per key per process. The set
// is capped so a transcript full of novel types cannot grow it without bound.
func logClaudeRecordOnce(key, format string, args ...any) {
	claudeRecordLog.Lock()
	if claudeRecordLog.seen == nil {
		claudeRecordLog.seen = map[string]bool{}
	}
	if claudeRecordLog.seen[key] || len(claudeRecordLog.seen) >= claudeRecordLogCap {
		claudeRecordLog.Unlock()
		return
	}
	claudeRecordLog.seen[key] = true
	claudeRecordLog.Unlock()
	fmt.Fprintf(os.Stderr, format, args...)
}

// claudeRecordSample describes a transcript record without its content:
// the sorted top-level key names plus the short scalar values of type,
// subtype, operation and reason. Transcripts carry prompts and tool output,
// so message and content strings are never included.
func claudeRecordSample(line []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return "unparsable"
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		// Key names are schema, but keep an odd one from splitting the line.
		key = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-.", r) {
				return r
			}
			return '?'
		}, key)
		keys = append(keys, claudeClip(key, 64))
	}
	slices.Sort(keys)
	sample := "keys=[" + strings.Join(keys, " ") + "]"
	for _, key := range []string{"type", "subtype", "operation", "reason"} {
		var value string
		if raw, ok := fields[key]; ok && json.Unmarshal(raw, &value) == nil && len(value) <= 64 {
			sample += fmt.Sprintf(" %s=%q", key, value)
		}
	}
	return claudeClip(sample, 200)
}

// claudeClip shortens value to at most limit bytes on a rune boundary.
func claudeClip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

// claudeTranscriptSnapshot reads the whole transcript and decides idleness
// from turn state: a completed turn, no pending tool call and no freshly
// queued input. A record type the parser does not know is logged once and
// otherwise ignored, so the pane check decides; other parse errors still
// refuse unless a later completed turn supersedes them.
func claudeTranscriptSnapshot(b runtimeBinding, now time.Time) (claudeWakeSnapshot, error) {
	path, err := activityTranscript(b)
	if err != nil || path == "" {
		return claudeWakeSnapshot{}, errors.New("Claude transcript unavailable")
	}
	var cursor activityCursor
	strictUnknown := false
	unknownReason := ""
	var unknownTypes []string
	for i := 0; i < 128; i++ {
		if err := readActivityAppend(path, &cursor, func(line []byte, cursor *activityCursor) error {
			err := parseClaudeActivity(line, cursor)
			var unknown unknownClaudeRecordError
			if errors.As(err, &unknown) {
				kind := claudeClip(unknown.Type, 64)
				if !slices.Contains(unknownTypes, kind) {
					unknownTypes = append(unknownTypes, kind)
					logClaudeRecordOnce("type/"+kind, "[tt relay] %s %s Claude transcript record type %q not recognized; idle from turn state and pane check; sample=%s\n", now.UTC().Format(time.RFC3339), b.Agent, kind, claudeRecordSample(line))
				}
				return nil
			}
			if err != nil {
				strictUnknown = true
				if unknownReason == "" {
					unknownReason = err.Error()
				}
			} else if cursor.TurnComplete && claudeCompletedRecord(line) {
				// A later fully completed turn supersedes an older malformed
				// record. Malformed data after that boundary still blocks input.
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
	reason := ""
	switch {
	case !cursor.Ready:
		reason = "transcript incomplete"
	case !cursor.SeenTurn:
		reason = "no completed turn yet"
	case !cursor.TurnComplete:
		reason = "turn in progress"
	case len(cursor.Pending) != 0:
		ids := slices.Sorted(maps.Keys(cursor.Pending))
		reason = "tool call pending: " + cursor.Pending[ids[0]].Name
	case claudeQueueFresh(cursor, now):
		reason = fmt.Sprintf("queued input pending (%d)", cursor.ClaudeQueued)
	case strictUnknown:
		reason = unknownReason
	case cursor.Unknown:
		reason = "transcript format unknown"
	}
	if reason != "" {
		return claudeWakeSnapshot{}, fmt.Errorf("Claude transcript busy, incomplete, or unknown: %s", reason)
	}
	if cursor.ClaudeQueued > 0 {
		logClaudeRecordOnce("queue-operation/stale", "[tt relay] %s %s Claude transcript has %d queued input older than %s after a completed turn; idle from turn state and pane check\n", now.UTC().Format(time.RFC3339), b.Agent, cursor.ClaudeQueued, claudeQueueStaleAfter)
	}
	return claudeWakeSnapshot{Path: path, FileID: cursor.FileID, Offset: cursor.Offset, Cursor: cursor, UnknownTypes: unknownTypes}, nil
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

// runtimePane is one exact-identity capture of an owned runtime pane.
type runtimePane struct {
	Pane      string
	SessionID string
	Created   string
	PanePID   int
	CursorX   int
	CursorY   int
	Raw       string
}

// inspectRuntimePane captures the one pane of a binding's tmux session after
// proving its identity. Discovery requires one pane, the exact tmux run
// environment and one live runtime process descended from that pane. It also
// checks creation identity and that the pane is not in a copy or view mode.
// name prefixes errors ("Claude", "Codex"). -e keeps text attributes; joined
// adds -J, which unwraps wrapped lines.
func inspectRuntimePane(ctx context.Context, b runtimeBinding, name string, joined bool) (runtimePane, error) {
	runtime := b.Runtime
	if runtime == "" {
		runtime = "codex"
	}
	receipt, err := nativeRuntimeDiscovery(ctx, b, api.Agent{Runtime: runtime})
	if err != nil {
		return runtimePane{}, fmt.Errorf("%s pane identity unavailable: %w", name, err)
	}
	fields := []string{"session_id", "session_created", "session_name", "TAILTERM_HUB", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "pane_id", "pane_pid", "pane_in_mode", "cursor_x", "cursor_y"}
	for i, field := range fields {
		fields[i] = `"#{q/e:` + field + `}"`
	}
	raw, err := startupTmux(ctx, "list-panes", "-s", "-t", b.Session, "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		return runtimePane{}, fmt.Errorf("%s pane unavailable: %w", name, err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var row []string
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &row) != nil || len(row) != len(fields) ||
		row[0] != receipt.SessionID || row[1] != receipt.SessionCreated || row[2] != b.Session ||
		row[3] != b.Hub || row[4] != b.Task || row[5] != b.Agent || row[6] != b.Run ||
		row[8] != fmt.Sprint(receipt.PanePID) || row[9] != "0" || row[7] == "" {
		return runtimePane{}, fmt.Errorf("%s pane identity changed or pane is in a mode", name)
	}
	cursorX, xerr := strconv.Atoi(row[10])
	cursorY, yerr := strconv.Atoi(row[11])
	if xerr != nil || yerr != nil || cursorY < 0 || cursorX < 0 {
		return runtimePane{}, fmt.Errorf("%s cursor identity unavailable", name)
	}
	args := []string{"capture-pane", "-p", "-e", "-t", row[7]}
	if joined {
		args = append(args, "-J")
	}
	screen, err := startupTmux(ctx, args...)
	if err != nil {
		return runtimePane{}, fmt.Errorf("%s pane capture unavailable: %w", name, err)
	}
	return runtimePane{Pane: row[7], SessionID: row[0], Created: row[1], PanePID: receipt.PanePID, CursorX: cursorX, CursorY: cursorY, Raw: string(screen)}, nil
}

func nativeClaudeInspect(ctx context.Context, b runtimeBinding, expected string) (claudeWakeSnapshot, error) {
	// -e keeps text attributes, so Claude's faint prompt suggestion can be told
	// apart from typed input that reads the same.
	pane, err := inspectRuntimePane(ctx, b, "Claude", expected != "")
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	visible, err := claudeInputScreen(pane.Raw, pane.CursorX, pane.CursorY, expected)
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	transcript, err := claudeTranscriptSnapshot(b, time.Now())
	if err != nil {
		return claudeWakeSnapshot{}, err
	}
	transcript.Pane, transcript.SessionID, transcript.Created, transcript.PanePID, transcript.Screen = pane.Pane, pane.SessionID, pane.Created, pane.PanePID, visible
	return transcript, nil
}

// claudeInputScreen turns one `capture-pane -p -e` capture into the plain
// screen the input checks read. With no expected text the cursor must be at
// the start of an empty input, and only the screen from the cursor row down
// is returned.
func claudeInputScreen(raw string, cursorX, cursorY int, expected string) (string, error) {
	full, visible, err := claudePlainScreen(raw)
	if err != nil {
		return "", err
	}
	// Dialogs are detected on everything drawn in the active prompt area,
	// faint text included; the transcript above the input box is not a prompt.
	if match, blocked := claudeDialog(full); blocked {
		return "", errors.New("Claude pane has a permission or selection prompt: " + match)
	}
	if expected == "" {
		lines := strings.Split(visible, "\n")
		if cursorX != 2 || cursorY >= len(lines) {
			return "", errors.New("Claude cursor is not at an empty input")
		}
		visible = strings.Join(lines[cursorY:], "\n")
	}
	if !exactClaudeInput(visible, expected) {
		return "", errors.New("Claude input is occupied, prompting, or unknown")
	}
	return visible, nil
}

type claudeCell struct {
	r     rune
	faint bool
}

// claudePlainScreen strips SGR attributes from a capture. It returns the full
// text and a copy without the faint text Claude Code draws after the input
// marker: its prompt suggestion and its Try "…" placeholder, neither of
// which is input (Claude Code 2.1.284 draws both with SGR 2 and removes the suggestion as soon as a key is typed; see
// testdata/claude-pane). Faint text must be a trailing run: faint text followed
// by normal text, or any escape other than SGR in the input area, fails closed.
// Attributes carry across rows, as tmux emits only changes.
func claudePlainScreen(raw string) (string, string, error) {
	var rows [][]claudeCell
	var bad []bool
	var row []claudeCell
	rowBad, faint := false, false
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case c == '\n':
			rows, bad = append(rows, row), append(bad, rowBad)
			row, rowBad = nil, false
			i++
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == '[':
			j := i + 2
			for j < len(raw) && (raw[j] < 0x40 || raw[j] > 0x7e) {
				j++
			}
			if j >= len(raw) {
				return "", "", errors.New("Claude pane capture has a truncated escape")
			}
			if raw[j] != 'm' || !applyClaudeSGR(raw[i+2:j], &faint) {
				rowBad = true
			}
			i = j + 1
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == ']':
			// OSC (for example a hyperlink) ends at BEL or ESC \.
			j := i + 2
			for j < len(raw) && raw[j] != 0x07 && !(raw[j] == 0x1b && j+1 < len(raw) && raw[j+1] == '\\') {
				j++
			}
			if j >= len(raw) {
				return "", "", errors.New("Claude pane capture has a truncated escape")
			}
			if raw[j] == 0x1b {
				j++
			}
			rowBad, i = true, j+1
		case c == 0x1b:
			rowBad, i = true, i+2
		default:
			r, size := utf8.DecodeRuneInString(raw[i:])
			row = append(row, claudeCell{r: r, faint: faint})
			i += size
		}
	}
	if len(row) > 0 {
		rows, bad = append(rows, row), append(bad, rowBad)
	}
	input := -1
	for i, cells := range rows {
		if strings.HasPrefix(strings.TrimSpace(claudeCellText(cells)), "❯") {
			input = i
		}
	}
	lines := make([]string, len(rows))
	for i, cells := range rows {
		lines[i] = claudeCellText(cells)
	}
	full := strings.Join(lines, "\n")
	if input >= 0 {
		// The input area runs from the marker row to Claude's lower border.
		end := input + 1
		for end < len(rows) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "──") {
			end++
		}
		seenFaint := false
		for i := input; i < end; i++ {
			if bad[i] {
				return "", "", errors.New("Claude input has unknown terminal attributes")
			}
			start := 0
			if i == input {
				marker := strings.IndexRune(lines[i], '❯')
				start = utf8.RuneCountInString(lines[i][:marker]) + 1
			}
			cut := -1
			if seenFaint {
				cut = start
			}
			for k := start; k < len(rows[i]); k++ {
				cell := rows[i][k]
				if unicode.IsSpace(cell.r) {
					continue
				}
				if cell.faint && !seenFaint {
					seenFaint, cut = true, k
				} else if !cell.faint && seenFaint {
					return "", "", errors.New("Claude input mixes faint and typed text")
				}
			}
			if cut >= 0 {
				lines[i] = claudeCellText(rows[i][:cut])
			}
		}
	}
	return full, strings.Join(lines, "\n"), nil
}

func claudeCellText(cells []claudeCell) string {
	var text strings.Builder
	for _, cell := range cells {
		text.WriteRune(cell.r)
	}
	return text.String()
}

// applyClaudeSGR tracks only faint (SGR 2) and reports false for parameters
// it cannot account for, including colon subparameters.
func applyClaudeSGR(params string, faint *bool) bool {
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if fields[i] == "" {
			n, err = 0, nil
		}
		if err != nil {
			return false
		}
		switch {
		case n == 0 || n == 22:
			*faint = false
		case n == 2:
			*faint = true
		case n == 38 || n == 48 || n == 58:
			if i+1 < len(fields) && fields[i+1] == "5" {
				i += 2
			} else if i+1 < len(fields) && fields[i+1] == "2" {
				i += 4
			} else {
				return false
			}
			if i >= len(fields) {
				return false
			}
		}
	}
	return true
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
	if !emptyClaudeInput(first.Screen) || !first.Cursor.TurnComplete || !first.Cursor.Ready || first.Cursor.Unknown || len(first.Cursor.Pending) != 0 || claudeQueueFresh(first.Cursor, ops.now()) {
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
