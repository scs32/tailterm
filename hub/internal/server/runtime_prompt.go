package server

import (
	"errors"
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) getRuntimePromptPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	p, err := s.store.RuntimePromptPolicy(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) setRuntimePromptPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.RuntimePromptPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.store.SetRuntimePromptPolicy(r.Context(), id, req)
	switch {
	case errors.Is(err, api.ErrRuntimePromptOwnerOnly):
		writeJSON(w, http.StatusForbidden, api.ErrorResponse{Error: err.Error(), Code: "owner-only"})
	case errors.Is(err, api.ErrInvalid):
		// The detail names the refused kind and action; it holds no user data.
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error(), Code: "invalid-action"})
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusOK, p)
	}
}
