package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Discord bridge settings (wi_dc04ac6a9ccf7402). The owner reads and saves
// the bridge's non-secret settings; the bridge credential may only read them
// (api.BridgeRoutes) and, on that read, report what it runs on. The report
// lives in hub memory only, so a leaked bridge token can change what
// Settings shows as "the bridge is using", never a saved setting.

type bridgeReports struct {
	mu   sync.Mutex
	last *api.BridgeReportView
}

func (b *bridgeReports) set(r api.BridgeReport, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = &api.BridgeReportView{ReportedAt: at, BridgeReport: r}
}

func (b *bridgeReports) get() *api.BridgeReportView {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == nil {
		return nil
	}
	copied := *b.last
	copied.Effective.OwnerIDs = append([]string(nil), b.last.Effective.OwnerIDs...)
	return &copied
}

func (s *Server) bridgeSettingsView(rec api.BridgeSettingsRecord) api.BridgeSettingsView {
	view := api.BridgeSettingsView{BridgeSettingsRecord: rec, Bridge: s.bridgeReports.get(),
		Secrets: api.BridgeSecrets{BridgeToken: api.SecretNotConfirmed, DiscordToken: api.SecretNotConfirmed}}
	if view.Bridge != nil {
		view.Secrets = api.BridgeSecrets{BridgeToken: api.SecretConfigured, DiscordToken: api.SecretConfiguredBridgeHost}
	}
	return view
}

func (s *Server) getBridgeSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	// Only the bridge reports; the owner's read ignores report parameters.
	if c.Node == api.BridgeNode {
		report, has, err := api.ParseBridgeReport(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, invalidRequestText(err))
			return
		}
		if has {
			s.bridgeReports.set(report, time.Now().UTC())
		}
	}
	rec, err := s.store.BridgeSettings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.bridgeSettingsView(rec))
}

func (s *Server) putBridgeSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	// The bridge credential never reaches this handler (not allowlisted).
	// The agent header is self-declared fairness, not a security boundary.
	if c.Node == api.BridgeNode || r.Header.Get(agentHeader) != "" {
		writeError(w, http.StatusForbidden, "only the owner can save the bridge settings")
		return
	}
	var req api.PutBridgeSettingsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: the body must be {settings, expectedRevision} with only the known non-secret fields")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request: expected one JSON object")
		return
	}
	rec, err := s.store.PutBridgeSettings(r.Context(), req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.bridgeSettingsView(rec))
}
