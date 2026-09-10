package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) createAuditExport(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CreateAuditExportRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.CreateAuditExport(r.Context(), id, req, by)
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

func (s *Server) getAuditExportChunk(w http.ResponseWriter, r *http.Request) {
	by, ok := s.caller(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid offset")
		return
	}
	limit := api.MaxAuditExportChunkBytes
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
	}
	out, err := s.store.GetAuditExportChunk(r.Context(), id, r.PathValue("exportID"), offset, limit, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
