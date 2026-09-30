package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The metadata-only endpoints reject item fields instead of silently dropping
// them, so callers cannot mistake a bookkeeping receipt for a scope edit.
func decodeScope(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeScopeLimited(w, r, v, api.MaxBody)
}

func decodeScopeLimited(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		refuseScopeDecode(w, err)
		return false
	}
	// Padding after a valid object reaches the size limit here, not above:
	// the first Decode returns as soon as the object is buffered.
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if !errors.As(err, &tooLarge) {
			err = errTrailingData
		}
		refuseScopeDecode(w, err)
		return false
	}
	return true
}

var errTrailingData = errors.New("trailing data")

// A refusal names its reason: callers only see the message text, and "too
// large" and "unknown field" need different fixes.
func refuseScopeDecode(w http.ResponseWriter, err error) {
	const prefix = "invalid scope metadata request: "
	var tooLarge *http.MaxBytesError
	var wrongType *json.UnmarshalTypeError
	reason, code := "malformed JSON", "malformed-json"
	switch {
	case errors.As(err, &tooLarge):
		reason, code = "body exceeds "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes", "body-too-large"
	case errors.Is(err, errTrailingData):
		reason, code = "trailing data after the JSON object", "trailing-data"
	case strings.HasPrefix(err.Error(), `json: unknown field "`):
		// encoding/json has no typed error for an unknown field.
		name := strings.TrimSuffix(strings.TrimPrefix(err.Error(), `json: unknown field "`), `"`)
		reason, code = "unknown field "+boundedFieldName(name), "unknown-field"
	case errors.As(err, &wrongType):
		reason, code = "wrong type for field "+boundedFieldName(wrongType.Field), "wrong-type"
	}
	writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: prefix + reason, Code: code})
}

// The name comes from the caller's body, so the echo is bounded and quoted.
func boundedFieldName(name string) string {
	const max = 64
	if len(name) > max {
		name = strings.ToValidUTF8(name[:max], "") + "..."
	}
	return strconv.Quote(name)
}

func (s *Server) confirmWorkOrderScope(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.ConfirmWorkOrderScopeRequest
	if !decodeScope(w, r, &req) {
		return
	}
	v, err := s.store.ConfirmWorkOrderScope(r.Context(), task, item, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getWorkOrderScopeConfirmation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	revision, err1 := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	order, err2 := strconv.ParseInt(r.URL.Query().Get("order"), 10, 64)
	if err1 != nil || err2 != nil || revision < 1 || order < 1 {
		fail(w, api.ErrInvalid)
		return
	}
	v, err := s.store.GetWorkOrderScopeConfirmation(r.Context(), task, item, revision, order)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) saveWorkOrderBookkeeping(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.WorkOrderBookkeepingRequest
	if !decodeScope(w, r, &req) {
		return
	}
	v, err := s.store.SaveWorkOrderBookkeeping(r.Context(), task, item, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getWorkOrderBookkeepingReceipt(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	v, err := s.store.GetWorkOrderBookkeepingReceipt(r.Context(), task, item, r.PathValue("requestID"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
