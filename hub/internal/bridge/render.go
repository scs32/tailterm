package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Discord limits: 2,000 characters of content, 4,096 in an embed
// description. The margins leave room for the header and marker lines.
const (
	contentLimit     = 2000
	partBudget       = 1700
	embedBudget      = 3500
	maxButtonsListed = 5
)

// Kinds of outbox rows.
const (
	kindLine       = "line"
	kindEscalation = "escalation"
	kindNotice     = "notice"
)

var kindColors = map[string]int{
	api.EnvelopeKindAssign: 0x5865F2, api.EnvelopeKindRequest: 0x5865F2, api.EnvelopeKindReview: 0x9B59B6,
	api.EnvelopeKindQuestion: 0xF1C40F, api.EnvelopeKindResult: 0x2ECC71, api.EnvelopeKindAnswer: 0x2ECC71,
	api.EnvelopeKindBlock: 0xE67E22, api.EnvelopeKindDecline: 0x95A5A6, api.EnvelopeKindFinding: 0xE74C3C,
	api.EnvelopeKindNotice: 0x95A5A6,
}

// roster names agents for display.
type roster map[string]api.Agent

func (r roster) name(agentID string) string {
	if a, ok := r[agentID]; ok {
		return a.Name
	}
	return agentID
}

func (r roster) sender(m api.Message) string {
	switch {
	case m.From.Node == api.BrokerNode:
		return "broker"
	case m.From.AgentID != "":
		return r.name(m.From.AgentID)
	case m.From.Node == api.BridgeNode:
		return "owner (Discord)"
	}
	return "owner"
}

func (r roster) recipient(m api.Message) string {
	if m.To == "" {
		return "everyone"
	}
	return r.name(m.To)
}

// marker is the last line of every bridge message. It tells the owner which
// board message this is, and lets the bridge recognize its own sends when a
// delivery was uncertain.
func marker(seq int64, part, parts int, what string) string {
	s := fmt.Sprintf("-# #%d", seq)
	if parts > 1 {
		s += fmt.Sprintf(" · part %d/%d", part, parts)
	}
	if what != "" {
		s += " · " + what
	}
	return s
}

// outboxPayload is what a row sends. Nonce and allowed mentions are applied
// at send time, so they can never be missing.
type outboxPayload struct {
	Message     discord.MessageSend `json:"message"`
	Marker      string              `json:"marker"`
	MentionUser []string            `json:"mentionUsers,omitempty"`
}

func payloadJSON(p outboxPayload) string {
	raw, _ := json.Marshal(p)
	return string(raw)
}

// renderMessage turns one board message into zero or more outbox rows.
// Owners' own Discord input and routine bookkeeping are not mirrored.
func (b *Bridge) renderMessage(task api.Task, r roster, m api.Message) []OutboxRow {
	if m.From.Node == api.BridgeNode || m.SystemNotice != nil {
		return nil
	}
	row := func(key, kind string, part int, p outboxPayload) OutboxRow {
		return OutboxRow{Key: fmt.Sprintf("%s:%s:%d:%d", key, m.TaskID, m.Seq, part), TaskID: m.TaskID, Seq: m.Seq, AgentID: m.From.AgentID, Kind: kind, Payload: payloadJSON(p)}
	}
	env := m.Envelope
	if env != nil && env.Kind == api.EnvelopeKindNotice {
		switch env.Refs["escalation"] {
		case "owner", "stall":
			return []OutboxRow{row("esc", kindEscalation, 1, b.renderEscalation(task, m))}
		case "lead":
			// A lead-level escalation mirrors as an ordinary line, without a ping.
		default:
			return nil // bookkeeping: shown on the status card, not as a line
		}
	}
	header := fmt.Sprintf("**%s** → **%s**", clean(r.sender(m)), clean(r.recipient(m)))
	if m.ReplyTo > 0 {
		header += fmt.Sprintf(" · re #%d", m.ReplyTo)
	}
	link := b.tailosLink(m)
	if env != nil {
		header += fmt.Sprintf(" · %s · %s", strings.ToUpper(env.Kind), clean(env.Subject))
		embed := discord.Embed{Description: truncate(typedBody(m.Text), embedBudget), Color: kindColors[env.Kind], URL: link}
		if link != "" {
			embed.Title = "Open in TailOS"
		}
		mk := marker(m.Seq, 1, 1, "")
		return []OutboxRow{row("msg", kindLine, 1, outboxPayload{
			Message: discord.MessageSend{Content: truncate(header, contentLimit-len(mk)-1) + "\n" + mk, Embeds: []discord.Embed{embed}},
			Marker:  mk,
		})}
	}
	parts := chunk(m.Text, partBudget)
	var rows []OutboxRow
	for i, text := range parts {
		mk := marker(m.Seq, i+1, len(parts), "")
		content := text + "\n" + mk
		if i == 0 {
			content = header + "\n" + content
		}
		rows = append(rows, row("msg", kindLine, i+1, outboxPayload{Message: discord.MessageSend{Content: content}, Marker: mk}))
	}
	return rows
}

// renderEscalation pings only the owners and offers the unstall controls.
func (b *Bridge) renderEscalation(task api.Task, m api.Message) outboxPayload {
	env := m.Envelope
	mk := marker(m.Seq, 1, 1, "escalation")
	var mentions []string
	for _, id := range b.ownerList {
		mentions = append(mentions, "<@"+id+">")
	}
	text := truncate(env.Body.Text, 1400)
	content := fmt.Sprintf("%s ⚠️ **%s**\n%s\n%s", strings.Join(mentions, " "), clean(env.Subject), text, mk)
	var buttons []discord.Component
	if oid := env.Refs["obligation"]; oid != "" && env.Refs["escalation"] == "owner" {
		buttons = append(buttons,
			discord.Component{Type: discord.ComponentButton, Style: discord.ButtonPrimary, Label: "Nudge", CustomID: "nudge:" + oid},
			discord.Component{Type: discord.ComponentButton, Style: discord.ButtonSecondary, Label: "Reassign…", CustomID: "reassign:" + oid},
		)
	} else {
		buttons = append(buttons, discord.Component{Type: discord.ComponentButton, Style: discord.ButtonPrimary, Label: "Show stalled work", CustomID: "stalled"})
	}
	if b.cfg.TailOSURL != "" {
		buttons = append(buttons, discord.Component{Type: discord.ComponentButton, Style: discord.ButtonLink, Label: "Open in TailOS", URL: b.tailosLink(m)})
	}
	return outboxPayload{
		Message:     discord.MessageSend{Content: content, Components: []discord.Component{{Type: discord.ComponentActionRow, Components: buttons}}},
		Marker:      mk,
		MentionUser: b.ownerList,
	}
}

func (b *Bridge) tailosLink(m api.Message) string {
	base := strings.TrimRight(b.cfg.TailOSURL, "/")
	if base == "" {
		return ""
	}
	if len(m.WorkItems) > 0 {
		return fmt.Sprintf("%s/#feature-history/%s/%s", base, m.WorkItems[0].ItemTaskID, m.WorkItems[0].ItemID)
	}
	return base + "/"
}

// typedBody drops the first line (it is the header) and the To line.
func typedBody(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 {
		lines = lines[1:]
	}
	out := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(l, "To: ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// clean keeps a header field on one line.
func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// chunk splits text into parts of at most max runes, preferring line
// breaks, and keeps code fences balanced: a fence left open at a split is
// closed there and reopened, with its language, at the start of the next part.
func chunk(text string, max int) []string {
	if text == "" {
		return []string{""}
	}
	var parts []string
	var cur strings.Builder
	curLen := 0
	fence := "" // the opening fence line while inside a code block
	flush := func() {
		s := cur.String()
		if fence != "" {
			s += "\n```"
		}
		parts = append(parts, strings.TrimRight(s, "\n"))
		cur.Reset()
		curLen = 0
		if fence != "" {
			cur.WriteString(fence + "\n")
			curLen = utf8.RuneCountInString(fence) + 1
		}
	}
	for _, line := range strings.SplitAfter(text, "\n") {
		for utf8.RuneCountInString(line) > max-8 {
			// A single overlong line: hard-split it by runes.
			runes := []rune(line)
			room := max - 8 - curLen
			if room <= 0 {
				flush()
				continue
			}
			cur.WriteString(string(runes[:room]))
			line = string(runes[room:])
			flush()
		}
		n := utf8.RuneCountInString(line)
		if curLen+n > max-8 && curLen > 0 {
			flush()
		}
		cur.WriteString(line)
		curLen += n
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") {
			if fence == "" {
				fence = trimmed
			} else {
				fence = ""
			}
		}
	}
	if cur.Len() > 0 {
		fence = ""
		parts = append(parts, strings.TrimRight(cur.String(), "\n"))
	}
	return parts
}

// card is a project's pinned status: roster, open obligations per agent and
// the overdue ones. Its hash ignores time, so it only changes on real change.
func renderCard(task api.Task, agents []api.Agent, obligations []api.Obligation) (discord.MessageSend, string) {
	open := map[string]int{}
	overdue := map[string]int{}
	var late []api.Obligation
	for _, o := range obligations {
		if o.State == api.ObligationClosed {
			continue
		}
		open[o.AgentID]++
		if o.Overdue != "" {
			overdue[o.AgentID]++
			late = append(late, o)
		}
	}
	names := roster{}
	for _, a := range agents {
		names[a.ID] = a
	}
	var lines []string
	for _, a := range agents {
		if a.Status == api.AgentClosed {
			continue
		}
		line := fmt.Sprintf("**%s** · %s", clean(a.Name), a.Status)
		if a.Role != "" {
			line += " · " + a.Role
		}
		if n := open[a.ID]; n > 0 {
			line += fmt.Sprintf(" · %d open", n)
			if d := overdue[a.ID]; d > 0 {
				line += fmt.Sprintf(" (%d overdue)", d)
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "No agents.")
	}
	sort.Slice(late, func(i, j int) bool { return late[i].MessageSeq < late[j].MessageSeq })
	embed := discord.Embed{Title: truncate("Status · "+task.Name, 250), Description: truncate(strings.Join(lines, "\n"), embedBudget), Color: 0x5865F2}
	if task.PauseState != "" && task.PauseState != api.ProjectPauseActive {
		embed.Description = "**Project " + task.PauseState + "**\n" + embed.Description
	}
	if len(late) > 0 {
		var ls []string
		for i, o := range late {
			if i == 8 {
				ls = append(ls, fmt.Sprintf("and %d more", len(late)-i))
				break
			}
			ls = append(ls, fmt.Sprintf("#%d %s → %s (%s)", o.MessageSeq, truncate(clean(o.Subject), 60), clean(names.name(o.AgentID)), o.Overdue))
		}
		embed.Fields = []discord.EmbedField{{Name: fmt.Sprintf("Overdue (%d)", len(late)), Value: truncate(strings.Join(ls, "\n"), 1000)}}
		embed.Color = 0xE67E22
	}
	raw, _ := json.Marshal(embed)
	sum := sha256.Sum256(raw)
	embed.Footer = &discord.EmbedFooter{Text: "Updated " + time.Now().UTC().Format("Jan 2 15:04 UTC")}
	return discord.MessageSend{Content: cardMarker, Embeds: []discord.Embed{embed}}, hex.EncodeToString(sum[:])
}

const cardMarker = "-# Tailterm status card"
