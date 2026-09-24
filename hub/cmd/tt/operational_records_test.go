package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperationalCLIUnsupportedCapability(t *testing.T) {
	for _, caps := range []string{`{}`, `{"operationalRecords":{"supported":false,"versions":[1]}}`, `{"operationalRecords":{"supported":true,"versions":[9]}}`} {
		t.Run(caps, func(t *testing.T) {
			actions := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(caps))
					return
				}
				actions++
				w.WriteHeader(500)
			}))
			defer srv.Close()
			e := env{hub: srv.URL, task: "tsk_0000000000000000"}
			if err := cmdOperationalRecord(e, []string{"get", "opr_0000000000000000"}); err == nil || actions != 0 {
				t.Fatalf("unknown capability actions=%d err=%v", actions, err)
			}
		})
	}
}
