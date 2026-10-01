package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
)

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	out, err := s.store.Releases(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) releaseAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.ReleaseRequest
	// A verification import carries the full plan and receipt, like an item's
	// verification save: a 72-check integrated receipt is about 106 KB.
	if !decodeScopeLimited(w, r, &req, api.MaxVerificationBody) {
		return
	}
	if req.Operation == "hand_releases" {
		// Read-only: the owner's recorded hand releases.
		out, err := s.store.HandReleases(r.Context(), r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if req.Operation == "hand_release" {
		out, err := s.store.RecordHandRelease(r.Context(), r.PathValue("id"), req, caller)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
		return
	}
	if req.Operation == "handler" {
		// Read-only: the deployer's handler requests follow the project rule.
		handler, err := s.store.ReleaseHandler(r.Context(), r.PathValue("id"), req.AgentID, req.RunID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, handler)
		return
	}
	out, err := s.store.ReleaseAction(r.Context(), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
