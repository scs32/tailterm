package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// budgetQueue is a parallel queue fixture with a fixed clock, for the token
// budget: n bug items with confirmed orders, two database handlers and no
// fixed cap. Nothing in it starts or pauses an agent.
type budgetQueue struct {
	*choresQueue
	t       *testing.T
	ctx     context.Context
	advance func(time.Duration)
	uploads int
}

func newBudgetQueue(t *testing.T, n int) *budgetQueue {
	t.Helper()
	f := &budgetQueue{choresQueue: newChoresQueue(t, n, 2, 0), t: t, ctx: context.Background()}
	f.advance = f.clock(t)
	return f
}

// estimate saves an estimate on item i as the owner.
func (f *budgetQueue) estimate(i int, tokens int64) {
	f.t.Helper()
	f.uploads++
	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.items[i].ID, estimateRequest(f.items[i].Revision, fmt.Sprintf("budget-estimate-%d", f.uploads), tokens, "synthetic", api.Agent{}), f.by); err != nil {
		f.t.Fatal(err)
	}
}

// budget saves a budget row whose window last reset at the given instant.
func (f *budgetQueue) budget(runtime, window string, allowance int64, reserve int, reset time.Time) {
	f.t.Helper()
	req := api.UsageBudgetRequest{Runtime: runtime, Window: window, AllowanceTokens: allowance, ReservePercent: reserve}
	if !reset.IsZero() {
		req.ResetAt = ts(reset)
	}
	if _, err := f.s.SetUsageBudget(f.ctx, f.task.ID, req, f.by); err != nil {
		f.t.Fatal(err)
	}
}

func (f *budgetQueue) defaults(small, smallRace, planned, plannedRace int64) {
	f.t.Helper()
	if _, err := f.s.SetUsageEstimateDefaults(f.ctx, f.task.ID, api.UsageEstimateDefaultsRequest{SmallTokens: small, SmallRaceTokens: smallRace, PlannedTokens: planned, PlannedRaceTokens: plannedRace}, f.by); err != nil {
		f.t.Fatal(err)
	}
}

// spend reports project usage of a runtime at the store's now, attributed to
// no item: it counts toward the allowance source only.
func (f *budgetQueue) spend(runtime string, tokens int64) {
	f.t.Helper()
	f.uploads++
	key := fmt.Sprintf("budget-spend-%d", f.uploads)
	batch := usageBatch(f.handlers[0], key, budgetTurn(key, runtime, f.s.now(), tokens))
	if _, err := f.s.ReportUsage(f.ctx, f.task.ID, f.handlers[0].ID, batch); err != nil {
		f.t.Fatal(err)
	}
}

// reading reports a fresh ok provider reading for a host.
func (f *budgetQueue) reading(host string, fiveHourUsed float64, resets time.Time) {
	f.t.Helper()
	if _, err := f.s.ReportProviderUsage(f.ctx, api.ProviderUsageReport{Host: host, Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: ts(f.s.now()),
		Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: fiveHourUsed, ResetsAt: ts(resets)}, {Window: api.UsageWindowSevenDay, UsedPercent: 1, ResetsAt: ts(resets.Add(100 * time.Hour))}}}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *budgetQueue) listed(id string) api.TeamQueueEntry {
	f.t.Helper()
	return listedEntry(f.t, f.s, f.task.ID, id)
}

// queueState is every queue row, reservation and saved request, to prove a
// refused claim wrote nothing.
func (f *budgetQueue) queueState() string {
	f.t.Helper()
	return tableDump(f.t, f.s, `SELECT id,state,revision,admitted_at,handler_id,updated_at FROM team_queue_entries WHERE task_id=? ORDER BY id`, f.task.ID) +
		tableDump(f.t, f.s, `SELECT entry_id,state FROM team_launch_reservations WHERE task_id=? ORDER BY entry_id`, f.task.ID) +
		tableDump(f.t, f.s, `SELECT count(*) FROM team_queue_requests WHERE task_id=?`, f.task.ID)
}

// refused asserts that a claim of the entry gets the quiet "not queue head"
// conflict and writes nothing.
func (f *budgetQueue) refused(q api.TeamQueueEntry, why string) {
	f.t.Helper()
	before := f.queueState()
	_, err := claimEntry(f.s, f.task, q)
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "not queue head") {
		f.t.Fatalf("%s: claim %v, want the not queue head conflict", why, err)
	}
	if after := f.queueState(); after != before {
		f.t.Fatalf("%s: the refused claim wrote\n%s\nwas\n%s", why, after, before)
	}
}

func queueBudgetHoldRows(t *testing.T, s *Store, task string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM team_queue_budget_holds WHERE task_id=?`, task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// a8, D5: the Go race rule follows the verification matrix Go group.
func TestQueueGoRaceFollowsMatrixRule(t *testing.T) {
	for name, c := range map[string]struct {
		owns []string
		want bool
	}{
		"go.mod under hub":      {[]string{"hub/go.mod"}, true},
		"a Go file under hub":   {[]string{"docs/project-queue.md", "hub/internal/store/usage.go"}, true},
		"a directory under hub": {[]string{"hub/internal/store"}, true},
		"the hub directory":     {[]string{"hub"}, true},
		"no ownership":          {nil, true},
		"docs only":             {[]string{"docs/project-queue.md", "docs/usage-accounting.md"}, false},
		"client only":           {[]string{"client/src/app.js"}, false},
		"a hub lookalike":       {[]string{"hubris/main.go", "docs/hub/notes.md"}, false},
	} {
		if got := queueGoRace(c.owns); got != c.want {
			t.Fatalf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// a2, a3: an entry whose estimate the remaining budget cannot cover stays
// queued with a stated reason and is admitted, with no other change, after
// the window resets and after the allowance is raised.
func TestQueueBudgetHoldsAndAdmits(t *testing.T) {
	f := newBudgetQueue(t, 2)
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	f.estimate(0, 750_000)
	f.estimate(1, 750_000)
	f.budget("codex", api.UsageWindowFiveHour, 1_000_000, 10, reset)
	a := f.add(t, 0, "src/a")
	f.spend("codex", 100_000)
	// 900K remain, 100K reserve: 800K covers 750K.
	if got := f.listed(a.ID); got.BlockReason != "" || got.Stall != nil {
		t.Fatalf("an entry that fits: %q %+v", got.BlockReason, got.Stall)
	}
	f.spend("codex", 100_000)
	// 800K remain, 100K reserve: 700K does not cover 750K.
	f.refused(a, "too little budget")
	want := fmt.Sprintf("Token budget (codex five_hour): needs about 750.00K tokens; 800.00K remain before the reset at %s, reserve 10%% (source: allowance minus reported usage; provider reading for host mini is not reported)", reset.Add(5*time.Hour).Format(time.RFC3339))
	got := f.listed(a.ID)
	if got.State != "queued" || got.BlockReason != want || got.Stall != nil || got.EstimateDefault != nil {
		t.Fatalf("held entry %s reason\n%q\nwant\n%q", got.State, got.BlockReason, want)
	}
	if one, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, a.ID); err != nil || one.BlockReason != want {
		t.Fatalf("entry read reason %q %v", one.BlockReason, err)
	}
	// a3: the reset passes, and nothing else changes.
	f.advance(4*time.Hour + time.Second)
	if got := f.listed(a.ID); got.BlockReason != "" {
		t.Fatalf("after the reset: %q", got.BlockReason)
	}
	claimed, err := claimEntry(f.s, f.task, a)
	if err != nil || claimed.State != "launching" || claimed.AdmittedAt != ts(f.s.now()) {
		t.Fatalf("claim after the reset: %+v %v", claimed, err)
	}
	// a3: a budget change. The second item is held in the new window, then
	// admitted when the allowance is raised.
	b := f.add(t, 1, "src/b")
	f.spend("codex", 200_000)
	f.refused(b, "too little budget in the new window")
	f.budget("codex", api.UsageWindowFiveHour, 2_000_000, 10, reset)
	if claimed, err = claimEntry(f.s, f.task, b); err != nil || claimed.State != "launching" {
		t.Fatalf("claim after the allowance was raised: %+v %v", claimed, err)
	}
}

// a4: a later entry that fits is claimed while an earlier one is held, and a
// running entry is never failed, held or changed by the budget check.
func TestQueueBudgetSkipsHeldHead(t *testing.T) {
	f := newBudgetQueue(t, 3)
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	f.estimate(0, 900_000)
	f.estimate(1, 100_000)
	f.estimate(2, 100_000)
	f.budget("codex", api.UsageWindowFiveHour, 1_000_000, 20, reset)
	big := f.add(t, 0, "src/a")
	small := f.add(t, 1, "src/b")
	f.refused(big, "the large head")
	running := f.run(t, small)
	if got := f.listed(big.ID); got.State != "queued" || !strings.HasPrefix(got.BlockReason, "Token budget (codex five_hour): needs about 900.00K tokens; ") {
		t.Fatalf("the passed head: %s %q", got.State, got.BlockReason)
	}
	// The budget now covers nothing; the running entry is untouched.
	row := func() string {
		return tableDump(t, f.s, `SELECT state,revision,failure,released_at,admitted_at,updated_at FROM team_queue_entries WHERE id=?`, running.ID)
	}
	before := row()
	f.spend("codex", 5_000_000)
	later := f.add(t, 2, "src/c")
	f.refused(later, "nothing remains")
	f.refused(big, "nothing remains")
	got := f.listed(running.ID)
	if row() != before || got.State != "running" || got.BlockReason != "" || got.BudgetHold != nil || got.Stall != nil || queueBudgetHoldRows(t, f.s, f.task.ID) != 0 {
		t.Fatalf("running entry changed: %s reason %q hold %+v", got.State, got.BlockReason, got.BudgetHold)
	}
	if reason := f.listed(later.ID).BlockReason; !strings.Contains(reason, "0 remain before the reset") {
		t.Fatalf("an overspent budget reads as nothing left: %q", reason)
	}
}

// a5: with no budget row the queue admits and lists as before; with a budget
// row and no usable source a queued entry is held with the neither-source
// reason.
func TestQueueBudgetNotConfiguredAndNoSource(t *testing.T) {
	f := newBudgetQueue(t, 2)
	f.estimate(0, 900_000_000)
	a := f.add(t, 0, "src/a")
	if got := f.listed(a.ID); got.BlockReason != "" || got.EstimateDefault != nil || got.BudgetHold != nil {
		t.Fatalf("no budget row: reason %q default %+v", got.BlockReason, got.EstimateDefault)
	}
	if reason, err := queueBudgetAdmission(f.ctx, f.s.db, f.task.ID, a, f.s.now()); err != nil || reason != "" {
		t.Fatalf("no budget row: %q %v", reason, err)
	}
	if claimed, err := claimEntry(f.s, f.task, a); err != nil || claimed.State != "launching" {
		t.Fatalf("claim with no budget row: %+v %v", claimed, err)
	}
	f.estimate(1, 1)
	b := f.add(t, 1, "src/b")
	f.budget("claude", api.UsageWindowFiveHour, 1_000_000, 10, time.Time{})
	f.refused(b, "a budget with no source")
	want := "Token budget (claude five_hour): no usable source. Provider reading for host mini is not reported and no reset time is set"
	if got := f.listed(b.ID); got.BlockReason != want {
		t.Fatalf("no source reason %q", got.BlockReason)
	}
	// An invalidated reading says which state it is in.
	if _, err := f.s.ReportProviderUsage(f.ctx, api.ProviderUsageReport{Host: "mini", Runtime: "claude", State: api.ProviderUsageMalformed}); err != nil {
		t.Fatal(err)
	}
	if got := f.listed(b.ID); !strings.Contains(got.BlockReason, "Provider reading for host mini is malformed and no reset time is set") {
		t.Fatalf("malformed reading reason %q", got.BlockReason)
	}
}

// a6: a fresh reading for the entry's host decides by percentage, and an
// entry must fit every budget row.
func TestQueueBudgetProviderReadingAndTwoRows(t *testing.T) {
	f := newBudgetQueue(t, 1)
	now := f.s.now().UTC().Truncate(time.Second)
	f.estimate(0, 500_000)
	f.budget("claude", api.UsageWindowFiveHour, 1_000_000, 10, time.Time{})
	a := f.add(t, 0, "src/a")
	// 45 percent used: 550K remain, 100K reserve, 450K does not cover 500K.
	f.reading("mini", 45, now.Add(2*time.Hour))
	f.refused(a, "45 percent used")
	want := fmt.Sprintf("Token budget (claude five_hour): needs about 500.00K tokens; 550.00K remain before the reset at %s, reserve 10%% (source: provider reading)", now.Add(2*time.Hour).Format(time.RFC3339))
	if got := f.listed(a.ID).BlockReason; got != want {
		t.Fatalf("provider reason\n%q\nwant\n%q", got, want)
	}
	// Another host's reading does not decide for an entry on mini.
	f.reading("air", 1, now.Add(2*time.Hour))
	f.refused(a, "another host's reading")
	// 40 percent used: 600K remain, 500K after the reserve: it fits.
	f.reading("mini", 40, now.Add(2*time.Hour))
	if got := f.listed(a.ID).BlockReason; got != "" {
		t.Fatalf("40 percent used: %q", got)
	}
	// A second row that does not cover it holds the entry although the first does.
	f.budget("codex", api.UsageWindowSevenDay, 400_000, 0, now.Add(-time.Hour))
	f.refused(a, "fits one row and not the other")
	if got := f.listed(a.ID).BlockReason; !strings.HasPrefix(got, "Token budget (codex seven_day): needs about 500.00K tokens; 400.00K remain") {
		t.Fatalf("second row reason %q", got)
	}
	f.budget("codex", api.UsageWindowSevenDay, 500_000, 0, now.Add(-time.Hour))
	if claimed, err := claimEntry(f.s, f.task, a); err != nil || claimed.State != "launching" {
		t.Fatalf("claim that fits both rows: %+v %v", claimed, err)
	}
}

// a8: an entry whose item has no saved estimate is checked against the lane
// default for its template and Go race and carries it in the list; an item
// with a saved estimate carries none; with a budget and no defaults row such
// an entry is held with the reason naming the command.
func TestQueueBudgetLaneDefaults(t *testing.T) {
	f := newBudgetQueue(t, 5)
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	plannedRace := f.add(t, 0, "hub/internal/store")
	planned := f.add(t, 1, "docs/project-queue.md")
	small, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: f.items[2].ID, OrderMessageSeq: f.orders[2].Seq, Template: "small", Host: "mini", Cwd: "/worktrees/small", Repository: "repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"docs/host-setup.md"}})
	if err != nil {
		t.Fatal(err)
	}
	smallRace, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: f.items[3].ID, OrderMessageSeq: f.orders[3].Seq, Template: "small", Host: "mini", Cwd: "/worktrees/small-race", Repository: "repo", BaseCommit: strings.Repeat("a", 40), Ownership: []string{"hub/go.mod"}})
	if err != nil {
		t.Fatal(err)
	}
	f.estimate(4, 5_000)
	saved := f.add(t, 4, "client/src")
	for _, q := range []api.TeamQueueEntry{plannedRace, planned, small, smallRace, saved} {
		if got := f.listed(q.ID); got.EstimateDefault != nil || got.BlockReason != "" {
			t.Fatalf("no defaults and no budget: %+v %q", got.EstimateDefault, got.BlockReason)
		}
	}
	// A budget with no defaults row holds the entries without an estimate.
	f.budget("codex", api.UsageWindowFiveHour, 100_000_000, 0, reset)
	f.refused(plannedRace, "no estimate and no lane default")
	if got := f.listed(planned.ID).BlockReason; got != "Token budget: no estimate and no lane default; set one with tt usage defaults set" {
		t.Fatalf("no default reason %q", got)
	}
	if got := f.listed(saved.ID); got.BlockReason != "" {
		t.Fatalf("a saved estimate needs no default: %q", got.BlockReason)
	}
	f.defaults(18_000_000, 17_000_000, 44_000_000, 70_000_000)
	for name, c := range map[string]struct {
		entry api.TeamQueueEntry
		want  api.TeamQueueEstimateDefault
	}{
		"planned with Go race":    {plannedRace, api.TeamQueueEstimateDefault{Tokens: 70_000_000, Lane: "planned", GoRace: true}},
		"planned without Go race": {planned, api.TeamQueueEstimateDefault{Tokens: 44_000_000, Lane: "planned"}},
		"small without Go race":   {small, api.TeamQueueEstimateDefault{Tokens: 18_000_000, Lane: "small"}},
		"small with Go race":      {smallRace, api.TeamQueueEstimateDefault{Tokens: 17_000_000, Lane: "small", GoRace: true}},
	} {
		got := f.listed(c.entry.ID)
		if got.EstimateDefault == nil || *got.EstimateDefault != c.want || got.BlockReason != "" {
			t.Fatalf("%s: default %+v reason %q, want %+v", name, got.EstimateDefault, got.BlockReason, c.want)
		}
		if one, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, c.entry.ID); err != nil || one.EstimateDefault == nil || *one.EstimateDefault != c.want {
			t.Fatalf("%s: entry read default %+v %v", name, one.EstimateDefault, err)
		}
	}
	if got := f.listed(saved.ID); got.EstimateDefault != nil {
		t.Fatalf("an item with a saved estimate carries a default: %+v", got.EstimateDefault)
	}
	// 100M allowance, 45M spent: 55M covers the planned 44M and not the 70M.
	f.spend("codex", 45_000_000)
	f.refused(plannedRace, "70M default against 55M")
	want := fmt.Sprintf("Token budget (codex five_hour): needs about 70.00M tokens (default, planned, Go race); 55.00M remain before the reset at %s, reserve 0%% (source: allowance minus reported usage; provider reading for host mini is not reported)", reset.Add(5*time.Hour).Format(time.RFC3339))
	if got := f.listed(plannedRace.ID).BlockReason; got != want {
		t.Fatalf("default reason\n%q\nwant\n%q", got, want)
	}
	if claimed, err := claimEntry(f.s, f.task, planned); err != nil || claimed.State != "launching" {
		t.Fatalf("the planned default fits and passes the held head: %+v %v", claimed, err)
	}
	// The steward recomputes the defaults: the 50M default would fit the 55M
	// alone, but the planned team just admitted has drawn none of its 44M.
	f.defaults(18_000_000, 17_000_000, 44_000_000, 50_000_000)
	f.refused(plannedRace, "50M default against 55M with 44M reserved")
	want = fmt.Sprintf("Token budget (codex five_hour): needs about 50.00M tokens (default, planned, Go race); 55.00M remain before the reset at %s, reserve 0%%, 44.00M reserved for 1 admitted team (source: allowance minus reported usage; provider reading for host mini is not reported)", reset.Add(5*time.Hour).Format(time.RFC3339))
	if got := f.listed(plannedRace.ID).BlockReason; got != want {
		t.Fatalf("reserved reason\n%q\nwant\n%q", got, want)
	}
	// A larger allowance covers both: the held entry is admitted.
	f.budget("codex", api.UsageWindowFiveHour, 200_000_000, 0, reset)
	if claimed, err := claimEntry(f.s, f.task, plannedRace); err != nil || claimed.State != "launching" {
		t.Fatalf("claim after the allowance was raised: %+v %v", claimed, err)
	}
	// A claimed entry keeps its label in the list.
	if got := f.listed(plannedRace.ID); got.EstimateDefault == nil || got.EstimateDefault.Tokens != 50_000_000 {
		t.Fatalf("launching entry default %+v", got.EstimateDefault)
	}
}

// a22, queued half: waiting for the budget is not a stall. A queued entry
// that would stall behind a failed entry shows its budget reason instead and
// its stall notice is refused; with the budget cleared the stall returns.
func TestQueueBudgetHeldEntryHasNoStall(t *testing.T) {
	f := newBudgetQueue(t, 2)
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	a := f.run(t, f.add(t, 0, "src"))
	f.estimate(1, 900_000)
	b := f.add(t, 1, "src/b")
	failed := f.fail(t, a)
	f.member(t, 0, "member-a")
	assertStall(t, f.choresQueue, b.ID, api.StallFailedEntry, failed.ID)
	stall := f.stall(t, b.ID)
	f.budget("codex", api.UsageWindowFiveHour, 1_000_000, 50, reset)
	got := f.listed(b.ID)
	if got.Stall != nil || !strings.HasPrefix(got.BlockReason, "Token budget (codex five_hour): needs about 900.00K tokens; 1.00M remain") {
		t.Fatalf("budget-held entry: stall %+v reason %q", got.Stall, got.BlockReason)
	}
	f.advance(time.Hour)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: stall.NoticeRequestID(b.ID), Operation: "stall_notice", EntryID: b.ID}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "the stall is stale or has cleared") {
		t.Fatalf("stall notice for a budget-held entry: %v", err)
	}
	if _, err := f.s.DeleteUsageBudget(f.ctx, f.task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour}, f.by); err != nil {
		t.Fatal(err)
	}
	assertStall(t, f.choresQueue, b.ID, api.StallFailedEntry, failed.ID)
}

// within puts the fixture's calls under a 30 second deadline, so a query
// blocked on the store's one connection fails by name.
func (f *budgetQueue) within() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f.t.Cleanup(cancel)
	f.ctx = ctx
}

// claim claims an entry under the fixture's context.
func (f *budgetQueue) claim(q api.TeamQueueEntry) (api.TeamQueueEntry, error) {
	return f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
}

// held asserts that a claim of the entry under the fixture's context gets the
// quiet "not queue head" conflict and writes nothing, and returns the reason
// the list gives for the entry.
func (f *budgetQueue) held(q api.TeamQueueEntry, why string) string {
	f.t.Helper()
	before := f.queueState()
	_, err := f.claim(q)
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "not queue head") {
		f.t.Fatalf("%s: claim %v, want the not queue head conflict", why, err)
	}
	if after := f.queueState(); after != before {
		f.t.Fatalf("%s: the refused claim wrote\n%s\nwas\n%s", why, after, before)
	}
	list, err := f.s.ListTeamQueue(f.ctx, f.task.ID)
	if err != nil {
		f.t.Fatalf("%s: list %v", why, err)
	}
	for _, e := range list.Entries {
		if e.ID == q.ID {
			return e.BlockReason
		}
	}
	f.t.Fatalf("%s: entry %s is not listed", why, q.ID)
	return ""
}

// teamSpend uploads one turn of a team member attributed to item i, of a
// runtime, at the store's now.
func (f *budgetQueue) teamSpend(a api.Agent, i int, runtime string, tokens int64) {
	f.t.Helper()
	f.uploads++
	key := fmt.Sprintf("budget-team-%d", f.uploads)
	turn := budgetTurn(key, runtime, f.s.now(), tokens)
	turn.Handled = []api.UsageEvidence{{TaskID: f.orders[i].TaskID, Seq: f.orders[i].Seq, Operation: "ack", At: turn.At}}
	if _, err := f.s.ReportUsage(f.ctx, f.task.ID, a.ID, usageBatch(a, key, turn)); err != nil {
		f.t.Fatal(err)
	}
}

// s3, allowance source: two entries that each fit alone and not together are
// not both admitted, and the reservation is the part of the admitted team's
// estimate it has not spent yet.
func TestQueueBudgetReservesAdmittedTeams(t *testing.T) {
	f := newBudgetQueue(t, 2)
	f.within()
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	f.estimate(0, 700_000)
	f.estimate(1, 400_000)
	f.budget("codex", api.UsageWindowFiveHour, 1_000_000, 0, reset)
	a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
	for _, q := range []api.TeamQueueEntry{a, b} {
		if got := f.listed(q.ID).BlockReason; got != "" {
			t.Fatalf("each entry fits alone: %q", got)
		}
	}
	a = f.run(t, a)
	end := reset.Add(5 * time.Hour).Format(time.RFC3339)
	reason := func(remain, reserved string) string {
		return fmt.Sprintf("Token budget (codex five_hour): needs about 400.00K tokens; %s remain before the reset at %s, reserve 0%%%s (source: allowance minus reported usage; provider reading for host mini is not reported)", remain, end, reserved)
	}
	if got, want := f.held(b, "the first estimate is reserved"), reason("1.00M", ", 700.00K reserved for 1 admitted team"); got != want {
		t.Fatalf("second entry\n%q\nwant\n%q", got, want)
	}
	// The team spends 250K: the remaining and the reserved amounts both fall
	// by exactly that.
	member := f.member(t, 0, "member-a")
	f.teamSpend(member, 0, "codex", 250_000)
	if got, want := f.held(b, "250K of the estimate is spent"), reason("750.00K", ", 450.00K reserved for 1 admitted team"); got != want {
		t.Fatalf("after the team spent 250K\n%q\nwant\n%q", got, want)
	}
	// Past its estimate the team reserves nothing, and never a negative amount.
	f.teamSpend(member, 0, "codex", 500_000)
	if got, want := f.held(b, "the team is past its estimate"), reason("250.00K", ""); got != want {
		t.Fatalf("after the team passed its estimate\n%q\nwant\n%q", got, want)
	}
	f.budget("codex", api.UsageWindowFiveHour, 1_150_000, 0, reset)
	if claimed, err := f.claim(b); err != nil || claimed.State != "launching" {
		t.Fatalf("400K against 400K with nothing reserved: %+v %v", claimed, err)
	}
}

// s3, provider source: remaining comes from the reading, which reflects use
// only up to its capture. Spend uploaded after it does not shrink the
// reservation until a newer reading arrives.
func TestQueueBudgetProviderReservationWaitsForReading(t *testing.T) {
	f := newBudgetQueue(t, 2)
	f.within()
	f.estimate(0, 700_000)
	f.estimate(1, 400_000)
	f.budget("claude", api.UsageWindowFiveHour, 1_000_000, 0, time.Time{})
	resets := f.s.now().UTC().Truncate(time.Second).Add(3 * time.Hour)
	f.reading("mini", 0, resets)
	a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
	a = f.run(t, a)
	reason := func(remain, reserved string) string {
		return fmt.Sprintf("Token budget (claude five_hour): needs about 400.00K tokens; %s remain before the reset at %s, reserve 0%%, %s reserved for 1 admitted team (source: provider reading)", remain, resets.Format(time.RFC3339), reserved)
	}
	before := f.held(b, "the first estimate is reserved")
	if want := reason("1.00M", "700.00K"); before != want {
		t.Fatalf("second entry\n%q\nwant\n%q", before, want)
	}
	// 400K in turns after the reading's capture: the reading does not show
	// them, so the reservation stays whole.
	member := f.member(t, 0, "member-a")
	f.advance(time.Minute)
	f.teamSpend(member, 0, "claude", 400_000)
	if got := f.held(b, "spend the reading does not reflect"); got != before {
		t.Fatalf("after an upload with no newer reading\n%q\nwant\n%q", got, before)
	}
	// A newer reading, captured after those turns, shows them.
	f.advance(time.Minute)
	f.reading("mini", 40, resets)
	if got, want := f.held(b, "a newer reading reflects the spend"), reason("600.00K", "300.00K"); got != want {
		t.Fatalf("after a newer reading\n%q\nwant\n%q", got, want)
	}
}

// s3: a failed entry holds its slot and its runs can still spend, so it
// reserves until it is released.
func TestQueueBudgetFailedEntryReservesUntilRelease(t *testing.T) {
	f := newBudgetQueue(t, 2)
	f.within()
	reset := f.s.now().UTC().Truncate(time.Second).Add(-time.Hour)
	f.estimate(0, 700_000)
	f.estimate(1, 400_000)
	f.budget("codex", api.UsageWindowFiveHour, 1_000_000, 0, reset)
	a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
	a = f.run(t, a)
	before := f.held(b, "the running entry reserves")
	if !strings.Contains(before, "1.00M remain") || !strings.Contains(before, ", 700.00K reserved for 1 admitted team") {
		t.Fatalf("behind a running entry: %q", before)
	}
	failed := f.fail(t, a)
	if got := f.held(b, "the failed entry still reserves"); got != before {
		t.Fatalf("behind a failed entry\n%q\nwant\n%q", got, before)
	}
	// The release needs the started lead of the frozen plan closed and cleaned
	// up, as the runner leaves it.
	var plan struct {
		Members []struct {
			RunID  string `json:"runId"`
			Fields struct {
				AgentID string `json:"agentId"`
				Name    string `json:"name"`
			} `json:"fields"`
		} `json:"members"`
	}
	if err := json.Unmarshal(failed.LaunchJSON, &plan); err != nil || len(plan.Members) != 1 {
		t.Fatalf("frozen plan %s: %v", failed.LaunchJSON, err)
	}
	lead := plan.Members[0]
	if _, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: lead.Fields.Name, AgentID: lead.Fields.AgentID, Host: "mini", Session: lead.Fields.Name}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET run_id=?,status='closed',cleanup_done=1 WHERE task_id=? AND id=?`, lead.RunID, f.task.ID, lead.Fields.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "release", EntryID: failed.ID, ExpectedRevision: failed.Revision}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := f.listed(b.ID).BlockReason; got != "" {
		t.Fatalf("after the release: %q", got)
	}
	if claimed, err := f.claim(b); err != nil || claimed.State != "launching" {
		t.Fatalf("claim after the release: %+v %v", claimed, err)
	}
}

// s4: listing cost on a production-shaped window. The allowance source reads
// one indexed sum over the window's totals rows: 60,000 codex turns in a
// seven day window beside 10,000 claude turns and 20,000 older codex turns,
// with 20 queued entries and a running team. The window sum is bounded at 250
// ms without the race detector, and one list and one claim at 2 seconds.
func TestQueueBudgetWindowSumCost(t *testing.T) {
	const queued, inWindow, otherRuntime, older, perTurn = 20, 60000, 10000, 20000, 1050
	f := newBudgetQueue(t, queued+1)
	f.within()
	now := f.s.now().UTC().Truncate(time.Second)
	week, fiveHours := api.UsageWindowLength(api.UsageWindowSevenDay), api.UsageWindowLength(api.UsageWindowFiveHour)
	// The seven day window began one second after now minus seven days.
	start := now.Add(-week).Add(time.Second)
	for i := 0; i <= queued; i++ {
		f.estimate(i, 1_000)
	}
	f.budget("codex", api.UsageWindowSevenDay, 1_000_000_000, 0, start)
	f.budget("claude", api.UsageWindowFiveHour, 1_000_000_000, 0, now.Add(-time.Hour))
	running := f.run(t, f.add(t, 0, "src/running"))
	f.teamSpend(f.member(t, 0, "member-running"), 0, "codex", 300)
	entries := []api.TeamQueueEntry{}
	for i := 1; i <= queued; i++ {
		entries = append(entries, f.add(t, i, fmt.Sprintf("src/queued-%d", i)))
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO usage_turn_totals(task_id,agent_id,run_id,request_id,turn_revision,runtime,at_ns,tokens) VALUES(?,?,?,?,1,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	agent := f.handlers[0]
	codexSpent, claudeSpent := int64(300), int64(0)
	row := func(id, runtime string, at time.Time) {
		if _, err := insert.Exec(f.task.ID, agent.ID, agent.RunID, id, runtime, at.UnixNano(), perTurn); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < inWindow; i++ {
		at := now.Add(-time.Duration(i) * 10 * time.Second)
		if at.Before(start) {
			t.Fatalf("codex turn %d at %s is before the window start %s", i, at, start)
		}
		row(fmt.Sprintf("codex-%d", i), "codex", at)
		codexSpent += perTurn
	}
	for i := 0; i < otherRuntime; i++ {
		at := now.Add(-time.Duration(i) * time.Minute)
		row(fmt.Sprintf("claude-%d", i), "claude", at)
		if !at.Before(now.Add(-time.Hour)) {
			claudeSpent += perTurn
		}
	}
	for i := 0; i < older; i++ {
		row(fmt.Sprintf("old-%d", i), "codex", start.Add(-time.Duration(i+1)*10*time.Second))
	}
	insert.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if fiveHours >= week || claudeSpent != 61*perTurn {
		t.Fatalf("fixture: claude turns in its window spent %d", claudeSpent)
	}
	// (i) The window sum over all 60,000 codex turns in range.
	var sum int64
	took := bestOf(t, func() {
		if sum, err = usageRuntimeTokensSince(f.ctx, f.s.db, f.task.ID, "codex", start); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("window sum over %d turns in the window took %s (best of three)", inWindow, took)
	if sum != codexSpent {
		t.Fatalf("window sum %d, want %d", sum, codexSpent)
	}
	if !raceBuilt() && took >= 250*time.Millisecond {
		t.Fatalf("window sum over %d turns took %s, want under 250ms", inWindow, took)
	}
	// (iii) The remaining figure is the arithmetic sum.
	if got, want := budgetStatus(t, f.s, f.task.ID, "mini", "codex", api.UsageWindowSevenDay).RemainingTokens, fmt.Sprint(1_000_000_000-codexSpent); got != want {
		t.Fatalf("codex remaining %s, want %s", got, want)
	}
	if got, want := budgetStatus(t, f.s, f.task.ID, "mini", "claude", api.UsageWindowFiveHour).RemainingTokens, fmt.Sprint(1_000_000_000-claudeSpent); got != want {
		t.Fatalf("claude remaining %s, want %s", got, want)
	}
	// (ii) One list and one claim.
	started := time.Now()
	list, err := f.s.ListTeamQueue(f.ctx, f.task.ID)
	listed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	waiting := 0
	for _, e := range list.Entries {
		if e.State == "queued" && e.BlockReason == "" {
			waiting++
		}
		if e.ID == running.ID && e.State != "running" {
			t.Fatalf("running entry is %s", e.State)
		}
	}
	if waiting != queued {
		t.Fatalf("%d queued entries pass the budget, want %d", waiting, queued)
	}
	started = time.Now()
	claimed, err := f.claim(entries[0])
	claim := time.Since(started)
	if err != nil || claimed.State != "launching" {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	t.Logf("with %d turns in the window and %d queued entries one list took %s and one claim took %s", inWindow, queued, listed, claim)
	if listed >= 2*time.Second {
		t.Fatalf("the list took %s, want under 2s", listed)
	}
	if claim >= 2*time.Second {
		t.Fatalf("the claim took %s, want under 2s", claim)
	}
}

// budgetHoldFixture is a running queue entry whose entry, admission, lead and
// members come from the store's own calls (newRebindFixture), with a
// registered owner helper and a fixed clock. No agent is started or paused:
// agents are rows, and "held" is only what the holds read reports.
type budgetHoldFixture struct {
	*rebindFixture
	t      *testing.T
	helper api.Agent
	clock  time.Time
	n      int
}

func newHoldFixture(t *testing.T) *budgetHoldFixture {
	t.Helper()
	f := &budgetHoldFixture{rebindFixture: newRebindFixture(t, true), t: t}
	f.clock = f.s.now().UTC().Truncate(time.Second).Add(2 * time.Second)
	f.s.now = func() time.Time { return f.clock }
	f.helper = *registerHelper(t, f.s, f.task.ID, helperRequest("hold-helper"), f.by).Agent
	return f
}

// use uploads one turn of an agent attributed to the fixture's item.
func (f *budgetHoldFixture) use(a api.Agent, tokens int64) api.UsageBatch {
	f.t.Helper()
	f.n++
	key := fmt.Sprintf("hold-turn-%d", f.n)
	batch := usageBatch(a, key, usageTurnFor(key, f.order, tokens))
	f.replay(a, batch)
	return batch
}

func (f *budgetHoldFixture) replay(a api.Agent, batch api.UsageBatch) {
	f.t.Helper()
	if receipt, err := f.s.ReportUsage(f.ctx, f.task.ID, a.ID, batch); err != nil || receipt.Turns != 1 {
		f.t.Fatalf("usage upload %+v %v", receipt, err)
	}
}

func (f *budgetHoldFixture) estimate(tokens int64) {
	f.t.Helper()
	f.n++
	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, estimateRequest(f.item.Revision, fmt.Sprintf("hold-estimate-%d", f.n), tokens, "synthetic", api.Agent{}), f.by); err != nil {
		f.t.Fatal(err)
	}
}

// holds is each hold row as "estimate state", oldest first.
func (f *budgetHoldFixture) holds() []string {
	f.t.Helper()
	rows, err := f.s.db.Query(`SELECT estimate_tokens,state FROM team_queue_budget_holds WHERE task_id=? AND entry_id=? ORDER BY created_at,estimate_tokens`, f.task.ID, f.entry.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var estimate int64
		var state string
		if err := rows.Scan(&estimate, &state); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s", estimate, state))
	}
	return out
}

// asks are the hold questions addressed to the owner helper, oldest first.
func (f *budgetHoldFixture) asks() []api.Message {
	f.t.Helper()
	messages, err := f.s.ListMessages(f.ctx, f.task.ID, 0, f.helper.ID, 500)
	if err != nil {
		f.t.Fatal(err)
	}
	out := []api.Message{}
	for _, m := range messages {
		if m.To == f.helper.ID && m.Envelope != nil && m.Envelope.Subject == budgetHoldSubject {
			out = append(out, m)
		}
	}
	return out
}

func (f *budgetHoldFixture) want(why string, asks int, holds ...string) {
	f.t.Helper()
	if got := f.holds(); fmt.Sprint(got) != fmt.Sprint(holds) {
		f.t.Fatalf("%s: holds %v, want %v", why, got, holds)
	}
	if got := len(f.asks()); got != asks {
		f.t.Fatalf("%s: %d asks, want %d", why, got, asks)
	}
}

// held reports whether the holds read says this exact run is held.
func (f *budgetHoldFixture) held(a api.Agent) bool {
	f.t.Helper()
	out, err := f.s.BudgetHolds(f.ctx, f.task.ID, a.ID, a.RunID)
	if err != nil {
		f.t.Fatal(err)
	}
	return out.Held
}

func (f *budgetHoldFixture) continueHold(agent string) error {
	f.t.Helper()
	q, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "budget_continue", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, AgentID: agent, Caller: f.by})
	if err == nil {
		f.entry = q
	}
	return err
}

// a11: exactly 3 times a saved estimate is not past it; one token more holds
// the team once and asks the owner helper once, with the figures and both
// commands, however often uploads repeat.
func TestBudgetHoldPastThreeTimesAsksOnce(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.use(f.worker, 2000)
	first := f.use(f.lead, 1000)
	f.want("3000 of 1000 is at the multiple, not past it", 0)
	if f.held(f.worker) || f.held(f.lead) || f.listedEntry().BudgetHold != nil {
		t.Fatal("a team at exactly 3 times is held")
	}
	crossing := f.use(f.worker, 1)
	f.want("3001 of 1000 is past 3 times", 1, "1000 held")
	ask := f.asks()[0]
	e := ask.Envelope
	continueCommand := "tt usage hold continue --project " + f.task.ID + " --entry " + f.entry.ID
	stopCommand := "tt team queue fail --task " + f.task.ID + " --entry " + f.entry.ID + " --reason TEXT"
	wantRefs := map[string]string{"item": f.item.ID, "entry": f.entry.ID, "estimateTokens": "1000", "teamTokens": "3001", "teamState": "measured", "ratio": "3001/1000", "multiple": "3", "continue": continueCommand, "stop": stopCommand}
	if e.Kind != api.EnvelopeKindQuestion || fmt.Sprint(e.Refs) != fmt.Sprint(wantRefs) || e.Body.Question != "Continue or stop the held team?" || len(e.Body.Options) != 2 || !strings.Contains(e.Body.Options["continue"], continueCommand) || !strings.Contains(e.Body.Options["stop"], stopCommand) {
		t.Fatalf("ask %+v", e)
	}
	for _, part := range []string{"Item " + f.item.ID, "queue entry " + f.entry.ID, "has used 3001 tokens", "saved estimate of 1000", "3.00 times", continueCommand, "from an unbound owner shell: " + stopCommand, "a turn in progress and every write finish"} {
		if !strings.Contains(e.Body.Text, part) {
			t.Fatalf("ask text lacks %q: %q", part, e.Body.Text)
		}
	}
	if ask.From.AgentID != "" || ask.From.Node != "system" || ask.From.User != "usage-hold" {
		t.Fatalf("ask sender %+v", ask.From)
	}
	var askAgent string
	var askSeq int64
	if err := f.s.db.QueryRow(`SELECT ask_agent,ask_seq FROM team_queue_budget_holds WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry.ID).Scan(&askAgent, &askSeq); err != nil || askAgent != f.helper.ID || askSeq != ask.Seq {
		t.Fatalf("hold row ask %s %d %v", askAgent, askSeq, err)
	}
	// Repeated and replayed uploads create no second row or message.
	f.replay(f.lead, first)
	f.replay(f.worker, crossing)
	f.use(f.worker, 500)
	f.use(f.lead, 5000)
	f.want("repeated and replayed uploads", 1, "1000 held")
	// The list shows the hold on the running entry.
	listed := f.listedEntry()
	if listed.BudgetHold == nil || listed.BudgetHold.State != api.BudgetHoldHeld || listed.BudgetHold.EstimateTokens != 1000 || listed.BudgetHold.TeamTokens != "3001" || listed.BudgetHold.AskSeq != ask.Seq ||
		!strings.HasPrefix(listed.BlockReason, "Held past 3 times its token estimate (3.00K used, estimate 1.00K)") || !strings.Contains(listed.BlockReason, continueCommand) || !strings.Contains(listed.BlockReason, stopCommand) {
		t.Fatalf("listed hold %+v reason %q", listed.BudgetHold, listed.BlockReason)
	}
	if one, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID); err != nil || one.BudgetHold == nil || one.BlockReason != listed.BlockReason {
		t.Fatalf("entry read hold %+v %v", one.BudgetHold, err)
	}
}

func (f *budgetHoldFixture) listedEntry() api.TeamQueueEntry {
	f.t.Helper()
	return listedEntry(f.t, f.s, f.task.ID, f.entry.ID)
}

// a11: the owner helper acknowledges the ask, then answers it; while it is
// unacknowledged past the grace its other posts are refused, and afterwards
// they are accepted.
func TestBudgetHoldAskAcknowledgeThenAnswer(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	ask := f.asks()[0]
	post := func(text string) error {
		_, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.helper.ID, RunID: f.helper.RunID, RequestID: api.NewID("req"), Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Owner helper status for the held team", Body: api.EnvelopeBody{Text: text}}}, f.by)
		return err
	}
	f.clock = f.clock.Add(api.ObligationAckGrace + time.Second)
	var unacked *api.UnacknowledgedError
	if err := post("before the acknowledgement"); !errors.As(err, &unacked) {
		t.Fatalf("an unacknowledged ask did not gate the helper: %v", err)
	}
	found := false
	for _, item := range unacked.Items {
		found = found || item.Seq == ask.Seq
	}
	if !found {
		t.Fatalf("the gate names %+v, not the ask #%d", unacked.Items, ask.Seq)
	}
	for _, item := range unacked.Items {
		if _, err := f.s.ObligationAction(f.ctx, f.task.ID, item.Seq, "ack", api.ObligationActionRequest{AgentID: f.helper.ID, RunID: f.helper.RunID}, f.s.now()); err != nil {
			t.Fatalf("tt ack #%d: %v", item.Seq, err)
		}
	}
	if err := post("acknowledged, deciding"); err != nil {
		t.Fatalf("a post after the acknowledgement: %v", err)
	}
	if err := f.continueHold(f.helper.ID); err != nil {
		t.Fatalf("continue by the owner helper: %v", err)
	}
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.helper.ID, RunID: f.helper.RunID, RequestID: api.NewID("req"), ReplyTo: ask.Seq, Envelope: &api.Envelope{Kind: api.EnvelopeKindAnswer, Subject: "Continued the held team", Body: api.EnvelopeBody{Answer: "continue: the hold is lifted"}}}, f.by); err != nil {
		t.Fatalf("the answer to the ask: %v", err)
	}
	if err := post("after the answer"); err != nil {
		t.Fatalf("a post after the answer: %v", err)
	}
	f.want("continued", 1, "1000 continued")
}

// a11: a fault in the hold leaves the upload stored and nothing of the hold
// behind; once it clears, the next upload holds.
func TestBudgetHoldFailureKeepsUpload(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatal(query, err)
		}
		return n
	}
	questions := func() int {
		return count(`SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE ?`, f.task.ID, "%"+budgetHoldSubject+"%")
	}
	if _, err := f.s.db.Exec(`ALTER TABLE team_queue_budget_holds RENAME TO team_queue_budget_holds_away`); err != nil {
		t.Fatal(err)
	}
	f.use(f.worker, 3001)
	if got := count(`SELECT count(*) FROM usage_turns WHERE task_id=?`, f.task.ID); got != 1 {
		t.Fatalf("%d turns stored, want 1", got)
	}
	if got := count(`SELECT count(*) FROM usage_receipts WHERE task_id=?`, f.task.ID); got != 1 {
		t.Fatalf("%d usage receipts, want 1", got)
	}
	if questions() != 0 || len(f.asks()) != 0 {
		t.Fatal("an ask was posted without its hold row")
	}
	// The same fault in the insert: the ask is rolled back with it.
	if _, err := f.s.db.Exec(`ALTER TABLE team_queue_budget_holds_away RENAME TO team_queue_budget_holds`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER hold_fault BEFORE INSERT ON team_queue_budget_holds BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	f.use(f.worker, 1)
	if got := count(`SELECT count(*) FROM usage_turns WHERE task_id=?`, f.task.ID); got != 2 {
		t.Fatalf("%d turns stored, want 2", got)
	}
	if questions() != 0 || queueBudgetHoldRows(t, f.s, f.task.ID) != 0 {
		t.Fatalf("a failed hold left %d asks and %d rows", questions(), queueBudgetHoldRows(t, f.s, f.task.ID))
	}
	if obligations := count(`SELECT count(*) FROM obligations WHERE task_id=? AND subject=?`, f.task.ID, budgetHoldSubject); obligations != 0 {
		t.Fatalf("a failed hold left %d obligations", obligations)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER hold_fault`); err != nil {
		t.Fatal(err)
	}
	f.use(f.worker, 1)
	f.want("after the fault clears", 1, "1000 held")
}

// a10: an item admitted on a lane default never warns at 1.5 times and is
// never held at 3 times, whatever its team reports.
func TestBudgetHoldNeverOnLaneDefault(t *testing.T) {
	f := newHoldFixture(t)
	if _, err := f.s.SetUsageEstimateDefaults(f.ctx, f.task.ID, api.UsageEstimateDefaultsRequest{SmallTokens: 100, SmallRaceTokens: 100, PlannedTokens: 100, PlannedRaceTokens: 100}, f.by); err != nil {
		t.Fatal(err)
	}
	if got := f.listedEntry().EstimateDefault; got == nil || got.Tokens != 100 {
		t.Fatalf("fixture: the entry is not on a lane default: %+v", got)
	}
	f.use(f.worker, 151)
	f.use(f.worker, 301)
	f.use(f.lead, 50_000_000)
	f.want("a lane default of 100 against 50M", 0)
	var warnings int
	if err := f.s.db.QueryRow(`SELECT (SELECT count(*) FROM usage_entry_warnings WHERE task_id=?)+(SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE ?)`, f.task.ID, f.task.ID, "%"+usageWarningTestSubject+"%").Scan(&warnings); err != nil || warnings != 0 {
		t.Fatalf("a lane default warned: %d %v", warnings, err)
	}
	if f.held(f.worker) || f.listedEntry().BudgetHold != nil {
		t.Fatal("a lane default held the team")
	}
}

// a13: the holds read lists the team's runs by exact agent and run and never
// a bound database handler, backlog steward, deployment agent or owner
// helper; their tokens are not in the compared figure.
func TestBudgetHoldExcludesProjectRoles(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	ref := api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}
	bind := func(a api.Agent) {
		t.Helper()
		if _, err := f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,0,'member',?,'{}',?)`,
			a.ID, a.RunID, f.task.ID, f.item.ID, f.item.Revision, ref.TaskID, ref.Seq, strings.Repeat("d", 64), ts(f.s.now())); err != nil {
			t.Fatal(err)
		}
	}
	roles := []api.Agent{f.handler, f.helper}
	for _, role := range []string{api.AgentRoleBacklogSteward, api.AgentRoleDeployment} {
		a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: role, AgentID: api.NewID("agt"), Host: "mini", Session: role}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`UPDATE agents SET role=?,status='running' WHERE id=?`, role, a.ID); err != nil {
			t.Fatal(err)
		}
		roles = append(roles, a)
	}
	for _, a := range roles {
		bind(a)
		f.use(a, 1_000_000)
	}
	f.want("four bound project roles spent 4M against 1000", 0)
	f.use(f.worker, 3001)
	f.want("the team crosses on its own tokens", 1, "1000 held")
	if refs := f.asks()[0].Envelope.Refs; refs["teamTokens"] != "3001" {
		t.Fatalf("compared figure %v", refs)
	}
	all, err := f.s.BudgetHolds(f.ctx, f.task.ID, "", "")
	if err != nil || !all.Held || len(all.Holds) != 1 || all.Holds[0].EntryID != f.entry.ID {
		t.Fatalf("holds %+v %v", all, err)
	}
	want := map[string]string{f.lead.ID: f.lead.RunID, f.worker.ID: f.worker.RunID, f.gone.ID: f.gone.RunID}
	got := map[string]string{}
	for _, run := range all.Holds[0].Runs {
		got[run.AgentID] = run.RunID
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("held runs %v, want the team %v", got, want)
	}
	for _, a := range roles {
		if f.held(a) {
			t.Fatalf("%s (%s) is held", a.Name, a.Role)
		}
	}
	if !f.held(f.worker) || !f.held(f.lead) {
		t.Fatal("the team's own runs are not held")
	}
	// Exact run: the same agent under another run id is not held, nor is an
	// agent of the project that is not bound to the item.
	if out, err := f.s.BudgetHolds(f.ctx, f.task.ID, f.worker.ID, api.NewID("run")); err != nil || out.Held || len(out.Holds) != 0 {
		t.Fatalf("another run of a held agent: %+v %v", out, err)
	}
	outsider, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "outsider", AgentID: api.NewID("agt"), Host: "mini", Session: "outsider"}, f.by)
	if err != nil || f.held(outsider) {
		t.Fatalf("an unbound agent is held: %v", err)
	}
	one, err := f.s.BudgetHolds(f.ctx, f.task.ID, f.worker.ID, f.worker.RunID)
	if err != nil || !one.Held || len(one.Holds) != 1 || len(one.Holds[0].Runs) != 1 || one.Holds[0].Runs[0] != (api.BudgetHoldRun{AgentID: f.worker.ID, RunID: f.worker.RunID}) {
		t.Fatalf("exact run read %+v %v", one, err)
	}
	if _, err := f.s.BudgetHolds(f.ctx, f.task.ID, f.worker.ID, ""); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("an agent without its run: %v", err)
	}
	if _, err := f.s.BudgetHolds(f.ctx, "tsk_0000000000000000", "", ""); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// a15: while a hold is in force the hub refuses nothing because of it: a held
// agent's post, acknowledgement, work-item write and usage upload are stored.
func TestBudgetHeldAgentWritesAreAccepted(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	if !f.held(f.worker) || !f.held(f.lead) {
		t.Fatal("fixture: the team is not held")
	}
	links := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
	order := &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}
	assign, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.worker.ID, RequestID: api.NewID("req"), WorkItems: links, WorkOrderMessage: order,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, To: f.worker.Name, Subject: "Finish the write that is in progress", Body: api.EnvelopeBody{Objective: "finish it", Owns: []string{"hub/x.go"}, Acceptance: map[string]string{"a1": "saved"}}}}, f.by)
	if err != nil {
		t.Fatalf("a held lead's post: %v", err)
	}
	if _, err := f.s.ObligationAction(f.ctx, f.task.ID, assign.Seq, "ack", api.ObligationActionRequest{AgentID: f.worker.ID, RunID: f.worker.RunID}, f.s.now()); err != nil {
		t.Fatalf("a held agent's acknowledgement: %v", err)
	}
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, To: f.lead.ID, ReplyTo: assign.Seq, RequestID: api.NewID("req"), WorkItems: links, WorkOrderMessage: order,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindResult, To: f.lead.Name, Subject: "Finished the write that was in progress", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "saved"}}}}, f.by); err != nil {
		t.Fatalf("a held agent's result: %v", err)
	}
	if _, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "A bug a held agent found", AgentID: f.worker.ID, RequestID: "held-create"}, f.by); err != nil {
		t.Fatalf("a held agent's work-item write: %v", err)
	}
	f.use(f.worker, 77)
	f.use(f.lead, 5)
	if budget, err := loadTokenBudget(f.ctx, f.s.db, f.task.ID, f.item.ID); err != nil || budget.Team == nil || budget.Team.ActualTokens != "3083" {
		t.Fatalf("usage while held %+v %v", budget, err)
	}
	f.want("still one hold", 1, "1000 held")
}

// a16: continue lifts the hold for the owner and the owner helper only; the
// entry is not held again at the same estimate; a newly saved estimate
// re-arms the hold at 3 times the new value.
func TestBudgetHoldContinueAndRearm(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	if err := f.continueHold(""); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "has no token budget hold in force") {
		t.Fatalf("continue with no hold: %v", err)
	}
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	for name, agent := range map[string]string{"a team member": f.worker.ID, "the database handler": f.handler.ID, "the lead": f.lead.ID} {
		if err := f.continueHold(agent); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "only the owner or the owner helper continues a held team") {
			t.Fatalf("continue by %s: %v", name, err)
		}
	}
	f.want("refused continues change nothing", 1, "1000 held")
	revision := f.entry.Revision
	if err := f.continueHold(f.helper.ID); err != nil {
		t.Fatalf("continue by the owner helper: %v", err)
	}
	f.want("continued", 1, "1000 continued")
	if f.held(f.worker) || f.held(f.lead) || f.listedEntry().BudgetHold != nil || f.listedEntry().BlockReason != "" || f.entry.State != "running" || f.entry.Revision != revision+1 {
		t.Fatalf("after continue: entry %s r%d reason %q", f.entry.State, f.entry.Revision, f.listedEntry().BlockReason)
	}
	var byAgent, byUser, resolved string
	if err := f.s.db.QueryRow(`SELECT by_agent,by_user,resolved_at FROM team_queue_budget_holds WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry.ID).Scan(&byAgent, &byUser, &resolved); err != nil || byAgent != f.helper.ID || byUser != "owner" || resolved != ts(f.clock) {
		t.Fatalf("continued by %s %s at %s %v", byAgent, byUser, resolved, err)
	}
	// No estimate was written by the continue.
	if item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID); err != nil || item.Budget == nil || item.Budget.Estimate == nil || item.Budget.Estimate.Tokens != 1000 {
		t.Fatalf("estimate after continue: %+v %v", item.Budget, err)
	}
	f.use(f.worker, 9000)
	f.want("a later upload at the same estimate does not hold again", 1, "1000 continued")
	if f.held(f.worker) {
		t.Fatal("held again at the same estimate")
	}
	// A new saved estimate: 12001 of 5000 is not past 15000.
	f.estimate(5000)
	f.use(f.worker, 1)
	f.want("below 3 times the new estimate", 1, "1000 continued")
	f.use(f.worker, 2998)
	f.want("15000 of 5000 is at the multiple", 1, "1000 continued")
	f.use(f.worker, 1)
	f.want("past 3 times the new estimate", 2, "1000 continued", "5000 held")
	if !f.held(f.worker) {
		t.Fatal("not held at the new estimate")
	}
	// The owner, with no agent identity, continues too.
	if err := f.continueHold(""); err != nil {
		t.Fatalf("continue by the owner: %v", err)
	}
	f.want("continued again", 2, "1000 continued", "5000 continued")
}

// a17: stop is the existing fail of the entry. With the lead owing open
// obligations, it sets the entry failed with the reason, posts the usual
// failure notice once and resolves the hold as stopped in the same
// transaction. The team's runs stay listed while the entry is failed and
// unreleased, and no further ask is posted.
func TestBudgetHoldStopByEntryFail(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	// The lead owes an open obligation, so the team close is not available.
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.lead.ID, RequestID: "open-for-lead", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.lead.Name, Subject: "Report the state of the candidate", Body: api.EnvelopeBody{Ask: "Report it"}}}, f.by); err != nil {
		t.Fatal(err)
	}
	var open int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM obligations WHERE task_id=? AND agent_id=? AND state NOT IN ('done','cancelled','withdrawn')`, f.task.ID, f.lead.ID).Scan(&open); err != nil || open == 0 {
		t.Fatalf("fixture: the lead owes %d open obligations %v", open, err)
	}
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	failures := func() int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE '%Team queue failed and requires owner action%'`, f.task.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.clock = f.clock.Add(time.Minute)
	reason := "Owner failed this entry: three times the estimate with no result in sight"
	failed := f.action(t, api.TeamQueueRequest{Operation: "fail", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Failure: reason, AgentID: f.helper.ID, Caller: f.by})
	f.entry = failed
	if failed.State != "failed" || failed.Failure != reason || failed.EscalationSeq == 0 || failures() != 1 {
		t.Fatalf("failed entry %s %q escalation %d notices %d", failed.State, failed.Failure, failed.EscalationSeq, failures())
	}
	f.want("stopped", 1, "1000 stopped")
	var resolved, byUser string
	if err := f.s.db.QueryRow(`SELECT resolved_at,by_user FROM team_queue_budget_holds WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry.ID).Scan(&resolved, &byUser); err != nil || resolved != failed.UpdatedAt || byUser != "owner" {
		t.Fatalf("hold stopped at %s by %s, entry failed at %s %v", resolved, byUser, failed.UpdatedAt, err)
	}
	// Failed and unreleased: the stopped team starts no new turns.
	if !f.held(f.worker) || !f.held(f.lead) {
		t.Fatal("the stopped team's runs are no longer listed")
	}
	all, err := f.s.BudgetHolds(f.ctx, f.task.ID, "", "")
	if err != nil || len(all.Holds) != 1 || all.Holds[0].State != api.BudgetHoldStopped || all.Holds[0].ResolvedBy == nil {
		t.Fatalf("holds while failed %+v %v", all, err)
	}
	if listed := f.listedEntry(); listed.BudgetHold != nil || !strings.HasPrefix(listed.BlockReason, "Failed") && listed.BlockReason != "" {
		t.Fatalf("failed entry lists hold %+v reason %q", listed.BudgetHold, listed.BlockReason)
	}
	// More usage from a turn that was in progress: no further ask, no new row.
	f.use(f.worker, 50_000)
	f.want("no further ask after the stop", 1, "1000 stopped")
	if failures() != 1 {
		t.Fatalf("%d failure notices", failures())
	}
	if err := f.continueHold(""); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("continue of a stopped hold: %v", err)
	}
	// Released: the runs are no longer listed.
	if _, err := f.s.db.Exec(`UPDATE agents SET status='closed',cleanup_done=1 WHERE task_id=? AND role=''`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "release", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision})
	if f.held(f.worker) || f.held(f.lead) {
		t.Fatal("a released entry's team is still listed as held")
	}
	if all, err := f.s.BudgetHolds(f.ctx, f.task.ID, "", ""); err != nil || all.Held || len(all.Holds) != 0 {
		t.Fatalf("holds after the release %+v %v", all, err)
	}
	f.want("released", 1, "1000 stopped")
}

// A hold in force is resolved when its entry leaves the queue another way:
// the owner records an integration, or the team closes with the item open.
func TestBudgetHoldResolvedWhenEntryEnds(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "owner_integrated", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, OwnerIntegrationCommit: strings.Repeat("b", 40), OwnerIntegrationEvidence: "owner release record", Caller: f.by})
	f.want("owner integrated", 1, "1000 stopped")
	if f.held(f.worker) {
		t.Fatal("an integrated entry's team is still held")
	}
	g := newHoldFixture(t)
	g.estimate(1000)
	g.use(g.worker, 3001)
	g.want("held", 1, "1000 held")
	tx, err := g.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := failQueueEntryForOpenClose(g.ctx, tx, g.task.ID, g.item.ID, "superseded", ts(g.clock)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	g.want("team closed with the item open", 1, "1000 stopped")
}

// With no owner helper the Board carries the ask to the owner, once.
func TestBudgetHoldWithoutOwnerHelperNotifiesBoard(t *testing.T) {
	f := newHoldFixture(t)
	if _, err := f.s.db.Exec(`UPDATE agents SET status='closed' WHERE id=?`, f.helper.ID); err != nil {
		t.Fatal(err)
	}
	f.estimate(1000)
	f.use(f.worker, 3001)
	f.use(f.worker, 1)
	f.want("held with no helper", 0, "1000 held")
	var notices int
	var envelope string
	if err := f.s.db.QueryRow(`SELECT count(*),max(envelope) FROM messages WHERE task_id=? AND envelope LIKE ?`, f.task.ID, "%"+budgetHoldSubject+"%").Scan(&notices, &envelope); err != nil || notices != 1 || !strings.Contains(envelope, `"escalation":"owner"`) || !strings.Contains(envelope, `"kind":"notice"`) {
		t.Fatalf("board notice %d %s %v", notices, envelope, err)
	}
	if err := f.continueHold(""); err != nil {
		t.Fatalf("the owner continues: %v", err)
	}
}

// a22, running half: a queued entry that would stall behind an idle running
// team does not while that team is held: its reason names the hold and its
// stall notice is refused. With no hold the stall is as before.
func TestBudgetHeldEntryCausesNoIdleStall(t *testing.T) {
	f, advance, a, b, lead, worker := idleTeam(t)
	ctx := context.Background()
	advance(40 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID)
	stall := f.stall(t, b.ID)
	// The team passes 3 times its saved estimate.
	tokens := int64(1000)
	if _, _, err := f.s.CreateWorkItemUpdate(ctx, f.task.ID, f.items[0].ID, estimateRequest(f.items[0].Revision, "idle-estimate", tokens, "synthetic", api.Agent{}), f.by); err != nil {
		t.Fatal(err)
	}
	batch := usageBatch(worker, "idle-turn", usageTurnFor("idle-turn", f.orders[0], 3001))
	if _, err := f.s.ReportUsage(ctx, f.task.ID, worker.ID, batch); err != nil {
		t.Fatal(err)
	}
	if queueBudgetHoldRows(t, f.s, f.task.ID) != 1 {
		t.Fatal("fixture: the idle team is not held")
	}
	_ = lead
	advance(40 * time.Minute)
	f.activity(t, lead, "idle")
	f.activity(t, worker, "idle")
	advance(40 * time.Minute)
	got := listedEntry(t, f.s, f.task.ID, b.ID)
	want := "Waits behind entry " + a.ID + ", held past 3 times its token estimate until the owner helper continues or stops it"
	if got.Stall != nil || got.BlockReason != want {
		t.Fatalf("queued behind a held team: stall %+v reason %q", got.Stall, got.BlockReason)
	}
	if held := listedEntry(t, f.s, f.task.ID, a.ID); held.BudgetHold == nil || held.Stall != nil {
		t.Fatalf("held running entry %+v", held.BudgetHold)
	}
	for _, id := range []string{stall.NoticeRequestID(b.ID), api.TeamQueueStall{Cause: api.StallIdleEntry, BlockerEntryID: a.ID, BlockerRevision: listedEntry(t, f.s, f.task.ID, a.ID).Revision}.NoticeRequestID(b.ID)} {
		if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: b.ID}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "the stall is stale or has cleared") {
			t.Fatalf("stall notice behind a held team: %v", err)
		}
	}
	// Continued: the team is idle and unheld, so the stall is real again.
	held := listedEntry(t, f.s, f.task.ID, a.ID)
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "budget_continue", EntryID: a.ID, ExpectedRevision: held.Revision, Caller: f.by}); err != nil {
		t.Fatal(err)
	}
	advance(40 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID)
}

// ask posts a directed request from the owner, which queues a wake job.
func (f *budgetHoldFixture) ask(to api.Agent, key string) {
	f.t.Helper()
	if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: to.ID, RequestID: key, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: to.Name, Subject: "Report the state of the candidate", Body: api.EnvelopeBody{Ask: "Report it"}}}, f.by); err != nil {
		f.t.Fatal(err)
	}
}

// later moves the clock on and keeps every agent online at the new time.
func (f *budgetHoldFixture) later(d time.Duration) {
	f.t.Helper()
	f.clock = f.clock.Add(d)
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE task_id=? AND status<>'closed'`, ts(f.clock), f.task.ID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *budgetHoldFixture) lease(a api.Agent) *api.WakeJob {
	f.t.Helper()
	job, err := f.s.LeaseWakeJob(f.ctx, f.task.ID, a.ID, a.RunID, f.clock)
	if err != nil {
		f.t.Fatalf("lease for %s: %v", a.Name, err)
	}
	return job
}

// wakeJobs is every wake job of an agent, to prove a refused lease wrote nothing.
func (f *budgetHoldFixture) wakeJobs(a api.Agent) string {
	f.t.Helper()
	return tableDump(f.t, f.s, `SELECT id,state,due_at,lease_token,lease_expires_at,run_id FROM wake_jobs WHERE agent_id=? ORDER BY id`, a.ID)
}

// a14, hub half: the wake-job lease returns no job for a held run and writes
// nothing, so the job stays due and is leased on the first attempt after
// continue. A job leased before the hold stays with its holder; an expired
// lease is not renewed during the hold; another run id and a role agent are
// answered as before.
func TestBudgetHoldRefusesWakeJobLease(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.ask(f.worker, "wake-worker")
	f.ask(f.lead, "wake-lead")
	f.later(time.Minute)
	// Before any hold a due job is leased.
	early := f.lease(f.worker)
	if early == nil || early.LeaseToken == "" {
		t.Fatal("a due job was not leased before the hold")
	}
	// The hold is written (the same upload also queues the lead's warning
	// notice). The very next lease returns no job and writes nothing.
	f.use(f.worker, 3001)
	f.want("held", 1, "1000 held")
	pending := f.wakeJobs(f.lead)
	if !strings.Contains(pending, wakePending) {
		t.Fatalf("fixture: the lead has no pending wake job: %s", pending)
	}
	if job := f.lease(f.lead); job != nil {
		t.Fatalf("a held run was leased %+v", job)
	}
	if after := f.wakeJobs(f.lead); after != pending {
		t.Fatalf("the refused lease wrote:\n%s\nwas\n%s", after, pending)
	}
	// The job leased before the hold stays leased to its holder.
	leased := f.wakeJobs(f.worker)
	if !strings.Contains(leased, early.ID) || !strings.Contains(leased, early.LeaseToken) || !strings.Contains(leased, wakeLeased) {
		t.Fatalf("the earlier lease changed: %s", leased)
	}
	// Its lease expires during the hold: it is not leased again.
	f.later(wakeLease + time.Minute)
	if job := f.lease(f.worker); job != nil {
		t.Fatalf("an expired lease was renewed for a held run: %+v", job)
	}
	if after := f.wakeJobs(f.worker); after != leased {
		t.Fatalf("the refused lease wrote:\n%s\nwas\n%s", after, leased)
	}
	// Another run id of the same agent is refused as before, never matched as held.
	_, refused := f.s.LeaseWakeJob(f.ctx, f.task.ID, f.worker.ID, api.NewID("run"), f.clock)
	if held, err := budgetHeldRun(f.ctx, f.s.db, f.task.ID, f.worker.ID, api.NewID("run")); refused == nil || err != nil || held {
		t.Fatalf("another run id: lease %v, held %v %v", refused, held, err)
	}
	// A run with a project role bound to the same item is leased as before.
	if _, err := f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,0,'member',?,'{}',?)`,
		f.handler.ID, f.handler.RunID, f.task.ID, f.item.ID, f.item.Revision, f.task.ID, f.order.Seq, strings.Repeat("d", 64), ts(f.clock)); err != nil {
		t.Fatal(err)
	}
	f.ask(f.handler, "wake-handler")
	f.later(time.Minute)
	if job := f.lease(f.handler); job == nil {
		t.Fatal("a bound database handler was not leased during the hold")
	}
	// Continue: the first lease returns the same jobs.
	if err := f.continueHold(""); err != nil {
		t.Fatal(err)
	}
	if job := f.lease(f.lead); job == nil || !strings.Contains(pending, job.ID) {
		t.Fatalf("the lead's job after continue: %+v, was %s", job, pending)
	}
	if job := f.lease(f.worker); job == nil || job.ID != early.ID {
		t.Fatalf("the worker's expired job after continue: %+v, want %s", job, early.ID)
	}
}

// a14, hub half: a held team stopped by entry fail gets no lease while the
// entry is failed and unreleased.
func TestBudgetHoldStoppedTeamGetsNoLease(t *testing.T) {
	f := newHoldFixture(t)
	f.estimate(1000)
	f.ask(f.lead, "wake-lead")
	f.later(time.Minute)
	f.use(f.worker, 3001)
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "fail", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Failure: "Owner failed this entry: stop", Caller: f.by})
	f.want("stopped", 1, "1000 stopped")
	before := f.wakeJobs(f.lead)
	f.later(time.Minute)
	if job := f.lease(f.lead); job != nil || f.wakeJobs(f.lead) != before {
		t.Fatalf("a stopped team was leased: %+v", job)
	}
}
