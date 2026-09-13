package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) createRequiredDelivery(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CreateRequiredDeliveryRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.CreateRequiredDelivery(r.Context(), id, req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) currentAssignment(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	aid, ok := agentID(w, r)
	if !ok {
		return
	}
	out, err := s.store.CurrentAssignment(r.Context(), id, aid, r.URL.Query().Get("runId"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deliveryCoverage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	aid, ok := agentID(w, r)
	if !ok {
		return
	}
	out, err := s.store.DeliveryCoverage(r.Context(), id, aid, r.URL.Query().Get("runId"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) checkDeliveryFollowThrough(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.DeliveryFollowThroughCheckRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.CheckDeliveryFollowThrough(r.Context(), id, r.PathValue("deliveryId"), req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) reportDeliveryFollowThrough(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.DeliveryFollowThroughReportRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.ReportDeliveryFollowThrough(r.Context(), id, r.PathValue("deliveryId"), req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deliveryAction(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	deliveryID := r.PathValue("deliveryId")
	operation := r.PathValue("operation")
	var (
		out api.DeliveryMutation
		err error
	)
	switch operation {
	case "ack", "progress", "result":
		var req api.DeliveryActionRequest
		if !decode(w, r, &req) {
			return
		}
		switch operation {
		case "ack":
			out, err = s.store.AcknowledgeDelivery(r.Context(), id, deliveryID, req, by)
		case "progress":
			out, err = s.store.ProgressDelivery(r.Context(), id, deliveryID, req, by)
		case "result":
			out, err = s.store.ResultDelivery(r.Context(), id, deliveryID, req, by)
		}
	case "blocks":
		var req api.DeliveryBlockRequest
		if !decode(w, r, &req) {
			return
		}
		out, err = s.store.BlockDelivery(r.Context(), id, deliveryID, req, by)
	case "resume":
		var req api.DeliveryResumeRequest
		if !decode(w, r, &req) {
			return
		}
		out, err = s.store.ResumeDelivery(r.Context(), id, deliveryID, req, by)
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) resolveDeliveryBlock(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.DeliveryResolutionRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.ResolveDeliveryBlock(r.Context(), id, r.PathValue("deliveryId"), r.PathValue("blockId"), req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
