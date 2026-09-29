package main

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

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

// classifyClaudePrompt reads Claude's active prompt area. Below an idle
// input box's top rule (claudePromptArea) nothing is a dialog. With no input
// box, as while a dialog is up, the dialog starts at the rule Claude draws
// above it: the nearest rule above its first numbered option, or the last
// rule on screen. The transcript above that rule is never read.
func classifyClaudePrompt(lines []string) (runtimePromptMatch, bool) {
	area, boxed := claudePromptArea(lines)
	if !boxed {
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
	}
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
