package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A queue listing larger than the default 4 MiB response cap must still parse.
func TestListTeamQueueAcceptsListsOverFourMiB(t *testing.T) {
	pad := strings.Repeat("x", 1<<20)
	entries := make([]TeamQueueEntry, 5)
	for i := range entries {
		entries[i] = TeamQueueEntry{ID: "tqe_" + string(rune('a'+i)), Cwd: pad}
	}
	body, err := json.Marshal(TeamQueueList{Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 4<<20 {
		t.Fatalf("fixture is %d bytes, want more than 4 MiB", len(body))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = "token"
	list, err := c.ListTeamQueue(context.Background(), "tsk_test")
	if err != nil {
		t.Fatalf("list over 4 MiB: %v", err)
	}
	if len(list.Entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(list.Entries))
	}
}
