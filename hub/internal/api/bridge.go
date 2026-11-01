package api

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BridgeNode is the caller node of the Discord bridge credential (broker
// phase 2b). It acts for the owner, but only through BridgeRoutes.
const BridgeNode = "discord-bridge"

// SourceDiscord is the MessageSource kind for messages bridged from Discord.
const SourceDiscord = "discord"

// MessageSource is where a bridged human message came from.
type MessageSource struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`     // Discord message or interaction ID
	UserID string `json:"userId"` // Discord user who wrote it
}

var snowflake = regexp.MustCompile(`^[0-9]{1,20}$`)

// ValidMessageSource accepts only a Discord source with numeric IDs.
func ValidMessageSource(src MessageSource) bool {
	return src.Kind == SourceDiscord && snowflake.MatchString(src.ID) && snowflake.MatchString(src.UserID)
}

// BridgeRoutes are the only route patterns the bridge credential may call.
// Everything else answers 403, so a leaked bridge token cannot launch,
// close, pause or reconfigure anything.
var BridgeRoutes = map[string]bool{
	"GET /v1/whoami":                                     true,
	"GET /v1/tasks":                                      true,
	"GET /v1/tasks/{id}":                                 true,
	"GET /v1/tasks/{id}/agents":                          true,
	"GET /v1/tasks/{id}/team-queue":                      true,
	"GET /v1/tasks/{id}/messages":                        true,
	"POST /v1/tasks/{id}/messages":                       true,
	"GET /v1/tasks/{id}/messages/receipts/{requestID}":   true,
	"GET /v1/tasks/{id}/obligations":                     true,
	"POST /v1/tasks/{id}/obligations/{oid}/reassign":     true,
	"POST /v1/tasks/{id}/obligations/{oid}/nudge":        true,
	"POST /v1/tasks/{id}/obligations/{oid}/extend":       true,
	"POST /v1/tasks/{id}/obligations/{oid}/answer":       true,
	"POST /v1/tasks/{id}/obligations/{oid}/cancel":       true,
	"POST /v1/tasks/{id}/agents/{aid}/resume":            true,
	"GET /v1/tasks/{id}/delegation-windows":              true,
	"POST /v1/tasks/{id}/delegation-windows":             true,
	"POST /v1/tasks/{id}/delegation-windows/{wid}/close": true,
	// Create only: /bug and /feature file an owner item; the bridge can
	// never update, dispatch or queue one.
	"POST /v1/tasks/{id}/work-items": true,
	// Read only: the owner helper's /status shows work item titles.
	"GET /v1/tasks/{id}/work-items/{wid}": true,
	// Read only: the bridge reads its saved settings (and reports what it
	// runs on); only the owner can save them.
	"GET /v1/bridge/settings": true,
}

// BridgeSettings are the Discord bridge's non-secret settings the owner can
// save in TailOS Settings (docs/discord-helper-chat.md "## Configure"). No
// token is ever part of them: both tokens stay token files on the bridge host.
type BridgeSettings struct {
	GuildID         string   `json:"guildId"`
	OwnerIDs        []string `json:"ownerIds"`
	HelperTask      string   `json:"helperTask"`
	HelperChannelID string   `json:"helperChannelId"`
	TailOSURL       string   `json:"tailosUrl"`
}

// MaxBridgeOwners bounds ownerIds.
const MaxBridgeOwners = 25

const maxBridgeURL = 2048

// ValidateBridgeSettings trims and checks the settings and returns them
// normalized (owner IDs deduplicated in order). It does not check that
// helperTask names an existing project; the hub does that when saving.
func ValidateBridgeSettings(in BridgeSettings) (BridgeSettings, error) {
	out := BridgeSettings{
		GuildID:         strings.TrimSpace(in.GuildID),
		HelperTask:      strings.TrimSpace(in.HelperTask),
		HelperChannelID: strings.TrimSpace(in.HelperChannelID),
		TailOSURL:       strings.TrimSpace(in.TailOSURL),
	}
	if !snowflake.MatchString(out.GuildID) {
		return out, fmt.Errorf("%w: guildId must be a Discord server ID (digits only)", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, id := range in.OwnerIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if !snowflake.MatchString(id) {
			return out, fmt.Errorf("%w: ownerIds must be Discord user IDs (digits only)", ErrInvalid)
		}
		seen[id] = true
		out.OwnerIDs = append(out.OwnerIDs, id)
	}
	if len(out.OwnerIDs) == 0 || len(out.OwnerIDs) > MaxBridgeOwners {
		return out, fmt.Errorf("%w: ownerIds needs 1 to %d Discord user IDs", ErrInvalid, MaxBridgeOwners)
	}
	if out.HelperTask != "" && !ValidID(out.HelperTask, "tsk") {
		return out, fmt.Errorf("%w: helperTask must be empty or a project ID (tsk_…)", ErrInvalid)
	}
	if out.HelperChannelID != "" {
		if !snowflake.MatchString(out.HelperChannelID) {
			return out, fmt.Errorf("%w: helperChannelId must be empty or a Discord channel ID (digits only)", ErrInvalid)
		}
		if out.HelperTask == "" {
			return out, fmt.Errorf("%w: helperChannelId needs helperTask", ErrInvalid)
		}
	}
	if out.TailOSURL != "" {
		u, err := url.Parse(out.TailOSURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(out.TailOSURL) > maxBridgeURL {
			return out, fmt.Errorf("%w: tailosUrl must be empty or an absolute https:// URL", ErrInvalid)
		}
	}
	return out, nil
}

// Where the bridge's running settings came from.
const (
	BridgeSourceHub        = "hub"        // the saved record, revision Applied
	BridgeSourceLastGood   = "last-good"  // the last saved record that reached Discord
	BridgeSourceDeployment = "deployment" // the bridge host's environment
	BridgeSourceNone       = "none"       // not connected to Discord (parked)
)

// Why the bridge refused a saved revision. Fixed codes, never error text, so
// nothing from Discord or the bridge's log reaches the hub.
const (
	BridgeRefusedInvalid            = "invalid"
	BridgeRefusedStartupFailed      = "startup-failed"
	BridgeRefusedStoppedDuringStart = "stopped-during-start"
	BridgeRefusedGuildChange        = "guild-change"
)

var bridgeReasons = map[string]bool{
	BridgeRefusedInvalid: true, BridgeRefusedStartupFailed: true,
	BridgeRefusedStoppedDuringStart: true, BridgeRefusedGuildChange: true,
}

// BridgeReport is what the bridge says it runs on. It travels as query
// parameters of the bridge's settings read and holds only non-secret values.
type BridgeReport struct {
	Source    string         `json:"source"`
	Applied   int64          `json:"applied"`
	Effective BridgeSettings `json:"effective"`
	Refused   int64          `json:"refused"`
	Reason    string         `json:"reason,omitempty"`
}

// ValidateBridgeReport checks a report with the settings' field rules and
// the fixed source and reason sets.
func ValidateBridgeReport(in BridgeReport) (BridgeReport, error) {
	out := in
	switch in.Source {
	case BridgeSourceHub:
		if in.Applied < 1 {
			return out, fmt.Errorf("%w: a hub report names the revision it applied", ErrInvalid)
		}
	case BridgeSourceLastGood:
		if in.Applied < 0 {
			return out, fmt.Errorf("%w: applied must not be negative", ErrInvalid)
		}
	case BridgeSourceDeployment, BridgeSourceNone:
		if in.Applied != 0 {
			return out, fmt.Errorf("%w: only hub and last-good reports name an applied revision", ErrInvalid)
		}
	default:
		return out, fmt.Errorf("%w: unknown report source", ErrInvalid)
	}
	if in.Source == BridgeSourceNone {
		if in.Effective.GuildID != "" || len(in.Effective.OwnerIDs) > 0 || in.Effective.HelperTask != "" || in.Effective.HelperChannelID != "" || in.Effective.TailOSURL != "" {
			return out, fmt.Errorf("%w: a bridge that is not connected runs on no values", ErrInvalid)
		}
		out.Effective = BridgeSettings{}
	} else {
		eff, err := ValidateBridgeSettings(in.Effective)
		if err != nil {
			return out, err
		}
		out.Effective = eff
	}
	if in.Refused < 0 || (in.Refused == 0) != (in.Reason == "") || (in.Reason != "" && !bridgeReasons[in.Reason]) {
		return out, fmt.Errorf("%w: a refused revision needs one known reason code", ErrInvalid)
	}
	return out, nil
}

// Query encodes the report as the settings read's query parameters.
func (r BridgeReport) Query() url.Values {
	q := url.Values{}
	q.Set("source", r.Source)
	q.Set("applied", strconv.FormatInt(r.Applied, 10))
	q.Set("guild", r.Effective.GuildID)
	q.Set("owners", strings.Join(r.Effective.OwnerIDs, ","))
	q.Set("helperTask", r.Effective.HelperTask)
	q.Set("helperChannel", r.Effective.HelperChannelID)
	q.Set("tailosUrl", r.Effective.TailOSURL)
	q.Set("refused", strconv.FormatInt(r.Refused, 10))
	q.Set("reason", r.Reason)
	return q
}

// ParseBridgeReport reads a report from query parameters. ok is false when
// the request carries none (no source parameter).
func ParseBridgeReport(q url.Values) (BridgeReport, bool, error) {
	if !q.Has("source") {
		return BridgeReport{}, false, nil
	}
	num := func(key string) (int64, error) {
		v := q.Get(key)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %s must be a number", ErrInvalid, key)
		}
		return n, nil
	}
	r := BridgeReport{Source: q.Get("source"), Reason: q.Get("reason"), Effective: BridgeSettings{
		GuildID: q.Get("guild"), HelperTask: q.Get("helperTask"),
		HelperChannelID: q.Get("helperChannel"), TailOSURL: q.Get("tailosUrl"),
	}}
	for _, id := range strings.Split(q.Get("owners"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			r.Effective.OwnerIDs = append(r.Effective.OwnerIDs, id)
		}
	}
	var err error
	if r.Applied, err = num("applied"); err != nil {
		return r, true, err
	}
	if r.Refused, err = num("refused"); err != nil {
		return r, true, err
	}
	r, err = ValidateBridgeReport(r)
	return r, true, err
}

// Secret status values. "Configured" means a bridge report arrived since the
// hub started: the bridge reads both token files and refuses to start
// without them, and only the bridge token reaches the report. It does not
// prove Discord accepts the bot token.
const (
	SecretConfigured           = "configured"
	SecretConfiguredBridgeHost = "configured-on-bridge-host"
	SecretNotConfirmed         = "not-confirmed"
)

// BridgeSecrets says only whether each token is known to be configured.
type BridgeSecrets struct {
	BridgeToken  string `json:"bridgeToken"`
	DiscordToken string `json:"discordToken"`
}

// BridgeReportView is the last report and when it arrived (hub memory only).
type BridgeReportView struct {
	ReportedAt time.Time `json:"reportedAt"`
	BridgeReport
}

// BridgeSettingsRecord is the saved settings (Settings nil before the first
// save).
type BridgeSettingsRecord struct {
	Settings  *BridgeSettings `json:"settings"`
	Revision  int64           `json:"revision"`
	UpdatedAt *time.Time      `json:"updatedAt,omitempty"`
	UpdatedBy *Caller         `json:"updatedBy,omitempty"`
}

// BridgeSettingsView answers GET /v1/bridge/settings.
type BridgeSettingsView struct {
	BridgeSettingsRecord
	Bridge  *BridgeReportView `json:"bridge"`
	Secrets BridgeSecrets     `json:"secrets"`
}

// PutBridgeSettingsRequest saves the settings when ExpectedRevision is still
// the saved revision (0 before the first save).
type PutBridgeSettingsRequest struct {
	Settings         BridgeSettings `json:"settings"`
	ExpectedRevision int64          `json:"expectedRevision"`
}

// BridgeSettings reads the saved settings. A non-nil report tells the hub
// what the bridge runs on.
func (c *Client) BridgeSettings(ctx context.Context, report *BridgeReport) (BridgeSettingsView, error) {
	path := "/v1/bridge/settings"
	if report != nil {
		path += "?" + report.Query().Encode()
	}
	var out BridgeSettingsView
	return out, c.do(ctx, "GET", path, nil, &out)
}

// PutBridgeSettings saves the settings (owner only).
func (c *Client) PutBridgeSettings(ctx context.Context, req PutBridgeSettingsRequest) (BridgeSettingsView, error) {
	var out BridgeSettingsView
	return out, c.do(ctx, "PUT", "/v1/bridge/settings", req, &out)
}
