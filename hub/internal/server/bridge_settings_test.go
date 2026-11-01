package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// wi_dc04ac6a9ccf7402: the bridge settings routes.

// raw sends a request and returns the status and the body text.
func (h tokenHub) raw(token, method, path, body string, header map[string]string) (int, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(data)
}

func settingsBody(t *testing.T, req api.PutBridgeSettingsRequest) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// noTokens checks a response holds neither fixture token and that every
// key naming a token is a secrets status, never a value.
func noTokens(t *testing.T, where, body string) {
	t.Helper()
	for _, secret := range []string{ownerToken, bridgeToken} {
		if strings.Contains(body, secret) {
			t.Fatalf("%s holds a token", where)
		}
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	status := map[any]bool{api.SecretConfigured: true, api.SecretConfiguredBridgeHost: true, api.SecretNotConfirmed: true}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.Contains(strings.ToLower(k), "token") && (path != ".secrets" || !status[child]) {
					t.Fatalf("%s: %s.%s = %v is not a secrets status", where, path, k, child)
				}
				walk(path+"."+k, child)
			}
		case []any:
			for _, child := range v {
				walk(path+"[]", child)
			}
		}
	}
	walk("", doc)
}

// a4: GET /v1/bridge/settings is the bridge's only new route; it cannot
// save, and every route it could not reach before stays closed.
func TestBridgeSettingsRouteForBridgeCredential(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	var added []string
	for pattern := range api.BridgeRoutes {
		if strings.Contains(pattern, "/bridge/") {
			added = append(added, pattern)
		}
	}
	if len(added) != 1 || added[0] != "GET /v1/bridge/settings" {
		t.Fatalf("bridge settings allowlist = %v", added)
	}
	if code, body := h.raw(bridgeToken, "GET", "/v1/bridge/settings", "", nil); code != 200 {
		t.Fatalf("bridge GET = %d %s", code, body)
	}
	good := settingsBody(t, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}})
	if code, _ := h.raw(bridgeToken, "PUT", "/v1/bridge/settings", good, nil); code != http.StatusForbidden {
		t.Fatalf("bridge PUT = %d, want 403", code)
	}
	base := "/v1/tasks/" + task.ID
	item := base + "/work-items/wi_0123456789abcdef"
	for _, denied := range []struct{ method, path string }{
		{"POST", "/v1/tasks"}, {"PATCH", base}, {"DELETE", base}, {"POST", base + "/pause"},
		{"POST", base + "/agents"}, {"PATCH", base + "/agents/" + builder.ID}, {"DELETE", base + "/agents/" + builder.ID},
		{"POST", base + "/messages/read"}, {"GET", base + "/queue"},
		{"PATCH", item}, {"POST", item + "/dispatch"}, {"POST", item + "/updates"},
		{"POST", base + "/decisions"}, {"GET", "/v1/nonexistent"},
		{"POST", "/v1/bridge/settings"}, {"DELETE", "/v1/bridge/settings"}, {"PATCH", "/v1/bridge/settings"},
		{"GET", "/v1/work-items"}, {"GET", "/v1/team-queues"},
	} {
		if code, _ := h.raw(bridgeToken, denied.method, denied.path, "{}", nil); code != http.StatusForbidden {
			t.Errorf("bridge %s %s = %d, want 403", denied.method, denied.path, code)
		}
	}
	if rec, err := h.st.BridgeSettings(context.Background()); err != nil || rec.Revision != 0 {
		t.Fatalf("the bridge saved settings: %+v %v", rec, err)
	}
}

// a2/a4: the owner saves with a revision check and validation; agents cannot
// save; unknown fields (such as a token) are refused, never stored.
func TestBridgeSettingsOwnerSave(t *testing.T) {
	h := newTokenHub(t)
	task, _, _ := h.project()
	var view api.BridgeSettingsView
	if code, _ := h.do(ownerToken, "GET", "/v1/bridge/settings", nil, &view); code != 200 || view.Settings != nil || view.Revision != 0 || view.Bridge != nil {
		t.Fatalf("empty view = %d %+v", code, view)
	}
	if view.Secrets.BridgeToken != api.SecretNotConfirmed || view.Secrets.DiscordToken != api.SecretNotConfirmed {
		t.Fatalf("secrets before any report = %+v", view.Secrets)
	}
	settings := api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222", "333"}, HelperTask: task.ID, HelperChannelID: "444", TailOSURL: "https://tailos.example"}
	code, body := h.raw(ownerToken, "PUT", "/v1/bridge/settings", settingsBody(t, api.PutBridgeSettingsRequest{Settings: settings}), nil)
	if code != 200 {
		t.Fatalf("owner PUT = %d %s", code, body)
	}
	noTokens(t, "PUT response", body)
	if err := json.Unmarshal([]byte(body), &view); err != nil || view.Revision != 1 || view.Settings.HelperTask != task.ID || view.UpdatedBy.User != "owner" {
		t.Fatalf("saved view = %+v %v", view, err)
	}
	stale := settingsBody(t, api.PutBridgeSettingsRequest{Settings: settings, ExpectedRevision: 0})
	if code, body := h.raw(ownerToken, "PUT", "/v1/bridge/settings", stale, nil); code != http.StatusConflict || !strings.Contains(body, "changed elsewhere") {
		t.Fatalf("stale PUT = %d %s", code, body)
	}
	bad := settingsBody(t, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "not-a-guild", OwnerIDs: []string{"222"}}, ExpectedRevision: 1})
	if code, body := h.raw(ownerToken, "PUT", "/v1/bridge/settings", bad, nil); code != http.StatusBadRequest || !strings.Contains(body, "guildId") {
		t.Fatalf("invalid PUT = %d %s", code, body)
	}
	withToken := `{"settings":{"guildId":"111","ownerIds":["222"],"discordToken":"x"},"expectedRevision":1}`
	if code, _ := h.raw(ownerToken, "PUT", "/v1/bridge/settings", withToken, nil); code != http.StatusBadRequest {
		t.Fatalf("PUT with a token field = %d, want 400", code)
	}
	good := settingsBody(t, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}, ExpectedRevision: 1})
	if code, _ := h.raw(ownerToken, "PUT", "/v1/bridge/settings", good, map[string]string{agentHeader: "agt_0123456789abcdef"}); code != http.StatusForbidden {
		t.Fatalf("agent PUT = %d, want 403", code)
	}
	if rec, _ := h.st.BridgeSettings(context.Background()); rec.Revision != 1 || len(rec.Settings.OwnerIDs) != 2 {
		t.Fatalf("a refused PUT changed the record: %+v", rec)
	}
}

func reportQuery(r api.BridgeReport) string { return "?" + r.Query().Encode() }

// a2/a3: the bridge's report is held in memory and shown to the owner; a
// malformed report is refused and records nothing; the owner's read ignores
// report parameters; a report never changes a saved setting.
func TestBridgeSettingsReport(t *testing.T) {
	h := newTokenHub(t)
	saved := settingsBody(t, api.PutBridgeSettingsRequest{Settings: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}})
	if code, body := h.raw(ownerToken, "PUT", "/v1/bridge/settings", saved, nil); code != 200 {
		t.Fatalf("owner PUT = %d %s", code, body)
	}
	// The owner's read does not record a report.
	fake := api.BridgeReport{Source: api.BridgeSourceHub, Applied: 1, Effective: api.BridgeSettings{GuildID: "999", OwnerIDs: []string{"222"}}}
	var view api.BridgeSettingsView
	if code, _ := h.do(ownerToken, "GET", "/v1/bridge/settings"+reportQuery(fake), nil, &view); code != 200 || view.Bridge != nil {
		t.Fatalf("owner read recorded a report: %d %+v", code, view.Bridge)
	}
	for name, q := range map[string]string{
		"unknown reason":    reportQuery(api.BridgeReport{Source: api.BridgeSourceDeployment, Effective: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}, Refused: 1, Reason: "discord said 401 Unauthorized"}),
		"reason no refusal": reportQuery(api.BridgeReport{Source: api.BridgeSourceDeployment, Effective: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}, Reason: "invalid"}),
		"unknown source":    reportQuery(api.BridgeReport{Source: "vault", Effective: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}}),
		"text in a field":   reportQuery(api.BridgeReport{Source: api.BridgeSourceDeployment, Effective: api.BridgeSettings{GuildID: "<b>111</b>", OwnerIDs: []string{"222"}}}),
		"none with values":  reportQuery(api.BridgeReport{Source: api.BridgeSourceNone, Effective: api.BridgeSettings{GuildID: "111"}}),
		"hub without rev":   reportQuery(api.BridgeReport{Source: api.BridgeSourceHub, Effective: api.BridgeSettings{GuildID: "111", OwnerIDs: []string{"222"}}}),
		"bad number":        "?source=deployment&applied=x&guild=111&owners=222",
	} {
		if code, _ := h.raw(bridgeToken, "GET", "/v1/bridge/settings"+q, "", nil); code != http.StatusBadRequest {
			t.Errorf("%s: bridge report = %d, want 400", name, code)
		}
	}
	if h.do(ownerToken, "GET", "/v1/bridge/settings", nil, &view); view.Bridge != nil || view.Secrets.BridgeToken != api.SecretNotConfirmed {
		t.Fatalf("a malformed report was recorded: %+v %+v", view.Bridge, view.Secrets)
	}
	report := api.BridgeReport{Source: api.BridgeSourceDeployment, Effective: api.BridgeSettings{GuildID: "555", OwnerIDs: []string{"666"}, TailOSURL: "https://tailos.example"}, Refused: 1, Reason: api.BridgeRefusedStartupFailed}
	code, body := h.raw(bridgeToken, "GET", "/v1/bridge/settings"+reportQuery(report), "", nil)
	if code != 200 {
		t.Fatalf("bridge report = %d %s", code, body)
	}
	noTokens(t, "bridge read", body)
	code, body = h.raw(ownerToken, "GET", "/v1/bridge/settings", "", nil)
	noTokens(t, "owner read", body)
	if err := json.Unmarshal([]byte(body), &view); err != nil || code != 200 {
		t.Fatalf("owner read = %d %v", code, err)
	}
	b := view.Bridge
	if b == nil || b.ReportedAt.IsZero() || b.Source != api.BridgeSourceDeployment || b.Effective.GuildID != "555" || b.Refused != 1 || b.Reason != api.BridgeRefusedStartupFailed {
		t.Fatalf("owner sees report %+v", b)
	}
	if view.Secrets.BridgeToken != api.SecretConfigured || view.Secrets.DiscordToken != api.SecretConfiguredBridgeHost {
		t.Fatalf("secrets after a report = %+v", view.Secrets)
	}
	if view.Revision != 1 || view.Settings.GuildID != "111" {
		t.Fatalf("a report changed the saved settings: %+v", view.BridgeSettingsRecord)
	}
	// A bare bridge read records nothing new and still answers.
	if code, _ := h.raw(bridgeToken, "GET", "/v1/bridge/settings", "", nil); code != 200 {
		t.Fatalf("bare bridge read = %d", code)
	}
}
