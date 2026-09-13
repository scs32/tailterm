package server

import (
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"net/http"
	"strconv"
)

func (s *Server) proposeOperationalRecord(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ProposeOperationalRecordRequest
	if !decodeOperational(w, r, &req) {
		return
	}
	out, err := s.store.ProposeOperationalRecord(r.Context(), id, req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
func (s *Server) commitOperationalRecord(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CommitOperationalRecordRequest
	if !decodeOperational(w, r, &req) {
		return
	}
	out, err := s.store.CommitOperationalRecord(r.Context(), id, r.PathValue("recordId"), req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) getOperationalRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var version int64
	var err error
	if raw := r.URL.Query().Get("version"); raw != "" {
		version, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || version < 1 {
			fail(w, api.ErrInvalid)
			return
		}
	}
	out, err := s.store.GetOperationalRecord(r.Context(), id, r.PathValue("recordId"), version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeOperational(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid operational record schema")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "expected exactly one operational record")
		return false
	}
	return true
}
