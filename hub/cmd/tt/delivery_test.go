package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeliveryCLIFailsClosedOnOldHub(t *testing.T) {
	actions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 1})
			return
		}
		actions++
		http.Error(w, "unsafe action reached old hub", 500)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_0000000000000001", agent: "agt_0000000000000001", runID: "run_0000000000000001"}
	err := cmdDelivery(e, []string{"coverage"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") || actions != 0 {
		t.Fatalf("old hub fallback was not fail-closed: err=%v actions=%d", err, actions)
	}
	err = cmdCurrentAssignment(e, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") || actions != 0 {
		t.Fatalf("old hub current-assignment was not fail-closed: err=%v actions=%d", err, actions)
	}
}
