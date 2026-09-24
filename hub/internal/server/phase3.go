package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// retiredWriter answers the legacy follow-through writers that broker phase 3
// retired (docs/broker-phase-3.md), so an old tt or a stale prompt fails
// loudly instead of silently doing nothing.
func retiredWriter(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusGone, api.ErrorResponse{Code: "retired",
		Error: "retired in broker phase 3: legacy directives and operational records no longer accept writes. Send typed messages with tt send; the hub tracks them as obligations (tt obligations, tt ack, tt progress)."})
}

func (s *Server) ownerRoute(w http.ResponseWriter, r *http.Request, body any, run func(api.Caller, string) (api.OwnerActionResult, error)) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if !decode(w, r, body) {
		return
	}
	out, err := run(c, id)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if out.Replay {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// extendObligation, answerObligation, cancelObligation and resumeAgent are
// the owner's phase-3 controls (the workspace token or the Discord bridge).
func (s *Server) extendObligation(w http.ResponseWriter, r *http.Request) {
	var req api.ObligationExtendRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.ExtendObligation(r.Context(), id, r.PathValue("oid"), req, c)
	})
}

func (s *Server) answerObligation(w http.ResponseWriter, r *http.Request) {
	var req api.ObligationAnswerRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.AnswerObligation(r.Context(), id, r.PathValue("oid"), req, c)
	})
}

func (s *Server) cancelObligation(w http.ResponseWriter, r *http.Request) {
	var req api.ObligationCancelRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.CancelObligation(r.Context(), id, r.PathValue("oid"), req, c)
	})
}

func (s *Server) resumeAgent(w http.ResponseWriter, r *http.Request) {
	var req api.AgentResumeRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.ResumeAgent(r.Context(), id, r.PathValue("aid"), req, c)
	})
}
