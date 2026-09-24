// Package discord is the small slice of the Discord API the Tailterm bridge
// uses (broker phase 2b, docs/broker-phase-2b.md): REST for channels,
// messages and interaction replies, and the Gateway for events.
package discord

import "encoding/json"

// Channel types and message flags used by the bridge.
const (
	ChannelText     = 0
	ChannelCategory = 4

	FlagEphemeral = 1 << 6
)

// Gateway intents: guild channel events, guild messages and their content.
const (
	IntentGuilds         = 1 << 0
	IntentGuildMessages  = 1 << 9
	IntentMessageContent = 1 << 15
)

// Interaction and response types.
const (
	InteractionCommand   = 2
	InteractionComponent = 3

	ResponseMessage         = 4
	ResponseDeferredMessage = 5
	ResponseDeferredUpdate  = 6
	ResponseUpdateMessage   = 7
)

// Component types and button styles.
const (
	ComponentActionRow  = 1
	ComponentButton     = 2
	ComponentStringMenu = 3

	ButtonPrimary   = 1
	ButtonSecondary = 2
	ButtonDanger    = 4
	ButtonLink      = 5
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot,omitempty"`
}

type Member struct {
	User *User `json:"user,omitempty"`
}

type Channel struct {
	ID       string `json:"id"`
	Type     int    `json:"type"`
	GuildID  string `json:"guild_id,omitempty"`
	Name     string `json:"name"`
	Topic    string `json:"topic,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
}

type MessageReference struct {
	MessageID string `json:"message_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	GuildID   string `json:"guild_id,omitempty"`
	// FailIfNotExists false lets a reply post even if its target was deleted.
	FailIfNotExists *bool `json:"fail_if_not_exists,omitempty"`
}

type Message struct {
	ID               string            `json:"id"`
	ChannelID        string            `json:"channel_id"`
	GuildID          string            `json:"guild_id,omitempty"`
	Author           User              `json:"author"`
	Content          string            `json:"content"`
	WebhookID        string            `json:"webhook_id,omitempty"`
	Type             int               `json:"type"`
	MessageReference *MessageReference `json:"message_reference,omitempty"`
	Attachments      []json.RawMessage `json:"attachments,omitempty"`
	Pinned           bool              `json:"pinned,omitempty"`
}

type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type EmbedFooter struct {
	Text string `json:"text"`
}

type Embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	URL         string       `json:"url,omitempty"`
	Color       int          `json:"color,omitempty"`
	Fields      []EmbedField `json:"fields,omitempty"`
	Footer      *EmbedFooter `json:"footer,omitempty"`
}

// AllowedMentions is sent on every message: Parse is always empty, so
// nothing in mirrored text can ping anyone; Users names the only pings.
type AllowedMentions struct {
	Parse []string `json:"parse"`
	Users []string `json:"users,omitempty"`
}

type SelectOption struct {
	Label       string `json:"label"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type Component struct {
	Type        int            `json:"type"`
	Components  []Component    `json:"components,omitempty"`
	Style       int            `json:"style,omitempty"`
	Label       string         `json:"label,omitempty"`
	CustomID    string         `json:"custom_id,omitempty"`
	URL         string         `json:"url,omitempty"`
	Disabled    bool           `json:"disabled,omitempty"`
	Options     []SelectOption `json:"options,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
}

// MessageSend creates a message. Nonce with EnforceNonce makes a retry within
// a few minutes return the original message instead of a duplicate.
type MessageSend struct {
	Content          string            `json:"content,omitempty"`
	Embeds           []Embed           `json:"embeds,omitempty"`
	Components       []Component       `json:"components,omitempty"`
	AllowedMentions  AllowedMentions   `json:"allowed_mentions"`
	MessageReference *MessageReference `json:"message_reference,omitempty"`
	Nonce            string            `json:"nonce,omitempty"`
	EnforceNonce     bool              `json:"enforce_nonce,omitempty"`
	Flags            int               `json:"flags,omitempty"`
}

// MessageEdit edits a message; nil fields are left unchanged.
type MessageEdit struct {
	Content         *string          `json:"content,omitempty"`
	Embeds          *[]Embed         `json:"embeds,omitempty"`
	Components      *[]Component     `json:"components,omitempty"`
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
}

type CreateChannel struct {
	Name     string `json:"name"`
	Type     int    `json:"type"`
	Topic    string `json:"topic,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
}

type ModifyChannel struct {
	Name     *string `json:"name,omitempty"`
	Topic    *string `json:"topic,omitempty"`
	ParentID *string `json:"parent_id,omitempty"`
}

type CommandOption struct {
	Type        int    `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

// Command option types.
const (
	OptionString = 3
)

type Command struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Options     []CommandOption `json:"options,omitempty"`
}

type InteractionOption struct {
	Name  string          `json:"name"`
	Type  int             `json:"type"`
	Value json.RawMessage `json:"value,omitempty"`
}

// String returns a string option's value.
func (o InteractionOption) String() string {
	var s string
	_ = json.Unmarshal(o.Value, &s)
	return s
}

type InteractionData struct {
	Name          string              `json:"name,omitempty"`
	Options       []InteractionOption `json:"options,omitempty"`
	CustomID      string              `json:"custom_id,omitempty"`
	ComponentType int                 `json:"component_type,omitempty"`
	Values        []string            `json:"values,omitempty"`
}

type Interaction struct {
	ID            string          `json:"id"`
	ApplicationID string          `json:"application_id"`
	Type          int             `json:"type"`
	Data          InteractionData `json:"data"`
	GuildID       string          `json:"guild_id,omitempty"`
	ChannelID     string          `json:"channel_id,omitempty"`
	Member        *Member         `json:"member,omitempty"`
	User          *User           `json:"user,omitempty"`
	Token         string          `json:"token"`
	Message       *Message        `json:"message,omitempty"`
}

// UserID is the invoking user, in a guild (Member) or a DM (User).
func (i Interaction) UserID() string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

type InteractionResponseData struct {
	Content         string           `json:"content,omitempty"`
	Embeds          []Embed          `json:"embeds,omitempty"`
	Components      []Component      `json:"components,omitempty"`
	AllowedMentions *AllowedMentions `json:"allowed_mentions,omitempty"`
	Flags           int              `json:"flags,omitempty"`
}

type InteractionResponse struct {
	Type int                      `json:"type"`
	Data *InteractionResponseData `json:"data,omitempty"`
}
