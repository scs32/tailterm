package server

import (
	"errors"
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// failHandlerRotation keeps a refusal's named reason in the 409 body.
func failHandlerRotation(w http.ResponseWriter, err error) {
	var refusal *api.HandlerRotationRefusal
	if errors.As(err, &refusal) {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: refusal.Error(), Code: refusal.Code})
		return
	}
	fail(w, err)
}

func (s *Server) getHandlerRotationPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	p, err := s.store.HandlerRotationPolicy(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) setHandlerRotationPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.HandlerRotationPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.store.SetHandlerRotationPolicy(r.Context(), id, req)
	if err != nil {
		failHandlerRotation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlerRotationAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.HandlerRotationRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.HandlerRotationAction(r.Context(), id, req, caller)
	if err != nil {
		failHandlerRotation(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listHandlerRotations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.ListHandlerRotations(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rotations": out})
}

func (s *Server) getHandlerRotation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.GetHandlerRotation(r.Context(), id, r.PathValue("rid"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlerRotationsDue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	q := r.URL.Query()
	out, err := s.store.HandlerRotationsDue(r.Context(), q.Get("host"), q.Get("templateDigest"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
