package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ownerStatus serves the owner status call (wi_475a3df9b345e13e;
// docs/owner-report.md, "Owner status call"): GET
// /v1/tasks/{id}/owner-status?from=T&to=T&tz=ZONE. from and to are RFC3339
// and required; tz is an IANA zone. It is a read: the daily baseline it may
// record is never subject to the ack gate.
func (s *Server) ownerStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	values := r.URL.Query()
	q := api.OwnerStatusQuery{Zone: values.Get("tz")}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		t, err := time.Parse(time.RFC3339Nano, values.Get(p.name))
		if err != nil {
			fail(w, fmt.Errorf("%w: %s must be an RFC3339 time", api.ErrInvalid, p.name))
			return
		}
		*p.dst = t
	}
	out, err := s.store.OwnerStatus(r.Context(), task, q)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
