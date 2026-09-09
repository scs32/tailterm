package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkItemRevisionClientPreservesStructuredGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(WorkItemHistoryGapResponse{Error: "revision unavailable", Gap: HistoryGap{Seq: 7, FirstRevision: 2, LastRevision: 4, ReasonCode: "missing_revision"}})
	}))
	defer server.Close()
	client, err := NewClient(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetWorkItemRevision(context.Background(), "tsk_0000000000000001", "wi_0000000000000002", 3)
	var gap *WorkItemHistoryGapError
	if !errors.As(err, &gap) || gap.Response.Gap.Seq != 7 || gap.Response.Gap.FirstRevision != 2 || gap.Error() != "revision unavailable" {
		t.Fatalf("gap error=%T %+v", err, err)
	}
}
