package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Owner delegation windows (docs/owner-delegation-windows.md): the owner's
// controls, from the workspace token (TailOS, tt) or the Discord bridge.

func (s *Server) openDelegationWindow(w http.ResponseWriter, r *http.Request) {
	var req api.OpenDelegationWindowRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.OpenDelegationWindow(r.Context(), id, req, c)
	})
}

func (s *Server) closeDelegationWindow(w http.ResponseWriter, r *http.Request) {
	var req api.CloseDelegationWindowRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.CloseDelegationWindow(r.Context(), id, r.PathValue("wid"), req, c)
	})
}

func (s *Server) listDelegationWindows(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListDelegationWindows(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
