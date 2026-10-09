package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// retentionOldRows is what a fixture held before its rows were put back into
// the form the hub wrote before turns, batches and launch plans were stored
// once.
type retentionOldRows struct {
	turns      []api.UsageTurn
	batches    []api.UsageBatch
	receipts   []api.UsageReceipt
	payloads   map[string]string // turn ID to the old payload
	freeze     api.TeamQueueRequest
	fullEntry  []byte
	oddRequest string
	release    api.ReleaseRequest
}

// oldForm fills a fixture with rows of all four kinds the cleanup converts,
// in their old form, plus one row of each kind it must leave: a superseded
// revision, a turn whose payload differs from its projection, a queue result
// that does not re-encode to its stored bytes, and an old release receipt.
// turns is how many convertible turns to store.
func (f *retentionFixture) oldForm(turns int) retentionOldRows {
	f.t.Helper()
	old := retentionOldRows{payloads: map[string]string{}}
	exec := func(query string, args ...any) {
		f.t.Helper()
		if _, err := f.s.db.Exec(query, args...); err != nil {
			f.t.Fatal(err)
		}
	}
	for i := 0; i < turns+1; i++ {
		id := fmt.Sprintf("turn-%d", i)
		turn := f.turn(id)
		turn.Complete = false
		batch := usageBatch(f.meter, "batch-"+id, turn)
		receipt, err := f.report(batch)
		if err != nil {
			f.t.Fatal(err)
		}
		old.turns, old.batches, old.receipts = append(old.turns, turn), append(old.batches, batch), append(old.receipts, receipt)
	}
	// One turn is revised by this hub, which leaves its first revision as
	// superseded history.
	revised := retentionRevised(old.turns[turns])
	if _, err := f.report(usageBatch(f.meter, "batch-revised", revised)); err != nil {
		f.t.Fatal(err)
	}
	old.turns[turns] = revised
	for i, turn := range old.turns {
		var projection struct {
			Turn json.RawMessage `json:"turn"`
		}
		raw := f.text(`SELECT projection FROM usage_turns WHERE request_id=?`, turn.ID)
		if err := json.Unmarshal([]byte(raw), &projection); err != nil {
			f.t.Fatal(err)
		}
		payload := string(projection.Turn)
		old.payloads[turn.ID] = payload
		exec(`UPDATE usage_turns SET payload=? WHERE request_id=?`, payload, turn.ID)
		exec(`INSERT INTO usage_turn_revisions VALUES(?,?,?,?,?,?,?)`, f.task.ID, f.meter.ID, f.meter.RunID, turn.ID, turn.Revision, payload, raw)
		exec(`UPDATE usage_receipts SET payload=? WHERE request_id=?`, string(retentionJSON(f.t, old.batches[i])), old.batches[i].RequestID)
	}
	// A turn whose payload is not the bytes of its projection's turn member.
	odd := f.turn("turn-odd")
	if _, err := f.report(usageBatch(f.meter, "batch-odd", odd)); err != nil {
		f.t.Fatal(err)
	}
	old.payloads[odd.ID] = string(retentionJSON(f.t, odd)) + " "
	exec(`UPDATE usage_turns SET payload=? WHERE request_id=?`, old.payloads[odd.ID], odd.ID)

	_, freeze, _, frozen := f.freeze()
	old.freeze = freeze
	old.fullEntry = retentionJSON(f.t, frozen)
	exec(`UPDATE team_queue_requests SET result_json=?,launch_digest='' WHERE request_id=?`, old.fullEntry, freeze.RequestID)
	// The same result with a space in it decodes but does not re-encode.
	old.oddRequest = "freeze-odd"
	exec(`INSERT INTO team_queue_requests(task_id,request_id,payload_hash,result_json,launch_digest) VALUES(?,?,?,?,'')`, f.task.ID, old.oddRequest, "odd", append([]byte(" "), old.fullEntry...))

	req, job := f.imported()
	old.release = req
	old.release.RequestID = "release-import-old-form"
	exec(`INSERT INTO release_action_receipts VALUES(?,?,?,?)`, f.task.ID, old.release.RequestID, verificationDigest(old.release), string(retentionJSON(f.t, job)))
	if !bytes.Contains(old.fullEntry, []byte(`"launch":`)) {
		f.t.Fatal("the old-form queue result holds no launch plan")
	}
	// The fixture's own first open finished the cleanup on an empty database.
	exec(`DELETE FROM storage_migrations`)
	return old
}

// ledger reads one cleanup step's state and counters.
func (f *retentionFixture) ledger(step string) (state string, examined, converted, left int) {
	f.t.Helper()
	if err := f.s.db.QueryRow(`SELECT state,examined,converted,"left" FROM storage_migrations WHERE step=?`, step).Scan(&state, &examined, &converted, &left); err != nil {
		f.t.Fatal(step, err)
	}
	return
}

// retentionKeptTables are every table the cleanup reads or writes, with all
// their columns, and the ledger without its clock columns.
var retentionKeptTables = append([][2]string{
	{"release_check_lists", "digest,body"},
	{"usage_item_shares", "*"},
	{"usage_turn_totals", "*"},
	{"team_queue_entries", "*"},
}, retentionTables...)

func (f *retentionFixture) keptTables() map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, table := range retentionKeptTables {
		out[table[0]] = f.tableRows(table[0], strings.Replace(table[1], "result_json", "result_json,launch_digest", 1))
	}
	out["storage_migrations"] = retentionLedger(f.t, f.s.db, retentionLedgerCounters)
	return out
}

// retentionLedgerCounters are the ledger's columns without its clocks and
// without what depends on how the run was cut into transactions.
const retentionLedgerCounters = `step,state,cursor,examined,converted,"left",bytes_before,bytes_after`

// retentionLedger hashes the cleanup's ledger rows, in step order.
func retentionLedger(t *testing.T, db *sql.DB, columns string) string {
	t.Helper()
	h := sha256.New()
	n, err := digestRows(context.Background(), db, h, `SELECT `+columns+` FROM storage_migrations ORDER BY step`)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d rows sha256 %s", n, hex.EncodeToString(h.Sum(nil)))
}

// digest closes the fixture's store, takes the usage digest of its file and
// opens it again. Opening changes nothing once the cleanup is finished or its
// ledger is empty of unfinished steps.
func (f *retentionFixture) digest() string {
	f.t.Helper()
	file := f.path()
	if err := f.s.Close(); err != nil {
		f.t.Fatal(err)
	}
	out := retentionDigest(f.t, file)
	db, err := sql.Open("sqlite", file)
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { db.Close() })
	f.s = &Store{MaxAgents: api.MaxAgentsPerTask, db: db, waiters: map[string]chan struct{}{}, now: f.s.now}
	return out
}

func retentionDigest(t *testing.T, file string) string {
	t.Helper()
	var out bytes.Buffer
	if err := UsageDigest(context.Background(), file, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// digestLines keeps the lines of a digest that must not change: every project
// and every table.
func digestLines(digest string, kinds ...string) string {
	var out []string
	for _, line := range strings.Split(digest, "\n") {
		for _, kind := range kinds {
			if strings.HasPrefix(line, kind+" ") {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}

func retentionFileSum(t *testing.T, file string) (int64, string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(b)), retentionSum(b)
}

// b3, b5, b1 on a fixture: every converted row rebuilds to its original, rows
// that cannot be converted are unchanged and counted, release receipts are
// untouched, and the digest's project and table lines do not move.
func TestRetentionCleanupConvertsOldRowsLosslessly(t *testing.T) {
	f := newRetentionFixture(t)
	old := f.oldForm(3)
	before := f.keptTables()
	projections := map[string]string{}
	for _, turn := range old.turns {
		projections[turn.ID] = f.text(`SELECT projection FROM usage_turns WHERE request_id=?`, turn.ID)
	}
	superseded := f.tableRows("usage_turn_revisions WHERE revision=1 AND request_id='"+old.turns[3].ID+"'", "payload,projection")
	oddResult := f.tableRows("team_queue_requests WHERE request_id='"+old.oddRequest+"'", "result_json,launch_digest")
	report := func() []byte {
		t.Helper()
		r, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		return retentionJSON(t, r)
	}
	reportBefore := report()
	digestBefore := f.digest()

	if err := runRetentionCleanup(f.s.db, retentionRun{}); err != nil {
		t.Fatal(err)
	}
	after := f.keptTables()
	digestAfter := f.digest()

	// Nothing a report reads moved, and no release receipt or check list.
	for _, table := range []string{"release_action_receipts", "release_check_lists", "usage_item_shares", "usage_turn_totals", "team_queue_entries"} {
		if before[table] != after[table] {
			t.Fatalf("%s changed: %s, then %s", table, before[table], after[table])
		}
	}
	retentionSame(t, "usage report changed", reportBefore, report())
	if a, b := digestLines(digestBefore, "project", "table"), digestLines(digestAfter, "project", "table"); a != b || a == "" {
		t.Fatalf("digest project and table lines changed: sha256 %s, then %s", retentionSum([]byte(a)), retentionSum([]byte(b)))
	}
	if digestLines(digestBefore, "form") == digestLines(digestAfter, "form") {
		t.Fatal("the digest's form lines did not move")
	}

	// Turns: the payload is empty and the projection, byte for byte the one
	// stored before, still holds the old payload as its turn member.
	for _, turn := range old.turns {
		if got := f.text(`SELECT payload FROM usage_turns WHERE request_id=?`, turn.ID); got != "" {
			t.Fatalf("turn kept %d payload bytes", len(got))
		}
		var projection struct {
			Turn json.RawMessage `json:"turn"`
		}
		raw := f.text(`SELECT projection FROM usage_turns WHERE request_id=?`, turn.ID)
		if raw != projections[turn.ID] || json.Unmarshal([]byte(raw), &projection) != nil {
			t.Fatal("a projection changed")
		}
		retentionSame(t, "a turn does not rebuild", []byte(old.payloads[turn.ID]), projection.Turn)
		// The deleted revision row was the current row.
		if n := f.count(`SELECT count(*) FROM usage_turn_revisions WHERE request_id=? AND revision=?`, turn.ID, turn.Revision); n != 0 {
			t.Fatal("a copy of a current revision was kept")
		}
	}
	// The unequal turn and the superseded revision are as they were.
	if got := f.text(`SELECT payload FROM usage_turns WHERE request_id='turn-odd'`); got != old.payloads["turn-odd"] {
		t.Fatal("an unequal payload was rewritten")
	}
	if got := f.tableRows("usage_turn_revisions WHERE revision=1 AND request_id='"+old.turns[3].ID+"'", "payload,projection"); got != superseded || !strings.HasPrefix(got, "1 rows") {
		t.Fatalf("superseded revision: %s, then %s", superseded, got)
	}
	if n := f.count(`SELECT count(*) FROM usage_turn_revisions`); n != 1 {
		t.Fatalf("%d revision rows, want the superseded one", n)
	}

	// Receipts: the hash of the bytes stored before; the same batch retried
	// gets the stored receipt, a changed one a conflict.
	for i, batch := range old.batches {
		if got := f.text(`SELECT payload FROM usage_receipts WHERE request_id=?`, batch.RequestID); got != usageBatchHash(retentionJSON(t, batch)) {
			t.Fatal("a receipt does not hold the hash of its batch")
		}
		again, err := f.report(batch)
		if err != nil || again != old.receipts[i] {
			t.Fatalf("converted batch retry: %v", err)
		}
		changed := batch
		changed.Coverage = "changed"
		if _, err = f.report(changed); !errors.Is(err, api.ErrConflict) {
			t.Fatal("converted changed batch", err)
		}
	}

	// Queue result: what a new request stores, and with the entry's plan it
	// re-encodes to the bytes stored before.
	var stored []byte
	var digest string
	if err := f.s.db.QueryRow(`SELECT result_json,launch_digest FROM team_queue_requests WHERE request_id=?`, old.freeze.RequestID).Scan(&stored, &digest); err != nil {
		t.Fatal(err)
	}
	var entry api.TeamQueueEntry
	if err := json.Unmarshal(old.fullEntry, &entry); err != nil {
		t.Fatal(err)
	}
	forward, forwardDigest, err := compactQueueResult(entry)
	if err != nil || digest != forwardDigest || digest == "" {
		t.Fatal("converted queue result has another digest than a new one", err)
	}
	retentionSame(t, "converted queue result differs from a new one", forward, stored)
	if bytes.Contains(stored, []byte(`"launch"`)) {
		t.Fatal("the converted queue result holds a launch plan")
	}
	var rebuilt api.TeamQueueEntry
	if err := json.Unmarshal(stored, &rebuilt); err != nil {
		t.Fatal(err)
	}
	var launch []byte
	if err := f.s.db.QueryRow(`SELECT launch_json FROM team_queue_entries WHERE id=?`, rebuilt.ID).Scan(&launch); err != nil {
		t.Fatal(err)
	}
	rebuilt.LaunchJSON = launch
	retentionSame(t, "queue result does not rebuild", old.fullEntry, retentionJSON(t, rebuilt))
	if launch := retentionJSON(t, rebuilt.LaunchJSON); retentionHash(launch) != digest {
		t.Fatal("the launch digest is not the hash of the plan")
	}
	retentionSame(t, "queue replay differs", old.fullEntry, retentionJSON(t, f.queue(old.freeze)))
	if got := f.tableRows("team_queue_requests WHERE request_id='"+old.oddRequest+"'", "result_json,launch_digest"); got != oddResult {
		t.Fatal("a queue result that does not re-encode was rewritten")
	}

	// The ledger: every step done, with what it converted and what it left.
	for _, want := range []struct {
		step                      string
		examined, converted, left int
	}{
		{"revisions", 5, 4, 1},
		{"turn-payload", 5, 4, 1},
		{"receipt-batch", 6, 4, 0},
		{"queue-launch", f.count(`SELECT count(*) FROM team_queue_requests`), 1, 1},
	} {
		state, examined, converted, left := f.ledger(want.step)
		if state != "done" || examined != want.examined || converted != want.converted || left != want.left {
			t.Fatalf("%s: %s examined %d converted %d left %d", want.step, state, examined, converted, left)
		}
	}

	// b5: both triggers are present and still refuse.
	if n := f.count(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name='release_action_receipts'`); n != 2 {
		t.Fatalf("%d release receipt triggers", n)
	}
	for _, statement := range []string{
		`UPDATE release_action_receipts SET payload_hash='' WHERE rowid=(SELECT min(rowid) FROM release_action_receipts)`,
		`DELETE FROM release_action_receipts WHERE rowid=(SELECT min(rowid) FROM release_action_receipts)`,
	} {
		if _, err := f.s.db.Exec(statement); err == nil || !strings.Contains(err.Error(), "immutable release receipt") {
			t.Fatal("a release receipt could be changed", err)
		}
	}
	if got := f.tableRows("release_action_receipts", retentionTables[3][1]); got != before["release_action_receipts"] || strings.HasPrefix(got, "0 rows") {
		t.Fatal("release receipts changed")
	}
	// The old-form release receipt still replays whole.
	replayed := f.release(old.release)
	if !bytes.Contains(retentionJSON(t, replayed), []byte(retentionMarkerCheck)) {
		t.Fatal("the old release receipt lost its check lists")
	}
}

// retentionCopy copies a closed database file.
func retentionCopy(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// retentionRaw opens a database file without migrating it.
func retentionRaw(t *testing.T, file string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// b4: a cleanup stopped after a batch of every step and resumed ends with the
// same rows and the same ledger counters as one that ran through, and a
// further open changes nothing.
func TestRetentionCleanupResumesAfterAnInterruptedBatch(t *testing.T) {
	f := newRetentionFixture(t)
	f.oldForm(5)
	file := f.path()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	// An ordinary open records what it finds in work item history with the
	// time it found it. Record it once, before the file is copied, so the two
	// copies do not differ by when each was first opened.
	db := retentionRaw(t, file)
	if err := reconcileWorkItemHistory(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	through, stopped := filepath.Join(dir, "through.sqlite"), filepath.Join(dir, "stopped.sqlite")
	retentionCopy(t, file, through)
	retentionCopy(t, file, stopped)

	tables := func(file string) (map[string]string, string) {
		t.Helper()
		db := retentionRaw(t, file)
		g := &retentionFixture{t: t, s: &Store{db: db}}
		out := g.keptTables()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return out, digestLines(retentionDigest(t, file), "project", "table", "form")
	}

	db = retentionRaw(t, through)
	if err := runRetentionCleanup(db, retentionRun{batchRows: 2}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Both files are then opened the ordinary way, which reconciles work item
	// history whatever the cleanup did.
	s, err := Open(through)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	wantTables, wantDigest := tables(through)

	// Stop once after the first batch of each step, as a killed hub would
	// between two transactions, then open the file the ordinary way.
	stops := 0
	for _, step := range retentionSteps {
		db = retentionRaw(t, stopped)
		err := runRetentionCleanup(db, retentionRun{batchRows: 2, stop: func(name string, batch int) bool { return name == step.name && batch == 1 }})
		db.Close()
		if !errors.Is(err, errRetentionStopped) {
			t.Fatalf("%s did not stop: %v", step.name, err)
		}
		stops++
		db = retentionRaw(t, stopped)
		var state string
		var cursor int
		if err = db.QueryRow(`SELECT state,cursor FROM storage_migrations WHERE step=?`, step.name).Scan(&state, &cursor); err != nil || state != "running" || cursor == 0 {
			t.Fatalf("%s after a stop: %s cursor %d %v", step.name, state, cursor, err)
		}
		db.Close()
	}
	if stops != 4 {
		t.Fatal(stops)
	}
	if s, err = Open(stopped); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	gotTables, gotDigest := tables(stopped)
	for name, want := range wantTables {
		if gotTables[name] != want {
			t.Fatalf("%s after a resumed cleanup: %s, uninterrupted %s", name, gotTables[name], want)
		}
	}
	if gotDigest != wantDigest || gotDigest == "" {
		t.Fatalf("digest after a resumed cleanup: sha256 %s, uninterrupted %s", retentionSum([]byte(gotDigest)), retentionSum([]byte(wantDigest)))
	}
	if !strings.Contains(gotDigest, "form usage_turns new=6 old=1") || !strings.Contains(gotDigest, "form usage_receipts new=8 old=0") {
		t.Fatal("the resumed cleanup did not convert the fixture")
	}

	// A further open changes no row and no ledger counter, clocks included.
	ledger := func() string {
		t.Helper()
		db := retentionRaw(t, stopped)
		defer db.Close()
		return retentionLedger(t, db, "*")
	}
	ledgerBefore := ledger()
	for i := 0; i < 2; i++ {
		if s, err = Open(stopped); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	againTables, againDigest := tables(stopped)
	for name, want := range gotTables {
		if againTables[name] != want {
			t.Fatalf("%s changed on a further open", name)
		}
	}
	if againDigest != gotDigest || ledger() != ledgerBefore || !strings.HasPrefix(ledgerBefore, "4 rows") {
		t.Fatal("a further open changed the digest or the ledger")
	}
}

// A database that lacks the form a step converts to records the step as
// skipped and opens.
func TestRetentionCleanupSkipsAStepWhoseFormIsMissing(t *testing.T) {
	db := retentionRaw(t, filepath.Join(t.TempDir(), "bare.sqlite"))
	defer db.Close()
	if err := migrateRetention(db); err != nil {
		t.Fatal(err)
	}
	// Queue request results as an earlier hub made them: no digest column.
	if _, err := db.Exec(`CREATE TABLE team_queue_requests(task_id TEXT NOT NULL,request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,result_json BLOB NOT NULL,PRIMARY KEY(task_id,request_id));
 INSERT INTO team_queue_requests VALUES('t','r','h','{"id":"x","launch":{"a":1}}')`); err != nil {
		t.Fatal(err)
	}
	if err := runRetentionCleanup(db, retentionRun{}); err != nil {
		t.Fatal(err)
	}
	var skipped, examined int
	if err := db.QueryRow(`SELECT count(*),sum(examined) FROM storage_migrations WHERE state='skipped'`).Scan(&skipped, &examined); err != nil || skipped != 4 || examined != 0 {
		t.Fatalf("%d steps skipped, %d rows examined: %v", skipped, examined, err)
	}
	var result string
	if err := db.QueryRow(`SELECT result_json FROM team_queue_requests`).Scan(&result); err != nil || !strings.Contains(result, "launch") {
		t.Fatal("a result was rewritten without a digest column", err)
	}
	if err := runRetentionCleanup(db, retentionRun{}); err != nil {
		t.Fatal(err)
	}
}

// b8: the cleanup never compacts the file. No statement in the two store
// files names the command, and the file is as large after the cleanup as
// before it.
func TestRetentionCleanupNeverCompacts(t *testing.T) {
	command := regexp.MustCompile(`(?i)vacuum`)
	for _, name := range []string{"retention.go", "retention_migrate.go"} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if command.Match(source) {
			t.Fatalf("%s names the compaction command", name)
		}
	}
	f := newRetentionFixture(t)
	f.oldForm(40)
	file := f.path()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := retentionFileSum(t, file)
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	var free, converted int64
	if err = s.db.QueryRow(`SELECT (SELECT freelist_count FROM pragma_freelist_count),(SELECT sum(converted) FROM storage_migrations)`).Scan(&free, &converted); err != nil {
		t.Fatal(err)
	}
	var auto int
	if err = s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&auto); err != nil || auto != 0 {
		t.Fatal("the database shrinks by itself", auto, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	after, _ := retentionFileSum(t, file)
	if before != after {
		t.Fatalf("file size %d, then %d", before, after)
	}
	if converted < 100 || free == 0 {
		t.Fatalf("converted %d rows, %d free pages: the cleanup freed nothing", converted, free)
	}
}

// The digest opens the file read-only and immutable: it runs on a database in
// write-ahead mode, creates and changes no file, prints the same bytes twice,
// refuses a log that is not empty, and prints no step line for a database no
// cleanup has opened.
func TestUsageDigestIsReadOnlyAndDeterministic(t *testing.T) {
	f := newRetentionFixture(t)
	f.oldForm(2)
	file := f.path()

	// The store is open and has written: its log is not empty.
	if wal, err := os.Stat(file + "-wal"); err != nil || wal.Size() == 0 {
		t.Fatal("the fixture has no write-ahead log", err)
	}
	if err := UsageDigest(f.ctx, file, io.Discard); !errors.Is(err, ErrUsageDigestWAL) {
		t.Fatal("a database with a log was not refused", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	listing := func() string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Dir(file))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			names = append(names, fmt.Sprintf("%s %d %d", e.Name(), info.Size(), info.ModTime().UnixNano()))
		}
		return strings.Join(names, "\n")
	}
	size, sum := retentionFileSum(t, file)
	files := listing()
	first := retentionDigest(t, file)
	second := retentionDigest(t, file)
	if first != second {
		t.Fatalf("two digests of one file differ: sha256 %s, then %s", retentionSum([]byte(first)), retentionSum([]byte(second)))
	}
	if gotSize, gotSum := retentionFileSum(t, file); gotSize != size || gotSum != sum || listing() != files {
		t.Fatal("the digest changed the database or its directory")
	}
	var journal string
	db := retentionRaw(t, file)
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatal("the fixture is not in write-ahead mode", journal, err)
	}
	// No ledger yet: no step line. Then, without the table: still no error.
	if strings.Contains(first, "\nstep ") {
		t.Fatal("step lines without a ledger row")
	}
	for _, want := range []string{
		fmt.Sprintf("file size=%d ", size),
		"table release_action_receipts rows=", "table usage_turns rows=4 ", "table usage_turn_revisions.superseded rows=1 ",
		"form usage_turns new=0 old=4\n", "form usage_turn_revisions new=1 old=3\n", "form usage_receipts new=2 old=3\n",
		"form team_queue_requests new=", " old=2 neither=",
		"project " + retentionHash([]byte(f.task.ID))[:16] + " turns=4 report=",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("the digest lacks a line starting %q", want)
		}
	}
	if _, err := db.Exec(`DROP TABLE storage_migrations`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	without := retentionDigest(t, file)
	if digestLines(without, "project", "table", "form") != digestLines(first, "project", "table", "form") {
		t.Fatal("the ledger table changes the digest's other lines")
	}
	// Every line is a name with numbers and hashes.
	line := regexp.MustCompile(`^(file|table|form|step|project|export)( [a-z_.\-]*[0-9a-f]*)?( [a-z_0-9]+=[0-9a-z_.\-]+| absent)*$`)
	for _, l := range strings.Split(strings.TrimSuffix(first, "\n"), "\n") {
		if !line.MatchString(l) {
			t.Fatalf("a digest line of %d bytes is not names, numbers and hashes", len(l))
		}
	}
}

// r6 and b8: the cleanup's log lines and the digest carry no marker string,
// from a fixture that holds one in a message body, a turn, an upload batch, a
// check list and a launch plan.
func TestRetentionCleanupAndDigestCarryNoMarker(t *testing.T) {
	var hubLog bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&hubLog)
	defer log.SetOutput(oldLog)

	f := newRetentionFixture(t)
	f.oldForm(2)
	stored := 0
	for _, query := range []string{
		`SELECT count(*) FROM messages WHERE text LIKE '%` + retentionMarkerBody + `%'`,
		`SELECT count(*) FROM usage_turns WHERE payload LIKE '%` + retentionMarkerTurn + `%'`,
		`SELECT count(*) FROM usage_receipts WHERE payload LIKE '%` + retentionMarkerTurn + `%'`,
		`SELECT count(*) FROM release_action_receipts WHERE record_json LIKE '%` + retentionMarkerCheck + `%'`,
		`SELECT count(*) FROM team_queue_requests WHERE CAST(result_json AS TEXT) LIKE '%` + retentionMarkerLaunch + `%'`,
	} {
		if f.count(query) > 0 {
			stored++
		}
	}
	if stored != 5 {
		t.Fatalf("%d of 5 markers are stored", stored)
	}
	before := f.digest()
	if err := runRetentionCleanup(f.s.db, retentionRun{}); err != nil {
		t.Fatal(err)
	}
	after := f.digest()
	log.SetOutput(oldLog)
	if !strings.Contains(hubLog.String(), "storage cleanup turn-payload: examined 4, converted 3, left 1") {
		t.Fatal("the cleanup logged no step line")
	}
	sum := sha256.Sum256([]byte(f.task.ID))
	for name, output := range map[string]string{"log": hubLog.String(), "digest before": before, "digest after": after} {
		for i, marker := range append(append([]string{}, retentionMarkers...), f.task.ID, f.items[0].ID, f.meter.ID) {
			if strings.Contains(output, marker) {
				t.Fatalf("marker %d appears in the %s (%d bytes)", i, name, len(output))
			}
		}
	}
	if !strings.Contains(after, "project "+hex.EncodeToString(sum[:])[:16]+" ") {
		t.Fatal("the project line is not named by a hash of its ID")
	}
}

// retentionExportStreams hashes every stream of the audit export by the
// export's own query, with every column.
func (f *retentionFixture) exportStreams() map[string]string {
	f.t.Helper()
	queries, err := digestExportQueries(f.ctx, f.s.db)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, q := range queries {
		h := sha256.New()
		if _, err = digestRows(f.ctx, f.s.db, h, q.sql, q.args(f.task.ID)...); err != nil {
			f.t.Fatal(q.name, err)
		}
		out[q.name] = digestHex(h)
	}
	return out
}

// b2: two ordinary opens of one database, with nothing for the cleanup to
// convert, differ in the audit export only in the time of the open that
// work_item_history_state.checked_at holds. The digest's export hash leaves
// out that one column and nothing else, and says so.
func TestUsageDigestExportLeavesOutOnlyTheOpenClock(t *testing.T) {
	f := newRetentionFixture(t)
	f.reopen() // the first open after the fixture's writes settles its history
	if n := f.count(`SELECT coalesce(sum(converted),0) FROM storage_migrations`); n != 0 {
		t.Fatalf("the cleanup converted %d rows of a fixture in the new form", n)
	}
	state := func() (clocks, rest string) {
		t.Helper()
		return f.tableRows("work_item_history_state", "checked_at"), f.tableRows("work_item_history_state", "item_task_id,item_id,observed_current_revision,latest_materialized_revision,complete")
	}
	streamsBefore := f.exportStreams()
	clocksBefore, restBefore := state()
	digestBefore := f.digest()
	f.reopen()
	streamsAfter := f.exportStreams()
	clocksAfter, restAfter := state()
	digestAfter := f.digest()

	if len(streamsBefore) < 40 || len(streamsAfter) != len(streamsBefore) {
		t.Fatalf("%d export streams, then %d", len(streamsBefore), len(streamsAfter))
	}
	for name, before := range streamsBefore {
		if moved := streamsAfter[name] != before; moved != (name == usageDigestClockStream) {
			t.Fatalf("export stream %s moved between two opens: %v", name, moved)
		}
	}
	if strings.HasPrefix(clocksBefore, "0 rows") || clocksBefore == clocksAfter {
		t.Fatal("the open did not write its time to the history state")
	}
	if restBefore != restAfter {
		t.Fatal("an open changed another column of the history state")
	}
	// The digest reads that stream by every column of its table but the one
	// it names, so a column added later is not left out unnoticed.
	rows, err := f.s.db.Query(`SELECT name FROM pragma_table_info('work_item_history_state') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	omitted := 0
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if "work_item_history_state."+name == usageDigestClockOmitted {
			omitted++
			continue
		}
		kept = append(kept, name)
	}
	rows.Close()
	if want := "SELECT " + strings.Join(kept, ",") + " FROM work_item_history_state WHERE item_task_id=? ORDER BY item_id"; omitted != 1 || usageDigestClockQuery != want {
		t.Fatalf("the digest's history state query leaves out %d named columns or reads other columns than the table's", omitted)
	}
	if a, b := digestLines(digestBefore, "project", "table"), digestLines(digestAfter, "project", "table"); a != b || !strings.Contains(a, " export=") {
		t.Fatalf("digest project and table lines moved between two opens: sha256 %s, then %s", retentionSum([]byte(a)), retentionSum([]byte(b)))
	}
	if !strings.Contains(digestAfter, "\nexport omits=work_item_history_state.checked_at\n") {
		t.Fatal("the digest does not name the column its export hash leaves out")
	}
	// Any other change to the stream moves the export hash.
	if _, err = f.s.db.Exec(`UPDATE work_item_history_state SET complete=1-complete`); err != nil {
		t.Fatal(err)
	}
	if digestLines(f.digest(), "project") == digestLines(digestAfter, "project") {
		t.Fatal("the export hash does not cover the other columns of the history state")
	}
}

// The digest hashes the streams the current audit export writes. The export
// assembles its list where it is written, so this compares the digest's list
// with the streams of an export made here.
func TestUsageDigestCoversEveryAuditExportStream(t *testing.T) {
	f := newRetentionFixture(t)
	by := api.Caller{Node: "fixture", User: "owner"}
	created, err := f.s.CreateAuditExport(f.ctx, f.task.ID, api.CreateAuditExportRequest{RequestID: "digest-streams", FormatVersion: api.AuditExportFormatVersion}, by)
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	for offset := int64(0); offset < created.ByteCount; {
		chunk, err := f.s.GetAuditExportChunk(f.ctx, f.task.ID, created.ID, offset, api.MaxAuditExportChunkBytes, by)
		if err != nil || chunk.NextOffset <= offset {
			t.Fatal("audit export chunk", err)
		}
		content = append(content, chunk.Data...)
		offset = chunk.NextOffset
	}
	var export struct {
		Streams map[string]json.RawMessage `json:"streams"`
	}
	if err = json.Unmarshal(content, &export); err != nil {
		t.Fatal("the audit export does not decode", err)
	}
	queries, err := digestExportQueries(f.ctx, f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != len(export.Streams) || len(queries) < 40 {
		t.Fatalf("the digest hashes %d streams, the audit export writes %d", len(queries), len(export.Streams))
	}
	clock := 0
	for _, q := range queries {
		if _, ok := export.Streams[q.name]; !ok {
			t.Fatalf("the digest hashes stream %s, which the audit export does not write", q.name)
		}
		if q.name == usageDigestClockStream {
			clock++
		}
	}
	if clock != 1 {
		t.Fatalf("%d streams hold the open clock", clock)
	}
}
