package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) withdrawObligation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ObligationWithdrawRequest
	if !decode(w, r, &req) {
		return
	}
	o, err := s.store.WithdrawObligation(r.Context(), id, r.PathValue("oid"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}
