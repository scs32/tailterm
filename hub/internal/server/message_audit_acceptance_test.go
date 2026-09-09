package server

// Independent HTTP acceptance for wi_abc84eb23688d903-a1, work order #450,
// contract #453/#457. All actors, records, servers, and SQLite files are synthetic.
// Store failure injection/migration tests remain with the implementation owner.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type messageAuditFixture struct {
	c    *client
	path string
	st   *store.Store
}

func newMessageAuditFixture(t *testing.T) *messageAuditFixture {
	t.Helper()
	f := &messageAuditFixture{
		c:    &client{t: t, who: api.Caller{Node: "audit-fixture-node", User: "audit-fixture-user"}},
		path: filepath.Join(t.TempDir(), "audit.sqlite"),
	}
	f.open()
	t.Cleanup(func() { f.c.srv.Close(); f.st.Close() })
	return f
}

func (f *messageAuditFixture) open() {
	f.c.t.Helper()
	var err error
	f.st, err = store.Open(f.path)
	if err != nil {
		f.c.t.Fatal(err)
	}
	f.c.srv = httptest.NewServer(New(f.st, func(r *http.Request) (api.Caller, error) {
		if r.Header.Get("X-Test-Deny") != "" {
			return api.Caller{}, fmt.Errorf("synthetic identity denied")
		}
		who := f.c.who
		if node := r.Header.Get("X-Audit-Node"); node != "" {
			who.Node = node
		}
		if user := r.Header.Get("X-Audit-User"); user != "" {
			who.User = user
		}
		return who, nil
	}))
}

func (f *messageAuditFixture) reopen() {
	f.c.srv.Close()
	if err := f.st.Close(); err != nil {
		f.c.t.Fatal(err)
	}
	f.open()
}

func auditExchange(c *client, method, path string, body any, headers map[string]string) (int, []byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequest(method, c.srv.URL+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, err = io.ReadAll(res.Body)
	return res.StatusCode, data, err
}

func auditMust(c *client, want int, method, path string, body, out any) {
	c.t.Helper()
	code, data, err := auditExchange(c, method, path, body, nil)
	if err != nil || code != want {
		c.t.Fatalf("%s %s: status=%d want=%d err=%v body=%s", method, path, code, want, err, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("decode %s: %v", path, err)
		}
	}
}

func auditItem(c *client, task api.Task, key string) api.WorkItem {
	c.t.Helper()
	var item api.WorkItem
	auditMust(c, 201, "POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Synthetic audit issue", RequestID: key}, &item)
	return item
}

func auditPostPath(task string) string { return "/v1/tasks/" + task + "/messages" }
func auditReceiptPath(task, key, agent string) string {
	path := auditPostPath(task) + "/receipts/" + url.PathEscape(key)
	if agent != "" {
		path += "?agentId=" + url.QueryEscape(agent)
	}
	return path
}

func auditLinked(item api.WorkItem, key string) api.PostMessageRequest {
	return api.PostMessageRequest{Text: "Synthetic audited work", RequestID: key, WorkItems: []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
}

func auditReceipt(t *testing.T, message api.Message, request api.PostMessageRequest) {
	t.Helper()
	if !reflect.DeepEqual(message.WorkItems, request.WorkItems) || !reflect.DeepEqual(message.WorkOrderMessage, request.WorkOrderMessage) {
		t.Fatalf("creation context changed: message=%+v request=%+v", message, request)
	}
	r := message.PostReceipt
	if r == nil || r.ID == "" || r.RequestID != request.RequestID || r.TaskID != message.TaskID || r.MessageSeq != message.Seq || r.CreatedAt.IsZero() {
		t.Fatalf("invalid committed message receipt: %+v", message)
	}
}

type auditSnapshot struct {
	Messages []api.Message
	Events   []api.Event
}

func auditRead(c *client, task string) auditSnapshot {
	c.t.Helper()
	var messages api.MessageList
	var events api.EventList
	auditMust(c, 200, "GET", auditPostPath(task)+"?limit=1000", nil, &messages)
	auditMust(c, 200, "GET", "/v1/tasks/"+task+"/events?limit=1000", nil, &events)
	return auditSnapshot{messages.Messages, events.Events}
}

func auditUnchanged(c *client, task string, before auditSnapshot) {
	c.t.Helper()
	if after := auditRead(c, task); !reflect.DeepEqual(before, after) {
		c.t.Fatalf("request changed messages/events: before=%+v after=%+v", before, after)
	}
}

func auditAgent(c *client, task, id string) api.Agent {
	c.t.Helper()
	var agent api.Agent
	auditMust(c, 200, "GET", "/v1/tasks/"+task+"/agents/"+id, nil, &agent)
	return agent
}

func auditRetire(c *client, task api.Task, agent api.Agent) {
	c.t.Helper()
	auditMust(c, 201, "POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: agent.ID, RunID: agent.RunID}, nil)
	status := api.AgentRetired
	auditMust(c, 200, "PATCH", "/v1/tasks/"+task.ID+"/agents/"+agent.ID, api.UpdateAgentRequest{Status: &status}, nil)
}

func TestMessageAuditAcceptanceLegacyAndTypedRoundTrip(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task := c.task("audit-round-trip")
	item := auditItem(c, task, "item")
	var legacy, reply, linked, keyed api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Legacy order text"}, &legacy)
	auditMust(c, 201, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Legacy reply", ReplyTo: legacy.Seq}, &reply)
	if legacy.PostReceipt != nil || legacy.WorkItems != nil || legacy.WorkOrderMessage != nil || reply.ReplyTo != legacy.Seq {
		t.Fatalf("legacy shape/reply changed: %+v %+v", legacy, reply)
	}
	req := auditLinked(item, "round-trip")
	req.ReplyTo = legacy.Seq
	req.WorkOrderMessage = &api.MessageReference{TaskID: task.ID, Seq: legacy.Seq}
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, &linked)
	auditReceipt(t, linked, req)
	if linked.From.Node != c.who.Node || linked.From.User != c.who.User || linked.From.AgentID != "" || linked.ReplyTo != legacy.Seq {
		t.Fatalf("authorship/reply changed: %+v", linked)
	}
	unlinked := api.PostMessageRequest{Text: "Observe-mode keyed unlinked post", RequestID: "keyed-unlinked"}
	auditMust(c, 201, "POST", auditPostPath(task.ID), unlinked, &keyed)
	auditReceipt(t, keyed, unlinked)
	list := auditRead(c, task.ID)
	if len(list.Messages) != 4 || !reflect.DeepEqual(list.Messages[2], linked) || !reflect.DeepEqual(list.Messages[3], keyed) {
		t.Fatalf("list lost original context: %+v", list.Messages)
	}
	before := auditRead(c, task.ID)
	var recovered api.Message
	auditMust(c, 200, "GET", auditReceiptPath(task.ID, req.RequestID, ""), nil, &recovered)
	if !reflect.DeepEqual(linked, recovered) {
		t.Fatalf("recovery changed original: %+v %+v", linked, recovered)
	}
	auditUnchanged(c, task.ID, before)
}

func TestMessageAuditAcceptanceInvalidReferencesAreAtomic(t *testing.T) {
	cases := []string{"missing-key", "missing-item", "foreign-item", "wrong-item-owner", "stale-revision", "future-revision", "related-link", "two-primaries", "order-without-primary", "foreign-order", "missing-order", "foreign-reply", "foreign-actor", "malformed-key"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			c := newMessageAuditFixture(t).c
			task, foreign := c.task("atomic"), c.task("foreign")
			target, other := c.agent(task, "recipient"), c.agent(foreign, "foreign-agent")
			item, foreignItem := auditItem(c, task, "item"), auditItem(c, foreign, "foreign-item")
			var order, foreignOrder api.Message
			auditMust(c, 201, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Order"}, &order)
			auditMust(c, 201, "POST", auditPostPath(foreign.ID), api.PostMessageRequest{Text: "Foreign order"}, &foreignOrder)
			auditRetire(c, task, target)
			req := auditLinked(item, "atomic-key")
			req.To = target.ID
			req.WorkOrderMessage = &api.MessageReference{TaskID: task.ID, Seq: order.Seq}
			switch name {
			case "missing-key":
				req.RequestID = ""
			case "missing-item":
				req.WorkItems[0].ItemID = "wi_ffffffffffffffff"
			case "foreign-item":
				req.WorkItems[0] = auditLinked(foreignItem, "").WorkItems[0]
			case "wrong-item-owner":
				req.WorkItems[0].ItemID = foreignItem.ID
			case "stale-revision":
				title := "Updated fixture"
				auditMust(c, 200, "PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, &item)
			case "future-revision":
				req.WorkItems[0].ItemRevision = 999
			case "related-link":
				req.WorkItems[0].Relationship = "related"
			case "two-primaries":
				req.WorkItems = append(req.WorkItems, req.WorkItems[0])
			case "order-without-primary":
				req.WorkItems = nil
			case "foreign-order":
				req.WorkOrderMessage = &api.MessageReference{TaskID: foreign.ID, Seq: foreignOrder.Seq}
			case "missing-order":
				req.WorkOrderMessage.Seq = 999999
			case "foreign-reply":
				req.ReplyTo = foreignOrder.Seq
			case "foreign-actor":
				req.AgentID = other.ID
			case "malformed-key":
				req.RequestID = "invalid key"
			}
			before, foreignBefore := auditRead(c, task.ID), auditRead(c, foreign.ID)
			code, body, err := auditExchange(c, "POST", auditPostPath(task.ID), req, nil)
			want := 400
			if name == "stale-revision" || name == "future-revision" {
				want = 409
			}
			if err != nil || code != want {
				t.Fatalf("invalid reference accepted/failed unexpectedly: %d %s %v", code, body, err)
			}
			auditUnchanged(c, task.ID, before)
			auditUnchanged(c, foreign.ID, foreignBefore)
			if got := auditAgent(c, task.ID, target.ID); got.Status != api.AgentRetired || got.RunID != target.RunID {
				t.Fatalf("rejected post resumed recipient: %+v", got)
			}
			if name != "missing-key" && name != "malformed-key" {
				auditMust(c, 404, "GET", auditReceiptPath(task.ID, req.RequestID, ""), nil, nil)
			}
			// A failed validation must not poison the otherwise reusable request key.
			repaired := auditLinked(item, "atomic-key")
			repaired.To = target.ID
			var committed api.Message
			auditMust(c, 201, "POST", auditPostPath(task.ID), repaired, &committed)
			auditReceipt(t, committed, repaired)
			if len(auditRead(c, task.ID).Messages) != len(before.Messages)+1 {
				t.Fatal("invalid post left a hidden message")
			}
		})
	}
}

func TestMessageAuditAcceptanceChangedIntentConflicts(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task := c.task("intent")
	item, second := auditItem(c, task, "one"), auditItem(c, task, "two")
	target, alternate := c.agent(task, "target"), c.agent(task, "alternate")
	var order, alternateOrder api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Order one"}, &order)
	auditMust(c, 201, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Order two"}, &alternateOrder)
	req := auditLinked(item, "same-intent-key")
	req.To = target.ID
	req.WorkOrderMessage = &api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, nil)
	before := auditRead(c, task.ID)
	for _, name := range []string{"text", "recipient", "item", "order", "reply", "unlinked"} {
		changed := req
		changed.WorkItems = append([]api.MessageWorkItem(nil), req.WorkItems...)
		switch name {
		case "text":
			changed.Text = "Different intent"
		case "recipient":
			changed.To = alternate.ID
		case "item":
			changed.WorkItems = auditLinked(second, "").WorkItems
		case "order":
			changed.WorkOrderMessage = &api.MessageReference{TaskID: task.ID, Seq: alternateOrder.Seq}
		case "reply":
			changed.ReplyTo = order.Seq
		case "unlinked":
			changed.WorkItems, changed.WorkOrderMessage = nil, nil
		}
		auditMust(c, 409, "POST", auditPostPath(task.ID), changed, nil)
		auditUnchanged(c, task.ID, before)
	}
}

func TestMessageAuditAcceptanceReplaySurvivesEditRetirementClosureAndReopen(t *testing.T) {
	f := newMessageAuditFixture(t)
	c := f.c
	task := c.task("durable-replay")
	item := auditItem(c, task, "item")
	registration := api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "synthetic-handler", Host: "fixture", Session: "fixture-only", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}
	var target api.Agent
	auditMust(c, 201, "POST", "/v1/tasks/"+task.ID+"/agents", registration, &target)
	auditRetire(c, task, target)
	req := auditLinked(item, "durable-replay")
	req.To = target.ID
	var original api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, &original)
	auditReceipt(t, original, req)
	if got := auditAgent(c, task.ID, target.ID); got.Status != api.AgentDone || got.RunID != target.RunID {
		t.Fatalf("first post did not resume original run: %+v", got)
	}
	resumes := 0
	for _, event := range auditRead(c, task.ID).Events {
		if event.Kind == api.EventResumed {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("initial resumes=%d", resumes)
	}
	auditRetire(c, task, target)
	title := "Edited after committed post"
	auditMust(c, 200, "PATCH", "/v1/tasks/"+task.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, nil)
	before := auditRead(c, task.ID)
	for _, method := range []string{"POST", "GET"} {
		var replay api.Message
		if method == "POST" {
			auditMust(c, 201, method, auditPostPath(task.ID), req, &replay)
		} else {
			auditMust(c, 200, method, auditReceiptPath(task.ID, req.RequestID, ""), nil, &replay)
		}
		if !reflect.DeepEqual(original, replay) {
			t.Fatalf("replay rewrote creation receipt: %+v %+v", original, replay)
		}
		auditUnchanged(c, task.ID, before)
		if got := auditAgent(c, task.ID, target.ID); got.Status != api.AgentRetired {
			t.Fatalf("replay resumed newly retired agent: %+v", got)
		}
	}
	// The supported handler restart API retains agent identity but changes run
	// identity. An old message receipt must not wake that replacement run either.
	exited := api.AgentExited
	auditMust(c, 200, "PATCH", "/v1/tasks/"+task.ID+"/agents/"+target.ID, api.UpdateAgentRequest{Status: &exited}, nil)
	registration.ExpectedRunID = target.RunID
	var replacement api.Agent
	auditMust(c, 201, "POST", "/v1/tasks/"+task.ID+"/agents", registration, &replacement)
	if replacement.ID != target.ID || replacement.RunID == target.RunID {
		t.Fatalf("synthetic restart did not replace the exact run: %+v", replacement)
	}
	auditRetire(c, task, replacement)
	before = auditRead(c, task.ID)
	var replacementReplay api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, &replacementReplay)
	if !reflect.DeepEqual(original, replacementReplay) {
		t.Fatal("replacement-run replay changed original message")
	}
	if got := auditAgent(c, task.ID, target.ID); got.Status != api.AgentRetired || got.RunID != replacement.RunID {
		t.Fatalf("old receipt changed the replacement run: %+v", got)
	}
	auditUnchanged(c, task.ID, before)
	auditMust(c, 200, "DELETE", "/v1/tasks/"+task.ID, nil, nil)
	before = auditRead(c, task.ID)
	var replay api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, &replay)
	if !reflect.DeepEqual(original, replay) {
		t.Fatal("closed-project replay changed original")
	}
	fresh := req
	fresh.RequestID = "new-after-closure"
	fresh.WorkItems = append([]api.MessageWorkItem(nil), req.WorkItems...)
	fresh.WorkItems[0].ItemRevision = 2
	auditMust(c, 409, "POST", auditPostPath(task.ID), fresh, nil)
	auditUnchanged(c, task.ID, before)
	f.reopen()
	auditMust(c, 200, "GET", auditReceiptPath(task.ID, req.RequestID, ""), nil, &replay)
	if !reflect.DeepEqual(original, replay) {
		t.Fatal("database reopen lost original receipt/context")
	}
	auditUnchanged(c, task.ID, before)
}

func TestMessageAuditAcceptanceConcurrentPostsCommitOnce(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task := c.task("concurrent")
	item := auditItem(c, task, "item")
	target := c.agent(task, "target")
	auditRetire(c, task, target)
	req := auditLinked(item, "concurrent-key")
	req.To = target.ID
	start := make(chan struct{})
	type result struct {
		code int
		body []byte
		err  error
	}
	results := make(chan result, 6)
	for range 6 {
		go func() {
			<-start
			code, body, err := auditExchange(c, "POST", auditPostPath(task.ID), req, nil)
			results <- result{code, body, err}
		}()
	}
	close(start)
	var original api.Message
	for i := 0; i < 6; i++ {
		r := <-results
		if r.err != nil || r.code != 201 {
			t.Fatalf("concurrent post: %d %s %v", r.code, r.body, r.err)
		}
		var message api.Message
		if err := json.Unmarshal(r.body, &message); err != nil {
			t.Fatal(err)
		}
		auditReceipt(t, message, req)
		if i == 0 {
			original = message
		} else if !reflect.DeepEqual(original, message) {
			t.Fatal("concurrent posts returned different commits")
		}
	}
	snapshot := auditRead(c, task.ID)
	if len(snapshot.Messages) != 1 {
		t.Fatalf("duplicate messages: %d", len(snapshot.Messages))
	}
	counts := map[string]int{}
	for _, event := range snapshot.Events {
		counts[event.Kind]++
	}
	if counts[api.EventMessage] != 1 || counts[api.EventResumed] != 1 {
		t.Fatalf("duplicate side effects: %+v", counts)
	}
}

func TestMessageAuditAcceptanceLostResponseRecoveryAndScope(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task, foreign := c.task("lost-response"), c.task("other-project")
	item := auditItem(c, task, "item")
	author, other := c.agent(task, "author"), c.agent(task, "other")
	req := auditLinked(item, "qa:a1.lost-response")
	req.AgentID = author.ID
	committed := make(chan api.Message, 1)
	// The proxy closes only after the real hub's handler commits a 201 response.
	// The submitting client receives no response bytes and must use recovery.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, data, err := auditExchange(c, "POST", auditPostPath(task.ID), req, nil)
		var message api.Message
		if err != nil || code != 201 || json.Unmarshal(data, &message) != nil {
			http.Error(w, "synthetic upstream did not commit", 500)
			return
		}
		committed <- message
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("drop response: %v", err)
			return
		}
		conn.Close()
	}))
	t.Cleanup(proxy.Close)
	body, _ := json.Marshal(req)
	res, err := (&http.Client{Timeout: 5 * time.Second}).Post(proxy.URL, "application/json", bytes.NewReader(body))
	if res != nil {
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("response-loss fixture unexpectedly returned a response")
	}
	var original api.Message
	select {
	case original = <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("response was dropped before a confirmed commit")
	}
	auditReceipt(t, original, req)
	before := auditRead(c, task.ID)
	apiClient, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := apiClient.GetMessagePostReceipt(context.Background(), task.ID, req.RequestID, author.ID)
	if err != nil || !reflect.DeepEqual(original, recovered) {
		t.Fatalf("typed client failed colon/dot key recovery: %+v %v", recovered, err)
	}
	auditMust(c, 404, "GET", auditReceiptPath(task.ID, "missing-receipt-key", author.ID), nil, nil)
	for _, tc := range []struct {
		name, task, agent string
		headers           map[string]string
	}{
		{"omitted-agent", task.ID, "", nil}, {"different-agent", task.ID, other.ID, nil},
		{"wrong-project", foreign.ID, author.ID, nil},
		{"different-node", task.ID, author.ID, map[string]string{"X-Audit-Node": "other-fixture-node"}},
		{"different-user", task.ID, author.ID, map[string]string{"X-Audit-User": "other-fixture-user"}},
	} {
		code, data, err := auditExchange(c, "GET", auditReceiptPath(tc.task, req.RequestID, tc.agent), nil, tc.headers)
		if err != nil || code != 404 {
			t.Fatalf("%s recovered another scope: %d %s %v", tc.name, code, data, err)
		}
	}
	for _, path := range []string{auditReceiptPath(task.ID, "invalid key", author.ID), auditReceiptPath(task.ID, req.RequestID, "bad-agent")} {
		auditMust(c, 400, "GET", path, nil, nil)
	}
	code, _, err := auditExchange(c, "GET", auditReceiptPath(task.ID, req.RequestID, author.ID), nil, map[string]string{"X-Test-Deny": "1"})
	if err != nil || code != 403 {
		t.Fatalf("denied recovery: %d %v", code, err)
	}
	var replay api.Message
	auditMust(c, 201, "POST", auditPostPath(task.ID), req, &replay)
	if !reflect.DeepEqual(original, replay) {
		t.Fatal("lost-response retry duplicated/changed original")
	}
	auditUnchanged(c, task.ID, before)
}

func TestMessageAuditAcceptanceHumanCrossProjectDispatch(t *testing.T) {
	c := newMessageAuditFixture(t).c
	source, target := c.task("dispatch-source"), c.task("dispatch-target")
	item := auditItem(c, source, "item")
	sourceAgent, targetAgent := c.agent(source, "source-agent"), c.agent(target, "target-lead")
	orchestrator := targetAgent.Name
	auditMust(c, 200, "PATCH", "/v1/tasks/"+target.ID, api.UpdateTaskRequest{Orchestrator: &orchestrator}, nil)
	path := "/v1/tasks/" + source.ID + "/work-items/" + item.ID + "/dispatch"
	req := api.DispatchWorkItemRequest{Revision: 1, TargetTaskID: target.ID, RequestID: "cross-project", AgentID: sourceAgent.ID}
	sourceBefore, targetBefore := auditRead(c, source.ID), auditRead(c, target.ID)
	auditMust(c, 400, "POST", path, req, nil)
	auditUnchanged(c, source.ID, sourceBefore)
	auditUnchanged(c, target.ID, targetBefore)
	req.AgentID = ""
	var original api.WorkItemDispatchResult
	auditMust(c, 201, "POST", path, req, &original)
	if original.Item.TaskID != source.ID || original.Dispatch.TargetAgentID != targetAgent.ID || original.Dispatch.TargetTaskID != target.ID {
		t.Fatalf("dispatch lost source/recipient: %+v", original)
	}
	messages := auditRead(c, target.ID).Messages
	if len(messages) != 1 || messages[0].Seq != original.Dispatch.MessageSeq || messages[0].To != targetAgent.ID || messages[0].From.AgentID != "" {
		t.Fatalf("dispatch message: %+v", messages)
	}
	wantLink := auditLinked(item, "").WorkItems
	if !reflect.DeepEqual(messages[0].WorkItems, wantLink) {
		t.Fatalf("dispatch source link: %+v want %+v", messages[0].WorkItems, wantLink)
	}
	title := "Source edited after dispatch"
	auditMust(c, 200, "PATCH", "/v1/tasks/"+source.ID+"/work-items/"+item.ID, api.UpdateWorkItemRequest{Revision: 1, Title: &title}, nil)
	auditMust(c, 200, "DELETE", "/v1/tasks/"+target.ID, nil, nil)
	auditMust(c, 200, "DELETE", "/v1/tasks/"+source.ID, nil, nil)
	sourceBefore, targetBefore = auditRead(c, source.ID), auditRead(c, target.ID)
	var replay api.WorkItemDispatchResult
	auditMust(c, 201, "POST", path, req, &replay)
	if !reflect.DeepEqual(original.Dispatch, replay.Dispatch) || replay.Item.TaskID != source.ID {
		t.Fatalf("dispatch replay changed identity/ownership: %+v", replay)
	}
	auditUnchanged(c, source.ID, sourceBefore)
	auditUnchanged(c, target.ID, targetBefore)
}
