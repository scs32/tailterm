package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPI is Discord's REST base URL.
const DefaultAPI = "https://discord.com/api/v10"

// Client calls the Discord REST API as a bot. It honors per-route buckets
// and every 429's retry_after, and never retries a request Discord rejected.
type Client struct {
	Token string
	Base  string // DefaultAPI unless a test points it elsewhere
	HTTP  *http.Client
	// MaxWait caps how long one call waits on rate limits before giving up
	// with ErrRateLimited, so a stuck bucket cannot hang the bridge.
	MaxWait time.Duration

	mu          sync.Mutex
	blocked     map[string]time.Time // bucket key → blocked until
	globalUntil time.Time
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
}

// ErrRateLimited means waiting for Discord's limits exceeded MaxWait.
var ErrRateLimited = errors.New("discord: rate limited")

// HTTPError is a request Discord answered with an error status. Code is
// Discord's JSON error code (10003 unknown channel, 50001 missing access, …).
type HTTPError struct {
	Status  int
	Code    int
	Message string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("discord: %d (code %d) %s", e.Status, e.Code, e.Message)
}

// Discord JSON error codes the bridge acts on.
const (
	CodeUnknownChannel     = 10003
	CodeUnknownMessage     = 10008
	CodeUnknownInteraction = 10062
	CodeMissingAccess      = 50001
	CodeMissingPermissions = 50013
)

// Permanent reports whether retrying the same request cannot succeed.
func Permanent(err error) bool {
	var h *HTTPError
	return errors.As(err, &h) && h.Status >= 400 && h.Status < 500 && h.Status != http.StatusTooManyRequests
}

// IsCode reports whether err is a Discord error with the given JSON code.
func IsCode(err error, code int) bool {
	var h *HTTPError
	return errors.As(err, &h) && h.Code == code
}

func (c *Client) init() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked == nil {
		c.blocked = map[string]time.Time{}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.sleep == nil {
		c.sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
}

// bucketKey approximates Discord's buckets: method and route, keeping the
// major parameter (channel, guild or webhook ID) and folding minor IDs.
func bucketKey(method, path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 1; i < len(parts); i++ {
		major := parts[i-1] == "channels" || parts[i-1] == "guilds" || parts[i-1] == "webhooks"
		if !major && isID(parts[i]) {
			parts[i] = "{id}"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

func isID(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Client) wait(ctx context.Context, key string, deadline time.Time) error {
	for {
		c.mu.Lock()
		until := c.blocked[key]
		if c.globalUntil.After(until) {
			until = c.globalUntil
		}
		now := c.now()
		c.mu.Unlock()
		if !until.After(now) {
			return nil
		}
		if until.After(deadline) {
			return ErrRateLimited
		}
		if err := c.sleep(ctx, until.Sub(now)); err != nil {
			return err
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	c.init()
	base := c.Base
	if base == "" {
		base = DefaultAPI
	}
	maxWait := c.MaxWait
	if maxWait == 0 {
		maxWait = 2 * time.Minute
	}
	key := bucketKey(method, strings.SplitN(path, "?", 2)[0])
	deadline := c.now().Add(maxWait)
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for {
		if err := c.wait(ctx, key, deadline); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+c.Token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/scs32/tailterm, 1)")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		httpClient := c.HTTP
		if httpClient == nil {
			httpClient = &http.Client{Timeout: 30 * time.Second}
		}
		res, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if readErr != nil {
			return readErr
		}
		c.record(key, res)
		if res.StatusCode == http.StatusTooManyRequests {
			var limit struct {
				RetryAfter float64 `json:"retry_after"`
				Global     bool    `json:"global"`
			}
			_ = json.Unmarshal(data, &limit)
			if limit.RetryAfter <= 0 {
				limit.RetryAfter = 1
			}
			until := c.now().Add(time.Duration(limit.RetryAfter*float64(time.Second)) + 50*time.Millisecond)
			c.mu.Lock()
			if limit.Global || res.Header.Get("X-RateLimit-Global") == "true" {
				c.globalUntil = until
			} else {
				c.blocked[key] = until
			}
			c.mu.Unlock()
			continue
		}
		if res.StatusCode >= 400 {
			var e struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data, &e)
			if e.Message == "" {
				e.Message = strings.TrimSpace(string(data))
			}
			return &HTTPError{Status: res.StatusCode, Code: e.Code, Message: e.Message}
		}
		if out != nil && len(data) > 0 {
			return json.Unmarshal(data, out)
		}
		return nil
	}
}

// record blocks a bucket that reports no remaining requests until it resets.
func (c *Client) record(key string, res *http.Response) {
	remaining := res.Header.Get("X-RateLimit-Remaining")
	resetAfter := res.Header.Get("X-RateLimit-Reset-After")
	if remaining != "0" || resetAfter == "" {
		return
	}
	seconds, err := strconv.ParseFloat(resetAfter, 64)
	if err != nil || seconds <= 0 {
		return
	}
	c.mu.Lock()
	c.blocked[key] = c.now().Add(time.Duration(seconds * float64(time.Second)))
	c.mu.Unlock()
}

// GatewayURL returns the WebSocket URL for bots.
func (c *Client) GatewayURL(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, "GET", "/gateway/bot", nil, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

func (c *Client) GuildChannels(ctx context.Context, guild string) ([]Channel, error) {
	var out []Channel
	return out, c.do(ctx, "GET", "/guilds/"+guild+"/channels", nil, &out)
}

func (c *Client) CreateChannel(ctx context.Context, guild string, req CreateChannel) (Channel, error) {
	var out Channel
	return out, c.do(ctx, "POST", "/guilds/"+guild+"/channels", req, &out)
}

func (c *Client) ModifyChannel(ctx context.Context, channel string, req ModifyChannel) (Channel, error) {
	var out Channel
	return out, c.do(ctx, "PATCH", "/channels/"+channel, req, &out)
}

func (c *Client) CreateMessage(ctx context.Context, channel string, req MessageSend) (Message, error) {
	var out Message
	return out, c.do(ctx, "POST", "/channels/"+channel+"/messages", req, &out)
}

func (c *Client) EditMessage(ctx context.Context, channel, message string, req MessageEdit) (Message, error) {
	var out Message
	return out, c.do(ctx, "PATCH", "/channels/"+channel+"/messages/"+message, req, &out)
}

func (c *Client) PinMessage(ctx context.Context, channel, message string) error {
	return c.do(ctx, "PUT", "/channels/"+channel+"/pins/"+message, nil, nil)
}

// ChannelMessages lists up to limit messages after the given ID, oldest first.
func (c *Client) ChannelMessages(ctx context.Context, channel, after string, limit int) ([]Message, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	q.Set("limit", strconv.Itoa(limit))
	var out []Message
	if err := c.do(ctx, "GET", "/channels/"+channel+"/messages?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	// Discord returns newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// RecentMessages lists the newest messages in a channel, newest first.
func (c *Client) RecentMessages(ctx context.Context, channel string, limit int) ([]Message, error) {
	var out []Message
	return out, c.do(ctx, "GET", "/channels/"+channel+"/messages?limit="+strconv.Itoa(limit), nil, &out)
}

// React adds a unicode emoji reaction.
func (c *Client) React(ctx context.Context, channel, message, emoji string) error {
	return c.do(ctx, "PUT", "/channels/"+channel+"/messages/"+message+"/reactions/"+url.PathEscape(emoji)+"/@me", nil, nil)
}

func (c *Client) RespondInteraction(ctx context.Context, id, token string, res InteractionResponse) error {
	return c.do(ctx, "POST", "/interactions/"+id+"/"+token+"/callback", res, nil)
}

// EditInteractionResponse replaces a deferred interaction's reply.
func (c *Client) EditInteractionResponse(ctx context.Context, app, token string, edit MessageEdit) error {
	return c.do(ctx, "PATCH", "/webhooks/"+app+"/"+token+"/messages/@original", edit, nil)
}

// SetGuildCommands replaces the application's commands in one guild.
func (c *Client) SetGuildCommands(ctx context.Context, app, guild string, commands []Command) error {
	return c.do(ctx, "PUT", "/applications/"+app+"/guilds/"+guild+"/commands", commands, nil)
}

// CurrentUser is the bot's own user.
func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var out User
	return out, c.do(ctx, "GET", "/users/@me", nil, &out)
}
