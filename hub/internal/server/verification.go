package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) saveVerification(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	var req api.VerificationRequest
	if !decodeScope(w, r, &req) {
		return
	}
	out, err := s.store.SaveVerification(r.Context(), r.PathValue("id"), r.PathValue("wid"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
func (s *Server) verificationHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	out, err := s.store.VerificationHistory(r.Context(), r.PathValue("id"), r.PathValue("wid"), r.URL.Query().Get("agent"), r.URL.Query().Get("run"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
