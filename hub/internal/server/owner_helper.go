package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// registerOwnerHelper binds the project's owner helper to the owner's session
// (docs/owner-helper.md). Workspace token only; it replays by request ID.
func (s *Server) registerOwnerHelper(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterOwnerHelperRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.RegisterOwnerHelper(r.Context(), id, req, c)
	})
}
