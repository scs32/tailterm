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
	"sync/atomic"
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
	runCtx     context.Context // interactions run under it, so shutdown waits for them
	inflight   sync.WaitGroup
	// needBackfill is set when a Gateway event could not be queued; the event
	// loop then backfills without waiting for its periodic pass.
	needBackfill atomic.Bool
}

// BackfillInterval is how often every channel is read for owner messages the
// Gateway did not deliver; the ingest cursor makes it cheap when caught up.
const BackfillInterval = time.Minute

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
	b := &Bridge{cfg: cfg, owners: map[string]bool{}, wake: make(chan struct{}, 1), categories: map[string]string{}, events: make(chan discord.Dispatch, 256), runCtx: context.Background()}
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
	// Broker phase 3.
	{Name: "extend", Description: "Give an open obligation more time", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "obligation", Description: "A message number or an obligation ID", Required: true},
		{Type: discord.OptionString, Name: "for", Description: "How long from now, such as 30m or 2h", Required: true},
		{Type: discord.OptionString, Name: "reason", Description: "Why (shown on the board)"}}},
	{Name: "answer", Description: "Answer a question or resolve a block on the recipient's behalf", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "obligation", Description: "A message number or an obligation ID", Required: true},
		{Type: discord.OptionString, Name: "text", Description: "The answer", Required: true}}},
	{Name: "cancel", Description: "Cancel an open obligation", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "obligation", Description: "A message number or an obligation ID", Required: true},
		{Type: discord.OptionString, Name: "reason", Description: "Why (the agent is told)", Required: true}}},
	{Name: "resume", Description: "Re-enable wake-ups for a retired agent", Options: []discord.CommandOption{
		{Type: discord.OptionString, Name: "agent", Description: "The agent's name", Required: true}}},
}

// Run starts every loop and blocks until ctx ends or the Gateway fails for
// good (a fatal close such as disallowed intents), in which case every loop
// stops and the error is returned so the process exits and is restarted.
func (b *Bridge) Run(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	b.runCtx = ctx
	me, err := b.cfg.Discord.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("discord identity: %w", err)
	}
	b.mu.Lock()
	b.botID = me.ID
	b.mu.Unlock()
	if err := b.registerCommands(ctx); err != nil {
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
	cancel()
	wg.Wait()
	b.inflight.Wait()
	if gatewayErr != nil && parent.Err() == nil {
		return gatewayErr
	}
	return parent.Err()
}

// registerCommands replaces the guild's commands only when they changed,
// so restarts do not spend Discord's daily command-update budget.
func (b *Bridge) registerCommands(ctx context.Context) error {
	raw, _ := json.Marshal(Commands)
	want := b.cfg.AppID + "|" + b.cfg.GuildID + "|" + string(raw)
	if have, err := b.cfg.State.Get(ctx, "commands"); err == nil && have == want {
		return nil
	}
	if err := b.cfg.Discord.SetGuildCommands(ctx, b.cfg.AppID, b.cfg.GuildID, Commands); err != nil {
		return err
	}
	return b.cfg.State.Set(ctx, "commands", want)
}

// Dispatch receives Gateway events. Interactions are answered at once, each
// on its own goroutine, because Discord needs a reply within 3 seconds;
// messages are queued and handled in order.
func (b *Bridge) Dispatch(d discord.Dispatch) {
	switch d.Type {
	case "INTERACTION_CREATE":
		var in discord.Interaction
		if json.Unmarshal(d.Data, &in) == nil {
			b.inflight.Add(1)
			go func() {
				defer b.inflight.Done()
				b.handleInteraction(b.runCtx, in)
			}()
		}
	case "MESSAGE_CREATE", "READY", "RESUMED":
		select {
		case b.events <- d:
		default:
			b.needBackfill.Store(true)
			b.logf("discord bridge: event queue full; %s will be recovered by backfill", d.Type)
		}
	}
}

func (b *Bridge) eventLoop(ctx context.Context) {
	ticker := time.NewTicker(BackfillInterval)
	defer ticker.Stop()
	for {
		if b.needBackfill.Swap(false) {
			b.backfill(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.backfill(ctx)
		case d := <-b.events:
			switch d.Type {
			case "MESSAGE_CREATE":
				var m discord.Message
				if json.Unmarshal(d.Data, &m) == nil && b.handleMessage(ctx, m) != nil {
					b.needBackfill.Store(true) // backfill will read it again
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
		// The newest few board messages, newest first. (TaskDetail.LatestSeq
		// counts events, a different sequence, so it cannot place the cursor.)
		recent, err := b.cfg.Hub.LatestMessages(ctx, t.ID, int(b.cfg.InitialHistory))
		if err != nil {
			return err
		}
		for _, m := range recent {
			if start == 0 || m.Seq-1 < start {
				start = m.Seq - 1
			}
		}
	}
	return b.cfg.State.SetChannel(ctx, t.ID, channelID, start)
}

// archive moves a closed project's channel to the archive category. Writes
// there are refused by the bridge itself (the bot has no Manage Roles). The
// board's last messages are mirrored first; if that fails nothing is marked
// and the next reconcile tries again.
func (b *Bridge) archive(ctx context.Context, t api.Task, m Mapping) error {
	if err := b.mirror(ctx, m); err != nil {
		return fmt.Errorf("final mirror: %w", err)
	}
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

// cardObligations keeps the historical closed-row fetch bounded.
func (b *Bridge) cardObligations(ctx context.Context, taskID string) ([]api.Obligation, error) {
	open, err := b.cfg.Hub.ListObligations(ctx, taskID, "", "", true, false)
	if err != nil {
		return nil, err
	}
	recent, err := b.cfg.Hub.ListRecentWithdrawn(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return append(open, recent...), nil
}

// refreshCard edits the pinned status card when the project changed, at
// most once per CardInterval, and creates and pins it when missing. A card
// that could not be pinned is pinned again at most every PinRetry.
func (b *Bridge) refreshCard(ctx context.Context, m Mapping) error {
	if m.CardMessageID != "" && !m.CardPinned && time.Since(m.CardPinAt) >= PinRetry {
		b.pinCard(ctx, m.TaskID, m.ChannelID, m.CardMessageID)
	}
	detail, err := b.cfg.Hub.GetTask(ctx, m.TaskID)
	if err != nil {
		return err
	}
	obligations, err := b.cardObligations(ctx, m.TaskID)
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
	edit := discord.MessageEdit{Content: &send.Content, Embeds: &send.Embeds, AllowedMentions: &send.AllowedMentions}
	if m.CardMessageID != "" {
		_, err := b.cfg.Discord.EditMessage(ctx, m.ChannelID, m.CardMessageID, edit)
		if err == nil {
			return b.cfg.State.SetCard(ctx, m.TaskID, m.CardMessageID, hash)
		}
		if !discord.IsCode(err, discord.CodeUnknownMessage) {
			return b.channelError(ctx, m.TaskID, err)
		}
	}
	// A create whose reply was lost left a card behind: find it first.
	cardID := ""
	if !m.CardAttemptAt.IsZero() {
		if cardID, err = b.findOwn(ctx, m.ChannelID, cardMarker, m.CardAttemptAt); err != nil {
			return b.channelError(ctx, m.TaskID, err)
		}
	}
	if cardID == "" {
		if err := b.cfg.State.SetCardAttempt(ctx, m.TaskID); err != nil {
			return err
		}
		msg, err := b.cfg.Discord.CreateMessage(ctx, m.ChannelID, send)
		if err != nil {
			return b.channelError(ctx, m.TaskID, err)
		}
		cardID = msg.ID
	} else if _, err := b.cfg.Discord.EditMessage(ctx, m.ChannelID, cardID, edit); err != nil {
		return b.channelError(ctx, m.TaskID, err)
	}
	if err := b.cfg.State.SetCard(ctx, m.TaskID, cardID, hash); err != nil {
		return err
	}
	b.pinCard(ctx, m.TaskID, m.ChannelID, cardID)
	return nil
}

// PinRetry spaces out pin attempts, for example while the bot lacks the
// Pin Messages permission.
const PinRetry = 10 * time.Minute

func (b *Bridge) pinCard(ctx context.Context, taskID, channelID, cardID string) {
	err := b.cfg.Discord.PinMessage(ctx, channelID, cardID)
	if err != nil {
		b.logf("discord bridge: pin status card in %s: %v", taskID, err)
	}
	if err := b.cfg.State.SetCardPinned(ctx, taskID, err == nil); err != nil {
		b.logf("discord bridge: record pin for %s: %v", taskID, err)
	}
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

// findOwn looks for the bot's own message carrying a marker line, among
// messages sent since shortly before `since` (the first attempt). It reads
// forward from that point, so an old send is found however many messages
// followed it. An error means "unknown", never "absent".
func (b *Bridge) findOwn(ctx context.Context, channelID, mk string, since time.Time) (string, error) {
	after := snowflakeAt(since.Add(-2 * time.Minute))
	b.mu.Lock()
	bot := b.botID
	b.mu.Unlock()
	for page := 0; page < maxReconcilePages; page++ {
		msgs, err := b.cfg.Discord.ChannelMessages(ctx, channelID, after, 100)
		if err != nil {
			return "", err
		}
		for _, msg := range msgs {
			if msg.Author.ID == bot && hasLine(msg.Content, mk) {
				return msg.ID, nil
			}
			after = msg.ID
		}
		if len(msgs) < 100 {
			return "", nil
		}
	}
	// So much traffic followed that the send would have been found by now if
	// it had landed: treat it as absent rather than blocking forever.
	b.logf("discord bridge: marker %q not found in %d pages; treating it as not sent", mk, maxReconcilePages)
	return "", nil
}

// maxReconcilePages bounds a reconciliation search (100 messages a page).
const maxReconcilePages = 50

// discordEpoch is the first second of 2015, the zero point of snowflakes.
const discordEpoch = 1420070400000

// snowflakeAt is the smallest Discord ID that could have been created at t.
func snowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatInt(ms<<22, 10)
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

// drainOutbox sends due rows in order within each project. Only the head
// of each project's queue is considered, so a failing project holds back its
// own later sends and nobody else's.
func (b *Bridge) drainOutbox(ctx context.Context) error {
	for sent := 0; sent < 500; {
		heads, err := b.cfg.State.PendingHeads(ctx)
		if err != nil {
			return err
		}
		progress := false
		now := time.Now()
		for _, row := range heads {
			if row.NextAt.After(now) {
				continue
			}
			if b.send(ctx, row) == nil {
				progress = true
				sent++
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if !progress {
			return nil
		}
	}
	return nil
}

// send delivers one row. It returns nil only when the row is settled (sent,
// or dropped as permanently undeliverable); any other outcome holds the
// project's queue and schedules a retry.
func (b *Bridge) send(ctx context.Context, row OutboxRow) error {
	m, err := b.cfg.State.Mapping(ctx, row.TaskID)
	if err != nil || m.ChannelID == "" {
		// No channel yet (or it is being re-provisioned): wait for it.
		_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(5*time.Second), "no channel")
		return errNoChannel
	}
	var p outboxPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil {
		return b.cfg.State.MarkFailed(ctx, row.ID, "bad payload: "+err.Error())
	}
	// A row tried before may already be in Discord (its reply lost, or the
	// bridge killed before recording it): find it by marker before sending.
	if row.Attempts > 0 && p.Marker != "" {
		id, err := b.findOwn(ctx, m.ChannelID, p.Marker, row.FirstAttemptAt)
		if err != nil {
			_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(30*time.Second), "reconcile: "+err.Error())
			return b.channelError(ctx, row.TaskID, err)
		}
		if id != "" {
			return b.cfg.State.MarkSent(ctx, row, id)
		}
	}
	if row, err = b.cfg.State.MarkAttempt(ctx, row.ID); err != nil {
		return err
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
		_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(5*time.Second), err.Error())
		return b.channelError(ctx, row.TaskID, err)
	}
	if discord.Permanent(err) {
		b.logf("discord bridge: dropping %s: %v", row.Key, err)
		return b.cfg.State.MarkFailed(ctx, row.ID, err.Error())
	}
	backoff := time.Duration(1<<min(row.Attempts, 6)) * time.Second
	_ = b.cfg.State.MarkRetry(ctx, row.ID, time.Now().Add(backoff), err.Error())
	return err
}

var errNoChannel = errors.New("no channel")

// ---- Discord → hub ----

// handleMessage takes an owner's message in a project channel to the board.
// It never moves the ingest cursor: only backfill does, reading every
// message in order, so a gap can never be skipped by a newer live message.
// It returns an error only when an owner message could not be recorded, so
// backfill stops there and reads it again next time.
func (b *Bridge) handleMessage(ctx context.Context, msg discord.Message) error {
	m, ok := b.ownerMessage(ctx, msg)
	if !ok {
		return nil
	}
	raw, _ := json.Marshal(msg)
	claimed, err := b.cfg.State.ClaimInbound(ctx, msg.ID, "message", m.TaskID, string(raw), time.Now().Add(time.Minute))
	if err != nil {
		b.logf("discord bridge: record message %s: %v", msg.ID, err)
		return err
	}
	if claimed {
		b.postInbound(ctx, m, msg, 0)
	}
	return nil
}

// ownerMessage returns the project of an owner's message in a mapped channel
// of the configured guild; anything else (other guilds, bots, webhooks, the
// bridge itself, strangers, system messages) is ignored.
func (b *Bridge) ownerMessage(ctx context.Context, msg discord.Message) (Mapping, bool) {
	if msg.GuildID != "" && msg.GuildID != b.cfg.GuildID {
		return Mapping{}, false
	}
	if msg.Author.Bot || msg.WebhookID != "" || (msg.Type != 0 && msg.Type != 19) || !b.owners[msg.Author.ID] {
		return Mapping{}, false
	}
	m, err := b.cfg.State.MappingByChannel(ctx, msg.ChannelID)
	return m, err == nil
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

// markerSeq reads the board message number from a bridge message's marker
// line ("-# #8812 · part 2/3").
func markerSeq(content string) (int64, bool) {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(lines[i], "-# #")
		if !ok {
			continue
		}
		num, _, _ := strings.Cut(rest, " ")
		seq, err := strconv.ParseInt(num, 10, 64)
		return seq, err == nil && seq > 0
	}
	return 0, false
}

// replyTarget finds the board message (and its agent) a Discord reply
// points at: from the record of what was sent, or, when that record was
// lost, from the marker line of the bot's own message. Owners cannot forge
// a target: only messages the bot itself wrote are read.
func (b *Bridge) replyTarget(ctx context.Context, m Mapping, msg discord.Message) (seq int64, agent string, err error) {
	ref := msg.MessageReference
	if ref == nil || ref.MessageID == "" {
		return 0, "", nil
	}
	mirrored, ok, err := b.cfg.State.MirroredMessage(ctx, ref.MessageID)
	if err != nil {
		return 0, "", err
	}
	if ok {
		if mirrored.TaskID != m.TaskID {
			return 0, "", nil
		}
		return mirrored.Seq, mirrored.AgentID, nil
	}
	target := msg.ReferencedMessage
	b.mu.Lock()
	bot := b.botID
	b.mu.Unlock()
	if target == nil || target.Author.ID != bot {
		return 0, "", nil
	}
	seq, ok = markerSeq(target.Content)
	if !ok {
		return 0, "", nil
	}
	agent, found, err := b.cfg.State.OutboxAgent(ctx, m.TaskID, seq)
	if err != nil || !found {
		return 0, "", err // not a board message of this project
	}
	return seq, agent, nil
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
	seq, agent, err := b.replyTarget(ctx, m, msg)
	if err != nil {
		b.retryInbound(ctx, msg.ID, attempts, err)
		return
	}
	// Replying to an agent's message addresses that agent.
	req.ReplyTo, req.To = seq, agent
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

// InteractionLifetime is how long Discord accepts replies to an interaction.
const InteractionLifetime = 15 * time.Minute

// inboundRetryLoop finishes owner messages the hub did not take yet and
// interactions interrupted by a crash. Every action is idempotent: posts by
// request ID, reassignments and nudges by the hub's own state checks.
func (b *Bridge) inboundRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		b.retryInbound1(ctx)
	}
}

func (b *Bridge) retryInbound1(ctx context.Context) {
	pending, err := b.cfg.State.PendingInbound(ctx, time.Now().UTC())
	if err != nil {
		b.logf("discord bridge: pending inbound: %v", err)
		return
	}
	for _, in := range pending {
		m, err := b.cfg.State.Mapping(ctx, in.TaskID)
		if err != nil {
			continue
		}
		switch in.Kind {
		case "message":
			var msg discord.Message
			if json.Unmarshal([]byte(in.Payload), &msg) != nil {
				_ = b.cfg.State.FinishInbound(ctx, in.SourceID, "rejected: unreadable")
				continue
			}
			b.postInbound(ctx, m, msg, in.Attempts+1)
		case "interaction":
			var it discord.Interaction
			if json.Unmarshal([]byte(in.Payload), &it) != nil || time.Since(in.CreatedAt) > InteractionLifetime-time.Minute {
				_ = b.cfg.State.FinishInbound(ctx, in.SourceID, "expired before it could be finished")
				continue
			}
			_ = b.cfg.State.RetryInbound(ctx, in.SourceID, time.Now().Add(3*time.Minute), "resuming")
			b.runInteraction(ctx, m, it, true)
		}
	}
}

// backfill ingests owner messages the Gateway did not deliver: it reads each
// channel forward from its cursor and advances the cursor past every
// message, whoever wrote it, so it stays close to the head of the channel.
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
	pages:
		for {
			msgs, err := b.cfg.Discord.ChannelMessages(ctx, m.ChannelID, after, 100)
			if err != nil {
				b.logf("discord bridge: backfill %s: %v", m.TaskID, err)
				break
			}
			stopped := false
			for _, msg := range msgs {
				if msg.GuildID == "" {
					msg.GuildID = b.cfg.GuildID
				}
				// The cursor never passes a message that was not recorded.
				if b.handleMessage(ctx, msg) != nil {
					stopped = true
					break
				}
				after = msg.ID
			}
			if err := b.cfg.State.SetIngestAfter(ctx, m.TaskID, after); err != nil {
				b.logf("discord bridge: ingest cursor %s: %v", m.TaskID, err)
				break
			}
			if stopped || len(msgs) < 100 || ctx.Err() != nil {
				break pages
			}
		}
	}
}
