package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func agentIDs(agents []api.Agent) []string {
	ids := []string{}
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	sort.Strings(ids)
	return ids
}

func (c *client) agents(task api.Task, query string) agentsPage {
	c.t.Helper()
	var page agentsPage
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents"+query, nil, &page); code != 200 {
		c.t.Fatalf("agents%s = %d", query, code)
	}
	return page
}

// The project detail without agents names the omission; the default detail
// is unchanged, roster included.
func TestAgentRosterDetailWithoutAgents(t *testing.T) {
	c := newClient(t)
	task := c.task("roster-detail")
	lead := c.agent(task, "lead")
	var raw map[string]json.RawMessage
	if code := c.do("GET", "/v1/tasks/"+task.ID+"?agents=none", nil, &raw); code != 200 {
		t.Fatalf("agents=none = %d", code)
	}
	if _, ok := raw["agents"]; ok || string(raw["agentsOmitted"]) != "true" || raw["task"] == nil || raw["latestSeq"] == nil {
		t.Fatalf("agents=none body: %v", raw)
	}
	var full map[string]json.RawMessage
	if code := c.do("GET", "/v1/tasks/"+task.ID, nil, &full); code != 200 {
		t.Fatalf("default detail = %d", code)
	}
	if _, ok := full["agentsOmitted"]; ok || len(full) != 3 {
		t.Fatalf("default detail keys changed: %v", full)
	}
	var detail api.TaskDetail
	if code := c.do("GET", "/v1/tasks/"+task.ID, nil, &detail); code != 200 || len(detail.Agents) != 1 || detail.Agents[0].ID != lead.ID {
		t.Fatalf("default detail agents = %d %+v", code, detail.Agents)
	}
	if code := c.do("GET", "/v1/tasks/"+task.ID+"?agents=all", nil, nil); code != 400 {
		t.Fatalf("agents=all = %d, want 400", code)
	}
}

// A since read returns only the agents whose row changed: activity alone, an
// unread count after a directed message, or a new agent.
func TestAgentRosterSinceReturnsOnlyChangedAgents(t *testing.T) {
	c := newClient(t)
	task := c.task("roster-since")
	lead := c.agent(task, "lead")
	builder := c.agent(task, "builder")
	c.agent(task, "reviewer")
	first := c.agents(task, "")
	if len(first.Agents) != 3 || first.Cursor == "" || first.Changes || first.Reset {
		t.Fatalf("whole list: %+v", first)
	}
	quiet := c.agents(task, "?since="+first.Cursor)
	if !quiet.Changes || len(quiet.Agents) != 0 || len(quiet.Removed) != 0 || quiet.Cursor != first.Cursor {
		t.Fatalf("no change: %+v", quiet)
	}
	report := api.ActivityReport{RequestID: "roster-activity", RunID: builder.RunID, Activity: api.AgentActivity{State: "working", ObservedAt: time.Now().UTC()}}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents/"+builder.ID+"/activity", report, nil); code != 200 && code != 201 {
		t.Fatalf("activity = %d", code)
	}
	activity := c.agents(task, "?since="+first.Cursor)
	if !activity.Changes || !reflect.DeepEqual(agentIDs(activity.Agents), []string{builder.ID}) || activity.Agents[0].Activity == nil || activity.Cursor == first.Cursor {
		t.Fatalf("activity change: %+v", activity)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "please look", To: lead.ID}, nil); code != 201 {
		t.Fatalf("message = %d", code)
	}
	unread := c.agents(task, "?since="+activity.Cursor)
	if !reflect.DeepEqual(agentIDs(unread.Agents), []string{lead.ID}) || unread.Agents[0].Unread != 1 {
		t.Fatalf("unread change: %+v", unread)
	}
	added := c.agent(task, "verifier")
	fresh := c.agents(task, "?since="+unread.Cursor)
	if !reflect.DeepEqual(agentIDs(fresh.Agents), []string{added.ID}) {
		t.Fatalf("new agent: %+v", fresh)
	}
	// An older cursor still gets every change after it, in one answer.
	all := c.agents(task, "?since="+first.Cursor)
	if want := []string{lead.ID, builder.ID, added.ID}; !reflect.DeepEqual(agentIDs(all.Agents), agentIDsOf(want)) || all.Cursor != fresh.Cursor {
		t.Fatalf("changes since the first cursor: %+v", all)
	}
}

func agentIDsOf(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// A cursor from another hub process, one ahead of this hub, or one below the
// pruned tombstone floor gets the whole roster with reset; a malformed one 400.
func TestAgentRosterResetsAndRefuses(t *testing.T) {
	c := newClient(t)
	task := c.task("roster-reset")
	c.agent(task, "lead")
	c.agent(task, "builder")
	first := c.agents(task, "")
	epoch, n, ok := parseRosterCursor(first.Cursor)
	if !ok || epoch == "" || n != 1 {
		t.Fatalf("cursor %q", first.Cursor)
	}
	for _, cursor := range []string{"00ff." + fmt.Sprint(n), epoch + "." + fmt.Sprint(n+1)} {
		reset := c.agents(task, "?since="+cursor)
		if !reset.Reset || reset.Changes || len(reset.Agents) != 2 || reset.Cursor != first.Cursor {
			t.Fatalf("reset for %s: %+v", cursor, reset)
		}
	}
	for _, bad := range []string{"", "1", "zz.1", epoch, epoch + ".", epoch + ".-1", epoch + ".01", "." + fmt.Sprint(n), epoch + ".1.2"} {
		if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents?since="+bad, nil, nil); code != 400 {
			t.Fatalf("since=%q = %d, want 400", bad, code)
		}
	}
	if code := c.do("GET", "/v1/tasks/"+api.NewID("tsk")+"/agents?since="+first.Cursor, nil, nil); code != 404 {
		t.Fatalf("missing project = %d", code)
	}
}

// Removed agents reach a since read as ids; once more than the bounded number
// of tombstones is dropped, an older cursor resets.
func TestAgentRosterRemovedIDsAndPrunedFloor(t *testing.T) {
	tracker := newRosterTracker()
	ctx := context.Background()
	roster := []api.Agent{}
	for i := 0; i < rosterTombstones+3; i++ {
		roster = append(roster, api.Agent{ID: fmt.Sprintf("agt_%016x", i), Name: fmt.Sprint(i)})
	}
	list := func(context.Context, string) ([]api.Agent, error) { return append([]api.Agent{}, roster...), nil }
	first, err := tracker.read(ctx, "tsk_a", "", list)
	if err != nil || len(first.All) != len(roster) {
		t.Fatalf("first read: %v %d", err, len(first.All))
	}
	gone := roster[0].ID
	roster = roster[1:]
	removed, err := tracker.read(ctx, "tsk_a", first.Cursor, list)
	if err != nil || removed.Reset || len(removed.Changed) != 0 || !reflect.DeepEqual(removed.Removed, []string{gone}) {
		t.Fatalf("one removed: %+v %v", removed, err)
	}
	// A removed agent that comes back is a change again, not a removal.
	roster = append(roster, api.Agent{ID: gone, Name: "back"})
	back, _ := tracker.read(ctx, "tsk_a", removed.Cursor, list)
	if len(back.Removed) != 0 || len(back.Changed) != 1 || back.Changed[0].ID != gone {
		t.Fatalf("returned agent: %+v", back)
	}
	// Remove every agent, one read at a time, until tombstones are pruned.
	cursor := back.Cursor
	for len(roster) > 0 {
		roster = roster[1:]
		next, err := tracker.read(ctx, "tsk_a", cursor, list)
		if err != nil || next.Reset || len(next.Removed) != 1 {
			t.Fatalf("removal with %d left: reset=%v removed=%v %v", len(roster), next.Reset, next.Removed, err)
		}
		cursor = next.Cursor
	}
	r := tracker.roster("tsk_a")
	if len(r.removed) != rosterTombstones || r.floor == 0 {
		t.Fatalf("tombstones %d floor %d", len(r.removed), r.floor)
	}
	old, _ := tracker.read(ctx, "tsk_a", first.Cursor, list)
	if !old.Reset {
		t.Fatalf("a cursor below the pruned floor did not reset: %+v", old.Removed)
	}
	recent, _ := tracker.read(ctx, "tsk_a", cursor, list)
	if recent.Reset || len(recent.Removed) != 0 {
		t.Fatalf("the newest cursor: %+v", recent)
	}
	// Projects are versioned separately.
	other, _ := tracker.read(ctx, "tsk_b", "", func(context.Context, string) ([]api.Agent, error) { return nil, nil })
	if !strings.HasSuffix(other.Cursor, ".0") {
		t.Fatalf("an empty project's cursor: %s", other.Cursor)
	}
}

// Readers following their own cursors while agents change concurrently each
// converge on the whole roster.
func TestAgentRosterConcurrentReadersConverge(t *testing.T) {
	c := newClient(t)
	task := c.task("roster-race")
	agents := []api.Agent{c.agent(task, "lead")}
	var mu sync.Mutex
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; i < 12; i++ {
			a, err := c.st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: fmt.Sprintf("worker-%d", i), Host: "devbox", Session: fmt.Sprintf("w%d", i), Runtime: "claude"}, c.who)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			agents = append(agents, a)
			mu.Unlock()
			_, _ = c.st.ReportActivity(context.Background(), task.ID, a.ID, api.ActivityReport{RequestID: fmt.Sprintf("race-%d", i), RunID: a.RunID, Activity: api.AgentActivity{State: "working", ObservedAt: time.Now().UTC()}})
		}
	}()
	var readers sync.WaitGroup
	results := make([]map[string]api.Agent, 4)
	for r := range results {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			mine := map[string]api.Agent{}
			cursor := ""
			apply := func() bool {
				delta, err := c.s.rosters.read(context.Background(), task.ID, cursor, c.st.ListAgents)
				if err != nil {
					t.Error(err)
					return false
				}
				if cursor == "" || delta.Reset {
					mine = map[string]api.Agent{}
					for _, a := range delta.All {
						mine[a.ID] = a
					}
				} else {
					for _, a := range delta.Changed {
						mine[a.ID] = a
					}
					for _, id := range delta.Removed {
						delete(mine, id)
					}
				}
				cursor = delta.Cursor
				return true
			}
			for {
				select {
				case <-stop:
					apply()
					results[r] = mine
					return
				default:
					if !apply() {
						return
					}
				}
			}
		}(r)
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	final, err := c.st.ListAgents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for r, mine := range results {
		if len(mine) != len(final) {
			t.Fatalf("reader %d has %d agents, want %d", r, len(mine), len(final))
		}
		for _, want := range final {
			got, ok := mine[want.ID]
			if !ok {
				t.Fatalf("reader %d misses %s", r, want.ID)
			}
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatalf("reader %d has a stale %s:\n%s\n%s", r, want.ID, a, b)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(agents) != len(final) {
		t.Fatalf("writer added %d, roster %d", len(agents), len(final))
	}
}
