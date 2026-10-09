package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Storage forms that keep one copy of a document. Every reader accepts the
// older form as well; retention_migrate.go rewrites the rows stored before
// these forms, except release receipts, which are never rewritten.

// usageReceiptHashPrefix marks a usage receipt payload that holds the hash of
// the upload batch in place of the batch itself.
const usageReceiptHashPrefix = "sha256:"

// retentionHash is the lower-case hex SHA-256 of the exact bytes given.
func retentionHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// usageBatchHash is what a usage receipt stores in place of the raw batch.
func usageBatchHash(raw []byte) string {
	return usageReceiptHashPrefix + retentionHash(raw)
}

// sameUsageBatch reports whether a retried batch is the one a receipt stored,
// by hash for the new form and byte for byte for the old.
func sameUsageBatch(stored string, raw []byte) bool {
	if strings.HasPrefix(stored, usageReceiptHashPrefix) {
		return stored == usageBatchHash(raw)
	}
	return stored == string(raw)
}

// migrateRetention creates the table that holds each release check list once,
// as immutable as the receipts that name its rows, and the ledger of the
// cleanup that rewrites rows stored before these forms. The ledger holds step
// names and numbers only, in one b-tree so it takes one page.
func migrateRetention(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS release_check_lists(digest TEXT PRIMARY KEY,body TEXT NOT NULL);
 CREATE TRIGGER IF NOT EXISTS release_check_list_no_update BEFORE UPDATE ON release_check_lists BEGIN SELECT RAISE(ABORT,'immutable release check list'); END;
 CREATE TRIGGER IF NOT EXISTS release_check_list_no_delete BEFORE DELETE ON release_check_lists BEGIN SELECT RAISE(ABORT,'immutable release check list'); END;
 CREATE TABLE IF NOT EXISTS storage_migrations(
 step TEXT PRIMARY KEY,state TEXT NOT NULL,cursor INTEGER NOT NULL DEFAULT 0,
 examined INTEGER NOT NULL DEFAULT 0,converted INTEGER NOT NULL DEFAULT 0,"left" INTEGER NOT NULL DEFAULT 0,
 bytes_before INTEGER NOT NULL DEFAULT 0,bytes_after INTEGER NOT NULL DEFAULT 0,wal_peak_bytes INTEGER NOT NULL DEFAULT 0,
 freelist_before INTEGER NOT NULL DEFAULT 0,freelist_after INTEGER NOT NULL DEFAULT 0,
 started_at TEXT NOT NULL DEFAULT '',finished_at TEXT NOT NULL DEFAULT '') WITHOUT ROWID;`)
	return err
}

// releaseCheckListRefs names the digest of each check list taken out of a
// stored release receipt. An empty digest means that list was left in place.
type releaseCheckListRefs struct {
	Plan                   string `json:"plan,omitempty"`
	IntegratedPlan         string `json:"integratedPlan,omitempty"`
	IntegratedVerification string `json:"integratedVerification,omitempty"`
}

// storedReleaseJob is a release receipt's record: the job, and for the compact
// form the storage-only checkLists member.
type storedReleaseJob struct {
	api.ReleaseJob
	CheckLists *releaseCheckListRefs `json:"checkLists,omitempty"`
}

// releaseCheckList is one check list taken out of a record.
type releaseCheckList struct {
	Digest string
	Body   string
}

// compactReleaseJob returns the record a release receipt stores: the job with
// each non-empty check list replaced by an empty array and named by the
// SHA-256 of its JSON, and the lists removed. The job passed in is unchanged.
func compactReleaseJob(j api.ReleaseJob) ([]byte, []releaseCheckList, error) {
	refs := releaseCheckListRefs{}
	var lists []releaseCheckList
	take := func(checks any) (string, error) {
		body, err := json.Marshal(checks)
		if err != nil {
			return "", err
		}
		digest := retentionHash(body)
		lists = append(lists, releaseCheckList{Digest: digest, Body: string(body)})
		return digest, nil
	}
	var err error
	if len(j.Plan.Checks) > 0 {
		if refs.Plan, err = take(j.Plan.Checks); err != nil {
			return nil, nil, err
		}
		j.Plan.Checks = []api.VerificationCheck{}
	}
	if j.IntegratedPlan != nil && len(j.IntegratedPlan.Checks) > 0 {
		plan := *j.IntegratedPlan
		if refs.IntegratedPlan, err = take(plan.Checks); err != nil {
			return nil, nil, err
		}
		plan.Checks = []api.VerificationCheck{}
		j.IntegratedPlan = &plan
	}
	if j.IntegratedVerification != nil && len(j.IntegratedVerification.Checks) > 0 {
		receipt := *j.IntegratedVerification
		if refs.IntegratedVerification, err = take(receipt.Checks); err != nil {
			return nil, nil, err
		}
		receipt.Checks = []api.VerificationResult{}
		j.IntegratedVerification = &receipt
	}
	stored := storedReleaseJob{ReleaseJob: j}
	if len(lists) > 0 {
		stored.CheckLists = &refs
	}
	record, err := json.Marshal(stored)
	return record, lists, err
}

// insertReleaseCheckLists stores each list once, under its digest.
func insertReleaseCheckLists(ctx context.Context, tx *sql.Tx, lists []releaseCheckList) error {
	for _, list := range lists {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO release_check_lists VALUES(?,?)`, list.Digest, list.Body); err != nil {
			return err
		}
	}
	return nil
}

// rebuildReleaseJob returns the job a release receipt recorded, with any check
// list the record names filled back in. A record in the old form is returned
// as it is. A named list that is not stored is an error, never an empty list.
func rebuildReleaseJob(ctx context.Context, q queryRower, raw []byte) (api.ReleaseJob, error) {
	var stored storedReleaseJob
	if err := json.Unmarshal(raw, &stored); err != nil {
		return api.ReleaseJob{}, err
	}
	j := stored.ReleaseJob
	if stored.CheckLists == nil {
		return j, nil
	}
	fill := func(name, digest string, into any) error {
		var body string
		err := q.QueryRowContext(ctx, `SELECT body FROM release_check_lists WHERE digest=?`, digest).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && retentionHash([]byte(body)) != digest) {
			return fmt.Errorf("release receipt check list %s is not stored", name)
		}
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(body), into)
	}
	refs := *stored.CheckLists
	if refs.Plan != "" {
		if err := fill("plan", refs.Plan, &j.Plan.Checks); err != nil {
			return api.ReleaseJob{}, err
		}
	}
	if refs.IntegratedPlan != "" {
		if j.IntegratedPlan == nil {
			return api.ReleaseJob{}, errors.New("release receipt names a check list for a missing integratedPlan")
		}
		if err := fill("integratedPlan", refs.IntegratedPlan, &j.IntegratedPlan.Checks); err != nil {
			return api.ReleaseJob{}, err
		}
	}
	if refs.IntegratedVerification != "" {
		if j.IntegratedVerification == nil {
			return api.ReleaseJob{}, errors.New("release receipt names a check list for a missing integratedVerification")
		}
		if err := fill("integratedVerification", refs.IntegratedVerification, &j.IntegratedVerification.Checks); err != nil {
			return api.ReleaseJob{}, err
		}
	}
	return j, nil
}

// compactQueueResult returns what a team queue request stores as its result:
// the entry without its launch plan, and the SHA-256 of the launch member's
// bytes as the full result would have held them. The digest is empty when the
// entry had no launch plan.
func compactQueueResult(e api.TeamQueueEntry) ([]byte, string, error) {
	digest := ""
	if len(e.LaunchJSON) > 0 {
		launch, err := json.Marshal(e.LaunchJSON)
		if err != nil {
			return nil, "", err
		}
		digest = retentionHash(launch)
		e.LaunchJSON = nil
	}
	encoded, err := json.Marshal(e)
	return encoded, digest, err
}

// refillQueueResult puts the entry's present launch plan into a replayed
// result that was stored without one. An entry that no longer has a plan is
// replied without one.
func refillQueueResult(ctx context.Context, q queryRower, e *api.TeamQueueEntry) error {
	var launch []byte
	err := q.QueryRowContext(ctx, `SELECT launch_json FROM team_queue_entries WHERE id=?`, e.ID).Scan(&launch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	e.LaunchJSON = nil
	if len(launch) > 0 {
		e.LaunchJSON = append([]byte(nil), launch...)
	}
	return nil
}
