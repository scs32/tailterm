package api

import "regexp"

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
	"GET /v1/whoami":                                   true,
	"GET /v1/tasks":                                    true,
	"GET /v1/tasks/{id}":                               true,
	"GET /v1/tasks/{id}/agents":                        true,
	"GET /v1/tasks/{id}/team-queue":                    true,
	"GET /v1/tasks/{id}/messages":                      true,
	"POST /v1/tasks/{id}/messages":                     true,
	"GET /v1/tasks/{id}/messages/receipts/{requestID}": true,
	"GET /v1/tasks/{id}/obligations":                   true,
	"POST /v1/tasks/{id}/obligations/{oid}/reassign":   true,
	"POST /v1/tasks/{id}/obligations/{oid}/nudge":      true,
	"POST /v1/tasks/{id}/obligations/{oid}/extend":     true,
	"POST /v1/tasks/{id}/obligations/{oid}/answer":     true,
	"POST /v1/tasks/{id}/obligations/{oid}/cancel":     true,
	"POST /v1/tasks/{id}/agents/{aid}/resume":          true,
}
