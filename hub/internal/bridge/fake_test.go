package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/discord"
)

// fakeDiscord is an in-memory Discord REST API with fault injection.
type fakeDiscord struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	nextID    int64
	botID     string
	channels  map[string]*discord.Channel
	order     []string // channel IDs in creation order
	messages  map[string][]*fakeMessage
	nonces    map[string]string // channel+nonce → message ID
	pins      map[string]bool
	reactions []string
	callbacks []recordedCallback
	edits     []recordedEdit
	commands  []discord.Command
	requests  []recordedRequest

	// Faults, consumed once each.
	loseCreateChannel int // create the channel, then answer 500
	loseCreateMessage int // create the message, then answer 500
	rateLimitMessages int // answer 429 with a short retry_after
	failMessages      int // answer 500 without creating
}

type fakeMessage struct {
	discord.Message
	Embeds          []discord.Embed
	Components      []discord.Component
	AllowedMentions *discord.AllowedMentions
	Reference       *discord.MessageReference
}

type recordedCallback struct {
	ID       string
	Response discord.InteractionResponse
}

type recordedEdit struct {
	Token string
	Edit  map[string]json.RawMessage
}

type recordedRequest struct {
	Method, Path string
	Body         map[string]json.RawMessage
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{t: t, nextID: 1552750000000000000, channels: map[string]*discord.Channel{}, messages: map[string][]*fakeMessage{}, nonces: map[string]string{}, pins: map[string]bool{}}
	f.botID = f.id()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) id() string {
	f.nextID++
	return strconv.FormatInt(f.nextID, 10)
}

func (f *fakeDiscord) client() *discord.Client {
	return &discord.Client{Token: "fake", Base: f.srv.URL, HTTP: f.srv.Client()}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDiscord) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path, body})
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	route := r.Method + " " + r.URL.Path
	switch {
	case route == "GET /users/@me":
		writeJSON(w, 200, discord.User{ID: f.botID, Username: "Tailterm Broker", Bot: true})
	case r.Method == "PUT" && len(parts) == 5 && parts[0] == "applications" && parts[4] == "commands":
		_ = json.Unmarshal(raw, &f.commands)
		writeJSON(w, 200, f.commands)
	case r.Method == "GET" && len(parts) == 3 && parts[0] == "guilds" && parts[2] == "channels":
		var out []discord.Channel
		for _, id := range f.order {
			out = append(out, *f.channels[id])
		}
		writeJSON(w, 200, out)
	case r.Method == "POST" && len(parts) == 3 && parts[0] == "guilds" && parts[2] == "channels":
		var c discord.Channel
		_ = json.Unmarshal(raw, &c)
		c.ID, c.GuildID = f.id(), parts[1]
		f.channels[c.ID] = &c
		f.order = append(f.order, c.ID)
		if f.loseCreateChannel > 0 {
			f.loseCreateChannel--
			writeJSON(w, 500, map[string]any{"message": "lost"})
			return
		}
		writeJSON(w, 201, c)
	case r.Method == "PATCH" && len(parts) == 2 && parts[0] == "channels":
		c, ok := f.channels[parts[1]]
		if !ok {
			writeJSON(w, 404, map[string]any{"code": discord.CodeUnknownChannel, "message": "Unknown Channel"})
			return
		}
		var mod discord.ModifyChannel
		_ = json.Unmarshal(raw, &mod)
		if mod.ParentID != nil {
			c.ParentID = *mod.ParentID
		}
		if mod.Topic != nil {
			c.Topic = *mod.Topic
		}
		writeJSON(w, 200, c)
	case len(parts) >= 3 && parts[0] == "channels" && parts[2] == "messages":
		f.serveMessages(w, r, parts, raw, body)
	case r.Method == "PUT" && len(parts) == 4 && parts[0] == "channels" && parts[2] == "pins":
		f.pins[parts[3]] = true
		w.WriteHeader(204)
	case r.Method == "POST" && len(parts) == 4 && parts[0] == "interactions" && parts[3] == "callback":
		var res discord.InteractionResponse
		_ = json.Unmarshal(raw, &res)
		f.callbacks = append(f.callbacks, recordedCallback{ID: parts[1], Response: res})
		w.WriteHeader(204)
	case r.Method == "PATCH" && len(parts) == 5 && parts[0] == "webhooks" && parts[4] == "@original":
		f.edits = append(f.edits, recordedEdit{Token: parts[2], Edit: body})
		writeJSON(w, 200, map[string]any{"id": f.id()})
	default:
		f.t.Errorf("fake discord: unexpected %s", route)
		writeJSON(w, 404, map[string]any{"message": "not found"})
	}
}

func (f *fakeDiscord) serveMessages(w http.ResponseWriter, r *http.Request, parts []string, raw []byte, body map[string]json.RawMessage) {
	channel := parts[1]
	if _, ok := f.channels[channel]; !ok {
		writeJSON(w, 404, map[string]any{"code": discord.CodeUnknownChannel, "message": "Unknown Channel"})
		return
	}
	switch {
	case r.Method == "POST" && len(parts) == 3:
		if f.rateLimitMessages > 0 {
			f.rateLimitMessages--
			writeJSON(w, 429, map[string]any{"retry_after": 0.05, "global": false})
			return
		}
		if f.failMessages > 0 {
			f.failMessages--
			writeJSON(w, 500, map[string]any{"message": "boom"})
			return
		}
		var send discord.MessageSend
		_ = json.Unmarshal(raw, &send)
		if send.EnforceNonce && send.Nonce != "" {
			if id, ok := f.nonces[channel+send.Nonce]; ok {
				writeJSON(w, 200, f.find(channel, id).Message)
				return
			}
		}
		m := &fakeMessage{Message: discord.Message{ID: f.id(), ChannelID: channel, Author: discord.User{ID: f.botID, Bot: true}, Content: send.Content, MessageReference: send.MessageReference},
			Embeds: send.Embeds, Components: send.Components, Reference: send.MessageReference}
		if _, ok := body["allowed_mentions"]; ok {
			m.AllowedMentions = &send.AllowedMentions
		}
		f.messages[channel] = append(f.messages[channel], m)
		if send.Nonce != "" {
			f.nonces[channel+send.Nonce] = m.ID
		}
		if f.loseCreateMessage > 0 {
			f.loseCreateMessage--
			writeJSON(w, 500, map[string]any{"message": "lost"})
			return
		}
		writeJSON(w, 200, m.Message)
	case r.Method == "GET" && len(parts) == 3:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		after := r.URL.Query().Get("after")
		var asc []discord.Message // oldest first
		for _, m := range f.messages[channel] {
			if after == "" || snowflakeAfter(m.ID, after) {
				asc = append(asc, m.Message)
			}
		}
		if after != "" && len(asc) > limit {
			asc = asc[:limit] // the oldest page after the cursor
		} else if len(asc) > limit {
			asc = asc[len(asc)-limit:] // the newest page
		}
		out := make([]discord.Message, 0, len(asc))
		for i := len(asc) - 1; i >= 0; i-- { // Discord answers newest first
			out = append(out, asc[i])
		}
		writeJSON(w, 200, out)
	case r.Method == "PATCH" && len(parts) == 4:
		m := f.find(channel, parts[3])
		if m == nil {
			writeJSON(w, 404, map[string]any{"code": discord.CodeUnknownMessage, "message": "Unknown Message"})
			return
		}
		var edit discord.MessageEdit
		_ = json.Unmarshal(raw, &edit)
		if edit.Content != nil {
			m.Content = *edit.Content
		}
		if edit.Embeds != nil {
			m.Embeds = *edit.Embeds
		}
		writeJSON(w, 200, m.Message)
	case r.Method == "PUT" && len(parts) == 7 && parts[4] == "reactions":
		f.reactions = append(f.reactions, parts[3]+":"+parts[5])
		w.WriteHeader(204)
	default:
		f.t.Errorf("fake discord: unexpected %s %s", r.Method, r.URL.Path)
		writeJSON(w, 404, map[string]any{"message": "not found"})
	}
}

func snowflakeAfter(a, b string) bool {
	x, _ := strconv.ParseInt(a, 10, 64)
	y, _ := strconv.ParseInt(b, 10, 64)
	return x > y
}

func (f *fakeDiscord) find(channel, id string) *fakeMessage {
	for _, m := range f.messages[channel] {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// userMessage is an owner (or anyone) typing in a channel; the fake stores
// it so backfill can find it, and returns the Gateway form.
func (f *fakeDiscord) userMessage(channel, author, content string, ref string) discord.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := &fakeMessage{Message: discord.Message{ID: f.id(), ChannelID: channel, GuildID: "guild", Author: discord.User{ID: author}, Content: content}}
	if ref != "" {
		m.MessageReference = &discord.MessageReference{MessageID: ref}
		m.Type = 19
	}
	f.messages[channel] = append(f.messages[channel], m)
	return m.Message
}

func (f *fakeDiscord) channelMessages(channel string) []*fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeMessage(nil), f.messages[channel]...)
}

func (f *fakeDiscord) botMessages(channel string) []*fakeMessage {
	var out []*fakeMessage
	for _, m := range f.channelMessages(channel) {
		if m.Author.ID == f.botID {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeDiscord) textChannels() []discord.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []discord.Channel
	for _, id := range f.order {
		if c := f.channels[id]; c.Type == discord.ChannelText {
			out = append(out, *c)
		}
	}
	return out
}

func (f *fakeDiscord) channel(id string) discord.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.channels[id]
}

func (f *fakeDiscord) categoryNamed(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.channels {
		if c.Type == discord.ChannelCategory && c.Name == name {
			return c.ID
		}
	}
	return ""
}

func (f *fakeDiscord) lastEdit(token string) map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.edits) - 1; i >= 0; i-- {
		if f.edits[i].Token == token {
			return f.edits[i].Edit
		}
	}
	return nil
}

func (f *fakeDiscord) sentWithoutAllowedMentions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bad []string
	for _, req := range f.requests {
		sends := (req.Method == "POST" && strings.HasSuffix(req.Path, "/messages")) || (req.Method == "PATCH" && strings.Contains(req.Path, "/messages/"))
		if !sends {
			continue
		}
		raw, ok := req.Body["allowed_mentions"]
		var am discord.AllowedMentions
		if !ok || json.Unmarshal(raw, &am) != nil || am.Parse == nil || len(am.Parse) != 0 {
			bad = append(bad, fmt.Sprintf("%s %s", req.Method, req.Path))
		}
	}
	return bad
}
