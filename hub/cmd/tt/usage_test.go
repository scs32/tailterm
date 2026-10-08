package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestUsageCLIRealHTTPFiltersJSONAndPrices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/v1/tasks/tsk_1111111111111111/usage":
			if r.URL.Query().Get("item") != "wi_1111111111111111" || r.URL.Query().Get("from") != "2026-09-27T00:00:00Z" || r.URL.Query().Get("to") != "2026-09-28T00:00:00Z" {
				t.Error(r.URL.String())
			}
			json.NewEncoder(w).Encode(api.UsageReport{Version: 1, ProjectID: "tsk_1111111111111111", Items: []api.UsageItemReport{}})
		case "/v1/tasks/tsk_1111111111111111/usage/prices":
			json.NewEncoder(w).Encode(api.UsagePrices{Rows: []api.UsagePrice{}})
		default:
			t.Error("unexpected endpoint", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_1111111111111111"}
	if err := cmdUsage(e, []string{"--item", "wi_1111111111111111", "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUsage(e, []string{"prices", "get"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUsage(e, []string{"--from", "2026-09-28T00:00:00Z", "--to", "2026-09-27T00:00:00Z"}); err == nil {
		t.Fatal("bad range contacted hub")
	}
	if requests != 2 {
		t.Fatal(requests)
	}
}

func TestUsageTextReadable(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{formatTokenCount("0"), "0"},
		{formatTokenCount("950"), "950"},
		{formatTokenCount("11/2"), "5.5"},
		{formatTokenCount("12100"), "12.1k"},
		{formatTokenCount("174400"), "174.4k"},
		{formatTokenCount("999960"), "1.00M"},
		{formatTokenCount("6710000"), "6.71M"},
		{formatTokenCount("61500000"), "61.50M"},
		{formatTokenCount("1230000000"), "1.23B"},
		{formatTokenCount("4207361/31"), "135.7k"},
		{formatTokenCount("not a number"), "unavailable"},
		{formatRatDecimal("11/2", 1), "5.5"},
		{formatRatDecimal("52", 1), "52"},
		{formatRatDecimal("1/3", 1), "0.3"},
		{formatPercent("39/40"), "97.5%"},
		{formatPercent("1"), "100.0%"},
		{formatPercent("0"), "0.0%"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	average, share := "4207361/31", "39/40"
	build := api.UsageSummary{State: "measured", Requests: 52, AllocatedTurns: "103/2",
		Tokens:         map[string]string{"input": "174400", "cached": "6710000", "cacheWrite": "0", "output": "12100"},
		AverageContext: &average, CachedShare: &share, PricedSubtotal: map[string]string{"USD": "1234/100", "EUR": "7/2"}}
	handoffs := api.UsageSummary{State: "measured", Requests: 3, AllocatedTurns: "3", Tokens: map[string]string{"input": "2500", "output": "400"}}
	// The item has a builder hand-offs phase row and measured time, so the
	// text must carry readable tokens and the time lines together.
	spent := &api.UsageTime{WallMs: "600000", ModelMs: "300000", ToolMs: "150000", WaitingMs: "150000", UnmeasuredMs: "0", PollMs: "0",
		Timeline: api.UsageTimeline{ModelMs: "300000", ToolsOnlyMs: "150000", IdleMs: "150000", UnmeasuredMs: "0"},
		Phases:   []api.UsageTimeSplit{{Key: "build", ModelMs: "240000", ToolMs: "150000", WaitingMs: "0"}, {Key: "hand-offs", ModelMs: "60000", ToolMs: "0", WaitingMs: "150000"}}}
	report := api.UsageReport{Version: 1, TimeVersion: 1, ProjectID: "tsk_1111111111111111", PriceRevision: 3, Items: []api.UsageItemReport{{TaskID: "tsk_1111111111111111", ItemID: "wi_1111111111111111", Title: "Synthetic item", Summary: build, Time: spent,
		Phases: []api.UsageGroup{{Key: "build", Label: "build", Summary: build}, {Key: "hand-offs", Label: "hand-offs", Summary: handoffs}}, Roles: []api.UsageGroup{{Key: "builder", Label: "builder", Summary: api.UsageSummary{State: "unavailable", AllocatedTurns: "0", Tokens: map[string]string{}}}}}},
		Overhead: api.UsageItemReport{Title: "Project overhead", Summary: api.UsageSummary{State: "measured", Requests: 1, AllocatedTurns: "1", Tokens: map[string]string{"input": "950"}, CostComplete: true, PricedSubtotal: map[string]string{"USD": "1/8"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(report) }))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	err = cmdUsage(env{hub: srv.URL, task: "tsk_1111111111111111"}, nil)
	os.Stdout = original
	write.Close()
	raw, _ := io.ReadAll(read)
	text := string(raw)
	if err != nil {
		t.Fatal(err, text)
	}
	for _, want := range []string{
		"Synthetic item: measured · requests 52 · allocated 51.5 · tokens 6.90M (input 174.4k, cached 6.71M, cache write 0, output 12.1k, reasoning unavailable) · average context 135.7k tokens · cached-input share 97.5% · estimated subtotal EUR 3.50, USD 12.34 (partial)\n",
		"  build: measured · requests 52 · ",
		"  role builder: unavailable · requests 0 · allocated 0 · tokens unavailable (input unavailable, cached unavailable, cache write unavailable, output unavailable, reasoning unavailable) · average context unavailable\n",
		"Project overhead: measured · requests 1 · allocated 1 · tokens 950 (input 950, cached unavailable, cache write unavailable, output unavailable, reasoning unavailable) · average context unavailable · estimated subtotal USD 0.13\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
	// Tokens and time together, in the order tt usage prints them.
	at := 0
	for _, want := range []string{
		"Synthetic item: measured · requests 52 · allocated 51.5 · tokens 6.90M (",
		"  time: wall 10m0s · model 50.0% · tool 25.0% · waiting 25.0% · polls 0 · ",
		"  timeline: some model working 50.0% · only tools running 25.0% · nobody active 25.0%\n",
		"  time phase build: model 4m0s · tool 2m30s · waiting 0s\n",
		"  time phase hand-offs: model 1m0s · tool 0s · waiting 2m30s\n",
		"  build: measured · requests 52 · allocated 51.5 · tokens 6.90M (",
		"  hand-offs: measured · requests 3 · allocated 3 · tokens 2.9k (input 2.5k, cached unavailable, cache write unavailable, output 400, reasoning unavailable) · average context unavailable\n",
		"Project overhead: measured · requests 1 · ",
		"  time: not measured\n",
	} {
		i := strings.Index(text[at:], want)
		if i < 0 {
			t.Fatalf("text output lacks %q after byte %d:\n%s", want, at, text)
		}
		at += i + len(want)
	}
	if strings.Contains(text, "map[") || regexp.MustCompile(`[0-9]/[0-9]`).MatchString(text) {
		t.Errorf("text output still prints a raw map or rational:\n%s", text)
	}
}

// tt usage warning against a real hub over HTTP (wi_3228104e700006c5, a9).
func TestUsageWarningCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	srv := httptest.NewServer(server.New(st, func(r *http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Usage warning CLI synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: srv.URL, task: task.ID}
	read := func(args ...string) api.UsageWarnings {
		t.Helper()
		text, err := captureStdout(t, func() error { return cmdUsage(e, append([]string{"warning"}, args...)) })
		if err != nil {
			t.Fatal(args, err)
		}
		var out api.UsageWarnings
		if err = json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(text, err)
		}
		return out
	}
	if out := read("get", "--json"); out.Threshold != "1.5" || !out.Default {
		t.Fatalf("default %+v", out)
	}
	text, err := captureStdout(t, func() error { return cmdUsage(e, []string{"warning", "set", "--threshold", "2"}) })
	if err != nil || text != "Warning level: 2× estimate\nNo warnings posted.\n" {
		t.Fatalf("set %q %v", text, err)
	}
	if out := read("get", "--json"); out.Threshold != "2" || out.Default || len(out.Warnings) != 0 {
		t.Fatalf("after set %+v", out)
	}
	if out := read("get", "--project", task.ID, "--json"); out.Threshold != "2" {
		t.Fatalf("explicit project %+v", out)
	}
	for _, bad := range [][]string{{"warning"}, {"warning", "show"}, {"warning", "set"}, {"warning", "get", "--threshold", "3"}, {"warning", "set", "--threshold", "abc"}, {"warning", "set", "--threshold", "0.5"}, {"warning", "get", "extra"}} {
		if _, err = captureStdout(t, func() error { return cmdUsage(e, bad) }); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	// An agent identity is named in the request, and only the owner helper's is accepted.
	worker, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "synthetic", Runtime: "claude"}, by)
	if err != nil {
		t.Fatal(err)
	}
	asWorker := e
	asWorker.agent = worker.ID
	if _, err = captureStdout(t, func() error { return cmdUsage(asWorker, []string{"warning", "set", "--threshold", "3"}) }); err == nil || !strings.Contains(err.Error(), "only the owner helper sets the warning threshold") {
		t.Fatalf("a worker set the threshold: %v", err)
	}
	if out := read("get", "--json"); out.Threshold != "2" {
		t.Fatalf("a refused set changed the threshold: %+v", out)
	}
	at := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	got := formatUsageWarnings(api.UsageWarnings{Threshold: "1.5", Default: true, Warnings: []api.UsageWarning{
		{ItemID: "wi_1111111111111111", EstimateTokens: 12000000, ActualTokens: "18360000", ActualState: "measured", Ratio: "153/100", Threshold: "1.5", LeadMessageSeq: 12, HelperMessageSeq: 13, At: at},
		{ItemID: "wi_2222222222222222", EstimateTokens: 1000, ActualTokens: "3001/2", ActualState: "partial", Ratio: "3001/2000", Threshold: "1.5", HelperMessageSeq: 14, At: at},
		// A warning with a queue entry compared that entry's team figure.
		{ItemID: "wi_1111111111111111", EntryID: "tqe_3333333333333333", EstimateTokens: 12000000, ActualTokens: "18360000", ActualState: "measured", Ratio: "153/100", LifetimeTokens: "41200000", LifetimeState: "measured", Threshold: "1.5", LeadMessageSeq: 15, HelperMessageSeq: 16, At: at},
		{ItemID: "wi_2222222222222222", EntryID: "tqe_4444444444444444", EstimateTokens: 1000, ActualTokens: "3001/2", ActualState: "partial", Ratio: "3001/2000", LifetimeTokens: "9001/2", LifetimeState: "partial", Threshold: "1.5", HelperMessageSeq: 17, At: at},
		{ItemID: "wi_2222222222222222", EntryID: "tqe_5555555555555555", EstimateTokens: 2000, ActualTokens: "3001", ActualState: "measured", Ratio: "3001/2000", LifetimeTokens: "9001/2", LifetimeState: "partial", Threshold: "1.5", HelperMessageSeq: 18, At: at}}})
	want := "Warning level: 1.5× estimate (default)\n" +
		"  wi_1111111111111111: estimate 12.00M · lifetime actual 18.36M · 1.53× · level 1.5× · lead #12 · owner helper #13 · 2026-10-08T04:00:00Z\n" +
		"  wi_2222222222222222: estimate 1.0k · lifetime actual at least 1.5k · at least 1.50× · level 1.5× · no lead · owner helper #14 · 2026-10-08T04:00:00Z\n" +
		"  wi_1111111111111111: entry tqe_3333333333333333 · estimate 12.00M · team actual 18.36M · 1.53× · lifetime 41.20M · level 1.5× · lead #15 · owner helper #16 · 2026-10-08T04:00:00Z\n" +
		"  wi_2222222222222222: entry tqe_4444444444444444 · estimate 1.0k · team actual at least 1.5k · at least 1.50× · lifetime at least 4.5k · level 1.5× · no lead · owner helper #17 · 2026-10-08T04:00:00Z\n" +
		"  wi_2222222222222222: entry tqe_5555555555555555 · estimate 2.0k · team actual 3.0k · 1.50× · lifetime at least 4.5k · level 1.5× · no lead · owner helper #18 · 2026-10-08T04:00:00Z\n"
	if got != want {
		t.Fatalf("text\n%s\nwant\n%s", got, want)
	}
}

// The budget line shows the running entry's team figure after the unchanged
// lifetime parts, and nothing more without a measured team.
func TestUsageTeamBudgetText(t *testing.T) {
	lifetime := api.TokenBudget{ActualTokens: "41200000", ActualState: "measured", Ratio: "103/30", Estimate: &api.WorkItemEstimate{Tokens: 12000000}}
	const plain = "estimate 12.00M · lifetime actual 41.20M · 3.43×"
	team := func(t *api.TeamTokenBudget) *api.TokenBudget {
		b := lifetime
		b.Team = t
		return &b
	}
	for want, b := range map[string]*api.TokenBudget{
		plain:                                team(nil),
		plain + " ":                          team(&api.TeamTokenBudget{EntryID: "tqe_3333333333333333", ActualTokens: "0", ActualState: "not measured"}),
		plain + " · this team 3.10M · 0.26×": team(&api.TeamTokenBudget{EntryID: "tqe_3333333333333333", ActualTokens: "3100000", ActualState: "measured", Ratio: "31/120"}),
		plain + " · this team at least 3.10M · at least 0.26×":  team(&api.TeamTokenBudget{EntryID: "tqe_3333333333333333", ActualTokens: "3100000", ActualState: "partial", Ratio: "31/120"}),
		"no estimate · lifetime actual 4.20M · this team 1.20M": {ActualTokens: "4200000", ActualState: "measured", Team: &api.TeamTokenBudget{EntryID: "tqe_3333333333333333", ActualTokens: "1200000", ActualState: "measured"}},
	} {
		if got := formatTokenBudget(b); got != strings.TrimSuffix(want, " ") {
			t.Fatalf("budget text %q, want %q", got, strings.TrimSuffix(want, " "))
		}
	}
}

// tt usage warning get and tt usage --item against a real hub: a warning
// recorded for a queue entry names the entry and labels the team and lifetime
// figures apart, beside a warning recorded before entries were compared
// (wi_12d5c1ef9a73ceae).
func TestUsageEntryWarningCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	srv := httptest.NewServer(server.New(st, func(r *http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Entry warning CLI synthetic", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Synthetic item", RequestID: "create-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	tokens, basis := int64(1000000), "synthetic"
	if _, _, err = st.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, RequestID: "estimate", EstimateTokens: &tokens, EstimateBasis: &basis}, by); err != nil {
		t.Fatal(err)
	}
	// agent registers an agent and returns it with an order for the item.
	agent := func(name, role string) (api.Agent, api.Message) {
		t.Helper()
		a, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Runtime: "codex", Role: role, Host: "fixture", Session: name}, by)
		if err != nil {
			t.Fatal(err)
		}
		m, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: "order-" + name, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a, m
	}
	uploads := 0
	use := func(a api.Agent, order api.Message, tokens int64) {
		t.Helper()
		uploads++
		at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
		turn := api.UsageTurn{ID: fmt.Sprintf("turn-%d", uploads), Revision: 1, Runtime: "codex", Session: "synthetic-session", Model: "synthetic-model", At: at, SourceDigest: strings.Repeat("a", 64), Activation: "activation-one", Complete: true,
			Tokens:  map[string]int64{"input": tokens, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0},
			Raw:     map[string]int64{"input_tokens": tokens, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0},
			Handled: []api.UsageEvidence{{TaskID: task.ID, Seq: order.Seq, Operation: "ack", At: at}}}
		batch := api.UsageBatch{Version: 1, RequestID: fmt.Sprintf("batch-%d", uploads), RunID: a.RunID, Session: "synthetic-session", StartedAt: at.Add(-time.Hour), Coverage: "synthetic complete", Turns: []api.UsageTurn{turn}}
		if receipt, err := st.ReportUsage(ctx, task.ID, a.ID, batch); err != nil || receipt.Turns != 1 {
			t.Fatalf("usage upload %+v %v", receipt, err)
		}
	}
	handler, handlerOrder := agent("handler", api.AgentRoleDatabaseHandler)
	member, memberOrder := agent("member", "")
	lead, _ := agent("lead", "")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(query, err)
		}
	}
	admitted := time.Now().UTC().Truncate(time.Second)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
	entry := api.NewID("tqe")
	exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,created_at,updated_at,repository,base_commit) VALUES(?,?,?,1,1,'planned',1,'running',1,?,?,'fixture',?)`,
		entry, task.ID, item.ID, stamp(admitted), stamp(admitted), strings.Repeat("c", 40))
	for _, a := range []api.Agent{member, lead} {
		exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,'member',?,'{}',?)`,
			a.ID, a.RunID, task.ID, item.ID, task.ID, memberOrder.Seq, strings.Repeat("d", 64), stamp(admitted.Add(500*time.Millisecond)))
	}
	exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, task.ID, item.ID, lead.ID, lead.RunID)
	// A warning recorded while the lifetime figure was the one compared.
	exec(`INSERT INTO usage_budget_warnings(task_id,item_id,estimate_tokens,threshold,actual_tokens,actual_state,lead_agent,lead_message_seq,helper_agent,helper_message_seq,created_at) VALUES(?,?,500000,'1.5','900000','measured','',0,'',0,?)`,
		task.ID, item.ID, stamp(admitted.Add(-time.Hour)))

	e := env{hub: srv.URL, task: task.ID}
	run := func(args ...string) string {
		t.Helper()
		text, err := captureStdout(t, func() error { return cmdUsage(e, args) })
		if err != nil {
			t.Fatal(args, err)
		}
		return text
	}
	old := "  " + item.ID + ": estimate 500.0k · lifetime actual 900.0k · 1.80× · level 1.5× · no lead · no owner helper · " + admitted.Add(-time.Hour).Format(time.RFC3339) + "\n"
	use(handler, handlerOrder, 10000000)
	use(member, memberOrder, 1500000)
	if text := run("warning", "get"); text != "Warning level: 1.5× estimate (default)\n"+old {
		t.Fatalf("a team at the level, with 10M earlier tokens on the item:\n%s", text)
	}
	if text := run("--item", item.ID); !strings.Contains(text, "  budget: estimate 1.00M · lifetime actual 11.50M · 11.50× · this team 1.50M · 1.50×\n") {
		t.Fatalf("budget line with a running entry:\n%s", text)
	}
	use(member, memberOrder, 1)
	text := run("warning", "get")
	row := regexp.MustCompile(`^  ` + item.ID + `: entry ` + entry + ` · estimate 1\.00M · team actual 1\.50M · 1\.50× · lifetime 11\.50M · level 1\.5× · lead #\d+ · no owner helper · \S+\n$`)
	if rest, ok := strings.CutPrefix(text, "Warning level: 1.5× estimate (default)\n"+old); !ok || !row.MatchString(rest) {
		t.Fatalf("warning list:\n%s", text)
	}
	var list struct {
		Warnings []map[string]any `json:"warnings"`
	}
	if err = json.Unmarshal([]byte(run("warning", "get", "--json")), &list); err != nil || len(list.Warnings) != 2 {
		t.Fatalf("warning JSON %+v %v", list, err)
	}
	was, now := list.Warnings[0], list.Warnings[1]
	for _, key := range []string{"entryId", "lifetimeTokens", "lifetimeState"} {
		if _, ok := was[key]; ok {
			t.Fatalf("the earlier warning has %s: %v", key, was)
		}
	}
	if was["actualTokens"] != "900000" || now["entryId"] != entry || now["actualTokens"] != "1500001" || now["actualState"] != "measured" || now["lifetimeTokens"] != "11500001" || now["lifetimeState"] != "measured" || now["ratio"] != "1500001/1000000" {
		t.Fatalf("warning JSON %v\n%v", was, now)
	}
	var report struct {
		Items []struct {
			Budget map[string]any `json:"budget"`
		} `json:"items"`
	}
	if err = json.Unmarshal([]byte(run("--item", item.ID, "--json")), &report); err != nil || len(report.Items) != 1 {
		t.Fatalf("usage JSON %+v %v", report, err)
	}
	budget := report.Items[0].Budget
	if team, _ := budget["team"].(map[string]any); budget["actualTokens"] != "11500001" || team["entryId"] != entry || team["actualTokens"] != "1500001" || team["actualState"] != "measured" || team["ratio"] != "1500001/1000000" {
		t.Fatalf("budget JSON %v", budget)
	}
	// Without a running entry the line is the lifetime one alone.
	exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, stamp(admitted.Add(time.Second)), entry)
	if text = run("--item", item.ID); !strings.Contains(text, "  budget: estimate 1.00M · lifetime actual 11.50M · 11.50×\n") || strings.Contains(text, "this team") {
		t.Fatalf("budget line without a running entry:\n%s", text)
	}
	report.Items = nil // a fresh decode: an existing map would keep its keys
	if err = json.Unmarshal([]byte(run("--item", item.ID, "--json")), &report); err != nil || len(report.Items) != 1 {
		t.Fatalf("usage JSON %+v %v", report, err)
	}
	if _, ok := report.Items[0].Budget["team"]; ok || report.Items[0].Budget["actualTokens"] != "11500001" {
		t.Fatalf("budget JSON without a running entry %v", report.Items[0].Budget)
	}
}

// budgetCLIFixture is a real hub over HTTP with one queued entry for the team
// fixture's item. held drives the entry to running and its one bound member
// past 3 times a saved estimate. No agent session is started.
type budgetCLIFixture struct {
	teamFixture
	t     *testing.T
	entry api.TeamQueueEntry
}

func newBudgetCLIFixture(t *testing.T) *budgetCLIFixture {
	t.Helper()
	f := &budgetCLIFixture{teamFixture: newTeamFixture(t, true), t: t}
	old := usageBudgetHost
	usageBudgetHost = func() string { return "fixture" }
	t.Cleanup(func() { usageBudgetHost = old })
	var err error
	if f.entry, err = f.c.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: "budget-cli-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *budgetCLIFixture) estimate(tokens int64) {
	f.t.Helper()
	basis := "synthetic"
	if _, _, err := f.st.CreateWorkItemUpdate(context.Background(), f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: f.item.Revision, RequestID: fmt.Sprintf("budget-cli-estimate-%d", tokens), EstimateTokens: &tokens, EstimateBasis: &basis}, api.Caller{Node: "team-fixture", User: "owner"}); err != nil {
		f.t.Fatal(err)
	}
}

// held runs the entry and has a bound member report 3001 tokens against a
// saved estimate of 1000. It returns the member.
func (f *budgetCLIFixture) held() api.Agent {
	f.t.Helper()
	ctx := context.Background()
	f.estimate(1000)
	f.entry = runQueueEntry(f.t, f.teamFixture, f.entry.ID)
	member, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "member-a", Host: "fixture", Session: "member-a", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order}, ContextBundle: teamCloseCLIContext(f.t, f.item, api.Message{TaskID: f.task.ID, Seq: f.order, Text: "bounded fixture order"})}})
	if err != nil {
		f.t.Fatal(err)
	}
	at := time.Now().UTC()
	turn := api.UsageTurn{ID: "budget-cli-turn", Revision: 1, Runtime: "codex", Session: "synthetic-session", Model: "synthetic-model", At: at, SourceDigest: strings.Repeat("a", 64), Activation: "activation-one", Complete: true,
		Tokens:  map[string]int64{"input": 3001, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0},
		Raw:     map[string]int64{"input_tokens": 3001, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0},
		Handled: []api.UsageEvidence{{TaskID: f.task.ID, Seq: f.order, Operation: "ack", At: at}}}
	if _, err := f.c.ReportUsage(ctx, f.task.ID, member.ID, api.UsageBatch{Version: 1, RequestID: "budget-cli-batch", RunID: member.RunID, Session: "synthetic-session", StartedAt: at.Add(-time.Hour), Coverage: "synthetic complete", Turns: []api.UsageTurn{turn}}); err != nil {
		f.t.Fatal(err)
	}
	holds, err := f.c.UsageHolds(ctx, f.task.ID, member.ID, member.RunID)
	if err != nil || !holds.Held {
		f.t.Fatalf("fixture: the member is not held: %+v %v", holds, err)
	}
	return member
}

func (f *budgetCLIFixture) usage(args ...string) (string, error) {
	f.t.Helper()
	return captureStdout(f.t, func() error { return cmdUsage(f.e, args) })
}

// a1, a5: tt usage budget set, get and clear round-trip a row over a real
// hub; bad values and unauthorised agents are refused with a named reason;
// with no row, get prints "not configured".
func TestUsageBudgetCLI(t *testing.T) {
	f := newBudgetCLIFixture(t)
	text, err := f.usage("budget", "get")
	if err != nil || text != "Token budget: not configured\n" {
		t.Fatalf("get with no row: %q %v", text, err)
	}
	reset := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	text, err = f.usage("budget", "set", "--runtime", "claude", "--window", "five_hour", "--allowance", "200000000", "--reserve", "10", "--reset-at", reset, "--stale", "10m")
	want := "claude five_hour: allowance 200.00M · reserve 10% · reading stale after 10m0s · owner reset " + reset + " · source allowance minus reported usage · remaining 200.00M · resets "
	if err != nil || !strings.HasPrefix(text, want) || !strings.HasSuffix(text, " · provider reading for host fixture is not reported\n") {
		t.Fatalf("set: %q %v", text, err)
	}
	text, err = f.usage("budget", "get", "--json")
	var out api.UsageBudgets
	if err != nil || json.Unmarshal([]byte(text), &out) != nil || !out.Configured || len(out.Budgets) != 1 {
		t.Fatalf("get --json: %q %v", text, err)
	}
	row := out.Budgets[0]
	if row.Runtime != "claude" || row.Window != api.UsageWindowFiveHour || row.AllowanceTokens != 200_000_000 || row.ReservePercent != 10 || row.ResetAt != reset || row.StaleSeconds != 600 || row.Status == nil || row.Status.Host != "fixture" {
		t.Fatalf("round trip %+v", row)
	}
	// A provider reading for the named host becomes the source.
	now := time.Now().UTC()
	if _, err := f.c.ReportProviderUsage(context.Background(), api.ProviderUsageReport{Host: "air", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: now.Format(time.RFC3339Nano),
		Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 13, ResetsAt: now.Add(2 * time.Hour).Format(time.RFC3339)}}}); err != nil {
		t.Fatal(err)
	}
	if text, err = f.usage("budget", "get", "--host", "air"); err != nil || !strings.Contains(text, " · source provider reading for host air · remaining 174.00M · resets ") {
		t.Fatalf("get --host: %q %v", text, err)
	}
	// With no reset and no reading, the row says admission is held.
	if text, err = f.usage("budget", "set", "--runtime", "codex", "--window", "seven_day", "--allowance", "5"); err != nil || !strings.Contains(text, "codex seven_day: allowance 5 · reserve 0% · reading stale after 15m0s · no usable source: provider reading for host fixture is not reported and no reset time is set; admission is held\n") {
		t.Fatalf("set with no source: %q %v", text, err)
	}
	for name, bad := range map[string][]string{
		"no subcommand":     {"budget"},
		"unknown":           {"budget", "show"},
		"missing allowance": {"budget", "set", "--runtime", "claude", "--window", "five_hour"},
		"zero allowance":    {"budget", "set", "--runtime", "claude", "--window", "five_hour", "--allowance", "0"},
		"unknown window":    {"budget", "set", "--runtime", "claude", "--window", "daily", "--allowance", "5"},
		"unknown runtime":   {"budget", "set", "--runtime", "gemini", "--window", "five_hour", "--allowance", "5"},
		"reserve too high":  {"budget", "set", "--runtime", "claude", "--window", "five_hour", "--allowance", "5", "--reserve", "95"},
		"values on get":     {"budget", "get", "--allowance", "5"},
		"clear with value":  {"budget", "clear", "--runtime", "claude", "--window", "five_hour", "--allowance", "5"},
		"clear without row": {"budget", "clear", "--runtime", "claude"},
		"extra argument":    {"budget", "get", "extra"},
	} {
		if _, err := f.usage(bad...); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := f.usage("budget", "set", "--runtime", "claude", "--window", "daily", "--allowance", "5"); err == nil || !strings.Contains(err.Error(), "budget window must be five_hour or seven_day") {
		t.Fatalf("unknown window reason: %v", err)
	}
	worker, err := f.c.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	asWorker, asHandler := f.e, f.e
	asWorker.agent, asHandler.agent = worker.ID, f.handler.ID
	for _, args := range [][]string{{"budget", "set", "--runtime", "claude", "--window", "five_hour", "--allowance", "5"}, {"budget", "clear", "--runtime", "claude", "--window", "five_hour"}} {
		if _, err := captureStdout(t, func() error { return cmdUsage(asWorker, args) }); err == nil || !strings.Contains(err.Error(), "only the owner, the owner helper or a database handler sets the token budget") {
			t.Fatalf("a worker ran %v: %v", args, err)
		}
	}
	if _, err := captureStdout(t, func() error {
		return cmdUsage(asHandler, []string{"budget", "set", "--runtime", "claude", "--window", "five_hour", "--allowance", "300000000"})
	}); err != nil {
		t.Fatalf("a database handler set the budget: %v", err)
	}
	if text, err = f.usage("budget", "clear", "--runtime", "codex", "--window", "seven_day"); err != nil || strings.Contains(text, "codex") || !strings.Contains(text, "claude five_hour: allowance 300.00M") {
		t.Fatalf("clear one row: %q %v", text, err)
	}
	if text, err = f.usage("budget", "clear", "--runtime", "claude", "--window", "five_hour"); err != nil || text != "Token budget: not configured\n" {
		t.Fatalf("clear the last row: %q %v", text, err)
	}
}

// a8: tt usage defaults set then get round-trips the four figures.
func TestUsageDefaultsCLI(t *testing.T) {
	f := newBudgetCLIFixture(t)
	text, err := f.usage("defaults", "get")
	if err != nil || text != "Lane default estimates: not set (tt usage defaults set)\n" {
		t.Fatalf("get with none: %q %v", text, err)
	}
	text, err = f.usage("defaults", "set", "--small", "18000000", "--small-race", "17000000", "--planned", "44000000", "--planned-race", "70000000")
	if err != nil || !strings.HasPrefix(text, "Lane default estimates: small 18.00M · small with Go race 17.00M · planned 44.00M · planned with Go race 70.00M · set ") {
		t.Fatalf("set: %q %v", text, err)
	}
	text, err = f.usage("defaults", "get", "--json")
	var out api.UsageEstimateDefaults
	if err != nil || json.Unmarshal([]byte(text), &out) != nil || !out.Configured || out.SmallTokens != 18_000_000 || out.SmallRaceTokens != 17_000_000 || out.PlannedTokens != 44_000_000 || out.PlannedRaceTokens != 70_000_000 {
		t.Fatalf("get --json: %q %v", text, err)
	}
	for name, bad := range map[string][]string{
		"no subcommand": {"defaults"},
		"three figures": {"defaults", "set", "--small", "1", "--small-race", "1", "--planned", "1"},
		"values on get": {"defaults", "get", "--small", "1"},
		"extra":         {"defaults", "get", "extra"},
	} {
		if _, err := f.usage(bad...); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	worker, err := f.c.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	asWorker := f.e
	asWorker.agent = worker.ID
	if _, err := captureStdout(t, func() error {
		return cmdUsage(asWorker, []string{"defaults", "set", "--small", "1", "--small-race", "1", "--planned", "1", "--planned-race", "1"})
	}); err == nil || !strings.Contains(err.Error(), "the lane default estimates") {
		t.Fatalf("a worker set the defaults: %v", err)
	}
}

// a16: tt usage hold list shows a held team with both commands; continue is
// refused for an agent that is not the owner helper and lifts the hold for
// the owner.
func TestUsageHoldCLI(t *testing.T) {
	f := newBudgetCLIFixture(t)
	text, err := f.usage("hold", "list")
	if err != nil || text != "No token budget holds.\n" {
		t.Fatalf("list with none: %q %v", text, err)
	}
	member := f.held()
	text, err = f.usage("hold", "list")
	want := f.entry.ID + " " + f.item.ID + " held: past 3 times its token estimate since "
	if err != nil || !strings.HasPrefix(text, want) || !strings.Contains(text, "(team 3.0k, estimate 1.0k); no new turns start. Continue: tt usage hold continue --project "+f.task.ID+" --entry "+f.entry.ID+". Stop, from an unbound owner shell: tt team queue fail --task "+f.task.ID+" --entry "+f.entry.ID+" --reason TEXT\n") {
		t.Fatalf("list: %q %v", text, err)
	}
	for name, bad := range map[string][]string{"no subcommand": {"hold"}, "continue without entry": {"hold", "continue"}, "list with entry": {"hold", "list", "--entry", f.entry.ID}, "unknown entry": {"hold", "continue", "--entry", "tqe_0000000000000000"}} {
		if _, err := f.usage(bad...); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	asMember := f.e
	asMember.agent, asMember.runID = member.ID, member.RunID
	if _, err := captureStdout(t, func() error { return cmdUsage(asMember, []string{"hold", "continue", "--entry", f.entry.ID}) }); err == nil || !strings.Contains(err.Error(), "only the owner or the owner helper continues a held team") {
		t.Fatalf("a team member continued its own hold: %v", err)
	}
	if text, err = f.usage("hold", "continue", "--entry", f.entry.ID); err != nil || text != "Continued entry "+f.entry.ID+"; its team is woken again.\nNo token budget holds.\n" {
		t.Fatalf("continue: %q %v", text, err)
	}
	if holds, err := f.c.UsageHolds(context.Background(), f.task.ID, member.ID, member.RunID); err != nil || holds.Held {
		t.Fatalf("after continue: %+v %v", holds, err)
	}
	if _, err := f.usage("hold", "continue", "--entry", f.entry.ID); err == nil || !strings.Contains(err.Error(), "has no token budget hold in force") {
		t.Fatalf("continue with no hold: %v", err)
	}
}
