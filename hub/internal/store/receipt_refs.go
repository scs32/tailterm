package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// receiptRefTables names, for each saved-receipt prefix, the query that reads
// the item a receipt belongs to (wi_fc18ecc940523a0e). A typed message's
// receipt refs are checked against these tables so the Board cannot cite a
// receipt the hub never saved, or one of another item.
var receiptRefTables = map[string]string{
	"wob":  `SELECT item_id FROM work_order_bookkeeping WHERE id=?`,
	"wir":  `SELECT item_id FROM work_item_update_requests WHERE receipt_id=?`,
	"nrr":  `SELECT item_id FROM narrative_receipts WHERE id=?`,
	"nart": `SELECT item_id FROM narrative_artifacts WHERE id=?`,
	"nlnk": `SELECT item_id FROM narrative_links WHERE id=?`,
	// A release job's task may differ from the message's, so no task filter.
	"rel": `SELECT COALESCE(json_extract(record_json,'$.itemId'),'') FROM release_jobs WHERE id=?`,
}

// isReceiptRefKey reports whether a ref key names a saved receipt: "receipt"
// or any key ending in "-receipt". Keys such as receipt1 or
// receipt-generation are not checked.
func isReceiptRefKey(key string) bool {
	return key == "receipt" || strings.HasSuffix(key, "-receipt")
}

// checkReceiptRefs refuses a typed post whose receipt refs name a saved-receipt
// ID the hub does not hold, or a receipt of an item other than the message's
// linked items. Values with other prefixes are not checked. It runs inside the
// post's transaction, after request-id replay, so a resend of a message
// accepted earlier still replays.
func checkReceiptRefs(ctx context.Context, tx *sql.Tx, req api.PostMessageRequest) error {
	if req.Envelope == nil || len(req.Envelope.Refs) == 0 {
		return nil
	}
	refs := req.Envelope.Refs
	keys := make([]string, 0, len(refs))
	for key := range refs {
		if isReceiptRefKey(key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	linked := map[string]bool{}
	for _, link := range req.WorkItems {
		if link.ItemID != "" {
			linked[link.ItemID] = true
		}
	}
	if item, _, _ := strings.Cut(strings.TrimSpace(refs["item"]), "@"); api.ValidID(item, "wi") {
		linked[item] = true
	}
	linkedIDs := make([]string, 0, len(linked))
	for id := range linked {
		linkedIDs = append(linkedIDs, id)
	}
	sort.Strings(linkedIDs)
	var problems []api.Problem
	for _, key := range keys {
		value := strings.TrimSpace(refs[key])
		prefix, _, _ := strings.Cut(value, "_")
		query, ok := receiptRefTables[prefix]
		if !ok || !strings.HasPrefix(value, prefix+"_") {
			continue
		}
		missing := api.Problem{Field: "refs." + key, Reason: value + " is not a saved receipt on this hub; copy the ID from the saved readback"}
		id, version, versioned := strings.Cut(value, "@")
		n, err := strconv.Atoi(version)
		if !validNarrativeID(id, prefix) || (versioned && (err != nil || n < 1 || strconv.Itoa(n) != version)) {
			problems = append(problems, missing)
			continue
		}
		var itemID string
		err = tx.QueryRowContext(ctx, query, id).Scan(&itemID)
		if err == nil && versioned && prefix == "nart" {
			var one int
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM narrative_artifact_versions WHERE artifact_id=? AND version=?`, id, n).Scan(&one)
		}
		if errors.Is(err, sql.ErrNoRows) {
			problems = append(problems, missing)
			continue
		}
		if err != nil {
			return err
		}
		if itemID != "" && len(linked) > 0 && !linked[itemID] {
			problems = append(problems, api.Problem{Field: "refs." + key, Reason: value + " belongs to item " + itemID + ", not the linked item " + strings.Join(linkedIDs, ", ")})
		}
	}
	if len(problems) > 0 {
		return &api.EnvelopeError{Problems: problems}
	}
	return nil
}
