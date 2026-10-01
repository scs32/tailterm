package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/testverification"
)

func TestReleaseHTTPHandlerBoundaryAndEmptyRead(t *testing.T) {
	c := newClient(t)
	task := c.task("release-http")
	path := "/v1/tasks/" + task.ID + "/releases"
	var jobs []api.ReleaseJob
	if code := c.do("GET", path, nil, &jobs); code != 200 || len(jobs) != 0 {
		t.Fatal(code, jobs)
	}
	req := api.ReleaseRequest{RequestID: "fixture", Operation: "enqueue", AgentID: api.NewID("agt"), RunID: api.NewID("run"), EntryID: api.NewID("tqe")}
	if code := c.do("POST", path+"/actions", req, nil); code != 409 {
		t.Fatal("nonhandler enqueue", code)
	}
}

// f5: the read-only handler operation refuses anyone but the exact deployer.
func TestReleaseHTTPHandlerResolutionRequiresDeployer(t *testing.T) {
	c := newClient(t)
	task := c.task("release-handler-http")
	req := api.ReleaseRequest{Operation: "handler", AgentID: api.NewID("agt"), RunID: api.NewID("run")}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/releases/actions", req, nil); code != 409 {
		t.Fatal("nondeployer handler resolution", code)
	}
}

// s1 over HTTP: the owner records a hand release citing a release
// intervention (201), agents and uncited records are refused (409), the list
// is read-only (200), and supersede refuses (409) without a covering record
// and accepts (201) with one. The job row is seeded in this test's own
// isolated database: an enqueue needs the full verification chain, which the
// store tests exercise.
func TestReleaseHTTPHandReleaseAndSupersede(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	ctx := context.Background()
	task := c.task("release-hand-http")
	actions := "/v1/tasks/" + task.ID + "/releases/actions"
	h, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "devbox", Session: "handler", Role: api.AgentRoleDatabaseHandler}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "hand release", RequestID: "item"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	var intervention api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/interventions", api.CreateInterventionRequest{Kind: "release", ItemID: item.ID, Text: "released by hand", RequestID: "intervention"}, &intervention); code != 201 {
		t.Fatal("intervention", code)
	}
	jobCommit, released := strings.Repeat("b", 40), strings.Repeat("c", 40)
	hand := func(key string, seq int64, commits ...string) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: key, Operation: "hand_release", HandRelease: &api.HandRelease{InterventionSeq: seq, ReleasedCommit: released, Release: "20260930-hand", Targets: []string{"hub", "bridge"}, Commits: commits}}
	}
	agentReq := hand("agent", intervention.Seq, jobCommit)
	agentReq.AgentID, agentReq.RunID = h.ID, h.RunID
	if code := c.do("POST", actions, agentReq, nil); code != 409 {
		t.Fatal("agent recorded a hand release", code)
	}
	if code := c.do("POST", actions, hand("uncited", intervention.Seq+100, jobCommit), nil); code != 409 {
		t.Fatal("record without a release intervention", code)
	}
	var record api.HandRelease
	if code := c.do("POST", actions, hand("record", intervention.Seq, jobCommit), &record); code != 201 || record.ID == "" || record.RecordedBy == nil || *record.RecordedBy != c.who {
		t.Fatal("record", code, record)
	}
	var list []api.HandRelease
	if code := c.do("POST", actions, api.ReleaseRequest{Operation: "hand_releases"}, &list); code != 200 || len(list) != 1 || list[0].ID != record.ID {
		t.Fatal("list", code, list)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	job := api.ReleaseJob{ID: api.NewID("rel"), TaskID: task.ID, EntryID: api.NewID("tqe"), ItemID: item.ID, BaseCommit: strings.Repeat("a", 40), Commit: jobCommit, State: "verified", Generation: 1}
	raw, _ := json.Marshal(job)
	if _, err = db.Exec(`INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, task.ID, job.ID, job.EntryID, job.State, job.Generation, string(raw)); err != nil {
		t.Fatal(err)
	}
	supersede := func(key string, sup api.ReleaseSupersession) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: job.ID, ExpectedGeneration: job.Generation, Supersession: &sup}
	}
	for name, sup := range map[string]api.ReleaseSupersession{
		"unknown record": {ReleasedCommit: released, HandReleaseID: "hrl_0123456789abcdef"},
		"commit differs": {ReleasedCommit: jobCommit, HandReleaseID: record.ID},
	} {
		if code := c.do("POST", actions, supersede(strings.ReplaceAll(name, " ", "-"), sup), nil); code != 409 {
			t.Fatal(name, code)
		}
	}
	var done api.ReleaseJob
	if code := c.do("POST", actions, supersede("supersede", api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: record.ID}), &done); code != 201 || done.State != "superseded" || strings.Join(done.Supersession.Targets, ",") != "hub,bridge" || done.SettledAt != record.CreatedAt {
		t.Fatal("supersede", code, done)
	}
}

// a6 over HTTP: retry is one more release action. A deployer's retry of a
// refused job is refused (409) and adds nothing; the handler's creates a new
// verified job linked to the refused one (201), and both are listed. The
// entry is a real accepted, verified candidate; only its queue row and the
// first job's refusal are seeded in this test's own isolated database.
func TestReleaseHTTPRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	ctx := context.Background()
	task := c.task("release-retry-http")
	actions := "/v1/tasks/" + task.ID + "/releases/actions"
	h, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "devbox", Session: "handler", Role: api.AgentRoleDatabaseHandler}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "devbox", Session: "deployer", Role: api.AgentRoleDeployment}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "retry", RequestID: "item"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.Prepare(native, task.ID, item); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if item, err = st.GetWorkItem(ctx, task.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	acceptance, _ := json.Marshal(api.TeamIntegrationAcceptance{Repository: "fixture", BaseCommit: testverification.Commit, Commit: testverification.Commit, ItemRevision: item.Revision})
	entry, now := api.NewID("tqe"), time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,repository,base_commit,acceptance_json) VALUES(?,?,?,?,1,'planned',1,'finished',?,?,'fixture',?,?)`, entry, task.ID, item.ID, item.Revision, now, now, testverification.Commit, string(acceptance)); err != nil {
		t.Fatal(err)
	}
	var first api.ReleaseJob
	if code := c.do("POST", actions, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry}, &first); code != 201 || first.State != "verified" {
		t.Fatal("enqueue", code, first)
	}
	// Refused before publication, as the deployer's own refuse leaves it.
	first.State, first.Generation, first.AgentID, first.RunID = "refused", first.Generation+2, d.ID, d.RunID
	raw, _ := json.Marshal(first)
	if _, err = db.Exec(`UPDATE release_jobs SET state=?,generation=?,record_json=? WHERE task_id=? AND id=?`, first.State, first.Generation, string(raw), task.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	retry := func(key string, a api.Agent) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: key, Operation: "retry", AgentID: a.ID, RunID: a.RunID, EntryID: entry, JobID: first.ID, ExpectedGeneration: first.Generation, Retry: &api.ReleaseRetry{Reason: "Environment fixed"}}
	}
	if code := c.do("POST", actions, api.ReleaseRequest{RequestID: "enqueue-again", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry}, nil); code != 409 {
		t.Fatal("enqueue over a refused job", code)
	}
	if code := c.do("POST", actions, retry("by-deployer", d), nil); code != 409 {
		t.Fatal("deployer retry", code)
	}
	noReason := retry("no-reason", h)
	noReason.Retry = nil
	if code := c.do("POST", actions, noReason, nil); code != 400 {
		t.Fatal("retry without a retry record", code)
	}
	var jobs []api.ReleaseJob
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/releases", nil, &jobs); code != 200 || len(jobs) != 1 {
		t.Fatal("refused retries added a job", code, jobs)
	}
	var second api.ReleaseJob
	if code := c.do("POST", actions, retry("retry", h), &second); code != 201 || second.ID == first.ID || second.State != "verified" || second.Generation != 1 || second.Commit != first.Commit || second.RetryOf == nil || second.RetryOf.JobID != first.ID || second.RetryOf.Attempt != 2 || second.RetryOf.AgentID != h.ID {
		t.Fatalf("handler retry %d %+v", code, second)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/releases", nil, &jobs); code != 200 || len(jobs) != 2 || jobs[0].ID != first.ID || jobs[0].State != "refused" || jobs[1].ID != second.ID {
		t.Fatal("history", code, jobs)
	}
}
