package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// A freeze carries the whole frozen launch plan, which passed the shared 64 KiB
// body limit on 09-30 and left an entry launching forever on 413s. Team queue
// actions take the larger launch bound; only bodies past it are refused.
func TestTeamQueueActionAcceptsLaunchPlansOverSharedBodyLimit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	task := c.task("team-queue-body")
	freeze := func(pad int) api.TeamQueueRequest {
		launch, _ := json.Marshal(map[string]string{"pad": strings.Repeat("p", pad)})
		return api.TeamQueueRequest{RequestID: "queue-freeze-fixture", Operation: "freeze", EntryID: "tqe_0000000000000000", ExpectedRevision: 1, LaunchJSON: launch}
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", freeze(96<<10), nil); code == http.StatusRequestEntityTooLarge {
		t.Fatalf("96 KiB freeze = 413; want the store to judge it")
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", freeze(int(api.MaxTeamQueueActionBody)), nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("freeze past MaxTeamQueueActionBody = %d, want 413", code)
	}
}
