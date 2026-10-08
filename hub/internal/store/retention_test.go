package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Marker strings stand in for content that must never reach test or hub
// output: a message body, a turn, a check list and a launch plan.
const (
	retentionMarkerBody   = "retention-marker-body-5d1c"
	retentionMarkerTurn   = "retention-marker-turn-83af"
	retentionMarkerCheck  = "retention-marker-check-c07e"
	retentionMarkerLaunch = "retention-marker-launch-19b2"
)

var retentionMarkers = []string{retentionMarkerBody, retentionMarkerTurn, retentionMarkerCheck, retentionMarkerLaunch}

// retentionSum is the SHA-256 a failure prints in place of content.
func retentionSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// retentionJSON encodes a reply the way the hub's API does.
func retentionJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// retentionSame fails, printing only hashes, when two encodings differ.
func retentionSame(t *testing.T, why string, a, b []byte) {
	t.Helper()
	if !bytes.Equal(a, b) {
		t.Fatalf("%s: %d bytes sha256 %s, then %d bytes sha256 %s", why, len(a), retentionSum(a), len(b), retentionSum(b))
	}
}

// retentionFixture is one database with a usage ledger, a team queue entry
// and a release job, each carrying its marker string.
type retentionFixture struct {
	t        *testing.T
	s        *Store
	ctx      context.Context
	task     api.Task
	items    []api.WorkItem
	orders   []api.Message
	handler  api.Agent
	deployer api.Agent
	meter    api.Agent
	entry    api.TeamQueueEntry
	job      api.ReleaseJob
}

func newRetentionFixture(t *testing.T) *retentionFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &retentionFixture{t: t, s: s, ctx: context.Background(), task: task, items: items, orders: orders}
	by := api.Caller{Node: "fixture", User: "owner"}
	agents, err := s.ListAgents(f.ctx, task.ID)
	if err != nil || len(agents) != 1 {
		t.Fatal(len(agents), err)
	}
	f.handler = agents[0]
	if f.meter, err = s.AddAgent(f.ctx, task.ID, api.AddAgentRequest{Name: "metered", AgentID: api.NewID("agt"), Runtime: "codex", Host: "fixture", Session: "synthetic"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(f.ctx, task.ID, api.PostMessageRequest{Text: retentionMarkerBody, RequestID: "marker-body"}, by); err != nil {
		t.Fatal(err)
	}
	return f
}

// path is the database file the fixture's store has open.
func (f *retentionFixture) path() string {
	f.t.Helper()
	var seq int
	var name, file string
	if err := f.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil || file == "" {
		f.t.Fatal(err)
	}
	return file
}

// reopen closes the store and opens its file again, as a hub restart does.
func (f *retentionFixture) reopen() {
	f.t.Helper()
	file := f.path()
	if err := f.s.Close(); err != nil {
		f.t.Fatal(err)
	}
	s, err := Open(file)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	f.s = s
}

// turn is a synthetic turn carrying the turn marker.
func (f *retentionFixture) turn(id string) api.UsageTurn {
	turn := syntheticUsageTurn(id)
	turn.Model = retentionMarkerTurn
	return turn
}

// revised is the next revision of a turn, with more output measured.
func retentionRevised(turn api.UsageTurn) api.UsageTurn {
	turn.Revision++
	turn.Tokens = map[string]int64{"input": 21, "cached": 80, "cacheWrite": 0, "output": 9, "reasoning": 2}
	turn.Raw = map[string]int64{"input_tokens": 101, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 11, "reasoning_output_tokens": 2}
	return turn
}

func (f *retentionFixture) report(batch api.UsageBatch) (api.UsageReceipt, error) {
	return f.s.ReportUsage(f.ctx, f.task.ID, f.meter.ID, batch)
}

// text reads one text value.
func (f *retentionFixture) text(query string, args ...any) string {
	f.t.Helper()
	var out string
	if err := f.s.db.QueryRow(query, args...).Scan(&out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// count reads one number.
func (f *retentionFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// launchPlan is a one-member launch plan whose member carries the marker.
func (f *retentionFixture) launchPlan() []byte {
	members := []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": retentionMarkerLaunch, "cwd": "/tmp"}}}
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.items[0].ID, "revision": f.items[0].Revision, "order": f.orders[0].Seq, "context": map[string]any{"version": 1}, "members": members})
	return plan
}

func (f *retentionFixture) queue(req api.TeamQueueRequest) api.TeamQueueEntry {
	f.t.Helper()
	e, err := f.s.TeamQueueAction(f.ctx, f.task.ID, req)
	if err != nil {
		f.t.Fatal(req.Operation, err)
	}
	return e
}

// Queue requests of the fixture, each retried under its own request ID.
func (f *retentionFixture) addRequest() api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/tmp"}
}
func (f *retentionFixture) claimRequest(added api.TeamQueueEntry) api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: "claim", Operation: "claim", EntryID: added.ID, ExpectedRevision: added.Revision, Host: "mini"}
}
func (f *retentionFixture) freezeRequest(claimed api.TeamQueueEntry, plan []byte) api.TeamQueueRequest {
	return api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: claimed.ID, ExpectedRevision: claimed.Revision, LaunchJSON: plan}
}

// freeze adds and claims an entry and freezes a launch plan on it, returning
// the claim and freeze requests with their first replies.
func (f *retentionFixture) freeze() (claim, freeze api.TeamQueueRequest, claimed, frozen api.TeamQueueEntry) {
	f.t.Helper()
	added := f.queue(f.addRequest())
	claim = f.claimRequest(added)
	claimed = f.queue(claim)
	freeze = f.freezeRequest(claimed, f.launchPlan())
	frozen = f.queue(freeze)
	f.entry = frozen
	return claim, freeze, claimed, frozen
}

func (f *retentionFixture) release(req api.ReleaseRequest) api.ReleaseJob {
	f.t.Helper()
	j, err := f.s.ReleaseAction(f.ctx, f.task.ID, req)
	if err != nil {
		f.t.Fatal(req.Operation, err)
	}
	return j
}

// markerChecks is a check list carrying the check list marker.
func retentionMarkerChecks() []api.VerificationCheck {
	return []api.VerificationCheck{
		{ID: retentionMarkerCheck, Argv: []string{"fixture", retentionMarkerCheck}, Cwd: ".", Environment: map[string]string{}},
		{ID: "synthetic-second", Argv: []string{"fixture", "second"}, Cwd: ".", Environment: map[string]string{}},
	}
}

// imported takes a release job as far as its integrated verification import
// and returns that request with its first reply. The job then holds three
// check lists: the plan's, the integrated plan's (equal to it) and the
// integrated receipt's.
func (f *retentionFixture) imported() (api.ReleaseRequest, api.ReleaseJob) {
	f.t.Helper()
	by := api.Caller{Node: "fixture", User: "owner"}
	var err error
	if f.deployer, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, by); err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(f.s.now()), f.deployer.ID); err != nil {
		f.t.Fatal(err)
	}
	entry := releaseEntry(f.t, f.s, f.task, "release item", "release-item")
	j := f.release(api.ReleaseRequest{RequestID: "release-enqueue", Operation: "enqueue", AgentID: f.handler.ID, RunID: f.handler.RunID, EntryID: entry})
	j = f.release(api.ReleaseRequest{RequestID: "release-claim", Operation: "claim", AgentID: f.deployer.ID, RunID: f.deployer.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	checks := retentionMarkerChecks()
	j = approveChecks(f.t, f.s, j, checks)
	req := integratedImport(j, f.handler, f.deployer, "release-import", checks)
	f.job = f.release(req)
	return req, f.job
}

// tableRows hashes every row of one table, in rowid order.
func (f *retentionFixture) tableRows(table, columns string) string {
	f.t.Helper()
	rows, err := f.s.db.Query(`SELECT ` + columns + ` FROM ` + table + ` ORDER BY rowid`)
	if err != nil {
		f.t.Fatal(table, err)
	}
	defer rows.Close()
	names, _ := rows.Columns()
	h := sha256.New()
	n := 0
	for rows.Next() {
		values := make([][]byte, len(names))
		into := make([]any, len(names))
		for i := range values {
			into[i] = &values[i]
		}
		if err = rows.Scan(into...); err != nil {
			f.t.Fatal(table, err)
		}
		for _, v := range values {
			fmt.Fprintf(h, "%d:", len(v))
			h.Write(v)
		}
		n++
	}
	if err = rows.Err(); err != nil {
		f.t.Fatal(table, err)
	}
	return fmt.Sprintf("%d rows sha256 %s", n, hex.EncodeToString(h.Sum(nil)))
}

// retentionTables are the five tables whose storage form changed.
var retentionTables = [][2]string{
	{"usage_turns", "task_id,agent_id,run_id,request_id,revision,at,payload,projection"},
	{"usage_turn_revisions", "task_id,agent_id,run_id,request_id,revision,payload,projection"},
	{"usage_receipts", "task_id,request_id,agent_id,run_id,payload,receipt"},
	{"release_action_receipts", "task_id,request_id,payload_hash,record_json"},
	{"team_queue_requests", "task_id,request_id,payload_hash,result_json"},
}

func (f *retentionFixture) tables() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, table := range retentionTables {
		out[table[0]] = f.tableRows(table[0], table[1])
	}
	return out
}

// a1: a turn is stored once, and only a superseded revision is kept.
func TestRetentionTurnStoredOnceAndSupersededRevisionKept(t *testing.T) {
	f := newRetentionFixture(t)
	first := f.turn("turn-one")
	first.Complete = false
	if _, err := f.report(usageBatch(f.meter, "batch-one", first)); err != nil {
		t.Fatal(err)
	}
	key := []any{f.task.ID, f.meter.ID, f.meter.RunID, first.ID}
	const where = ` WHERE task_id=? AND agent_id=? AND run_id=? AND request_id=?`
	if got := f.text(`SELECT payload FROM usage_turns`+where, key...); got != "" {
		t.Fatalf("a new turn stored a %d-byte payload", len(got))
	}
	if n := f.count(`SELECT count(*) FROM usage_turn_revisions`); n != 0 {
		t.Fatalf("a new turn stored %d revision rows", n)
	}
	// The projection is what the hub built before this change: the turn as
	// reported, resolved the same way.
	firstProjection := f.text(`SELECT projection FROM usage_turns`+where, key...)
	var stored api.UsageProjection
	if err := json.Unmarshal([]byte(firstProjection), &stored); err != nil {
		t.Fatal(err)
	}
	retentionSame(t, "projection.turn is not the reported turn", retentionJSON(t, stored.Turn), retentionJSON(t, first))
	retentionSame(t, "projection does not re-encode to its stored bytes", retentionJSON(t, stored), []byte(firstProjection))

	second := retentionRevised(first)
	second.Complete = true
	if _, err := f.report(usageBatch(f.meter, "batch-two", second)); err != nil {
		t.Fatal(err)
	}
	if got := f.text(`SELECT payload FROM usage_turns`+where, key...); got != "" {
		t.Fatalf("a revised turn stored a %d-byte payload", len(got))
	}
	if n := f.count(`SELECT revision FROM usage_turns`+where, key...); n != 2 {
		t.Fatalf("current revision %d", n)
	}
	stored = api.UsageProjection{}
	if err := json.Unmarshal([]byte(f.text(`SELECT projection FROM usage_turns`+where, key...)), &stored); err != nil {
		t.Fatal(err)
	}
	retentionSame(t, "revised projection.turn is not the revised turn", retentionJSON(t, stored.Turn), retentionJSON(t, second))
	// Exactly the superseded revision, as usage_turns held it.
	if n := f.count(`SELECT count(*) FROM usage_turn_revisions`); n != 1 {
		t.Fatalf("%d revision rows after one revision", n)
	}
	if n := f.count(`SELECT revision FROM usage_turn_revisions`+where, key...); n != 1 {
		t.Fatalf("kept revision %d, want the superseded revision 1", n)
	}
	if got := f.text(`SELECT payload FROM usage_turn_revisions`+where, key...); got != "" {
		t.Fatalf("superseded revision stored a %d-byte payload", len(got))
	}
	retentionSame(t, "superseded projection changed", []byte(f.text(`SELECT projection FROM usage_turn_revisions`+where, key...)), []byte(firstProjection))

	// The same revision again changes nothing; an out-of-order one conflicts.
	before := f.tables()
	if _, err := f.report(usageBatch(f.meter, "batch-three", second)); err != nil {
		t.Fatal(err)
	}
	after := f.tables()
	for _, table := range []string{"usage_turns", "usage_turn_revisions"} {
		if before[table] != after[table] {
			t.Fatalf("%s changed on an unchanged turn: %s, then %s", table, before[table], after[table])
		}
	}
	if _, err := f.report(usageBatch(f.meter, "batch-four", first)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("an older revision was accepted", err)
	}
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
	if err != nil || report.Summary.Requests != 1 || report.Summary.Tokens["output"] != "9" {
		t.Fatalf("report after a revision: %d requests, %v", report.Summary.Requests, err)
	}
}

// a2: the receipt keeps the hash of the batch, and a retry is judged by it.
func TestRetentionUsageReceiptStoresBatchHash(t *testing.T) {
	f := newRetentionFixture(t)
	batch := usageBatch(f.meter, "batch-one", f.turn("turn-one"))
	receipt, err := f.report(batch)
	if err != nil {
		t.Fatal(err)
	}
	stored := f.text(`SELECT payload FROM usage_receipts WHERE task_id=? AND request_id=?`, f.task.ID, batch.RequestID)
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(stored) {
		t.Fatalf("stored receipt payload is %d bytes, not sha256: and 64 hex characters", len(stored))
	}
	raw := retentionJSON(t, batch)
	if stored != "sha256:"+retentionSum(raw) {
		t.Fatal("stored hash is not the SHA-256 of the batch bytes")
	}
	again, err := f.report(batch)
	if err != nil || again != receipt {
		t.Fatalf("retry: %+v, want %+v: %v", again, receipt, err)
	}
	changed := batch
	changed.Coverage = "changed"
	if _, err = f.report(changed); !errors.Is(err, api.ErrConflict) {
		t.Fatal("a changed batch under the same request ID", err)
	}
	other := f.meter
	other.RunID = api.NewID("run")
	moved := batch
	moved.RunID = other.RunID
	if _, err = f.report(moved); !errors.Is(err, api.ErrConflict) {
		t.Fatal("the same request ID from another run", err)
	}
	if n := f.count(`SELECT count(*) FROM usage_receipts`); n != 1 {
		t.Fatalf("%d receipts", n)
	}
}

// a3: a release receipt names its check lists, each stored once, and a retry
// returns the job the first reply carried.
func TestRetentionReleaseReceiptStoresCheckListsOnce(t *testing.T) {
	f := newRetentionFixture(t)
	req, first := f.imported()
	if len(first.Plan.Checks) != 2 || first.IntegratedPlan == nil || len(first.IntegratedPlan.Checks) != 2 || first.IntegratedVerification == nil || len(first.IntegratedVerification.Checks) != 2 {
		t.Fatal("fixture job does not hold three check lists")
	}
	raw := f.text(`SELECT record_json FROM release_action_receipts WHERE task_id=? AND request_id=?`, f.task.ID, req.RequestID)
	var record struct {
		Plan struct {
			Checks json.RawMessage `json:"checks"`
		} `json:"plan"`
		IntegratedPlan struct {
			Checks json.RawMessage `json:"checks"`
		} `json:"integratedPlan"`
		IntegratedVerification struct {
			Checks json.RawMessage `json:"checks"`
		} `json:"integratedVerification"`
		CheckLists map[string]string `json:"checkLists"`
	}
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]json.RawMessage{"plan": record.Plan.Checks, "integratedPlan": record.IntegratedPlan.Checks, "integratedVerification": record.IntegratedVerification.Checks} {
		if string(list) != "[]" {
			t.Fatalf("stored %s check list is %d bytes, not empty", name, len(list))
		}
	}
	want := map[string]string{
		"plan":                   retentionSum(retentionJSON(t, first.Plan.Checks)),
		"integratedPlan":         retentionSum(retentionJSON(t, first.IntegratedPlan.Checks)),
		"integratedVerification": retentionSum(retentionJSON(t, first.IntegratedVerification.Checks)),
	}
	if len(record.CheckLists) != 3 {
		t.Fatalf("%d digests", len(record.CheckLists))
	}
	for name, digest := range want {
		if record.CheckLists[name] != digest {
			t.Fatalf("%s digest is not the SHA-256 of its list", name)
		}
		body := f.text(`SELECT body FROM release_check_lists WHERE digest=?`, digest)
		if retentionSum([]byte(body)) != digest {
			t.Fatalf("stored %s list does not hash to its digest", name)
		}
	}
	if strings.Contains(raw, retentionMarkerCheck) {
		t.Fatal("the stored record still holds a check list")
	}
	// The plan and the integrated plan carry the same list: two distinct lists.
	if want["plan"] != want["integratedPlan"] || want["plan"] == want["integratedVerification"] {
		t.Fatal("fixture lists are not two distinct lists")
	}
	if n := f.count(`SELECT count(*) FROM release_check_lists WHERE digest IN (?,?)`, want["plan"], want["integratedVerification"]); n != 2 {
		t.Fatalf("%d stored lists for two distinct lists", n)
	}
	// The enqueue and claim receipts before it shared one more list, the
	// plan's as it then stood, also stored once.
	distinct := f.count(`SELECT count(*) FROM release_check_lists`)
	if distinct != 3 || f.count(`SELECT count(*) FROM release_action_receipts`) != 3 {
		t.Fatalf("%d stored lists after three receipts", distinct)
	}
	again := f.release(req)
	retentionSame(t, "replayed job differs from the first reply", retentionJSON(t, first), retentionJSON(t, again))
	// A later action with the same lists stores no list again.
	merged := api.ReleaseRequest{RequestID: "release-merged", Operation: "merged", AgentID: f.deployer.ID, RunID: f.deployer.RunID, JobID: first.ID, ExpectedGeneration: first.Generation, IntegratedCommit: candidateA}
	next := f.release(merged)
	if n := f.count(`SELECT count(*) FROM release_check_lists`); n != distinct {
		t.Fatalf("%d stored lists after a second receipt", n)
	}
	retentionSame(t, "second replay differs from its first reply", retentionJSON(t, next), retentionJSON(t, f.release(merged)))
	// A changed request under the same ID is still refused.
	changed := req
	changed.IntegratedCommit = candidateB
	if _, err := f.s.ReleaseAction(f.ctx, f.task.ID, changed); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed retry", err)
	}
	// The release job itself is stored as before, lists included.
	saved, err := releaseLoad(f.ctx, f.s.db, f.task.ID, first.ID)
	if err != nil || len(saved.Plan.Checks) != 2 || len(saved.IntegratedVerification.Checks) != 2 {
		t.Fatal("the release job lost its check lists", err)
	}
}

// A job without check lists is stored as it always was, and a nil list stays
// null rather than becoming an empty array.
func TestRetentionReleaseRecordWithoutListsIsUnchanged(t *testing.T) {
	j := api.ReleaseJob{ID: "rel_fixture", TaskID: "tsk_fixture", State: "queued", IntegratedPlan: &api.VerificationPlan{}}
	record, lists, err := compactReleaseJob(j)
	if err != nil || len(lists) != 0 {
		t.Fatal(len(lists), err)
	}
	retentionSame(t, "a job without lists is not stored as the job", record, retentionJSON(t, j))
	if !bytes.Contains(record, []byte(`"checks":null`)) || bytes.Contains(record, []byte("checkLists")) {
		t.Fatal("a nil check list did not stay null")
	}
	empty := j
	empty.Plan.Checks = []api.VerificationCheck{}
	record, lists, err = compactReleaseJob(empty)
	if err != nil || len(lists) != 0 {
		t.Fatal(len(lists), err)
	}
	retentionSame(t, "a job with an empty list is not stored as the job", record, retentionJSON(t, empty))
}

// Compacting leaves the caller's job alone, and rebuilding gives it back.
func TestRetentionReleaseCompactAndRebuild(t *testing.T) {
	f := newRetentionFixture(t)
	checks := retentionMarkerChecks()
	plan := api.VerificationPlan{ItemID: "wi_fixture", Checks: checks}
	receipt := passingVerification(plan)
	integrated := plan
	j := api.ReleaseJob{ID: "rel_fixture", TaskID: f.task.ID, State: "claimed", Plan: plan, IntegratedPlan: &integrated, IntegratedVerification: &receipt}
	full := retentionJSON(t, j)
	record, lists, err := compactReleaseJob(j)
	if err != nil || len(lists) != 3 {
		t.Fatal(len(lists), err)
	}
	retentionSame(t, "compacting changed the caller's job", full, retentionJSON(t, j))
	if bytes.Contains(record, []byte(retentionMarkerCheck)) || len(record) >= len(full) {
		t.Fatalf("compact record is %d bytes of %d", len(record), len(full))
	}
	// A named list that is not stored is an error, never an empty list.
	if _, err = rebuildReleaseJob(f.ctx, f.s.db, record); err == nil {
		t.Fatal("a record whose lists are not stored was rebuilt")
	} else if strings.Contains(err.Error(), retentionMarkerCheck) {
		t.Fatal("the error carries check list content")
	}
	tx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 2; i++ {
		if err = insertReleaseCheckLists(f.ctx, tx, lists); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := rebuildReleaseJob(f.ctx, f.s.db, record)
	if err != nil {
		t.Fatal(err)
	}
	retentionSame(t, "rebuilt job differs", full, retentionJSON(t, rebuilt))
	// An old-form record is returned as it is.
	old, err := rebuildReleaseJob(f.ctx, f.s.db, full)
	if err != nil {
		t.Fatal(err)
	}
	retentionSame(t, "old-form record differs", full, retentionJSON(t, old))
}

// The stored lists are as immutable as the receipts that name them, and the
// receipts keep their own protection.
func TestRetentionReleaseCheckListsAreImmutable(t *testing.T) {
	f := newRetentionFixture(t)
	f.imported()
	for _, statement := range []string{
		`UPDATE release_check_lists SET body='[]'`,
		`DELETE FROM release_check_lists`,
		`UPDATE release_action_receipts SET record_json='{}'`,
		`DELETE FROM release_action_receipts`,
	} {
		if _, err := f.s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s: %v", strings.Fields(statement)[0], err)
		}
	}
	if n := f.count(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name IN ('release_check_lists','release_action_receipts')`); n != 4 {
		t.Fatalf("%d triggers", n)
	}
}

// a4: a queue request result is stored without its launch plan and with the
// plan's hash; a retried claim returns the first reply.
func TestRetentionQueueResultStoredWithoutLaunch(t *testing.T) {
	f := newRetentionFixture(t)
	claim, freeze, claimed, frozen := f.freeze()
	if len(frozen.LaunchJSON) == 0 || len(claimed.LaunchJSON) != 0 {
		t.Fatal("fixture replies: the freeze carries the plan, the claim none")
	}
	const result = `SELECT result_json FROM team_queue_requests WHERE task_id=? AND request_id=?`
	const digest = `SELECT launch_digest FROM team_queue_requests WHERE task_id=? AND request_id=?`
	stored := f.text(result, f.task.ID, freeze.RequestID)
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stored), &members); err != nil {
		t.Fatal(err)
	}
	if _, ok := members["launch"]; ok || strings.Contains(stored, retentionMarkerLaunch) {
		t.Fatal("the stored result still holds the launch plan")
	}
	// The hash is of the launch member's bytes as the full result held them.
	var full map[string]json.RawMessage
	if err := json.Unmarshal(retentionJSON(t, frozen), &full); err != nil {
		t.Fatal(err)
	}
	if got := f.text(digest, f.task.ID, freeze.RequestID); got != retentionSum(full["launch"]) || len(got) != 64 {
		t.Fatal("launch_digest is not the SHA-256 of the launch plan")
	}
	// A result that never had a plan stores no hash.
	for _, key := range []string{"add", claim.RequestID} {
		if got := f.text(digest, f.task.ID, key); got != "" {
			t.Fatalf("%s stored a launch hash without a launch plan", key)
		}
	}
	retentionSame(t, "retried claim differs from the first reply", retentionJSON(t, claimed), retentionJSON(t, f.queue(claim)))
	retentionSame(t, "retried freeze differs from the first reply", retentionJSON(t, frozen), retentionJSON(t, f.queue(freeze)))
	// A changed request under the same ID is still refused.
	changed := freeze
	changed.LaunchJSON = []byte(`{"changed":true}`)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, changed); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed retry", err)
	}
}

// a4: a retried manual reservation returns the first reply.
func TestRetentionQueueManualRetryIsIdentical(t *testing.T) {
	f := newRetentionFixture(t)
	manual := api.TeamQueueRequest{RequestID: "manual-" + f.task.ID + "-" + f.items[0].ID + "-" + fmt.Sprint(f.orders[0].Seq), Operation: "manual", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq}
	first := f.queue(manual)
	retentionSame(t, "retried manual differs from the first reply", retentionJSON(t, first), retentionJSON(t, f.queue(manual)))
	f.reopen()
	retentionSame(t, "retried manual differs after a restart", retentionJSON(t, first), retentionJSON(t, f.queue(manual)))
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(f.text(`SELECT result_json FROM team_queue_requests WHERE task_id=? AND request_id=?`, f.task.ID, manual.RequestID)), &members); err != nil {
		t.Fatal(err)
	}
	if _, ok := members["launch"]; ok {
		t.Fatal("the stored manual result holds a launch plan")
	}
}

// K8: once a launch attempt has rewritten the plan, a retry still succeeds. A
// claim, which never carried a plan, is the first reply; a freeze carries the
// plan the entry holds now.
func TestRetentionQueueRetryAfterAttemptCarriesCurrentPlan(t *testing.T) {
	f := newRetentionFixture(t)
	claim, freeze, claimed, frozen := f.freeze()
	attempted := f.queue(api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: frozen.ID, ExpectedRevision: frozen.Revision})
	if bytes.Equal(attempted.LaunchJSON, frozen.LaunchJSON) || fmt.Sprint(launchMemberStates(t, attempted)) != "[uncertain]" {
		t.Fatal("the attempt did not rewrite the plan")
	}
	again, err := f.s.TeamQueueAction(f.ctx, f.task.ID, claim)
	if err != nil {
		t.Fatal("retried claim after an attempt", err)
	}
	retentionSame(t, "retried claim after an attempt differs from the first reply", retentionJSON(t, claimed), retentionJSON(t, again))
	replayed, err := f.s.TeamQueueAction(f.ctx, f.task.ID, freeze)
	if err != nil {
		t.Fatal("retried freeze after an attempt", err)
	}
	current, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	retentionSame(t, "retried freeze does not carry the entry's current plan", current.LaunchJSON, replayed.LaunchJSON)
	// The rest of the reply is the stored one.
	replayed.LaunchJSON = frozen.LaunchJSON
	retentionSame(t, "retried freeze changed outside its plan", retentionJSON(t, frozen), retentionJSON(t, replayed))
}

// K9: a stored hash whose entry no longer has a plan is replied without one,
// never as an error.
func TestRetentionQueueRetryWithoutPlanIsNotAnError(t *testing.T) {
	f := newRetentionFixture(t)
	_, freeze, _, frozen := f.freeze()
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET launch_json='' WHERE id=?`, frozen.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := f.s.TeamQueueAction(f.ctx, f.task.ID, freeze)
	if err != nil || len(replayed.LaunchJSON) != 0 {
		t.Fatalf("retry without a plan: %d plan bytes, %v", len(replayed.LaunchJSON), err)
	}
	frozen.LaunchJSON = nil
	retentionSame(t, "retry without a plan changed the reply", retentionJSON(t, frozen), retentionJSON(t, replayed))
}

// The hash column reaches a database created before it existed.
func TestRetentionQueueColumnAddedToExistingDatabase(t *testing.T) {
	f := newRetentionFixture(t)
	f.queue(f.addRequest())
	before := f.tableRows("team_queue_requests", "task_id,request_id,payload_hash,result_json")
	if _, err := f.s.db.Exec(`ALTER TABLE team_queue_requests DROP COLUMN launch_digest`); err != nil {
		t.Fatal(err)
	}
	f.reopen()
	if n := f.count(`SELECT count(*) FROM pragma_table_info('team_queue_requests') WHERE name='launch_digest'`); n != 1 {
		t.Fatal("launch_digest was not added")
	}
	if after := f.tableRows("team_queue_requests", "task_id,request_id,payload_hash,result_json"); after != before {
		t.Fatalf("existing rows changed: %s, then %s", before, after)
	}
	if got := f.text(`SELECT launch_digest FROM team_queue_requests WHERE request_id='add'`); got != "" {
		t.Fatal("an existing row gained a hash")
	}
	f.reopen()
}

// a5: a database holding old-form rows of all five kinds behaves as it did,
// and opening it changes none of them.
func TestRetentionOldFormDatabaseBehavesTheSame(t *testing.T) {
	f := newRetentionFixture(t)
	turn := f.turn("turn-one")
	turn.Complete = false
	batch := usageBatch(f.meter, "batch-one", turn)
	receipt, err := f.report(batch)
	if err != nil {
		t.Fatal(err)
	}
	claim, freeze, claimed, frozen := f.freeze()
	req, job := f.imported()

	// Rewrite what the hub just stored into the form the previous hub wrote.
	// Turn: the turn also as the payload, and the current revision also in
	// usage_turn_revisions.
	var projection api.UsageProjection
	rawProjection := f.text(`SELECT projection FROM usage_turns WHERE request_id=?`, turn.ID)
	if err = json.Unmarshal([]byte(rawProjection), &projection); err != nil {
		t.Fatal(err)
	}
	payload := string(retentionJSON(t, projection.Turn))
	if _, err = f.s.db.Exec(`UPDATE usage_turns SET payload=? WHERE request_id=?`, payload, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`INSERT INTO usage_turn_revisions VALUES(?,?,?,?,?,?,?)`, f.task.ID, f.meter.ID, f.meter.RunID, turn.ID, 1, payload, rawProjection); err != nil {
		t.Fatal(err)
	}
	// Receipt: the raw batch.
	if _, err = f.s.db.Exec(`UPDATE usage_receipts SET payload=? WHERE request_id=?`, string(retentionJSON(t, batch)), batch.RequestID); err != nil {
		t.Fatal(err)
	}
	// Release receipt: the whole job, lists included. Receipts cannot be
	// rewritten, so the old-form row is a retry identity of its own.
	oldRelease := req
	oldRelease.RequestID = "release-import-old-form"
	fullJob := retentionJSON(t, job)
	if _, err = f.s.db.Exec(`INSERT INTO release_action_receipts VALUES(?,?,?,?)`, f.task.ID, oldRelease.RequestID, verificationDigest(oldRelease), string(fullJob)); err != nil {
		t.Fatal(err)
	}
	// Queue request: the whole entry, launch plan included, and no hash.
	fullEntry := retentionJSON(t, frozen)
	if _, err = f.s.db.Exec(`UPDATE team_queue_requests SET result_json=?,launch_digest='' WHERE request_id=?`, fullEntry, freeze.RequestID); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(fullEntry, []byte(`"launch":`)) || !bytes.Contains(fullJob, []byte(retentionMarkerCheck)) {
		t.Fatal("old-form fixture rows are not in the old form")
	}

	// Opening the database changes no row of the five tables.
	before := f.tables()
	f.reopen()
	f.reopen()
	after := f.tables()
	for _, table := range retentionTables {
		if before[table[0]] != after[table[0]] {
			t.Fatalf("%s changed on open: %s, then %s", table[0], before[table[0]], after[table[0]])
		}
		if strings.HasPrefix(before[table[0]], "0 rows") {
			t.Fatalf("%s holds no old-form row", table[0])
		}
	}

	// Batch retry, against the raw stored batch.
	again, err := f.report(batch)
	if err != nil || again != receipt {
		t.Fatalf("old-form batch retry: %+v %v", again, err)
	}
	changed := batch
	changed.Coverage = "changed"
	if _, err = f.report(changed); !errors.Is(err, api.ErrConflict) {
		t.Fatal("old-form changed batch", err)
	}
	// Release replay, from the full stored job.
	replayed := f.release(oldRelease)
	retentionSame(t, "old-form release replay differs", fullJob, retentionJSON(t, replayed))
	// Queue replay, from the full stored entry: the plan as it was stored,
	// even after the entry's plan has moved on.
	f.queue(api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: frozen.ID, ExpectedRevision: frozen.Revision})
	retentionSame(t, "old-form queue replay differs", fullEntry, retentionJSON(t, f.queue(freeze)))
	retentionSame(t, "claim replay differs", retentionJSON(t, claimed), retentionJSON(t, f.queue(claim)))
	// None of those reads changed a row of the three tables they read.
	for _, table := range []string{"usage_receipts", "release_action_receipts"} {
		if got := f.tableRows(table, map[string]string{"usage_receipts": retentionTables[2][1], "release_action_receipts": retentionTables[3][1]}[table]); got != before[table] {
			t.Fatalf("%s changed on a replay: %s, then %s", table, before[table], got)
		}
	}

	// Turn revision: the same turn is skipped, the next revision accepted,
	// and the old revision row keeps its bytes.
	if _, err = f.report(usageBatch(f.meter, "batch-same", turn)); err != nil {
		t.Fatal("old-form unchanged turn", err)
	}
	if got := f.tableRows("usage_turns", retentionTables[0][1]); got != before["usage_turns"] {
		t.Fatal("an unchanged old-form turn was rewritten")
	}
	revised := retentionRevised(turn)
	revised.Complete = true
	if _, err = f.report(usageBatch(f.meter, "batch-revised", revised)); err != nil {
		t.Fatal("old-form turn revision", err)
	}
	if got := f.tableRows("usage_turn_revisions", retentionTables[1][1]); got != before["usage_turn_revisions"] {
		t.Fatalf("the old revision row changed: %s, then %s", before["usage_turn_revisions"], got)
	}
	if n := f.count(`SELECT revision FROM usage_turns WHERE request_id=?`, turn.ID); n != 2 {
		t.Fatalf("current revision %d", n)
	}
	if got := f.text(`SELECT payload FROM usage_turns WHERE request_id=?`, turn.ID); got != "" {
		t.Fatal("a revised turn kept a payload")
	}
	skipped := revised
	skipped.Revision = 5
	if _, err = f.report(usageBatch(f.meter, "batch-skipped", skipped)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("a skipped revision was accepted", err)
	}
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
	if err != nil || report.Summary.Requests != 1 || report.Summary.Tokens["output"] != "9" {
		t.Fatalf("report: %d requests, %v", report.Summary.Requests, err)
	}
}

// a8: storing, replaying and refusing all five kinds writes no marker string
// to the hub log, to standard output or error, or into an error.
func TestRetentionOutputCarriesNoMarker(t *testing.T) {
	dir := t.TempDir()
	capture := func(name string) *os.File {
		file, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	stdout, stderr := capture("stdout"), capture("stderr")
	var hubLog bytes.Buffer
	oldOut, oldErr, oldLog := os.Stdout, os.Stderr, log.Writer()
	os.Stdout, os.Stderr = stdout, stderr
	log.SetOutput(&hubLog)
	restore := func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		log.SetOutput(oldLog)
	}
	defer restore()

	var refusals []string
	refused := func(err error) {
		if err != nil {
			refusals = append(refusals, err.Error())
		}
	}
	f := newRetentionFixture(t)
	turn := f.turn("turn-one")
	turn.Complete = false
	batch := usageBatch(f.meter, "batch-one", turn)
	_, err := f.report(batch)
	refused(err)
	_, err = f.report(batch)
	refused(err)
	changed := batch
	changed.Coverage = "changed"
	_, err = f.report(changed)
	refused(err)
	_, err = f.report(usageBatch(f.meter, "batch-two", retentionRevised(turn)))
	refused(err)
	stale := retentionRevised(turn)
	stale.Revision = 5
	_, err = f.report(usageBatch(f.meter, "batch-three", stale))
	refused(err)
	claim, freeze, _, frozen := f.freeze()
	f.queue(claim)
	f.queue(freeze)
	other := freeze
	other.LaunchJSON = []byte(`{"changed":true}`)
	_, err = f.s.TeamQueueAction(f.ctx, f.task.ID, other)
	refused(err)
	f.queue(api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: frozen.ID, ExpectedRevision: frozen.Revision})
	f.queue(freeze)
	req, job := f.imported()
	f.release(req)
	conflicting := req
	conflicting.IntegratedCommit = candidateB
	_, err = f.s.ReleaseAction(f.ctx, f.task.ID, conflicting)
	refused(err)
	// A record whose lists are missing is refused without quoting it.
	record, _, err := compactReleaseJob(api.ReleaseJob{ID: job.ID, Plan: api.VerificationPlan{Checks: []api.VerificationCheck{{ID: retentionMarkerCheck + "-unstored"}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rebuildReleaseJob(f.ctx, f.s.db, record)
	refused(err)
	f.reopen()
	f.release(req)
	f.queue(freeze)

	restore()
	if len(refusals) != 5 {
		t.Fatalf("%d refusals, want 5", len(refusals))
	}
	read := func(file *os.File) string {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		return string(b)
	}
	outputs := map[string]string{"standard output": read(stdout), "standard error": read(stderr), "hub log": hubLog.String(), "errors": strings.Join(refusals, "\n")}
	for name, output := range outputs {
		for i, marker := range retentionMarkers {
			if strings.Contains(output, marker) {
				t.Fatalf("marker %d appears in %s (%d bytes)", i, name, len(output))
			}
		}
	}
	// The markers are in the database, where they belong, so the check above
	// is not vacuous.
	for i, query := range []string{
		`SELECT count(*) FROM messages WHERE text LIKE '%` + retentionMarkerBody + `%'`,
		`SELECT count(*) FROM usage_turns WHERE projection LIKE '%` + retentionMarkerTurn + `%'`,
		`SELECT count(*) FROM release_check_lists WHERE body LIKE '%` + retentionMarkerCheck + `%'`,
		`SELECT count(*) FROM team_queue_entries WHERE CAST(launch_json AS TEXT) LIKE '%` + retentionMarkerLaunch + `%'`,
	} {
		if f.count(query) == 0 {
			t.Fatalf("marker %d was never stored", i)
		}
	}
}

// a6: the usage report, the item budgets and the warnings of a fixture ledger
// hash to the values the hub gave before turns were stored once. Item 0 holds
// turns in both storage forms. The function uses only helpers the previous
// hub's tests have, so the same body runs there against the same hashes.
func TestRetentionGoldenUsageReportBudgetsAndWarnings(t *testing.T) {
	const (
		goldenReport   = "6c24ee1792a4ca259e8f302ef27bdefde6221abf040a92392c0235e322ca1d30"
		goldenBudgets  = "5f4ddcf512d18106c07061dd37e7d3b3fbd070f3eba042090f07f566de9167af"
		goldenWarnings = "72fd27769a891beacfb965430158907ea5ed92d90b815aa24143155708da945d"
	)
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	f.setEstimate(0, 1000)
	f.setEstimate(1, 5000)
	oldForm := f.use(0, 900).Turns[0]
	f.use(1, 300)
	open := f.turn(0, 200)
	open.Complete = false
	f.upload(open)
	// Store two of item 0's turns the way the previous hub stored every turn:
	// the turn also as the payload, and the current revision also as history.
	// On the previous hub these statements change nothing.
	for _, id := range []string{oldForm.ID, open.ID} {
		var revision int64
		var raw string
		if err := f.s.db.QueryRow(`SELECT revision,projection FROM usage_turns WHERE task_id=? AND request_id=?`, f.task.ID, id).Scan(&revision, &raw); err != nil {
			t.Fatal(err)
		}
		var projection api.UsageProjection
		if err := json.Unmarshal([]byte(raw), &projection); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(projection.Turn)
		if _, err := f.s.db.Exec(`UPDATE usage_turns SET payload=? WHERE task_id=? AND request_id=?`, string(payload), f.task.ID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.db.Exec(`INSERT OR IGNORE INTO usage_turn_revisions VALUES(?,?,?,?,?,?,?)`, f.task.ID, f.metered.ID, f.metered.RunID, id, revision, string(payload), raw); err != nil {
			t.Fatal(err)
		}
	}
	revise := func(turn api.UsageTurn, tokens int64) {
		turn.Revision++
		turn.Complete = true
		turn.Tokens = map[string]int64{"input": tokens, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
		turn.Raw = map[string]int64{"input_tokens": tokens, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
		f.upload(turn)
	}
	// A revision of an old-form turn, then a new turn and a revision of it,
	// which takes item 0 past its warning level.
	revise(open, 350)
	later := f.turn(0, 100)
	later.Complete = false
	f.upload(later)
	revise(later, 400)
	f.use(0, 50)
	var turns, stored int
	if err := f.s.db.QueryRow(`SELECT count(*),count(nullif(payload,'')) FROM usage_turns WHERE task_id=?`, f.task.ID).Scan(&turns, &stored); err != nil {
		t.Fatal(err)
	}
	if turns != 5 || stored < 1 {
		t.Fatalf("%d turns, %d of them with a stored payload", turns, stored)
	}

	// Identifiers and clock times differ on every run: name each one.
	names := []string{f.task.ID, "TASK", f.items[0].ID, "ITEM-0", f.items[1].ID, "ITEM-1", f.entry, "ENTRY",
		f.handler.ID, "HANDLER", f.handler.RunID, "HANDLER-RUN", f.metered.ID, "MEMBER", f.metered.RunID, "MEMBER-RUN",
		f.lead.ID, "LEAD", f.lead.RunID, "LEAD-RUN", f.helper.ID, "HELPER", f.helper.RunID, "HELPER-RUN"}
	replacer := strings.NewReplacer(names...)
	clock := regexp.MustCompile(`"20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z"`)
	unnamed := regexp.MustCompile(`[a-z]+_[0-9a-f]{16}`)
	digest := func(what string, v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		text := clock.ReplaceAllStringFunc(replacer.Replace(string(raw)), func(at string) string {
			if strings.HasPrefix(at, `"2026-09-27T`) {
				return at // a synthetic turn's own time
			}
			return `"CLOCK"`
		})
		if n := len(unnamed.FindAllString(text, -1)); n != 0 {
			t.Fatalf("%s holds %d identifiers the test did not name", what, n)
		}
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])
	}
	report, err := f.s.Usage(ctx, f.task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 2 || report.Summary.Requests != 5 {
		t.Fatalf("report holds %d items and %d requests", len(report.Items), report.Summary.Requests)
	}
	// Report rows come in identifier order; put them in fixture order.
	sort.Slice(report.Items, func(i, j int) bool { return report.Items[i].Title < report.Items[j].Title })
	sort.Slice(report.Coverage, func(i, j int) bool {
		return replacer.Replace(report.Coverage[i].AgentID) < replacer.Replace(report.Coverage[j].AgentID)
	})
	budgets := []*api.TokenBudget{}
	for _, item := range f.items {
		saved, err := f.s.GetWorkItem(ctx, f.task.ID, item.ID)
		if err != nil || saved.Budget == nil {
			t.Fatal("item budget", err)
		}
		budgets = append(budgets, saved.Budget)
	}
	warnings, err := f.s.UsageWarnings(ctx, f.task.ID)
	if err != nil || len(warnings.Warnings) != 1 {
		t.Fatal("warnings", len(warnings.Warnings), err)
	}
	for _, golden := range []struct{ what, want, got string }{
		{"usage report", goldenReport, digest("usage report", report)},
		{"item budgets", goldenBudgets, digest("item budgets", budgets)},
		{"warnings", goldenWarnings, digest("warnings", warnings)},
	} {
		if golden.got != golden.want {
			t.Errorf("%s sha256 %s, want %s", golden.what, golden.got, golden.want)
		}
	}
}

// a1: the projection stored for a turn, and for a revision of it, hashes to
// what the previous hub stored for the same input. The function uses only
// helpers the previous hub's tests have, so the same body runs there.
func TestRetentionTurnProjectionGolden(t *testing.T) {
	const (
		goldenFirst   = "f9819fc5648adf2591458cca14789db9d2843459691c6b81671a592cb1a2215f"
		goldenRevised = "7772f8395eb4fe907dc3919e4b5f66cf6134df6f02c4337bd86ed5b738870572"
	)
	s, task, a, items, orders := usageFixture(t)
	ctx := context.Background()
	replacer := strings.NewReplacer(task.ID, "TASK", a.ID, "AGENT", a.RunID, "RUN", items[0].ID, "ITEM-0", items[1].ID, "ITEM-1")
	unnamed := regexp.MustCompile(`[a-z]+_[0-9a-f]{16}`)
	stored := func(what, want string) {
		t.Helper()
		var raw string
		if err := s.db.QueryRow(`SELECT projection FROM usage_turns WHERE task_id=? AND request_id='golden'`, task.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		text := replacer.Replace(raw)
		if n := len(unnamed.FindAllString(text, -1)); n != 0 {
			t.Fatalf("%s projection holds %d identifiers the test did not name", what, n)
		}
		sum := sha256.Sum256([]byte(text))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s projection sha256 %s, want %s", what, got, want)
		}
	}
	turn := syntheticUsageTurn("golden")
	turn.Complete = false
	turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: orders[0].Seq, Operation: "ack", At: turn.At}}
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "golden-one", turn)); err != nil {
		t.Fatal(err)
	}
	stored("first", goldenFirst)
	turn.Revision, turn.Complete = 2, true
	turn.Tokens = map[string]int64{"input": 21, "cached": 80, "cacheWrite": 0, "output": 9, "reasoning": 2}
	turn.Raw = map[string]int64{"input_tokens": 101, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 11, "reasoning_output_tokens": 2}
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "golden-two", turn)); err != nil {
		t.Fatal(err)
	}
	stored("revised", goldenRevised)
}

// a7: the previous hub reads a database this hub wrote. With
// RETENTION_PREVIOUS_HUB_DB unset the test writes a ledger, restarts and
// compares. With it set, RETENTION_PREVIOUS_HUB_MODE=write leaves the ledger
// at that path, and =read opens the file found there; each prints only the
// SHA-256 of the usage report, which the two must agree on. The function uses
// only helpers the previous hub's tests have, so the read runs there.
func TestRetentionPreviousHubReadsUsage(t *testing.T) {
	ctx := context.Background()
	reportSum := func(s *Store) string {
		t.Helper()
		var task string
		if err := s.db.QueryRow(`SELECT task_id FROM usage_turns LIMIT 1`).Scan(&task); err != nil {
			t.Fatal(err)
		}
		report, err := s.Usage(ctx, task, api.UsageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if report.Summary.Requests != 4 || report.Summary.Tokens["input"] != "2001" || len(report.Items) != 2 {
			t.Fatalf("usage totals: %d requests over %d items", report.Summary.Requests, len(report.Items))
		}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	read := func(path string) string {
		t.Helper()
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		return reportSum(s)
	}
	path, mode := os.Getenv("RETENTION_PREVIOUS_HUB_DB"), os.Getenv("RETENTION_PREVIOUS_HUB_MODE")
	if path != "" && mode == "read" {
		t.Logf("usage report sha256 %s", read(path))
		return
	}
	if path != "" && mode != "write" {
		t.Fatal("RETENTION_PREVIOUS_HUB_MODE must be write or read")
	}
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	f.use(0, 900)
	f.use(1, 300)
	open := f.turn(0, 200)
	open.Complete = false
	f.upload(open)
	open.Revision, open.Complete = 2, true
	open.Tokens = map[string]int64{"input": 350, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
	open.Raw = map[string]int64{"input_tokens": 350, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
	f.upload(open)
	f.use(0, 451)
	written := reportSum(f.s)
	var seq int
	var name, file string
	if err := f.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if path == "" {
		if got := read(file); got != written {
			t.Fatalf("usage report sha256 %s after a restart, %s before", got, written)
		}
		return
	}
	// Leave the closed database at the path named.
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(file + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path+suffix, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("usage report sha256 %s", written)
}
