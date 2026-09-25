package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// reply is what an interaction answers with.
type reply struct {
	Content    string
	Embeds     []discord.Embed
	Components []discord.Component
	Public     bool // visible to the channel, not only the owner
}

func ephemeral(format string, args ...any) reply {
	return reply{Content: fmt.Sprintf(format, args...)}
}

// handleInteraction answers a slash command, button or menu. Every action is
// checked against the configured guild, the owner allowlist and the channel's
// project on each use; button visibility is not authority.
func (b *Bridge) handleInteraction(ctx context.Context, in discord.Interaction) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if in.GuildID != b.cfg.GuildID || !b.owners[in.UserID()] {
		b.respondNow(ctx, in, ephemeral("⛔ Only the project owner can use Tailterm controls here."))
		return
	}
	m, err := b.cfg.State.MappingByChannel(ctx, in.ChannelID)
	if err != nil {
		b.respondNow(ctx, in, ephemeral("This channel isn't linked to a Tailterm project."))
		return
	}
	// The claim keeps the whole interaction, so a crash before it finishes
	// is resumed by the retry loop while Discord still accepts the reply.
	raw, _ := json.Marshal(in)
	claimed, err := b.cfg.State.ClaimInbound(ctx, "interaction:"+in.ID, "interaction", m.TaskID, string(raw), time.Now().Add(3*time.Minute))
	if err != nil || !claimed {
		return // a redelivery of an interaction already taken on
	}
	b.runInteraction(ctx, m, in, false)
}

func (b *Bridge) respondNow(ctx context.Context, in discord.Interaction, r reply) {
	data := &discord.InteractionResponseData{Content: r.Content, Embeds: r.Embeds, Components: r.Components, AllowedMentions: &discord.AllowedMentions{Parse: []string{}}}
	if !r.Public {
		data.Flags = discord.FlagEphemeral
	}
	if err := b.cfg.Discord.RespondInteraction(ctx, in.ID, in.Token, discord.InteractionResponse{Type: discord.ResponseMessage, Data: data}); err != nil {
		b.logf("discord bridge: answer interaction %s: %v", in.ID, err)
	}
}

// runInteraction defers, acts and edits the deferred reply. A resumed run
// tolerates the defer having already happened before the crash.
func (b *Bridge) runInteraction(ctx context.Context, m Mapping, in discord.Interaction, resumed bool) {
	source := "interaction:" + in.ID
	if m.Archived {
		if !resumed {
			b.respondNow(ctx, in, ephemeral("⛔ This project is closed. Nothing was changed."))
		}
		_ = b.cfg.State.FinishInbound(ctx, source, "rejected: project closed")
		return
	}
	flags := discord.FlagEphemeral
	if in.Type == discord.InteractionCommand && in.Data.Name == "say" {
		flags = 0
	}
	// Defer first: hub calls can take longer than Discord's 3-second limit.
	if err := b.cfg.Discord.RespondInteraction(ctx, in.ID, in.Token, discord.InteractionResponse{Type: discord.ResponseDeferredMessage, Data: &discord.InteractionResponseData{Flags: flags}}); err != nil && !resumed {
		b.logf("discord bridge: defer interaction %s: %v", in.ID, err)
		_ = b.cfg.State.FinishInbound(ctx, source, "not answered: "+err.Error())
		return
	}
	r := b.act(ctx, m.TaskID, in)
	edit := discord.MessageEdit{Content: &r.Content, Embeds: &r.Embeds, Components: &r.Components, AllowedMentions: &discord.AllowedMentions{Parse: []string{}}}
	if r.Embeds == nil {
		empty := []discord.Embed{}
		edit.Embeds = &empty
	}
	if r.Components == nil {
		empty := []discord.Component{}
		edit.Components = &empty
	}
	if err := b.cfg.Discord.EditInteractionResponse(ctx, b.cfg.AppID, in.Token, edit); err != nil {
		b.logf("discord bridge: reply to interaction %s: %v", in.ID, err)
	}
	_ = b.cfg.State.FinishInbound(ctx, source, truncate(r.Content, 200))
}

func (b *Bridge) act(ctx context.Context, taskID string, in discord.Interaction) reply {
	option := func(name string) string {
		for _, o := range in.Data.Options {
			if o.Name == name {
				return strings.TrimSpace(o.String())
			}
		}
		return ""
	}
	if in.Type == discord.InteractionCommand {
		switch in.Data.Name {
		case "status":
			return b.statusReply(ctx, taskID)
		case "stalled":
			return b.stalledReply(ctx, taskID)
		case "nudge":
			return b.nudgeTarget(ctx, taskID, option("target"), in.ID)
		case "say":
			return b.say(ctx, taskID, in, option("text"), option("agent"))
		case "reassign":
			return b.reassignCommand(ctx, taskID, option("obligation"), option("agent"))
		case "extend":
			return b.extend(ctx, taskID, option("obligation"), option("for"), option("reason"), in.ID)
		case "answer":
			return b.answer(ctx, taskID, option("obligation"), option("text"), in.ID)
		case "cancel":
			return b.cancel(ctx, taskID, option("obligation"), option("reason"), in.ID)
		case "resume":
			return b.resume(ctx, taskID, option("agent"), in.ID)
		}
		return ephemeral("Unknown command.")
	}
	id := in.Data.CustomID
	switch {
	case id == "stalled":
		return b.stalledReply(ctx, taskID)
	case strings.HasPrefix(id, "nudge:"):
		return b.nudge(ctx, taskID, strings.TrimPrefix(id, "nudge:"), in.ID)
	case strings.HasPrefix(id, "extend30:"):
		return b.extend(ctx, taskID, strings.TrimPrefix(id, "extend30:"), "30m", "extended from Discord", in.ID)
	case strings.HasPrefix(id, "reassign:"):
		return b.reassignMenu(ctx, taskID, strings.TrimPrefix(id, "reassign:"))
	case strings.HasPrefix(id, "reassignto:") && len(in.Data.Values) == 1:
		return b.reassign(ctx, taskID, strings.TrimPrefix(id, "reassignto:"), in.Data.Values[0])
	}
	return ephemeral("That control is no longer supported.")
}

func hubMessage(err error) string {
	var h *api.HTTPError
	if errors.As(err, &h) {
		return h.Msg
	}
	return "Tailterm is unreachable right now; try again shortly"
}

func (b *Bridge) statusReply(ctx context.Context, taskID string) reply {
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	obligations, err := b.cardObligations(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	card, _ := renderCard(detail.Task, detail.Agents, obligations)
	return reply{Embeds: card.Embeds}
}

// stalledReply lists every overdue obligation, oldest first, with controls
// for the first few.
func (b *Bridge) stalledReply(ctx context.Context, taskID string) reply {
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	overdue, err := b.cfg.Hub.ListObligations(ctx, taskID, "", "", true, true)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	if len(overdue) == 0 {
		return ephemeral("✅ Nothing is overdue in this project.")
	}
	sort.Slice(overdue, func(i, j int) bool { return overdue[i].CreatedAt.Before(overdue[j].CreatedAt) })
	r := roster{}
	for _, a := range detail.Agents {
		r[a.ID] = a
	}
	var lines []string
	var rows []discord.Component
	for i, o := range overdue {
		lines = append(lines, fmt.Sprintf("**#%d** %s → **%s** · %s overdue · %s old", o.MessageSeq, truncate(clean(o.Subject), 70), clean(r.name(o.AgentID)), o.Overdue, time.Since(o.CreatedAt).Round(time.Minute)))
		if i < maxButtonsListed {
			rows = append(rows, discord.Component{Type: discord.ComponentActionRow, Components: []discord.Component{
				{Type: discord.ComponentButton, Style: discord.ButtonPrimary, Label: fmt.Sprintf("Nudge #%d", o.MessageSeq), CustomID: "nudge:" + o.ID},
				{Type: discord.ComponentButton, Style: discord.ButtonSecondary, Label: "Extend 30m", CustomID: "extend30:" + o.ID},
				{Type: discord.ComponentButton, Style: discord.ButtonSecondary, Label: fmt.Sprintf("Reassign #%d…", o.MessageSeq), CustomID: "reassign:" + o.ID},
			}})
		}
	}
	return reply{Content: truncate(fmt.Sprintf("**%d overdue**\n%s", len(overdue), strings.Join(lines, "\n")), contentLimit), Components: rows}
}

// obligation finds one open obligation by ID, or the one a message created.
func (b *Bridge) obligation(ctx context.Context, taskID, ref string) (api.Obligation, error) {
	all, err := b.cfg.Hub.ListObligations(ctx, taskID, "", "", true, false)
	if err != nil {
		return api.Obligation{}, err
	}
	ref = strings.TrimPrefix(ref, "#")
	seq, seqErr := strconv.ParseInt(ref, 10, 64)
	var found []api.Obligation
	for _, o := range all {
		if o.State != api.ObligationClosed && (o.ID == ref || (seqErr == nil && o.MessageSeq == seq)) {
			found = append(found, o)
		}
	}
	switch len(found) {
	case 0:
		return api.Obligation{}, fmt.Errorf("%w: no open obligation matches %q", errNotOpen, ref)
	case 1:
		return found[0], nil
	}
	return api.Obligation{}, fmt.Errorf("%w: message #%s has %d open obligations; use /stalled and its buttons", errAmbiguous, ref, len(found))
}

var errAmbiguous = errors.New("ambiguous")

// errNotOpen means the lookup worked and nothing open matched; any other
// error is a failure to ask, never proof that the work was handled.
var errNotOpen = errors.New("not open")

// lookupReply turns a failed obligation lookup into the right answer.
func lookupReply(err error) reply {
	switch {
	case errors.Is(err, errAmbiguous):
		return ephemeral("⚠️ %s.", err.Error())
	case errors.Is(err, errNotOpen):
		return ephemeral("✔️ Already handled: that obligation is closed or was moved.")
	}
	return ephemeral("⚠️ %s.", hubMessage(err))
}

// agentByName matches a full name, or an item-scoped name's base
// ("builder" for "builder-41b1c632") when that is unambiguous.
func agentByName(agents []api.Agent, name string) (api.Agent, error) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "@"))
	var matches []api.Agent
	for _, a := range agents {
		if a.Status == api.AgentClosed {
			continue
		}
		if strings.EqualFold(a.Name, name) {
			return a, nil
		}
		if base, _, ok := strings.Cut(a.Name, "-"); ok && strings.EqualFold(base, name) {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return api.Agent{}, fmt.Errorf("%q matches %d agents; use the full name", name, len(matches))
	}
	return api.Agent{}, fmt.Errorf("no running agent is named %q", name)
}

func (b *Bridge) nudgeTarget(ctx context.Context, taskID, target, interactionID string) reply {
	if target == "" {
		return ephemeral("Name an agent, a message number or an obligation ID.")
	}
	o, err := b.obligation(ctx, taskID, target)
	if err == nil {
		return b.nudge(ctx, taskID, o.ID, interactionID)
	}
	if errors.Is(err, errAmbiguous) {
		return ephemeral("⚠️ %s.", err.Error())
	}
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	agent, err := agentByName(detail.Agents, target)
	if err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	open, err := b.cfg.Hub.ListObligations(ctx, taskID, "", "", true, false)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	var oldest *api.Obligation
	for i, o := range open {
		if o.AgentID == agent.ID && o.Needs != api.ObligationNeedsDelivery && (oldest == nil || o.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = &open[i]
		}
	}
	if oldest == nil {
		return ephemeral("%s has no open obligations to nudge.", clean(agent.Name))
	}
	return b.nudge(ctx, taskID, oldest.ID, interactionID)
}

// nudge re-wakes an obligation's recipient. A closed obligation, for example
// from a stale button, is reported as already handled and changes nothing.
// The interaction ID names the nudge, so replaying an interaction after a
// crash returns the original nudge instead of waking the agent again.
func (b *Bridge) nudge(ctx context.Context, taskID, obligationID, interactionID string) reply {
	o, err := b.obligation(ctx, taskID, obligationID)
	if err != nil {
		return lookupReply(err)
	}
	if _, err := b.cfg.Hub.NudgeObligation(ctx, taskID, o.ID, "discord-interaction-"+interactionID); err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && h.Status == http.StatusConflict {
			return ephemeral("✔️ Already handled: %s.", h.Msg)
		}
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	return ephemeral("👉 Nudged the recipient of #%d (%s). The relay wakes it now if its runtime can be woken.", o.MessageSeq, truncate(clean(o.Subject), 80))
}

func (b *Bridge) reassignMenu(ctx context.Context, taskID, obligationID string) reply {
	o, err := b.obligation(ctx, taskID, obligationID)
	if err != nil {
		return lookupReply(err)
	}
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	var options []discord.SelectOption
	for _, a := range detail.Agents {
		if a.ID == o.AgentID || a.Status == api.AgentClosed || a.Status == api.AgentExited || len(options) == 25 {
			continue
		}
		options = append(options, discord.SelectOption{Label: truncate(a.Name, 100), Value: a.ID, Description: truncate(a.Status+" "+a.Role, 100)})
	}
	if len(options) == 0 {
		return ephemeral("There is no other running agent to give #%d to.", o.MessageSeq)
	}
	return reply{
		Content: fmt.Sprintf("Reassign #%d (%s) to:", o.MessageSeq, truncate(clean(o.Subject), 80)),
		Components: []discord.Component{{Type: discord.ComponentActionRow, Components: []discord.Component{
			{Type: discord.ComponentStringMenu, CustomID: "reassignto:" + o.ID, Placeholder: "Choose an agent", Options: options},
		}}},
	}
}

func (b *Bridge) reassignCommand(ctx context.Context, taskID, ref, agentName string) reply {
	o, err := b.obligation(ctx, taskID, ref)
	if err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	agent, err := agentByName(detail.Agents, agentName)
	if err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	return b.reassign(ctx, taskID, o.ID, agent.ID)
}

func (b *Bridge) reassign(ctx context.Context, taskID, obligationID, agentID string) reply {
	o, err := b.obligation(ctx, taskID, obligationID)
	if err != nil {
		return lookupReply(err)
	}
	m, err := b.cfg.Hub.ReassignObligation(ctx, taskID, o.ID, api.ObligationReassignRequest{ToAgentID: agentID, Reason: "reassigned from Discord"})
	if err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && h.Status == http.StatusConflict {
			return ephemeral("✔️ Already handled: %s.", h.Msg)
		}
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	return ephemeral("↪️ Reassigned #%d; it was re-sent as #%d.", o.MessageSeq, m.Seq)
}

// say posts an owner message, visible in the channel as the command's reply.
func (b *Bridge) say(ctx context.Context, taskID string, in discord.Interaction, text, agentName string) reply {
	if text == "" {
		return ephemeral("Say something.")
	}
	req := api.PostMessageRequest{Text: text, RequestID: "discord-interaction-" + in.ID,
		Source: &api.MessageSource{Kind: api.SourceDiscord, ID: in.ID, UserID: in.UserID()}}
	to := "everyone"
	if agentName != "" {
		detail, err := b.cfg.Hub.GetTask(ctx, taskID)
		if err != nil {
			return ephemeral("⚠️ %s.", hubMessage(err))
		}
		agent, err := agentByName(detail.Agents, agentName)
		if err != nil {
			return ephemeral("⚠️ %s. Nothing was posted.", err.Error())
		}
		req.To, to = agent.ID, agent.Name
	}
	m, err := b.cfg.Hub.PostMessage(ctx, taskID, req)
	if err != nil {
		return ephemeral("⛔ Tailterm refused this message: %s. Nothing was posted.", hubMessage(err))
	}
	return reply{Public: true, Content: truncate(fmt.Sprintf("**owner** → **%s**\n%s", clean(to), text), contentLimit-40) + "\n" + marker(m.Seq, 1, 1, "")}
}

// ---- Broker phase 3 owner controls ----

// ownerOutcome turns a hub conflict into "already handled" and any other
// error into a readable reply.
func ownerOutcome(err error) (reply, bool) {
	if err == nil {
		return reply{}, false
	}
	var h *api.HTTPError
	if errors.As(err, &h) && h.Status == http.StatusConflict && strings.Contains(h.Msg, "already closed") {
		return ephemeral("✔️ Already handled: %s.", h.Msg), true
	}
	return ephemeral("⚠️ %s.", hubMessage(err)), true
}

func (b *Bridge) extend(ctx context.Context, taskID, ref, duration, reason, interactionID string) reply {
	o, err := b.obligation(ctx, taskID, ref)
	if err != nil {
		return lookupReply(err)
	}
	d, err := time.ParseDuration(strings.TrimSpace(duration))
	if err != nil || d < time.Minute || d > 7*24*time.Hour {
		return ephemeral("⚠️ Give a duration from 1m to 168h, such as 30m or 2h.")
	}
	out, err := b.cfg.Hub.ExtendObligation(ctx, taskID, o.ID, api.ObligationExtendRequest{For: d.String(), Reason: reason, RequestID: "discord-interaction-" + interactionID})
	if r, done := ownerOutcome(err); done {
		return r
	}
	return ephemeral("⏱️ Extended #%d until %s. Escalation restarts from there.", o.MessageSeq, out.Obligation.DueAt.UTC().Format("15:04 UTC"))
}

func (b *Bridge) answer(ctx context.Context, taskID, ref, text, interactionID string) reply {
	o, err := b.obligation(ctx, taskID, ref)
	if err != nil {
		return lookupReply(err)
	}
	if strings.TrimSpace(text) == "" {
		return ephemeral("Say what the answer is.")
	}
	out, err := b.cfg.Hub.AnswerObligation(ctx, taskID, o.ID, api.ObligationAnswerRequest{Text: text, RequestID: "discord-interaction-" + interactionID})
	if r, done := ownerOutcome(err); done {
		return r
	}
	seq := int64(0)
	if out.Message != nil {
		seq = out.Message.Seq
	}
	return reply{Public: true, Content: truncate(fmt.Sprintf("**owner** answered #%d on the recipient's behalf:\n%s", o.MessageSeq, text), contentLimit-40) + "\n" + marker(seq, 1, 1, "")}
}

func (b *Bridge) cancel(ctx context.Context, taskID, ref, reason, interactionID string) reply {
	o, err := b.obligation(ctx, taskID, ref)
	if err != nil {
		return lookupReply(err)
	}
	if strings.TrimSpace(reason) == "" {
		return ephemeral("Give a reason; the agent is told why.")
	}
	if _, err := b.cfg.Hub.CancelObligation(ctx, taskID, o.ID, api.ObligationCancelRequest{Reason: reason, RequestID: "discord-interaction-" + interactionID}); err != nil {
		r, _ := ownerOutcome(err)
		return r
	}
	return ephemeral("🛑 Cancelled #%d (%s); the recipient was told.", o.MessageSeq, truncate(clean(o.Subject), 80))
}

func (b *Bridge) resume(ctx context.Context, taskID, agentName, interactionID string) reply {
	detail, err := b.cfg.Hub.GetTask(ctx, taskID)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	agent, err := agentByName(detail.Agents, agentName)
	if err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	if _, err := b.cfg.Hub.ResumeRetiredAgent(ctx, taskID, agent.ID, api.AgentResumeRequest{RequestID: "discord-interaction-" + interactionID}); err != nil {
		r, _ := ownerOutcome(err)
		return r
	}
	return ephemeral("▶️ Resumed %s: automatic wake-ups are on again.", clean(agent.Name))
}
