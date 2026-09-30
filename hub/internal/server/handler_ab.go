package server

import (
	"errors"
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// failHandlerArm keeps a refusal's named reason in the 409 body and a
// validation reason in the 400 body.
func failHandlerArm(w http.ResponseWriter, err error) {
	var refusal *api.HandlerArmRefusal
	switch {
	case errors.As(err, &refusal):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: refusal.Error(), Code: refusal.Code})
	case errors.Is(err, api.ErrInvalid) && err != api.ErrInvalid:
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: err.Error(), Code: "invalid"})
	default:
		fail(w, err)
	}
}

func (s *Server) getHandlerArmPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.HandlerArmPolicy(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) setHandlerArmPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.HandlerArmPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.SetHandlerArmPolicy(r.Context(), id, req)
	if err != nil {
		failHandlerArm(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlerABReport(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.HandlerABReport(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
