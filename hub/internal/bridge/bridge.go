package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Config is everything the bridge needs; main builds it from flags and files.
type Config struct {
	Hub     *api.Client
	Discord *discord.Client
	// Gateway runs the event connection; nil in tests that inject events.
	Gateway *discord.Gateway

	AppID   string
	GuildID string
	Owners  []string // Discord user IDs allowed to write and act

	ActiveCategory  string // category names, created if missing
	ArchiveCategory string
	TailOSURL       string

	State *State
	Log   func(string, ...any)

	MirrorInterval time.Duration // hub message poll, default 2 s
	SyncInterval   time.Duration // project reconcile, default 30 s
	CardInterval   time.Duration // minimum time between card edits, default 30 s
	InitialHistory int64         // board messages mirrored into a new channel, default 10
}

// Bridge mirrors the hub to Discord and Discord owner input to the hub.
type Bridge struct {
	cfg       Config
	owners    map[string]bool
	ownerList []string
	wake      chan struct{}

	mu         sync.Mutex
	categories map[string]string // name → category ID
	botID      string
	events     chan discord.Dispatch
}

func New(cfg Config) (*Bridge, error) {
	if cfg.Hub == nil || cfg.Discord == nil || cfg.State == nil || cfg.GuildID == "" || cfg.AppID == "" || len(cfg.Owners) == 0 {
		return nil, errors.New("bridge: hub, discord, state, guild, application and at least one owner are required")
	}
	if cfg.ActiveCategory == "" {
		cfg.ActiveCategory = "Tailterm projects"
	}
	if cfg.ArchiveCategory == "" {
		cfg.ArchiveCategory = "Tailterm archive"
	}
	if cfg.MirrorInterval == 0 {
		cfg.MirrorInterval = 2 * time.Second
	}
	if cfg.SyncInterval == 0 {
		cfg.SyncInterval = 30 * time.Second
	}
	if cfg.CardInterval == 0 {
		cfg.CardInterval = 30 * time.Second
	}
	if cfg.InitialHistory == 0 {
		cfg.InitialHistory = 10
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	b := &Bridge{cfg: cfg, owners: map[string]bool{}, wake: make(chan struct{}, 1), categories: map[string]string{}, events: make(chan discord.Dispatch, 256)}
	for _, id := range cfg.Owners {
		if !b.owners[id] {
			b.owners[id] = true
			b.ownerList = append(b.ownerList, id)
		}
	}
	return b, nil
}

func (b *Bridge) logf(format string, args ...any) { b.cfg.Log(format, args...) }

func (b *Bridge) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Commands are the guild slash commands (broker phase 2b scope item 11).
var Commands = []discord.Command{
	{Name: "status", Description: "Show this project's agents and open work"},
	{Name: "stalled", Description: "List overdue work in this project, oldest first"},
	{Name: "nudge", Description: "Wake an agent about overdue work now", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "target", Description: "An agent name, a message number or an obligation ID", Required: true}}},
	{Name: "say", Description: "Post a message to this project's board", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "text", Description: "What to say", Required: true},
		{Type: discord.OptionString, Name: "agent", Description: "Send it to one agent instead of everyone"}}},
	{Name: "reassign", Description: "Move an open obligation to another agent", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "obligation", Description: "A message number or an obligation ID", Required: true},
		{Type: discord.OptionString, Name: "agent", Description: "The agent to give it to", Required: true}}},
}

// Run starts every loop and blocks until ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	me, err := b.cfg.Discord.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("discord identity: %w", err)
	}
	b.mu.Lock()
	b.botID = me.ID
	b.mu.Unlock()
	if err := b.cfg.Discord.SetGuildCommands(ctx, b.cfg.AppID, b.cfg.GuildID, Commands); err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	var wg sync.WaitGroup
	loops := []func(context.Context){b.syncLoop, b.outboxLoop, b.inboundRetryLoop, b.eventLoop}
	for _, loop := range loops {
		wg.Add(1)
		go func(loop func(context.Context)) {
			defer wg.Done()
			loop(ctx)
		}(loop)
	}
	var gatewayErr error
	if g := b.cfg.Gateway; g != nil {
		g.Handle = b.Dispatch
		gatewayErr = g.Run(ctx)
	} else {
		<-ctx.Done()
	}
	wg.Wait()
	if errors.Is(gatewayErr, discord.ErrFatalClose) {
		return gatewayErr
	}
	return ctx.Err()
}

// Dispatch receives Gateway events. Interactions are answered at once, each
// on its own goroutine, because Discord needs a reply within 3 seconds;
// messages are queued and handled in order.
func (b *Bridge) Dispatch(d discord.Dispatch) {
	switch d.Type {
	case "INTERACTION_CREATE":
		var in discord.Interaction
		if json.Unmarshal(d.Data, &in) == nil {
			go b.handleInteraction(context.Background(), in)
		}
	case "MESSAGE_CREATE", "READY", "RESUMED":
		select {
		case b.events <- d:
		default:
			b.logf("discord bridge: event queue full; %s will be recovered by backfill", d.Type)
		}
	}
}

func (b *Bridge) eventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-b.events:
			switch d.Type {
			case "MESSAGE_CREATE":
				var m discord.Message
				if json.Unmarshal(d.Data, &m) == nil {
					b.handleMessage(ctx, m)
				}
			case "READY", "RESUMED":
				b.backfill(ctx)
			}
		}
	}
}

// ---- hub → Discord ----

func (b *Bridge) syncLoop(ctx context.Context) {
	var lastReconcile time.Time
	ticker := time.NewTicker(b.cfg.MirrorInterval)
	defer ticker.Stop()
	for {
		if time.Since(lastReconcile) >= b.cfg.SyncInterval {
			if err := b.reconcileProjects(ctx); err != nil {
				b.logf("discord bridge: reconcile projects: %v", err)
			} else {
				lastReconcile = time.Now()
			}
		}
		if err := b.mirrorAll(ctx); err != nil {
			b.logf("discord bridge: mirror: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileProjects gives each open project a channel, archives closed
// projects' channels and refreshes status cards.
func (b *Bridge) reconcileProjects(ctx context.Context) error {
	tasks, err := b.cfg.Hub.ListTasks(ctx)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		m, mapErr := b.cfg.State.Mapping(ctx, t.ID)
		switch {
		case t.Status == api.TaskOpen && (errors.Is(mapErr, ErrNoMapping) || (mapErr == nil && m.ChannelID == "" && !m.Archived)):
			if err := b.provision(ctx, t, mapErr == nil); err != nil {
				b.logf("discord bridge: provision %s: %v", t.ID, err)
			}
		case t.Status != api.TaskOpen && mapErr == nil && !m.Archived && m.ChannelID != "":
			if err := b.archive(ctx, t, m); err != nil {
				b.logf("discord bridge: archive %s: %v", t.ID, err)
			}
		}
	}
	mappings, err := b.cfg.State.Mappings(ctx)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if !m.Archived && m.ChannelID != "" {
			if err := b.refreshCard(ctx, m); err != nil {
				b.logf("discord bridge: status card %s: %v", m.TaskID, err)
			}
		}
	}
	return nil
}

func channelMarker(taskID string) string { return "tailterm:" + taskID }

var slugDrop = regexp.MustCompile(`[^a-z0-9]+`)

func channelName(name string) string {
	s := strings.Trim(slugDrop.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "project"
	}
	if len(s) > 90 {
		s = strings.TrimRight(s[:90], "-")
	}
	return s
}

// category finds or creates a category by name.
func (b *Bridge) category(ctx context.Context, channels []discord.Channel, name string) (string, error) {
	b.mu.Lock()
	id := b.categories[name]
	b.mu.Unlock()
	for _, c := range channels {
		if c.Type == discord.ChannelCategory && (c.ID == id || (id == "" && strings.EqualFold(c.Name, name))) {
			b.mu.Lock()
			b.categories[name] = c.ID
			b.mu.Unlock()
			return c.ID, nil
		}
	}
	c, err := b.cfg.Discord.CreateChannel(ctx, b.cfg.GuildID, discord.CreateChannel{Name: name, Type: discord.ChannelCategory})
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.categories[name] = c.ID
	b.mu.Unlock()
	return c.ID, nil
}

// provision adopts the project's channel if one already carries its marker
// (for example after an uncertain create), and otherwise creates it.
func (b *Bridge) provision(ctx context.Context, t api.Task, remapping bool) error {
	channels, err := b.cfg.Discord.GuildChannels(ctx, b.cfg.GuildID)
	if err != nil {
		return err
	}
	channelID := ""
	for _, c := range channels {
		if c.Type == discord.ChannelText && strings.Contains(c.Topic, channelMarker(t.ID)) {
			channelID = c.ID
			break
		}
	}
	if channelID == "" {
		parent, err := b.category(ctx, channels, b.cfg.ActiveCategory)
		if err != nil {
			return err
		}
		c, err := b.cfg.Discord.CreateChannel(ctx, b.cfg.GuildID, discord.CreateChannel{
			Name: channelName(t.Name), Type: discord.ChannelText, ParentID: parent,
			Topic: truncate(fmt.Sprintf("Tailterm project %s. Messages here go to its board. %s", clean(t.Name), channelMarker(t.ID)), 1000),
		})
		if err != nil {
			return err
		}
		channelID = c.ID
		b.logf("discord bridge: created #%s for %s", c.Name, t.ID)
	}
	start := int64(0)
	if !remapping {
		detail, err := b.cfg.Hub.GetTask(ctx, t.ID)
		if err != nil {
			return err
		}
		if start = detail.LatestSeq - b.cfg.InitialHistory; start < 0 {
			start = 0
		}
	}
	if err := b.cfg.State.SetChannel(ctx, t.ID, channelID, start); err != nil {
		return err
	}
	// Owner messages older than the channel cannot exist, so the channel's
	// own ID is a safe starting point for backfill.
	return b.cfg.State.SetIngestAfter(ctx, t.ID, channelID)
}

// archive moves a closed project's channel to the archive category. Writes
// there are refused by the bridge itself (the bot has no Manage Roles).
func (b *Bridge) archive(ctx context.Context, t api.Task, m Mapping) error {
	channels, err := b.cfg.Discord.GuildChannels(ctx, b.cfg.GuildID)
	if err != nil {
		return err
	}
	parent, err := b.category(ctx, channels, b.cfg.ArchiveCategory)
	if err != nil {
		return err
	}
	if _, err := b.cfg.Discord.ModifyChannel(ctx, m.ChannelID, discord.ModifyChannel{ParentID: &parent}); err != nil {
		if discord.IsCode(err, discord.CodeUnknownChannel) {
			return b.cfg.State.SetArchived(ctx, t.ID)
		}
		return err
	}
	// The mirror runs one last time so the closing messages arrive.
	if err := b.mirror(ctx, m); err != nil {
		b.logf("discord bridge: final mirror %s: %v", t.ID, err)
	}
	mk := marker(0, 1, 1, "archived")
	if err := b.cfg.State.Enqueue(ctx, t.ID, 0, []OutboxRow{{Key: "archived:" + t.ID, TaskID: t.ID, Kind: kindNotice,
		Payload: payloadJSON(outboxPayload{Message: discord.MessageSend{Content: "📦 This project is closed and the channel is archived. Messages here are no longer sent to Tailterm.\n" + mk}, Marker: mk})}}); err != nil {
		return err
	}
	b.kick()
	return b.cfg.State.SetArchived(ctx, t.ID)
}

func (b *Bridge) mirrorAll(ctx context.Context) error {
	mappings, err := b.cfg.State.Mappings(ctx)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.Archived || m.ChannelID == "" {
			continue
		}
		if err := b.mirror(ctx, m); err != nil {
			return fmt.Errorf("%s: %w", m.TaskID, err)
		}
	}
	return nil
}

// mirror pages new board messages into the outbox. Each page's rows and the
// cursor move together, so a crash neither skips nor repeats a message.
func (b *Bridge) mirror(ctx context.Context, m Mapping) error {
	after := m.MirrorAfter
	var task *api.TaskDetail
	for {
		msgs, err := b.cfg.Hub.ListMessages(ctx, m.TaskID, after, "", 100)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		if task == nil {
			detail, err := b.cfg.Hub.GetTask(ctx, m.TaskID)
			if err != nil {
				return err
			}
			task = &detail
		}
		r := roster{}
		for _, a := range task.Agents {
			r[a.ID] = a
		}
		var rows []OutboxRow
		for _, msg := range msgs {
			if _, known := r[msg.From.AgentID]; msg.From.AgentID != "" && !known {
				// A message from an agent added after the roster was read.
				if detail, err := b.cfg.Hub.GetTask(ctx, m.TaskID); err == nil {
					task = &detail
					for _, a := range task.Agents {
						r[a.ID] = a
					}
				}
			}
			rows = append(rows, b.renderMessage(task.Task, r, msg)...)
			after = msg.Seq
		}
		if err := b.cfg.State.Enqueue(ctx, m.TaskID, after, rows); err != nil {
			return err
		}
		if len(rows) > 0 {
			b.kick()
		}
		if len(msgs) < 100 {
			return nil
		}
	}
}

// refreshCard edits the pinned status card when the project changed, at
// most once per CardInterval, and creates and pins it when missing.
func (b *Bridge) refreshCard(ctx context.Context, m Mapping) error {
	detail, err := b.cfg.Hub.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}
	obligations, err := b.cfg.Hub.ListObligations(ctx, m.TaskID, "", "", true, false)
	if err != nil {
		return err
	}
	send, hash := renderCard(detail.Task, detail.Agents, obligations)
	if hash == m.CardHash && m.CardMessageID != "" {
		return nil
	}
	if m.CardMessageID != "" && time.Since(m.CardEditedAt) < b.cfg.CardInterval {
		return nil
	}
	send.AllowedMentions = discord.AllowedMentions{Parse: []string{}}
	if m.CardMessageID != "" {
		_, err := b.cfg.Discord.EditMessage(ctx, m.ChannelID, m.CardMessageID, discord.MessageEdit{Content: &send.Content, Embeds: &send.Embeds, AllowedMentions: &send.AllowedMentions})
		if err == nil {
			return b.cfg.State.SetCard(ctx, m.TaskID, m.CardMessageID, hash)
		}
		if !discord.IsCode(err, discord.CodeUnknownMessage) {
			return b.channelError(ctx, m.TaskID, err)
		}
	}
	// Reuse a card an earlier, uncertain create already posted.
	cardID, err := b.findOwn(ctx, m.ChannelID, cardMarker)
	if err != nil {
		return b.channelError(ctx, m.TaskID, err)
	}
	if cardID == "" {
		msg, err := b.cfg.Discord.CreateMessage(ctx, m.ChannelID, send)
		if err != nil {
			return b.channelError(ctx, m.TaskID, err)
		}
		cardID = msg.ID
	} else if _, err := b.cfg.Discord.EditMessage(ctx, m.ChannelID, cardID, discord.MessageEdit{Content: &send.Content, Embeds: &send.Embeds, AllowedMentions: &send.AllowedMentions}); err != nil {
		return b.channelError(ctx, m.TaskID, err)
	}
	if err := b.cfg.Discord.PinMessage(ctx, m.ChannelID, cardID); err != nil {
		b.logf("discord bridge: pin status card in %s: %v", m.TaskID, err)
	}
	return b.cfg.State.SetCard(ctx, m.TaskID, cardID, hash)
}

// channelError forgets a channel Discord says no longer exists, so the next
// reconcile provisions a new one.
func (b *Bridge) channelError(ctx context.Context, taskID string, err error) error {
	if discord.IsCode(err, discord.CodeUnknownChannel) {
		b.logf("discord bridge: channel for %s is gone; it will be recreated", taskID)
		if ferr := b.cfg.State.ForgetChannel(ctx, taskID); ferr != nil {
			return ferr
		}
	}
	return err
}

// findOwn looks for the bot's own recent message containing a marker line.
func (b *Bridge) findOwn(ctx context.Context, channelID, mk string) (string, error) {
	recent, err := b.cfg.Discord.RecentMessages(ctx, channelID, 100)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	bot := b.botID
	b.mu.Unlock()
	for _, msg := range recent {
		if msg.Author.ID == bot && hasLine(msg.Content, mk) {
			return msg.ID, nil
		}
	}
	return "", nil
}

func hasLine(content, line string) bool {
	for _, l := range strings.Split(content, "\n") {
		if l == line {
			return true
		}
	}
	return false
}

func (b *Bridge) outboxLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := b.drainOutbox(ctx); err != nil && ctx.Err() == nil {
			b.logf("discord bridge: outbox: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-b.wake:
		}
	}
}

// drainOutbox sends due rows in order. A row that fails transiently holds
// back later rows for the same project, so a channel never shows messages
// out of order.
func (b *Bridge) drainOutbox(ctx context.Context) error {
	rows, err := b.cfg.State.PendingOutbox(ctx, 200)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	now := time.Now()
	for _, row := range rows {
		if held[row.TaskID] {
			continue
		}
		if row.NextAt.After(now) {
			held[row.TaskID] = true
			continue
		}
		if err := b.send(ctx, row); err != nil {
			held[row.TaskID] = true
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

func (b *Bridge) send(ctx context.Context, row OutboxRow) error {
	m, err := b.cfg.State.Mapping(ctx, row.TaskID)
	if err != nil || m.ChannelID == "" {
		// No channel yet (or it is being re-provisioned): try again later.
		return b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(5*time.Second), "no channel")
	}
	var p outboxPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		return b.cfg.State.MarkFailed(ctx, row.ID, "bad payload: "+err.Error())
	}
	// A retried row may already have landed: look for its marker first.
	if row.Attempts > 0 && p.Marker != "" {
		if id, err := b.findOwn(ctx, m.ChannelID, p.Marker); err == nil && id != "" {
			return b.cfg.State.MarkSent(ctx, row, id)
		}
	}
	send := p.Message
	send.AllowedMentions = discord.AllowedMentions{Parse: []string{}, Users: p.MentionUser}
	send.Nonce = "tt" + strconv.FormatInt(row.ID, 10)
	send.EnforceNonce = true
	msg, err := b.cfg.Discord.CreateMessage(ctx, m.ChannelID, send)
	if err == nil {
		return b.cfg.State.MarkSent(ctx, row, msg.ID)
	}
	if discord.IsCode(err, discord.CodeUnknownChannel) {
		_ = b.channelError(ctx, row.TaskID, err)
		_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(5*time.Second), err.Error())
		return err
	}
	if discord.Permanent(err) {
		b.logf("discord bridge: dropping %s: %v", row.Key, err)
		_ = b.cfg.State.MarkFailed(ctx, row.ID, err.Error())
		return nil
	}
	backoff := time.Duration(1<<min(row.Attempts, 6)) * time.Second
	_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(backoff), err.Error())
	return err
}

// ---- Discord → hub ----

// handleMessage takes an owner's message in a project channel to the board.
func (b *Bridge) handleMessage(ctx context.Context, msg discord.Message) {
	if msg.GuildID != "" && msg.GuildID != b.cfg.GuildID {
		return
	}
	if msg.Author.Bot || msg.WebhookID != "" || (msg.Type != 0 && msg.Type != 19) {
		return
	}
	m, err := b.cfg.State.MappingByChannel(ctx, msg.ChannelID)
	if err != nil {
		return
	}
	if !b.owners[msg.Author.ID] {
		return
	}
	raw, _ := json.Marshal(msg)
	claimed, err := b.cfg.State.ClaimInbound(ctx, msg.ID, "message", m.TaskID, string(raw))
	if err != nil {
		b.logf("discord bridge: record message %s: %v", msg.ID, err)
		return
	}
	if err := b.cfg.State.SetIngestAfter(ctx, m.TaskID, msg.ID); err != nil {
		b.logf("discord bridge: ingest cursor: %v", err)
	}
	if !claimed {
		return
	}
	b.postInbound(ctx, m, msg, 0)
}

func (b *Bridge) reply(ctx context.Context, msg discord.Message, text string) {
	no := false
	_, err := b.cfg.Discord.CreateMessage(ctx, msg.ChannelID, discord.MessageSend{
		Content: text, AllowedMentions: discord.AllowedMentions{Parse: []string{}},
		MessageReference: &discord.MessageReference{MessageID: msg.ID, FailIfNotExists: &no},
	})
	if err != nil {
		b.logf("discord bridge: reply to %s: %v", msg.ID, err)
	}
}

func (b *Bridge) postInbound(ctx context.Context, m Mapping, msg discord.Message, attempts int) {
	if m.Archived {
		b.reply(ctx, msg, "⛔ This project is closed. Nothing was posted to Tailterm.")
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: project closed")
		return
	}
	text := strings.TrimSpace(msg.Content)
	if text == "" {
		b.reply(ctx, msg, "⛔ Only text can be posted to Tailterm; attachments aren't supported yet. Nothing was posted.")
		_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: no text")
		return
	}
	req := api.PostMessageRequest{Text: text, RequestID: "discord-msg-" + msg.ID,
		Source: &api.MessageSource{Kind: api.SourceDiscord, ID: msg.ID, UserID: msg.Author.ID}}
	if ref := msg.MessageReference; ref != nil && ref.MessageID != "" {
		mirrored, ok, err := b.cfg.State.MirroredMessage(ctx, ref.MessageID)
		if err != nil {
			b.retryInbound(ctx, msg.ID, attempts, err)
			return
		}
		if ok && mirrored.TaskID == m.TaskID {
			req.ReplyTo = mirrored.Seq
			req.To = mirrored.AgentID // replying to an agent's message addresses that agent
		}
	}
	posted, err := b.cfg.Hub.PostMessage(ctx, m.TaskID, req)
	if err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != http.StatusTooManyRequests {
			b.reply(ctx, msg, "⛔ Tailterm refused this message: "+h.Msg+". Nothing was posted.")
			_ = b.cfg.State.FinishInbound(ctx, msg.ID, "rejected: "+h.Msg)
			return
		}
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	_ = b.cfg.State.FinishInbound(ctx, msg.ID, fmt.Sprintf("posted #%d", posted.Seq))
	if err := b.cfg.Discord.React(ctx, msg.ChannelID, msg.ID, "✅"); err != nil {
		b.logf("discord bridge: react to %s: %v", msg.ID, err)
	}
}

func (b *Bridge) retryInbound(ctx context.Context, id string, attempts int, err error) {
	backoff := time.Duration(1<<min(attempts, 6)) * 2 * time.Second
	b.logf("discord bridge: message %s not posted yet (%v); retrying in %s", id, err, backoff)
	_ = b.cfg.State.RetryInbound(ctx, id, time.Now().Add(backoff), err.Error())
}

// inboundRetryLoop re-posts owner messages the hub did not take yet; the
// request ID makes a repeat that already succeeded return the original.
func (b *Bridge) inboundRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pending, err := b.cfg.State.PendingInbound(ctx, time.Now().UTC())
		if err != nil {
			b.logf("discord bridge: pending inbound: %v", err)
			continue
		}
		for _, in := range pending {
			var msg discord.Message
			if json.Unmarshal([]byte(in.Payload), &msg) != nil {
				_ = b.cfg.State.FinishInbound(ctx, in.SourceID, "rejected: unreadable")
				continue
			}
			m, err := b.cfg.State.Mapping(ctx, in.TaskID)
			if err != nil {
				continue
			}
			b.postInbound(ctx, m, msg, in.Attempts+1)
		}
	}
}

// backfill ingests owner messages sent while the Gateway was disconnected.
func (b *Bridge) backfill(ctx context.Context) {
	mappings, err := b.cfg.State.Mappings(ctx)
	if err != nil {
		b.logf("discord bridge: backfill: %v", err)
		return
	}
	for _, m := range mappings {
		if m.ChannelID == "" || m.IngestAfter == "" {
			continue
		}
		after := m.IngestAfter
		for page := 0; page < 20; page++ {
			msgs, err := b.cfg.Discord.ChannelMessages(ctx, m.ChannelID, after, 100)
			if err != nil {
				b.logf("discord bridge: backfill %s: %v", m.TaskID, err)
				break
			}
			for _, msg := range msgs {
				if msg.GuildID == "" {
					msg.GuildID = b.cfg.GuildID
				}
				b.handleMessage(ctx, msg)
				after = msg.ID
			}
			if len(msgs) < 100 {
				break
			}
		}
	}
}
