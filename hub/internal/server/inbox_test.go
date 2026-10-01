package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// wi_a31c079e98518efc (i2): the messages route pages both ways from any seq
// and can return only messages addressed to an agent, while latest=1 alone
// and after/limit/to keep their earlier meaning for existing clients.
func TestListMessagesPaging(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	task := c.task("paging")
	a, err := c.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "alpha", Host: "h", Session: "alpha", Runtime: "codex"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "beta", Host: "h", Session: "beta", Runtime: "codex"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	// Ten messages: board-wide, to alpha, to beta, repeating.
	var all, forAlpha, toAlpha []int64
	for i := 0; i < 10; i++ {
		to := []string{"", a.ID, b.ID}[i%3]
		m, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: fmt.Sprintf("message %d", i), To: to}, c.who)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, m.Seq)
		if to != b.ID {
			forAlpha = append(forAlpha, m.Seq)
		}
		if to == a.ID {
			toAlpha = append(toAlpha, m.Seq)
		}
	}
	base := "/v1/tasks/" + task.ID + "/messages?"
	seqs := func(query string) []int64 {
		t.Helper()
		var out api.MessageList
		if code := c.do("GET", base+query, nil, &out); code != 200 {
			t.Fatalf("GET %s: %d", query, code)
		}
		got := []int64{}
		for _, m := range out.Messages {
			got = append(got, m.Seq)
		}
		return got
	}
	for _, tc := range []struct {
		name, query string
		want        []int64
	}{
		{"latest alone (unchanged)", "latest=1&limit=3", all[7:]},
		{"after limit to (unchanged)", fmt.Sprintf("after=%d&limit=4&to=%s", all[0], a.ID), forAlpha[1:5]},
		{"after alone (unchanged)", fmt.Sprintf("after=%d", all[8]), all[9:]},
		{"before newest", fmt.Sprintf("before=%d&latest=1&limit=2", all[5]), all[3:5]},
		{"before oldest", fmt.Sprintf("before=%d&limit=2", all[5]), all[0:2]},
		{"after and before", fmt.Sprintf("after=%d&before=%d", all[2], all[6]), all[3:6]},
		{"latest keeps after", fmt.Sprintf("after=%d&latest=1&limit=100", all[6]), all[7:]},
		{"directed newest", fmt.Sprintf("to=%s&directed=1&latest=1&limit=2", a.ID), toAlpha[1:]},
		{"directed after", fmt.Sprintf("to=%s&directed=1&after=%d", a.ID, toAlpha[0]), toAlpha[1:]},
	} {
		if got := seqs(tc.query); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %s = %v, want %v", tc.name, tc.query, got, tc.want)
		}
	}
	for _, query := range []string{"directed=1", "directed=1&to=agt_missing", "before=-1", "before=abc"} {
		if code := c.do("GET", base+query, nil, nil); code != 400 {
			t.Errorf("GET %s: %d, want 400", query, code)
		}
	}
}

// wi_e1b6ca74b7e58414: the roster route and the three calls tt inbox --unread
// makes stay under a second at 500 agents (450 closed) and 20,000 messages.
// The fixture mirrors the store's TestInboxScale; it is written through a
// second connection because the store's own is not exported.
func TestRosterAndInboxRoutesAtScale(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	c.srv = httptest.NewServer(New(st, func(*http.Request) (api.Caller, error) { return c.who, nil }))
	t.Cleanup(c.srv.Close)
	task := c.task("inbox-scale")
	items := []api.WorkItem{}
	for i := 0; i < 60; i++ {
		item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: fmt.Sprintf("Scale item %d", i), RequestID: fmt.Sprintf("scale-item-%d", i)}, c.who)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}

	const agents, bound, messages = 500, 480, 20000
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := func(q string, args ...any) int64 {
		t.Helper()
		res, err := tx.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	agentID := func(i int) string { return fmt.Sprintf("agt_%016x", i+1) }
	for i := 0; i < agents; i++ {
		status := api.AgentClosed
		if i%10 == 0 {
			status = api.AgentRunning
		}
		insert(`INSERT INTO agents (id,task_id,name,host,session,runtime,status,created_at,last_event_at,run_id) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			agentID(i), task.ID, fmt.Sprintf("scale-%d", i), "fixture", fmt.Sprintf("scale-%d", i), "codex", status, now, now, fmt.Sprintf("run_%016x", i+1))
	}
	// One third board-wide, the rest directed; every second message links an
	// item. Message 1 is addressed to agent 0 and links nothing. Agent 0 is
	// open, bound to item 0 and has read nothing; wantUnread is its count.
	var first, last, unlinkedDirected int64
	wantUnread := 0
	for j := 0; j < messages; j++ {
		to, linked := -1, j%2 == 0
		if j%3 != 0 {
			to = (j * 7) % agents
		}
		if j == 1 {
			to, linked = 0, false
		}
		from := (j*13 + 1) % agents
		if from == to {
			from = (from + 1) % agents
		}
		toID := ""
		if to >= 0 {
			toID = agentID(to)
		}
		last = insert(`INSERT INTO messages (task_id,from_agent,from_node,from_user,to_agent,text,created_at) VALUES (?,?,?,?,?,?,?)`,
			task.ID, agentID(from), "fixture", "owner", toID, fmt.Sprintf("scale message %d", j), now)
		switch j {
		case 0:
			first = last
		case 1:
			unlinkedDirected = last
		}
		if linked {
			item := items[j%len(items)]
			insert(`INSERT INTO message_work_item_links (message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,created_at) VALUES (?,?,?,?,?,'primary',?)`,
				last, task.ID, item.TaskID, item.ID, item.Revision, now)
		}
		if from != 0 && (to == 0 || (to < 0 && linked && j%len(items) == 0)) {
			wantUnread++
		}
	}
	for i := 0; i < agents; i++ {
		if i < bound {
			item := items[i%len(items)]
			insert(`INSERT INTO agent_work_item_bindings (agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES (?,?,?,?,?,?,?,0,'member',?,'{}',?)`,
				agentID(i), fmt.Sprintf("run_%016x", i+1), item.TaskID, item.ID, item.Revision, task.ID, first, strings.Repeat("0", 64), now)
		}
		if i%10 == 0 && i != 0 && i != bound {
			insert(`INSERT INTO read_cursors (task_id,agent_id,up_to) VALUES (?,?,?)`, task.ID, agentID(i), last-100)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	get := func(path string, out any) {
		t.Helper()
		if code := c.do("GET", path, nil, out); code != 200 {
			t.Fatalf("GET %s: %d", path, code)
		}
	}
	bestOf := func(fn func()) time.Duration {
		best := time.Duration(0)
		for run := 0; run < 3; run++ {
			start := time.Now()
			fn()
			if took := time.Since(start); run == 0 || took < best {
				best = took
			}
		}
		return best
	}
	base := "/v1/tasks/" + task.ID
	var roster api.AgentList
	rosterTime := bestOf(func() { get(base+"/agents", &roster) })
	if len(roster.Agents) != agents {
		t.Fatalf("roster has %d agents, want %d", len(roster.Agents), agents)
	}
	for _, a := range roster.Agents {
		if a.Status == api.AgentClosed && a.Unread != 0 {
			t.Fatalf("roster counted %d unread for closed agent %s", a.Unread, a.ID)
		}
		if a.ID == agentID(0) && a.Unread != wantUnread {
			t.Errorf("roster unread for the open bound agent = %d, want %d", a.Unread, wantUnread)
		}
	}
	var closed api.Agent
	if get(base+"/agents/"+agentID(1), &closed); closed.Status != api.AgentClosed || closed.Unread == 0 {
		t.Errorf("single-agent route lost a closed agent's unread count: %+v", closed)
	}

	// tt inbox --unread: the roster, the caller, then its page after the cursor.
	var self api.Agent
	var page api.MessageList
	inboxTime := bestOf(func() {
		get(base+"/agents", &roster)
		get(base+"/agents/"+agentID(0), &self)
		get(fmt.Sprintf("%s/messages?to=%s&after=%d&limit=50", base, agentID(0), self.ReadUpTo), &page)
	})
	if self.Unread != wantUnread || len(page.Messages) != 50 {
		t.Errorf("unread inbox: unread %d (want %d), page of %d (want 50)", self.Unread, wantUnread, len(page.Messages))
	}
	found := false
	for _, m := range page.Messages {
		found = found || m.Seq == unlinkedDirected
	}
	if !found {
		t.Errorf("unlinked directed message #%d is not in the bound agent's page", unlinkedDirected)
	}
	t.Logf("500 agents, 20,000 messages: roster route %v, unread inbox routes %v", rosterTime, inboxTime)
	race := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			race = race || (setting.Key == "-race" && setting.Value == "true")
		}
	}
	if testing.Short() || race {
		t.Log("wall-clock bounds are not checked in short or race runs")
		return
	}
	if rosterTime > time.Second {
		t.Errorf("roster route took %v, want under 1 s", rosterTime)
	}
	if inboxTime > time.Second {
		t.Errorf("unread inbox routes took %v, want under 1 s", inboxTime)
	}
}
