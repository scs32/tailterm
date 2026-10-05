package bridge

// The owner helper conversation (feature wi_2de2c1273e34473a,
// docs/discord-helper-chat.md): a configured owner's plain message in the
// bot DM or the helper channel goes to the helper project's owner_helper
// agent as a directed owner message, and the helper's reply comes back to
// the same DM or thread. /status and the digest work from hub data alone.
//
// Message text is data: it is trimmed and posted verbatim, never parsed for
// commands or routed on its content.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Inbound kinds for the helper path.
const (
	kindHelperMessage     = "helper-message"
	kindHelperInteraction = "helper-interaction"
)

// Limits (plan §7).
const (
	helperRateLimit     = 10
	helperRateWindow    = time.Minute
	helperSlowDownEvery = time.Minute
	helperRefusalEvery  = 10 * time.Minute
	// helperThreadBackfill bounds which conversation threads backfill reads.
	helperThreadBackfill = 7 * 24 * time.Hour
	summaryLines         = 5
)

func (b *Bridge) helperEnabled() bool { return b.cfg.HelperTask != "" }

func (b *Bridge) now() time.Time {
	if b.cfg.Now != nil {
		return b.cfg.Now()
	}
	return time.Now()
}

// helperLane is the outbox lane of one conversation (or other helper
// destination), kept apart from every project's lane.
func helperLane(id string) string { return "helper:" + id }

// digestCommand and the DM commands exist only while the helper is enabled.
var digestCommand = discord.Command{Name: "digest", Description: "Hourly Tailterm updates here, plus a line when work finishes or needs you", Options: []discord.CommandOption{
	{Type: discord.OptionString, Name: "state", Description: "on or off", Required: true},
	{Type: discord.OptionString, Name: "for", Description: "How long to keep it on, such as 8h (default: until off)"}}}

// guildCommands is the base list, plus /digest when the helper is enabled.
func (b *Bridge) guildCommands() []discord.Command {
	if !b.helperEnabled() {
		return Commands
	}
	return append(append([]discord.Command(nil), Commands...), digestCommand)
}

// globalCommands are the bot-DM commands. Contexts [BOT_DM] keeps them out
// of the guild, which already has its own /status.
func globalCommands() []discord.Command {
	dm := []int{discord.ContextBotDM}
	guildInstall := []int{discord.IntegrationGuildInstall}
	status := discord.Command{Name: "status", Description: "What's running, queued, waiting on you or stuck", Contexts: dm, IntegrationTypes: guildInstall}
	digest := digestCommand
	digest.Contexts, digest.IntegrationTypes = dm, guildInstall
	return []discord.Command{status, digest}
}

// registerGlobalCommands sets the DM commands when the helper is enabled,
// and clears commands a previous enabled run left behind when it is not.
func (b *Bridge) registerGlobalCommands(ctx context.Context) error {
	have, err := b.cfg.State.Get(ctx, "global-commands")
	if err != nil {
		return err
	}
	if !b.helperEnabled() {
		if have == "" {
			return nil
		}
		if err := b.cfg.Discord.SetGlobalCommands(ctx, b.cfg.AppID, []discord.Command{}); err != nil {
			return err
		}
		return b.cfg.State.Set(ctx, "global-commands", "")
	}
	raw, _ := json.Marshal(globalCommands())
	want := b.cfg.AppID + "|" + string(raw)
	if have == want {
		return nil
	}
	if err := b.cfg.Discord.SetGlobalCommands(ctx, b.cfg.AppID, globalCommands()); err != nil {
		return err
	}
	return b.cfg.State.Set(ctx, "global-commands", want)
}

// ---- state ----

// conversation is one helper conversation: an owner's DM channel, or a
// thread started from a top-level helper-channel message (its ID is the
// starter message's, which is also the thread's).
type conversation struct {
	ID           string
	Kind         string // convDM or convThread
	ChannelID    string // where replies and notes go
	OwnerID      string
	ThreadReady  bool
	LastReplySeq int64 // the helper's latest reply; follow-ups reply to it
	IngestAfter  string
	UpdatedAt    time.Time
}

const (
	convDM     = "dm"
	convThread = "thread"
)

const convCols = `id,kind,channel_id,owner_id,thread_ready,last_seq,ingest_after,updated_at`

func scanConversation(row interface{ Scan(...any) error }) (conversation, error) {
	var c conversation
	var updated string
	err := row.Scan(&c.ID, &c.Kind, &c.ChannelID, &c.OwnerID, &c.ThreadReady, &c.LastReplySeq, &c.IngestAfter, &updated)
	c.UpdatedAt = parseTS(updated)
	return c, err
}

func (s *State) conversation(ctx context.Context, id string) (conversation, bool, error) {
	if id == "" {
		return conversation{}, false, nil
	}
	c, err := scanConversation(s.db.QueryRowContext(ctx, `SELECT `+convCols+` FROM helper_conversations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

func (s *State) conversations(ctx context.Context) ([]conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+convCols+` FROM helper_conversations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// addConversation records a conversation the first time it is seen; an
// existing one keeps its state.
func (s *State) addConversation(ctx context.Context, c conversation) error {
	now := ts(s.now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO helper_conversations (id,kind,channel_id,owner_id,ingest_after,created_at,updated_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		c.ID, c.Kind, c.ChannelID, c.OwnerID, c.IngestAfter, now, now)
	return err
}

func (s *State) setThreadReady(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE helper_conversations SET thread_ready=1 WHERE id=?`, id)
	return err
}

func (s *State) setConversationIngest(ctx context.Context, id, after string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE helper_conversations SET ingest_after=? WHERE id=? AND (length(ingest_after)<length(?) OR (length(ingest_after)=length(?) AND ingest_after<?))`,
		after, id, after, after, after)
	return err
}

// recordHelperMessage maps a board message to its conversation. A helper
// reply also becomes the conversation's latest reply.
func (s *State) recordHelperMessage(ctx context.Context, taskID string, seq int64, convID, author string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO helper_messages (task_id,seq,conversation_id,author) VALUES (?,?,?,?) ON CONFLICT(task_id,seq) DO NOTHING`, taskID, seq, convID, author); err != nil {
		return err
	}
	if author == "helper" {
		if _, err := tx.ExecContext(ctx, `UPDATE helper_conversations SET last_seq=MAX(last_seq,?),updated_at=? WHERE id=?`, seq, ts(s.now()), convID); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE helper_conversations SET updated_at=? WHERE id=?`, ts(s.now()), convID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *State) helperMessageConversation(ctx context.Context, taskID string, seq int64) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id FROM helper_messages WHERE task_id=? AND seq=?`, taskID, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// helperMessagesWritten counts an owner's helper messages written (by their
// Discord ID's time) in the window ending with message upTo, apart from the
// ones already refused for the rate limit. Message time, not the time the
// bridge took a message on, so a backfill after an outage is not throttled.
func (s *State) helperMessagesWritten(ctx context.Context, owner, upTo string, window time.Duration) (int, error) {
	last, err := strconv.ParseInt(upTo, 10, 64)
	if err != nil {
		return 0, err
	}
	first, _ := strconv.ParseInt(snowflakeAt(snowflakeTime(upTo).Add(-window)), 10, 64)
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM inbound WHERE kind=? AND CAST(source_id AS INTEGER) BETWEEN ? AND ? AND result NOT LIKE 'rate limited%' AND json_extract(payload,'$.message.author.id')=?`,
		kindHelperMessage, first, last, owner).Scan(&n)
	return n, err
}

// snowflakeTime is when a Discord ID was created.
func snowflakeTime(id string) time.Time {
	n, _ := strconv.ParseInt(id, 10, 64)
	return time.UnixMilli((n >> 22) + discordEpoch)
}

func (s *State) getJSON(ctx context.Context, key string, v any) (bool, error) {
	raw, err := s.Get(ctx, key)
	if err != nil || raw == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *State) setJSON(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Set(ctx, key, string(raw))
}

// ---- startup ----

// validateHelperConfig refuses a helper channel that is a project's channel,
// so misconfiguration can never change project-channel behavior.
func validateHelperConfig(cfg Config) error {
	if cfg.HelperTask == "" {
		if cfg.HelperChannel != "" {
			return errors.New("bridge: DISCORD_HELPER_CHANNEL needs DISCORD_HELPER_TASK")
		}
		return nil
	}
	if !api.ValidID(cfg.HelperTask, "tsk") {
		return fmt.Errorf("bridge: DISCORD_HELPER_TASK %q is not a project ID", cfg.HelperTask)
	}
	if cfg.HelperChannel == "" {
		return nil
	}
	if !api.ValidMessageSource(api.MessageSource{Kind: api.SourceDiscord, ID: cfg.HelperChannel, UserID: cfg.HelperChannel}) {
		return fmt.Errorf("bridge: DISCORD_HELPER_CHANNEL %q is not a Discord channel ID", cfg.HelperChannel)
	}
	if m, err := cfg.State.MappingByChannel(context.Background(), cfg.HelperChannel); err == nil {
		return fmt.Errorf("bridge: DISCORD_HELPER_CHANNEL %s is project %s's channel; give the helper its own channel", cfg.HelperChannel, m.TaskID)
	} else if !errors.Is(err, ErrNoMapping) {
		return err
	}
	return nil
}

// startHelper starts the helper's cursors: a new channel or reply cursor
// starts now, so history is never replayed; an existing one is kept, so
// what arrived during an outage is still read.
func (b *Bridge) startHelper(ctx context.Context) error {
	if b.cfg.HelperChannel != "" {
		if have, err := b.cfg.State.Get(ctx, "helper:channel-after"); err != nil {
			return err
		} else if have == "" {
			if err := b.cfg.State.Set(ctx, "helper:channel-after", snowflakeAt(time.Now())); err != nil {
				return err
			}
		}
	}
	if have, err := b.cfg.State.Get(ctx, "helper:reply-after"); err != nil {
		return err
	} else if have == "" {
		latest, err := b.cfg.Hub.LatestMessages(ctx, b.cfg.HelperTask, 1)
		if err != nil {
			return err
		}
		after := int64(0)
		for _, m := range latest {
			after = max(after, m.Seq)
		}
		if err := b.cfg.State.Set(ctx, "helper:reply-after", strconv.FormatInt(after, 10)); err != nil {
			return err
		}
	}
	b.openOwnerDMs(ctx)
	return nil
}

// openOwnerDMs records each owner's DM channel, so backfill reads it even
// when the Gateway never delivered a DM. A failure is retried by backfill.
func (b *Bridge) openOwnerDMs(ctx context.Context) {
	for _, owner := range b.ownerList {
		if have, err := b.cfg.State.Get(ctx, "helper:dm:"+owner); err != nil || have != "" {
			continue
		}
		c, err := b.cfg.Discord.CreateDM(ctx, owner)
		if err != nil {
			b.logf("discord bridge: open DM with owner: %v", err)
			continue
		}
		if err := b.cfg.State.addConversation(ctx, conversation{ID: c.ID, Kind: convDM, ChannelID: c.ID, OwnerID: owner, IngestAfter: snowflakeAt(time.Now())}); err != nil {
			b.logf("discord bridge: record DM: %v", err)
			continue
		}
		_ = b.cfg.State.Set(ctx, "helper:dm:"+owner, c.ID)
	}
}

// ---- Discord → helper ----

type helperRoute string

const (
	routeNone    helperRoute = ""
	routeDM      helperRoute = "dm"
	routeChannel helperRoute = "channel" // a top-level helper-channel message
	routeThread  helperRoute = "thread"  // a message in a conversation thread
)

// helperRouteOf places a Gateway message. A DM is a recorded owner DM
// channel, or a message with no guild (Gateway events only: REST reads
// never carry guild_id, so backfill passes its route explicitly).
func (b *Bridge) helperRouteOf(ctx context.Context, msg discord.Message) (helperRoute, error) {
	if !b.helperEnabled() || (msg.GuildID != "" && msg.GuildID != b.cfg.GuildID) {
		return routeNone, nil
	}
	if b.cfg.HelperChannel != "" && msg.ChannelID == b.cfg.HelperChannel {
		return routeChannel, nil
	}
	c, ok, err := b.cfg.State.conversation(ctx, msg.ChannelID)
	if err != nil {
		return routeNone, err
	}
	switch {
	case ok && c.Kind == convDM:
		return routeDM, nil
	case ok && c.Kind == convThread && c.ChannelID == msg.ChannelID:
		return routeThread, nil
	case msg.GuildID == "":
		return routeDM, nil
	}
	return routeNone, nil
}

// helperInbound is what an inbound claim keeps, so a retry repeats exactly
// the same post.
type helperInbound struct {
	Route        helperRoute     `json:"route"`
	Conversation string          `json:"conversation"`
	ReplyTo      int64           `json:"replyTo,omitempty"`
	Message      discord.Message `json:"message"`
}

// handleHelperMessage takes one message in the DM, the helper channel or a
// conversation thread. It returns an error only when the message could not
// be recorded, so backfill reads it again.
func (b *Bridge) handleHelperMessage(ctx context.Context, route helperRoute, msg discord.Message) error {
	b.mu.Lock()
	bot := b.botID
	b.mu.Unlock()
	if msg.Author.ID == bot || msg.Author.Bot || msg.WebhookID != "" || (msg.Type != 0 && msg.Type != 19) {
		return nil
	}
	if !b.owners[msg.Author.ID] {
		return b.refuseStranger(ctx, msg)
	}
	c, err := b.conversationFor(ctx, route, msg)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(helperInbound{Route: route, Conversation: c.ID, ReplyTo: c.LastReplySeq, Message: msg})
	claimed, err := b.cfg.State.ClaimInbound(ctx, msg.ID, kindHelperMessage, b.cfg.HelperTask, string(raw), time.Now().Add(time.Minute))
	if err != nil || !claimed {
		return err // a redelivery does nothing
	}
	n, err := b.cfg.State.helperMessagesWritten(ctx, msg.Author.ID, msg.ID, helperRateWindow)
	if err != nil {
		return err
	}
	if n > helperRateLimit {
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rate limited")
		key := "helper-slowdown:" + msg.Author.ID
		last, _ := b.cfg.State.Get(ctx, key)
		if last == "" || time.Since(parseTS(last)) >= helperSlowDownEvery {
			_ = b.cfg.State.Set(ctx, key, ts(time.Now()))
			b.note(ctx, msg.ChannelID, msg, fmt.Sprintf("⏳ Slow down: at most %d helper messages a minute. This one was not sent.", helperRateLimit))
		}
		return nil
	}
	if route == routeChannel {
		// Start the thread now, so a follow-up typed before the helper
		// answers already has somewhere to go. A failure is retried before
		// the first send into the thread.
		if err := b.ensureHelperThread(ctx, helperLane(c.ID)); err != nil {
			b.logf("discord bridge: start helper thread %s: %v", c.ID, err)
		}
	}
	b.postHelper(ctx, helperInbound{Route: route, Conversation: c.ID, ReplyTo: c.LastReplySeq, Message: msg}, 0)
	return nil
}

// conversationFor finds or starts the message's conversation.
func (b *Bridge) conversationFor(ctx context.Context, route helperRoute, msg discord.Message) (conversation, error) {
	c := conversation{ID: msg.ChannelID, Kind: convDM, ChannelID: msg.ChannelID, OwnerID: msg.Author.ID, IngestAfter: msg.ID}
	switch route {
	case routeChannel:
		// Discord gives a thread started from a message that message's ID.
		c = conversation{ID: msg.ID, Kind: convThread, ChannelID: msg.ID, OwnerID: msg.Author.ID, IngestAfter: msg.ID}
	case routeThread:
		c.Kind = convThread
	}
	if err := b.cfg.State.addConversation(ctx, c); err != nil {
		return c, err
	}
	stored, ok, err := b.cfg.State.conversation(ctx, c.ID)
	if err != nil || !ok {
		return c, err
	}
	return stored, nil
}

// ensureHelperThread starts a conversation's thread once. Discord's
// "already created" answer means an earlier attempt landed.
func (b *Bridge) ensureHelperThread(ctx context.Context, lane string) error {
	id, ok := strings.CutPrefix(lane, "helper:")
	if !ok || b.cfg.HelperChannel == "" {
		return nil
	}
	c, found, err := b.cfg.State.conversation(ctx, id)
	if err != nil || !found || c.Kind != convThread || c.ThreadReady || c.ChannelID != c.ID {
		return err
	}
	_, err = b.cfg.Discord.StartThreadFromMessage(ctx, b.cfg.HelperChannel, c.ID, discord.StartThread{Name: "Owner helper", AutoArchiveDuration: 10080})
	if err != nil && !discord.IsCode(err, discord.CodeThreadAlreadyCreated) {
		return err
	}
	return b.cfg.State.setThreadReady(ctx, c.ID)
}

// refuseStranger answers a non-owner at most once per user per 10 minutes.
// Nothing is posted to Tailterm.
func (b *Bridge) refuseStranger(ctx context.Context, msg discord.Message) error {
	key := "helper-refused:" + msg.Author.ID
	last, err := b.cfg.State.Get(ctx, key)
	if err != nil {
		return err
	}
	if last != "" && time.Since(parseTS(last)) < helperRefusalEvery {
		return nil
	}
	if err := b.cfg.State.Set(ctx, key, ts(time.Now())); err != nil {
		return err
	}
	b.note(ctx, msg.ChannelID, msg, "⛔ Sorry, only the Tailterm owner can talk with the helper here. Nothing was sent.")
	return nil
}

// note queues one short bridge message about an owner's message, sent
// exactly once (its key is the Discord message ID).
func (b *Bridge) note(ctx context.Context, channel string, about discord.Message, text string) {
	mk := "-# re " + about.ID
	send := discord.MessageSend{Content: text + "\n" + mk}
	if channel == about.ChannelID {
		no := false
		send.MessageReference = &discord.MessageReference{MessageID: about.ID, FailIfNotExists: &no}
	}
	row := OutboxRow{Key: "helper-note:" + about.ID, TaskID: helperLane(channel), Kind: kindNotice, ChannelID: channel,
		Payload: payloadJSON(outboxPayload{Message: send, Marker: mk})}
	if err := b.cfg.State.Enqueue(ctx, row.TaskID, 0, []OutboxRow{row}); err != nil {
		b.logf("discord bridge: queue note for %s: %v", about.ID, err)
		return
	}
	b.kick()
}

// helperAvailability is the helper's state as the owner sees it.
type helperAvailability string

const (
	helperIdle    helperAvailability = "idle"
	helperBusy    helperAvailability = "busy"
	helperOffline helperAvailability = "offline"
	helperMissing helperAvailability = "none"
)

// busyActivity are execution states in which a turn is still running.
var busyActivity = map[string]bool{"working": true, "hung_tool": true, "looping": true, "runtime_prompt": true, "stuck": true}

func classifyHelper(a api.Agent, found bool) helperAvailability {
	switch {
	case !found:
		return helperMissing
	case !a.Online || a.Status == api.AgentRetired || a.Status == api.AgentExited:
		return helperOffline
	case a.Activity != nil && busyActivity[a.Activity.State]:
		return helperBusy
	}
	return helperIdle
}

// helperAgent is the helper project's open owner helper, if any.
func (b *Bridge) helperAgent(ctx context.Context) (api.Agent, bool, error) {
	agents, err := b.cfg.Hub.ListAgents(ctx, b.cfg.HelperTask)
	if err != nil {
		return api.Agent{}, false, err
	}
	var found *api.Agent
	for i, a := range agents {
		if a.Role != api.AgentRoleOwnerHelper || a.Status == api.AgentClosed {
			continue
		}
		if found == nil || found.Status == api.AgentExited {
			found = &agents[i]
		}
	}
	if found == nil {
		return api.Agent{}, false, nil
	}
	return *found, true, nil
}

// postHelper posts an owner's message to the helper. Every step is
// idempotent (the post by request ID, the note by key, the reaction by
// Discord), and the claim is finished last, so a crash at any point is
// completed by the retry loop without a duplicate.
func (b *Bridge) postHelper(ctx context.Context, in helperInbound, attempts int) {
	msg := in.Message
	c, found, err := b.cfg.State.conversation(ctx, in.Conversation)
	if err != nil {
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	if !found {
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: conversation missing")
		return
	}
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		b.note(ctx, c.ChannelID, msg, "⛔ Only text can be sent to the helper; attachments aren't supported yet. Nothing was sent.")
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: no text")
		return
	}
	helper, found, err := b.helperAgent(ctx)
	if err != nil {
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	state := classifyHelper(helper, found)
	if state == helperMissing {
		b.note(ctx, c.ChannelID, msg, "📴 No helper session is registered, so nothing was sent. /status works now from Tailterm's own records.")
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: no helper")
		return
	}
	posted, err := b.cfg.Hub.PostMessage(ctx, b.cfg.HelperTask, api.PostMessageRequest{
		To: helper.ID, Text: text, ReplyTo: in.ReplyTo, RequestID: "discord-msg-" + msg.ID,
		Source: &api.MessageSource{Kind: api.SourceDiscord, ID: msg.ID, UserID: msg.Author.ID},
	})
	if err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != http.StatusTooManyRequests {
			b.note(ctx, c.ChannelID, msg, "⛔ Tailterm refused this message: "+truncate(clean(h.Msg), 300)+". Nothing was sent.")
			_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: "+h.Msg)
			return
		}
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	if err := b.cfg.State.recordHelperMessage(ctx, b.cfg.HelperTask, posted.Seq, c.ID, "owner"); err != nil {
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	switch state {
	case helperBusy:
		b.note(ctx, c.ChannelID, msg, "📨 Received. The helper is busy and will answer when its current turn ends.")
	case helperOffline:
		b.note(ctx, c.ChannelID, msg, "📴 Received and saved, but the helper is offline; it will see this when it's back. /status works now.")
	default:
		if err := b.cfg.Discord.React(ctx, msg.ChannelID, msg.ID, "✅"); err != nil {
			b.logf("discord bridge: react to %s: %v", msg.ID, err)
		}
	}
	_ = b.cfg.State.FinishInbound(ctx, msg.ID, fmt.Sprintf("posted #%d", posted.Seq))
}

// retryHelperInbound finishes a helper message or interaction interrupted
// by a hub outage or a crash.
func (b *Bridge) retryHelperInbound(ctx context.Context, in Inbound) {
	switch in.Kind {
	case kindHelperMessage:
		var hi helperInbound
		if json.Unmarshal([]byte(in.Payload), &hi) != nil {
			_ = b.cfg.State.FinishInbound(ctx, in.SourceID, "rejected: unreadable")
			return
		}
		b.postHelper(ctx, hi, in.Attempts+1)
	case kindHelperInteraction:
		var it discord.Interaction
		if json.Unmarshal([]byte(in.Payload), &it) != nil || time.Since(in.CreatedAt) > InteractionLifetime-time.Minute {
			_ = b.cfg.State.FinishInbound(ctx, in.SourceID, "expired before it could be finished")
			return
		}
		_ = b.cfg.State.RetryInbound(ctx, in.SourceID, time.Now().Add(3*time.Minute), "resuming")
		b.runHelperInteraction(ctx, it, true)
	}
}

// helperBackfill reads the owners' DMs, the helper channel and recent
// conversation threads for messages the Gateway did not deliver. REST reads
// carry no guild_id, so each source passes its own route; nothing here
// relies on the guild stamp project backfill applies.
func (b *Bridge) helperBackfill(ctx context.Context) {
	b.openOwnerDMs(ctx)
	convs, err := b.cfg.State.conversations(ctx)
	if err != nil {
		b.logf("discord bridge: helper backfill: %v", err)
		return
	}
	for _, c := range convs {
		if c.Kind == convThread && (!c.ThreadReady || time.Since(c.UpdatedAt) > helperThreadBackfill) {
			continue
		}
		route := routeDM
		if c.Kind == convThread {
			route = routeThread
		}
		after := b.backfillChannel(ctx, c.ChannelID, c.IngestAfter, route)
		if after != c.IngestAfter {
			if err := b.cfg.State.setConversationIngest(ctx, c.ID, after); err != nil {
				b.logf("discord bridge: helper cursor %s: %v", c.ID, err)
			}
		}
	}
	if b.cfg.HelperChannel == "" {
		return
	}
	have, err := b.cfg.State.Get(ctx, "helper:channel-after")
	if err != nil || have == "" {
		return
	}
	if after := b.backfillChannel(ctx, b.cfg.HelperChannel, have, routeChannel); after != have {
		if err := b.cfg.State.Set(ctx, "helper:channel-after", after); err != nil {
			b.logf("discord bridge: helper channel cursor: %v", err)
		}
	}
}

// backfillChannel reads one channel forward from after and returns the new
// cursor, which never passes a message that was not recorded.
func (b *Bridge) backfillChannel(ctx context.Context, channel, after string, route helperRoute) string {
	for {
		msgs, err := b.cfg.Discord.ChannelMessages(ctx, channel, after, 100)
		if err != nil {
			b.logf("discord bridge: helper backfill %s: %v", channel, err)
			return after
		}
		for _, msg := range msgs {
			if msg.ChannelID == "" {
				msg.ChannelID = channel
			}
			if b.handleHelperMessage(ctx, route, msg) != nil {
				return after
			}
			after = msg.ID
		}
		if len(msgs) < 100 || ctx.Err() != nil {
			return after
		}
	}
}

// ---- helper → Discord ----

// helperReplies routes the helper's replies to their conversations: a
// board message from an owner_helper agent whose replyTo is a message of a
// conversation. Everything else stays on the board only.
func (b *Bridge) helperReplies(ctx context.Context) error {
	raw, err := b.cfg.State.Get(ctx, "helper:reply-after")
	if err != nil || raw == "" {
		return err
	}
	after, _ := strconv.ParseInt(raw, 10, 64)
	var names roster
	for {
		msgs, err := b.cfg.Hub.ListMessages(ctx, b.cfg.HelperTask, after, "", 100)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, msg := range msgs {
			if msg.From.AgentID != "" && msg.ReplyTo > 0 {
				if err := b.routeHelperReply(ctx, &names, msg); err != nil {
					return err
				}
			}
			after = msg.Seq
			if err := b.cfg.State.Set(ctx, "helper:reply-after", strconv.FormatInt(after, 10)); err != nil {
				return err
			}
		}
		if len(msgs) < 100 {
			return nil
		}
	}
}

func (b *Bridge) routeHelperReply(ctx context.Context, names *roster, msg api.Message) error {
	convID, ok, err := b.cfg.State.helperMessageConversation(ctx, b.cfg.HelperTask, msg.ReplyTo)
	if err != nil || !ok {
		return err
	}
	if _, known := (*names)[msg.From.AgentID]; !known {
		agents, err := b.cfg.Hub.ListAgents(ctx, b.cfg.HelperTask)
		if err != nil {
			return err
		}
		*names = roster{}
		for _, a := range agents {
			(*names)[a.ID] = a
		}
	}
	agent, known := (*names)[msg.From.AgentID]
	if !known || agent.Role != api.AgentRoleOwnerHelper {
		return nil
	}
	c, ok, err := b.cfg.State.conversation(ctx, convID)
	if err != nil || !ok {
		return err
	}
	if err := b.cfg.State.recordHelperMessage(ctx, b.cfg.HelperTask, msg.Seq, c.ID, "helper"); err != nil {
		return err
	}
	if err := b.cfg.State.Enqueue(ctx, helperLane(c.ID), 0, renderHelperReply(agent.Name, c, msg)); err != nil {
		return err
	}
	b.kick()
	return nil
}

// renderHelperReply is the helper's answer: a header naming the helper
// (never the owner), the redacted text split into parts, and a marker.
func renderHelperReply(name string, c conversation, msg api.Message) []OutboxRow {
	header := fmt.Sprintf("🤖 **%s** · owner helper", clean(name))
	body := msg.Text
	if env := msg.Envelope; env != nil {
		header += fmt.Sprintf(" · %s · %s", strings.ToUpper(env.Kind), clean(redactSecrets(env.Subject)))
		body = typedBody(msg.Text)
		// Helper answers and progress retain their original Markdown. The
		// normalized Board display text flattens envelope field line breaks.
		switch env.Kind {
		case api.EnvelopeKindAnswer:
			body = env.Body.Answer
		case api.EnvelopeKindNotice:
			body = env.Body.Text
		case api.EnvelopeKindResult:
			if env.Refs["helperReply"] == "final" {
				body = env.Body.Text
			}
		}
	}
	parts := chunk(redactSecrets(body), partBudget)
	rows := make([]OutboxRow, 0, len(parts))
	for i, text := range parts {
		mk := marker(msg.Seq, i+1, len(parts), "")
		content := text + "\n" + mk
		if i == 0 {
			content = truncate(header, 200) + "\n" + content
		}
		rows = append(rows, OutboxRow{Key: fmt.Sprintf("helper-reply:%s:%d:%d", msg.TaskID, msg.Seq, i+1), TaskID: helperLane(c.ID), ChannelID: c.ChannelID,
			Kind: kindLine, Payload: payloadJSON(outboxPayload{Message: discord.MessageSend{Content: content}, Marker: mk})})
	}
	return rows
}

// ---- redaction ----

var (
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	bearer   = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[^\s"']+`)
	// A key ending in a secret word (GITHUB_TOKEN, DB_PASSWORD,
	// OPENAI_API_KEY, AWS_SECRET_ACCESS_KEY, a JSON "token"), then = or :
	// and a value. The word must end the key, so tokens=N is not a secret.
	keyValue   = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:token|passw(?:or)?d|secret|api[_-]?key|access[_-]?key)["']?)(\s*[=:]\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;}]+)`)
	secretLike = []*regexp.Regexp{
		regexp.MustCompile(`\b[A-Za-z0-9_-]{24,28}\.[A-Za-z0-9_-]{6,7}\.[A-Za-z0-9_-]{27,40}\b`), // Discord bot token
		regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	}
)

const redacted = "[redacted]"

// redactSecrets hides credential-shaped text in what the helper sends to
// Discord. A bare commit SHA or digest (40 or 64 hex characters) matches no
// pattern and is kept; the same hex as a secret key's value is redacted. The
// board text itself is never changed.
func redactSecrets(s string) string {
	s = pemBlock.ReplaceAllString(s, redacted)
	s = bearer.ReplaceAllString(s, "${1}"+redacted)
	s = keyValue.ReplaceAllString(s, "${1}${2}"+redacted)
	for _, re := range secretLike {
		s = re.ReplaceAllString(s, redacted)
	}
	return s
}

// ---- /status and the digest: hub data only ----

// projectSnapshot is one open project's queue, open obligations and roster.
type projectSnapshot struct {
	Task        api.Task
	Agents      []api.Agent
	Obligations []api.Obligation // open only
	Queue       api.TeamQueueList
	At          time.Time
}

// rememberSnapshot keeps what the status card pass just read, so the
// digest reuses it instead of asking the hub again.
func (b *Bridge) rememberSnapshot(task api.Task, agents []api.Agent, obligations []api.Obligation, queue api.TeamQueueList) {
	if !b.helperEnabled() {
		return
	}
	open := make([]api.Obligation, 0, len(obligations))
	for _, o := range obligations {
		if o.State != api.ObligationClosed {
			open = append(open, o)
		}
	}
	b.mu.Lock()
	if b.snapshots == nil {
		b.snapshots = map[string]projectSnapshot{}
	}
	b.snapshots[task.ID] = projectSnapshot{Task: task, Agents: agents, Obligations: open, Queue: queue, At: time.Now()}
	b.mu.Unlock()
}

// openSnapshots returns every open project, helper project first, reading
// the hub only for projects without a snapshot younger than maxAge.
func (b *Bridge) openSnapshots(ctx context.Context, maxAge time.Duration) ([]projectSnapshot, error) {
	tasks, err := b.cfg.Hub.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	var out []projectSnapshot
	for _, t := range tasks {
		if t.Status != api.TaskOpen {
			continue
		}
		b.mu.Lock()
		s, ok := b.snapshots[t.ID]
		b.mu.Unlock()
		if !ok || time.Since(s.At) > maxAge {
			detail, err := b.cfg.Hub.GetTask(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			open, err := b.cfg.Hub.ListObligations(ctx, t.ID, "", "", true, false)
			if err != nil {
				return nil, err
			}
			queue, err := b.cfg.Hub.ListTeamQueue(ctx, t.ID)
			if err != nil {
				return nil, err
			}
			s = projectSnapshot{Task: detail.Task, Agents: detail.Agents, Obligations: open, Queue: queue, At: time.Now()}
			b.mu.Lock()
			if b.snapshots == nil {
				b.snapshots = map[string]projectSnapshot{}
			}
			b.snapshots[t.ID] = s
			b.mu.Unlock()
		}
		s.Task.Name = t.Name
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		hi, hj := out[i].Task.ID == b.cfg.HelperTask, out[j].Task.ID == b.cfg.HelperTask
		if hi != hj {
			return hi
		}
		return out[i].Task.Name < out[j].Task.Name
	})
	return out, nil
}

// itemTitle reads a work item's title through the bridge's read-only
// route, and falls back to its ID (for example on an older hub that
// answers 403).
func (b *Bridge) itemTitle(ctx context.Context, taskID, itemID string) string {
	b.mu.Lock()
	title, ok := b.titles[itemID]
	forbidden := b.titlesForbidden
	b.mu.Unlock()
	if ok {
		return title
	}
	if forbidden || itemID == "" {
		return itemID
	}
	item, err := b.cfg.Hub.GetWorkItem(ctx, taskID, itemID)
	if err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && (h.Status == http.StatusForbidden || h.Status == http.StatusNotFound) {
			b.mu.Lock()
			if h.Status == http.StatusForbidden {
				b.titlesForbidden = true
			}
			b.mu.Unlock()
		}
		return itemID
	}
	title = truncate(clean(redactSecrets(item.Title)), 80)
	b.mu.Lock()
	if b.titles == nil {
		b.titles = map[string]string{}
	}
	b.titles[itemID] = title
	b.mu.Unlock()
	return title
}

// entryStage is how far a running queue entry has got.
func entryStage(e api.TeamQueueEntry) string {
	switch {
	case e.Release != nil && (e.Release.State == "verified" || e.Release.State == "claimed" || e.Release.State == "merged"):
		return "releasing"
	case e.Release != nil && e.Release.State == "blocked":
		return "release blocked"
	case e.Integration != nil:
		return "ready to integrate"
	case e.Verification != nil && e.Verification.State != "":
		return "verification " + e.Verification.State
	case e.Reviews != nil && len(e.Reviews.Rounds) > 0:
		return fmt.Sprintf("review round %d", e.Reviews.Rounds[len(e.Reviews.Rounds)-1].Number)
	}
	return e.State
}

// since formats how long ago t was, to the minute.
func since(now, t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t)
	if d < time.Minute {
		return "<1m"
	}
	d = d.Round(time.Minute)
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
}

func parseRFC3339(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// stuckActivity are agent states the summary lists as stuck.
var stuckActivity = map[string]bool{"stuck": true, "hung_tool": true, "crashed": true, "looping": true}

// renderSummary is the hub-only status: running work and its stage, the
// next queued entry, what waits on the owner, what is stuck and the helper.
// It shows only names, IDs, titles, states and durations.
func (b *Bridge) renderSummary(ctx context.Context, snaps []projectSnapshot, helper api.Agent, state helperAvailability, now time.Time) discord.Embed {
	var running, next, waiting, stuck []string
	for _, s := range snaps {
		project := clean(s.Task.Name)
		names := roster{}
		for _, a := range s.Agents {
			names[a.ID] = a
		}
		var queued *api.TeamQueueEntry
		for i, e := range s.Queue.Entries {
			switch {
			case (e.State == "running" || e.State == "launching") && e.ReleasedAt == "":
				running = append(running, fmt.Sprintf("**%s** · %s · %s · updated %s ago", project, b.itemTitle(ctx, s.Task.ID, e.ItemID), entryStage(e), since(now, parseRFC3339(e.UpdatedAt))))
			case e.State == "queued" && (queued == nil || e.Position < queued.Position):
				queued = &s.Queue.Entries[i]
			}
			if e.Stall != nil {
				stuck = append(stuck, fmt.Sprintf("**%s** queue: %s for %s", project, clean(redactSecrets(e.Stall.Cause)), since(now, parseRFC3339(e.Stall.Since))))
			}
		}
		if queued != nil {
			line := fmt.Sprintf("**%s** · %s", project, b.itemTitle(ctx, s.Task.ID, queued.ItemID))
			if queued.BlockReason != "" {
				line += " · " + truncate(clean(redactSecrets(queued.BlockReason)), 80)
			}
			next = append(next, line)
		}
		obligations := append([]api.Obligation(nil), s.Obligations...)
		sort.Slice(obligations, func(i, j int) bool { return obligations[i].CreatedAt.Before(obligations[j].CreatedAt) })
		for _, o := range obligations {
			if o.RecipientKind == api.ObligationRecipientOwner {
				waiting = append(waiting, fmt.Sprintf("#%d %s · **%s** · %s", o.MessageSeq, truncate(clean(redactSecrets(o.Subject)), 70), project, since(now, o.CreatedAt)))
			} else if o.Overdue != "" {
				stuck = append(stuck, fmt.Sprintf("#%d %s → %s · %s overdue · open %s", o.MessageSeq, truncate(clean(redactSecrets(o.Subject)), 60), clean(names.name(o.AgentID)), o.Overdue, since(now, o.CreatedAt)))
			}
		}
		for _, a := range s.Agents {
			if a.Status == api.AgentClosed || a.Activity == nil || !stuckActivity[a.Activity.State] {
				continue
			}
			at := a.Activity.LastEventAt
			if at.IsZero() {
				at = a.Activity.ObservedAt
			}
			stuck = append(stuck, fmt.Sprintf("%s (**%s**) %s for %s", clean(a.Name), project, strings.ReplaceAll(a.Activity.State, "_", " "), since(now, at)))
		}
	}
	section := func(title string, lines []string, empty string) string {
		if len(lines) == 0 {
			return fmt.Sprintf("**%s**\n%s", title, empty)
		}
		shown := lines
		if len(shown) > summaryLines {
			shown = shown[:summaryLines]
		}
		out := fmt.Sprintf("**%s (%d)**\n• %s", title, len(lines), strings.Join(shown, "\n• "))
		if len(lines) > summaryLines {
			out += fmt.Sprintf("\n+%d more", len(lines)-summaryLines)
		}
		return out
	}
	var h string
	switch state {
	case helperMissing:
		h = "no helper session is registered"
	case helperBusy:
		h = fmt.Sprintf("%s is busy (%s)", clean(helper.Name), helper.Activity.State)
	case helperOffline:
		h = fmt.Sprintf("%s is offline", clean(helper.Name))
	default:
		h = fmt.Sprintf("%s is idle", clean(helper.Name))
	}
	text := strings.Join([]string{
		section("Running", running, "Nothing is running."),
		section("Next queued", next, "Nothing is queued."),
		section("Waiting on you", waiting, "Nothing is waiting on you."),
		section("Stuck", stuck, "Nothing is stuck."),
		"**Helper:** " + h,
	}, "\n\n")
	return discord.Embed{Title: "Tailterm status", Description: truncate(text, embedBudget), Color: 0x5865F2,
		Footer: &discord.EmbedFooter{Text: "From Tailterm's records · " + now.UTC().Format("Jan 2 15:04 UTC")}}
}

// summary reads what the summary needs; snapshots up to maxAge old are reused.
func (b *Bridge) summary(ctx context.Context, maxAge time.Duration) (discord.Embed, error) {
	snaps, err := b.openSnapshots(ctx, maxAge)
	if err != nil {
		return discord.Embed{}, err
	}
	helper, found, err := b.helperAgent(ctx)
	if err != nil {
		return discord.Embed{}, err
	}
	// Hub timestamps are real time, so durations are measured against it.
	return b.renderSummary(ctx, snaps, helper, classifyHelper(helper, found), time.Now()), nil
}

// ---- DM and helper-channel interactions ----

// isHelperInteraction is a command in the bot DM, the helper channel or a
// conversation thread.
func (b *Bridge) isHelperInteraction(ctx context.Context, in discord.Interaction) bool {
	if !b.helperEnabled() {
		return false
	}
	if in.GuildID == "" || (b.cfg.HelperChannel != "" && in.ChannelID == b.cfg.HelperChannel) {
		return true
	}
	c, ok, err := b.cfg.State.conversation(ctx, in.ChannelID)
	return err == nil && ok && c.Kind == convThread
}

func (b *Bridge) handleHelperInteraction(ctx context.Context, in discord.Interaction) {
	if (in.GuildID != "" && in.GuildID != b.cfg.GuildID) || !b.owners[in.UserID()] {
		b.respondNow(ctx, in, ephemeral("⛔ Sorry, only the Tailterm owner can use these commands."))
		return
	}
	if in.Type != discord.InteractionCommand || (in.Data.Name != "status" && in.Data.Name != "digest") {
		b.respondNow(ctx, in, ephemeral("Here only /status and /digest work; use a project channel for the rest."))
		return
	}
	raw, _ := json.Marshal(in)
	claimed, err := b.cfg.State.ClaimInbound(ctx, "interaction:"+in.ID, kindHelperInteraction, b.cfg.HelperTask, string(raw), time.Now().Add(3*time.Minute))
	if err != nil || !claimed {
		return
	}
	b.runHelperInteraction(ctx, in, false)
}

func (b *Bridge) runHelperInteraction(ctx context.Context, in discord.Interaction, resumed bool) {
	source := "interaction:" + in.ID
	if err := b.cfg.Discord.RespondInteraction(ctx, in.ID, in.Token, discord.InteractionResponse{Type: discord.ResponseDeferredMessage, Data: &discord.InteractionResponseData{Flags: discord.FlagEphemeral}}); err != nil && !resumed {
		b.logf("discord bridge: defer interaction %s: %v", in.ID, err)
		_ = b.cfg.State.FinishInbound(ctx, source, "not answered: "+err.Error())
		return
	}
	var r reply
	switch in.Data.Name {
	case "status":
		embed, err := b.summary(ctx, 45*time.Second)
		if err != nil {
			r = ephemeral("⚠️ %s.", hubMessage(err))
		} else {
			r = reply{Embeds: []discord.Embed{embed}}
		}
	case "digest":
		r = b.digestCommand(ctx, in)
	}
	edit := discord.MessageEdit{Content: &r.Content, Embeds: &r.Embeds, AllowedMentions: &discord.AllowedMentions{Parse: []string{}}}
	if r.Embeds == nil {
		empty := []discord.Embed{}
		edit.Embeds = &empty
	}
	if err := b.cfg.Discord.EditInteractionResponse(ctx, b.cfg.AppID, in.Token, edit); err != nil {
		b.logf("discord bridge: reply to interaction %s: %v", in.ID, err)
	}
	_ = b.cfg.State.FinishInbound(ctx, source, truncate(r.Content, 200))
}

// ---- digest ----

// digestState is the one digest destination and what it has already seen.
type digestState struct {
	On       bool            `json:"on"`
	Channel  string          `json:"channel"`
	Until    time.Time       `json:"until,omitempty"`
	LastHour string          `json:"lastHour"`
	Seen     map[string]bool `json:"seen"`
}

const digestKey = "helper:digest"

// digestCommand turns the digest on or off for the channel it ran in. On
// seeds everything current as seen, so nothing from before is sent.
func (b *Bridge) digestCommand(ctx context.Context, in discord.Interaction) reply {
	var state, duration string
	for _, o := range in.Data.Options {
		switch o.Name {
		case "state":
			state = strings.ToLower(strings.TrimSpace(o.String()))
		case "for":
			duration = strings.TrimSpace(o.String())
		}
	}
	var st digestState
	if _, err := b.cfg.State.getJSON(ctx, digestKey, &st); err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	switch state {
	case "off":
		st.On = false
		if err := b.cfg.State.setJSON(ctx, digestKey, st); err != nil {
			return ephemeral("⚠️ %s.", err.Error())
		}
		return ephemeral("🔕 Digest off.")
	case "on":
	default:
		return ephemeral("Use /digest on or /digest off.")
	}
	now := b.now()
	until := time.Time{}
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil || d < time.Minute || d > 7*24*time.Hour {
			return ephemeral("⚠️ Give how long from 1m to 168h, such as 8h, or leave it out.")
		}
		until = now.Add(d)
	}
	snaps, err := b.openSnapshots(ctx, 45*time.Second)
	if err != nil {
		return ephemeral("⚠️ %s.", hubMessage(err))
	}
	seen := map[string]bool{}
	for key := range b.digestEvents(ctx, snaps) {
		seen[key] = true
	}
	st = digestState{On: true, Channel: in.ChannelID, Until: until, LastHour: now.UTC().Truncate(time.Hour).Format(time.RFC3339), Seen: seen}
	if err := b.cfg.State.setJSON(ctx, digestKey, st); err != nil {
		return ephemeral("⚠️ %s.", err.Error())
	}
	end := "until /digest off"
	if !until.IsZero() {
		end = "until " + until.UTC().Format("Jan 2 15:04 UTC")
	}
	return ephemeral("🔔 Digest on here %s: a summary each hour, and a line when queued work finishes or something needs you.", end)
}

// digestEvents are the lines the digest sends once each, by key: queue
// entries that closed, were released or failed, and open owner requests.
func (b *Bridge) digestEvents(ctx context.Context, snaps []projectSnapshot) map[string]string {
	out := map[string]string{}
	for _, s := range snaps {
		project := clean(s.Task.Name)
		for _, e := range s.Queue.Entries {
			var what string
			switch {
			case e.State == "failed":
				what = "failed"
			case e.State == "closed":
				what = "closed"
			case e.ReleasedAt != "":
				what = "released"
			default:
				continue
			}
			key := fmt.Sprintf("entry:%s:%s", e.ID, what)
			out[key] = fmt.Sprintf("%s **%s** · %s %s", map[string]string{"failed": "❌", "closed": "✅", "released": "🚀"}[what], project, b.itemTitle(ctx, s.Task.ID, e.ItemID), what)
		}
		for _, o := range s.Obligations {
			if o.RecipientKind == api.ObligationRecipientOwner && o.State != api.ObligationClosed {
				out["owner:"+o.ID] = fmt.Sprintf("🙋 **%s** needs you: #%d %s", project, o.MessageSeq, truncate(clean(redactSecrets(o.Subject)), 80))
			}
		}
	}
	return out
}

func (b *Bridge) digestLoop(ctx context.Context) {
	interval := b.cfg.DigestInterval
	if interval == 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := b.digestTick(ctx); err != nil {
			b.logf("discord bridge: digest: %v", err)
		}
	}
}

// digestTick sends the hourly summary once per UTC hour and each new event
// line once. Every send has a fixed outbox key, so a restart or a repeated
// tick never duplicates one. Hub calls: one project list, plus a read of
// any project whose status-card snapshot is older than two minutes; the
// hourly summary also reads the helper's roster.
func (b *Bridge) digestTick(ctx context.Context) error {
	var st digestState
	ok, err := b.cfg.State.getJSON(ctx, digestKey, &st)
	if err != nil || !ok || !st.On {
		return err
	}
	now := b.now()
	if !st.Until.IsZero() && now.After(st.Until) {
		st.On = false
		return b.cfg.State.setJSON(ctx, digestKey, st)
	}
	snaps, err := b.openSnapshots(ctx, 2*time.Minute)
	if err != nil {
		return err
	}
	lane := helperLane("digest:" + st.Channel)
	row := func(key, text string, embeds []discord.Embed) OutboxRow {
		mk := "-# " + key
		return OutboxRow{Key: key, TaskID: lane, ChannelID: st.Channel, Kind: kindNotice,
			Payload: payloadJSON(outboxPayload{Message: discord.MessageSend{Content: strings.TrimSpace(text + "\n" + mk), Embeds: embeds}, Marker: mk})}
	}
	var rows []OutboxRow
	events := b.digestEvents(ctx, snaps)
	keys := make([]string, 0, len(events))
	for key := range events {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !st.Seen[key] {
			rows = append(rows, row("event:"+key, events[key], nil))
		}
	}
	if hour := now.UTC().Truncate(time.Hour).Format(time.RFC3339); hour != st.LastHour {
		helper, found, err := b.helperAgent(ctx)
		if err != nil {
			return err
		}
		embed := b.renderSummary(ctx, snaps, helper, classifyHelper(helper, found), time.Now())
		embed.Title = "Hourly digest"
		rows = append(rows, row("digest:"+st.Channel+":"+hour, "", []discord.Embed{embed}))
		st.LastHour = hour
	}
	if len(rows) > 0 {
		if err := b.cfg.State.Enqueue(ctx, lane, 0, rows); err != nil {
			return err
		}
		b.kick()
	}
	st.Seen = map[string]bool{}
	for _, key := range keys {
		st.Seen[key] = true
	}
	return b.cfg.State.setJSON(ctx, digestKey, st)
}

// redactHelperMessage is a board message as the project channel mirrors it:
// an owner helper's text passes through redactSecrets, like its DM copy.
// Other agents' messages are unchanged, and so is the board.
func redactHelperMessage(r roster, m api.Message) api.Message {
	if a, ok := r[m.From.AgentID]; !ok || m.From.AgentID == "" || a.Role != api.AgentRoleOwnerHelper {
		return m
	}
	m.Text = redactSecrets(m.Text)
	if m.Envelope != nil {
		env := *m.Envelope
		env.Subject = redactSecrets(env.Subject)
		env.Body.Text = redactSecrets(env.Body.Text)
		m.Envelope = &env
	}
	return m
}
