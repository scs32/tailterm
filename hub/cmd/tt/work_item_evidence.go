package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const (
	evidenceManifestVersion = 1
	evidenceStateNotFetched = "not_fetched"
	evidenceStatePartial    = "partial"
	evidenceStateAbsent     = "absent_after_complete_lookup"
	evidenceStateError      = "error"
	evidenceStateVerified   = "verified"
)

var evidenceSourceOrder = []string{"current", "revisions", "messages", "receipt"}

type workItemEvidenceSelection struct {
	Sources          []string `json:"sources"`
	MessageRevision  int64    `json:"messageRevision,omitempty"`
	ReceiptRequestID string   `json:"receiptRequestId,omitempty"`
	ReceiptAgentID   string   `json:"receiptAgentId,omitempty"`
	PageLimit        int      `json:"pageLimit"`
}

type workItemEvidenceSnapshot struct {
	ItemRevision  int64     `json:"itemRevision,omitempty"`
	ScopeRevision int64     `json:"scopeRevision,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	CompletedAt   time.Time `json:"completedAt,omitempty"`
}

type workItemEvidencePage struct {
	Phase       string            `json:"phase,omitempty"`
	Operation   string            `json:"operation"`
	Parameters  map[string]string `json:"parameters"`
	StatusCode  int               `json:"statusCode"`
	RequestedAt time.Time         `json:"requestedAt"`
	CompletedAt time.Time         `json:"completedAt"`
	Path        string            `json:"path"`
	Bytes       int64             `json:"bytes"`
	SHA256      string            `json:"sha256"`
	NextAfter   int64             `json:"nextAfter,omitempty"`
	Valid       bool              `json:"valid"`
}

type workItemEvidenceSource struct {
	State      string                 `json:"state"`
	Operation  string                 `json:"operation"`
	Parameters map[string]string      `json:"parameters"`
	Pages      []workItemEvidencePage `json:"pages"`
	Count      int64                  `json:"count,omitempty"`
	Coverage   *api.HistoryCoverage   `json:"coverage,omitempty"`
	LastError  string                 `json:"lastError,omitempty"`
}

type workItemEvidenceManifest struct {
	Version   int                                `json:"version"`
	TaskID    string                             `json:"taskId"`
	ItemID    string                             `json:"itemId"`
	Selection workItemEvidenceSelection          `json:"selection"`
	Snapshot  workItemEvidenceSnapshot           `json:"snapshot"`
	Sources   map[string]*workItemEvidenceSource `json:"sources"`
	Complete  bool                               `json:"complete"`
	Verified  bool                               `json:"verified"`
	CreatedAt time.Time                          `json:"createdAt"`
	UpdatedAt time.Time                          `json:"updatedAt"`
}

func parseEvidenceSources(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("--sources is required; choose current and the exact evidence families needed")
	}
	selected := map[string]bool{}
	for _, source := range strings.Split(raw, ",") {
		source = strings.TrimSpace(source)
		if source == "" || selected[source] {
			return nil, fmt.Errorf("invalid or duplicate evidence source %q", source)
		}
		selected[source] = true
	}
	if !selected["current"] {
		return nil, errors.New("current is required as the item/scope revision snapshot anchor")
	}
	var out []string
	for _, source := range evidenceSourceOrder {
		if selected[source] {
			out = append(out, source)
			delete(selected, source)
		}
	}
	if len(selected) > 0 {
		unknown := make([]string, 0, len(selected))
		for source := range selected {
			unknown = append(unknown, source)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown evidence source %q", unknown[0])
	}
	return out, nil
}

func evidenceSourceParameters(source string, selection workItemEvidenceSelection) map[string]string {
	params := map[string]string{}
	switch source {
	case "revisions", "messages":
		params["limit"] = strconv.Itoa(selection.PageLimit)
	case "receipt":
		params["requestId"] = selection.ReceiptRequestID
		if selection.ReceiptAgentID != "" {
			params["agentId"] = selection.ReceiptAgentID
		}
	}
	if source == "messages" && selection.MessageRevision > 0 {
		params["revision"] = strconv.FormatInt(selection.MessageRevision, 10)
	}
	return params
}

func newWorkItemEvidenceManifest(taskID, itemID string, selection workItemEvidenceSelection, now time.Time) workItemEvidenceManifest {
	m := workItemEvidenceManifest{
		Version: evidenceManifestVersion, TaskID: taskID, ItemID: itemID,
		Selection: selection, Sources: map[string]*workItemEvidenceSource{},
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	for _, source := range selection.Sources {
		m.Sources[source] = &workItemEvidenceSource{State: evidenceStateNotFetched, Operation: source, Parameters: evidenceSourceParameters(source, selection), Pages: []workItemEvidencePage{}}
	}
	return m
}

func writePrivateAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".evidence-manifest-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func saveWorkItemEvidenceManifest(path string, manifest *workItemEvidenceManifest) error {
	manifest.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writePrivateAtomic(path, data)
}

func loadWorkItemEvidenceManifest(path string) (workItemEvidenceManifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return workItemEvidenceManifest{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return workItemEvidenceManifest{}, errors.New("evidence manifest must be a regular mode 0600 file")
	}
	file, err := os.Open(path)
	if err != nil {
		return workItemEvidenceManifest{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 32<<20))
	if err != nil {
		return workItemEvidenceManifest{}, err
	}
	var manifest workItemEvidenceManifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Version != evidenceManifestVersion || manifest.Sources == nil {
		return manifest, errors.New("unsupported or incomplete evidence manifest")
	}
	return manifest, nil
}

func evidencePageDirectory(manifestPath string) string {
	return manifestPath + ".pages"
}

func recordEvidencePage(manifestPath, source, phase string, parameters map[string]string, response api.RawWorkItemEvidence, requestedAt, completedAt time.Time, manifest *workItemEvidenceManifest) (int, error) {
	digest := sha256.Sum256(response.Body)
	hexDigest := hex.EncodeToString(digest[:])
	dir := evidencePageDirectory(manifestPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return 0, errors.New("evidence page directory must be a private directory")
	}
	entry := manifest.Sources[source]
	prefix := fmt.Sprintf("%s-%03d-%s.json-", source, len(entry.Pages)+1, hexDigest[:16])
	file, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return 0, err
	}
	path := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		_ = file.Close()
		return 0, err
	}
	_, writeErr := file.Write(response.Body)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return 0, writeErr
	}
	remove = false
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	rel, err := filepath.Rel(filepath.Dir(manifestPath), path)
	if err != nil {
		return 0, err
	}
	entry.Pages = append(entry.Pages, workItemEvidencePage{
		Phase: phase, Operation: source, Parameters: parameters, StatusCode: response.StatusCode,
		RequestedAt: requestedAt.UTC(), CompletedAt: completedAt.UTC(), Path: rel,
		Bytes: int64(len(response.Body)), SHA256: hexDigest,
	})
	return len(entry.Pages) - 1, nil
}

func verifyEvidencePages(manifestPath string, manifest *workItemEvidenceManifest) error {
	root := filepath.Clean(filepath.Dir(manifestPath))
	for source, entry := range manifest.Sources {
		for _, page := range entry.Pages {
			path := filepath.Clean(filepath.Join(root, page.Path))
			rel, err := filepath.Rel(root, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("%s evidence page escapes manifest directory", source)
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return fmt.Errorf("%s evidence page %q is missing or not a mode 0600 regular file", source, page.Path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(data)
			if int64(len(data)) != page.Bytes || hex.EncodeToString(digest[:]) != page.SHA256 {
				return fmt.Errorf("%s evidence page %q does not match its immutable size/hash", source, page.Path)
			}
		}
	}
	return nil
}

func retainedEvidencePageBody(manifestPath string, page workItemEvidencePage) ([]byte, error) {
	return os.ReadFile(filepath.Join(filepath.Dir(manifestPath), page.Path))
}

func validEvidenceState(state string) bool {
	switch state {
	case evidenceStateNotFetched, evidenceStatePartial, evidenceStateAbsent, evidenceStateError, evidenceStateVerified:
		return true
	default:
		return false
	}
}

func validFullMessage(link api.WorkItemMessageLink, itemTaskID, itemID string, itemRevision, selectedRevision, after int64, seen map[string]bool) error {
	message := link.Message
	key := message.TaskID + "/" + strconv.FormatInt(message.Seq, 10)
	if link.ItemRevision < 1 || link.ItemRevision > itemRevision || (selectedRevision > 0 && link.ItemRevision != selectedRevision) || !api.ValidID(message.TaskID, "tsk") || message.Seq <= after || seen[key] {
		return errors.New("message page contains missing, duplicate, or nonadvancing source coordinates")
	}
	if link.RevisionCoverage != "verified" && link.RevisionCoverage != "checkpoint" && link.RevisionCoverage != "gap" {
		return errors.New("message page contains an unknown native revision-coverage state")
	}
	if link.Relationship != "" && link.Relationship != "primary" && link.Relationship != "related" {
		return errors.New("message page contains an unknown native item relationship")
	}
	// Validated cross-project dispatch is intentionally part of work-item
	// history. Its original Board task differs, so the full message envelope must
	// retain the exact primary work-item association that caused the native link.
	if message.TaskID != itemTaskID {
		associated := false
		for _, item := range message.WorkItems {
			if item.ItemTaskID == itemTaskID && item.ItemID == itemID && item.ItemRevision == link.ItemRevision && item.Relationship == "primary" {
				associated = true
				break
			}
		}
		if link.Source || link.Relationship != "primary" || !associated {
			return errors.New("message page contains a foreign-task message without exact primary dispatch provenance")
		}
	}
	if message.From.Node == "" || message.From.User == "" || message.Text == "" || message.CreatedAt.IsZero() {
		return errors.New("message page does not contain the full native body/from/timestamp envelope")
	}
	if message.From.AgentID != "" && !api.ValidID(message.From.AgentID, "agt") {
		return errors.New("message page contains an invalid native author identity")
	}
	seen[key] = true
	return nil
}

// validateRetainedEvidenceManifest derives the resumable position from the raw
// retained pages and compares it with the manifest index. The index alone is
// never execution evidence and cannot be edited to skip native reads.
func validateRetainedEvidenceManifest(manifestPath string, manifest *workItemEvidenceManifest) error {
	if !api.ValidID(manifest.TaskID, "tsk") || !api.ValidID(manifest.ItemID, "wi") {
		return errors.New("evidence manifest has invalid item coordinates")
	}
	wantSources, err := parseEvidenceSources(strings.Join(manifest.Selection.Sources, ","))
	if err != nil || !reflect.DeepEqual(wantSources, manifest.Selection.Sources) || len(manifest.Sources) != len(wantSources) {
		return errors.New("evidence manifest has an invalid or noncanonical source selection")
	}
	for _, source := range wantSources {
		entry := manifest.Sources[source]
		if entry == nil || entry.Operation != source || !validEvidenceState(entry.State) || !reflect.DeepEqual(entry.Parameters, evidenceSourceParameters(source, manifest.Selection)) {
			return fmt.Errorf("evidence manifest source %s does not match its frozen selection", source)
		}
		for _, page := range entry.Pages {
			if page.Operation != source || page.RequestedAt.IsZero() || page.CompletedAt.IsZero() || page.CompletedAt.Before(page.RequestedAt) || page.StatusCode < 100 {
				return fmt.Errorf("evidence manifest source %s has invalid page metadata", source)
			}
		}
	}

	current := manifest.Sources["current"]
	var startItem *api.WorkItem
	var derivedStartedAt, derivedCompletedAt time.Time
	currentTerminal, currentError := false, false
	for index, page := range current.Pages {
		body, readErr := retainedEvidencePageBody(manifestPath, page)
		if readErr != nil {
			return readErr
		}
		if page.Phase != "start" && page.Phase != "end" {
			return errors.New("evidence manifest current page has invalid phase")
		}
		if !reflect.DeepEqual(page.Parameters, map[string]string{"phase": page.Phase}) {
			return errors.New("evidence manifest current page parameters changed")
		}
		derivedValid := false
		switch page.StatusCode {
		case http.StatusOK:
			var item api.WorkItem
			if json.Unmarshal(body, &item) == nil && item.ID == manifest.ItemID && item.TaskID == manifest.TaskID && item.Revision > 0 && item.ScopeRevision > 0 {
				derivedValid = true
				if page.Phase == "start" {
					if startItem != nil || currentTerminal {
						return errors.New("evidence manifest has an impossible current-page chain")
					}
					copy := item
					startItem = &copy
					derivedStartedAt = page.CompletedAt
				} else if startItem == nil {
					currentError = true
				} else if item.Revision == startItem.Revision && item.ScopeRevision == startItem.ScopeRevision {
					currentTerminal, currentError = true, false
					derivedCompletedAt = page.CompletedAt
				} else {
					currentError = true
				}
				if page.Phase == "start" {
					currentError = false
				}
			} else {
				currentError = true
			}
		case http.StatusNotFound:
			if page.Phase == "start" && startItem == nil && index == len(current.Pages)-1 {
				derivedValid, currentTerminal = true, true
			} else {
				currentError = true
			}
		default:
			currentError = true
		}
		if page.Valid != derivedValid {
			return errors.New("evidence manifest current-page validity does not match retained native bytes")
		}
	}
	derivedCurrentState := evidenceStateNotFetched
	derivedCurrentCount := int64(0)
	if startItem != nil {
		derivedCurrentCount = 1
		derivedCurrentState = evidenceStatePartial
	}
	if currentTerminal {
		if startItem == nil {
			derivedCurrentState = evidenceStateAbsent
		} else {
			derivedCurrentState = evidenceStateVerified
		}
	}
	if currentError || (current.State == evidenceStateError && current.LastError != "") {
		derivedCurrentState = evidenceStateError
	}
	if current.State != derivedCurrentState || current.Count != derivedCurrentCount {
		return errors.New("evidence manifest current state/count does not derive from retained native pages")
	}
	if startItem == nil {
		if manifest.Snapshot.ItemRevision != 0 || manifest.Snapshot.ScopeRevision != 0 {
			return errors.New("evidence manifest snapshot is not backed by a retained current item")
		}
	} else if manifest.Snapshot.ItemRevision != startItem.Revision || manifest.Snapshot.ScopeRevision != startItem.ScopeRevision {
		return errors.New("evidence manifest snapshot does not match retained current-item bytes")
	}
	if !manifest.Snapshot.StartedAt.Equal(derivedStartedAt) || !manifest.Snapshot.CompletedAt.Equal(derivedCompletedAt) {
		return errors.New("evidence manifest snapshot timestamps do not derive from retained current pages")
	}

	for _, source := range []string{"revisions", "messages"} {
		entry, selected := manifest.Sources[source]
		if !selected {
			continue
		}
		after, count := int64(0), int64(0)
		var coverage *api.HistoryCoverage
		terminal, absent, failed := false, false, false
		for _, page := range entry.Pages {
			pageAfter := after
			body, readErr := retainedEvidencePageBody(manifestPath, page)
			if readErr != nil {
				return readErr
			}
			params := evidenceSourceParameters(source, manifest.Selection)
			params["after"] = strconv.FormatInt(after, 10)
			if !reflect.DeepEqual(page.Parameters, params) || terminal || absent {
				return fmt.Errorf("evidence manifest %s page chain is impossible", source)
			}
			derivedValid, pageFailed := false, false
			if page.StatusCode == http.StatusNotFound {
				if startItem == nil {
					derivedValid, absent = true, true
				} else {
					pageFailed = true
				}
			} else if page.StatusCode != http.StatusOK {
				pageFailed = true
			} else if source == "revisions" {
				var list api.WorkItemRevisionList
				if json.Unmarshal(body, &list) != nil || historyCoverageCompatible(manifest.Snapshot, list.Coverage, coverage) != nil {
					pageFailed = true
				} else {
					cursor := after
					for _, revision := range list.Revisions {
						if revision.TaskID != manifest.TaskID || revision.ItemID != manifest.ItemID || revision.Revision <= cursor {
							pageFailed = true
							break
						}
						cursor = revision.Revision
					}
					if !pageFailed && list.NextAfter != 0 && (list.NextAfter <= after || list.NextAfter != cursor) {
						pageFailed = true
					}
					if !pageFailed {
						derivedValid = true
						count += int64(len(list.Revisions))
						copy := list.Coverage
						coverage = &copy
						after = list.NextAfter
						terminal = list.NextAfter == 0
						if terminal && (!list.Coverage.Complete || count != list.Coverage.SnapshotCount) {
							pageFailed, derivedValid = true, false
							count -= int64(len(list.Revisions))
							after = pageAfter
						}
					}
				}
			} else {
				var list api.WorkItemMessageList
				if json.Unmarshal(body, &list) != nil || historyCoverageCompatible(manifest.Snapshot, list.Coverage, coverage) != nil {
					pageFailed = true
				} else {
					cursor := after
					seen := map[string]bool{}
					for _, link := range list.Links {
						if validFullMessage(link, manifest.TaskID, manifest.ItemID, manifest.Snapshot.ItemRevision, manifest.Selection.MessageRevision, after, seen) != nil || link.Message.Seq < cursor {
							pageFailed = true
							break
						}
						cursor = link.Message.Seq
					}
					if !pageFailed && list.NextAfter != 0 && (list.NextAfter <= after || list.NextAfter != cursor) {
						pageFailed = true
					}
					if !pageFailed {
						derivedValid = true
						count += int64(len(list.Links))
						copy := list.Coverage
						coverage = &copy
						after = list.NextAfter
						terminal = list.NextAfter == 0
						if terminal && !list.Coverage.Complete {
							pageFailed, derivedValid = true, false
							count -= int64(len(list.Links))
							after = pageAfter
						}
					}
				}
			}
			if page.Valid != derivedValid || (derivedValid && page.NextAfter != after) {
				return fmt.Errorf("evidence manifest %s page validity/cursor does not match retained native bytes", source)
			}
			failed = pageFailed
		}
		derivedState := evidenceStateNotFetched
		if len(entry.Pages) > 0 {
			derivedState = evidenceStatePartial
		}
		if terminal {
			derivedState = evidenceStateVerified
		}
		if absent {
			derivedState = evidenceStateAbsent
		}
		if failed || (entry.State == evidenceStateError && entry.LastError != "") {
			derivedState = evidenceStateError
		}
		if entry.State != derivedState || entry.Count != count || !reflect.DeepEqual(entry.Coverage, coverage) {
			return fmt.Errorf("evidence manifest %s state/count/coverage does not derive from retained native pages", source)
		}
	}

	if receipt, selected := manifest.Sources["receipt"]; selected {
		count, terminal, absent, failed := int64(0), false, false, false
		for _, page := range receipt.Pages {
			body, readErr := retainedEvidencePageBody(manifestPath, page)
			if readErr != nil {
				return readErr
			}
			if !reflect.DeepEqual(page.Parameters, evidenceSourceParameters("receipt", manifest.Selection)) || terminal || absent {
				return errors.New("evidence manifest receipt page chain or requested-agent coordinates changed")
			}
			derivedValid, pageFailed := false, false
			switch page.StatusCode {
			case http.StatusNotFound:
				derivedValid, absent = true, true
			case http.StatusOK:
				var result api.WorkItemUpdateResult
				if json.Unmarshal(body, &result) == nil && result.Receipt.TaskID == manifest.TaskID && result.Receipt.ItemID == manifest.ItemID && result.Receipt.RequestID == manifest.Selection.ReceiptRequestID && result.Receipt.ResultRevision == result.Revision.Revision && result.Revision.ItemID == manifest.ItemID && result.Revision.TaskID == manifest.TaskID && (manifest.Snapshot.ItemRevision == 0 || result.Revision.Revision <= manifest.Snapshot.ItemRevision) {
					derivedValid, terminal, count = true, true, 1
				} else {
					pageFailed = true
				}
			default:
				pageFailed = true
			}
			if page.Valid != derivedValid {
				return errors.New("evidence manifest receipt validity does not match retained native bytes")
			}
			failed = pageFailed
		}
		derivedState := evidenceStateNotFetched
		if len(receipt.Pages) > 0 {
			derivedState = evidenceStatePartial
		}
		if terminal {
			derivedState = evidenceStateVerified
		}
		if absent {
			derivedState = evidenceStateAbsent
		}
		if failed || (receipt.State == evidenceStateError && receipt.LastError != "") {
			derivedState = evidenceStateError
		}
		if receipt.State != derivedState || receipt.Count != count {
			return errors.New("evidence manifest receipt state/count does not derive from retained native bytes")
		}
	}
	complete, verified := manifest.Complete, manifest.Verified
	refreshEvidenceAggregate(manifest)
	if manifest.Complete != complete || manifest.Verified != verified {
		return errors.New("evidence manifest aggregate state does not derive from its selected sources")
	}
	return nil
}

type workItemEvidenceReader struct {
	client       *api.Client
	manifestPath string
	manifest     *workItemEvidenceManifest
}

func (r *workItemEvidenceReader) persist() error {
	refreshEvidenceAggregate(r.manifest)
	return saveWorkItemEvidenceManifest(r.manifestPath, r.manifest)
}

func refreshEvidenceAggregate(manifest *workItemEvidenceManifest) {
	manifest.Complete, manifest.Verified = true, true
	for _, source := range manifest.Selection.Sources {
		state := manifest.Sources[source].State
		if state != evidenceStateVerified && state != evidenceStateAbsent {
			manifest.Complete = false
		}
		if state != evidenceStateVerified {
			manifest.Verified = false
		}
	}
}

func (r *workItemEvidenceReader) fail(source string, err error) error {
	entry := r.manifest.Sources[source]
	entry.State = evidenceStateError
	entry.LastError = err.Error()
	if saveErr := r.persist(); saveErr != nil {
		return fmt.Errorf("%v (also failed to save manifest: %w)", err, saveErr)
	}
	return err
}

func (r *workItemEvidenceReader) fetch(ctx context.Context, source, phase string, read api.WorkItemEvidenceRead, parameters map[string]string) (api.RawWorkItemEvidence, int, error) {
	requestedAt := time.Now().UTC()
	response, err := r.client.ReadWorkItemEvidence(ctx, read)
	completedAt := time.Now().UTC()
	if err != nil {
		return response, -1, r.fail(source, fmt.Errorf("%s evidence transport: %w", source, err))
	}
	index, err := recordEvidencePage(r.manifestPath, source, phase, parameters, response, requestedAt, completedAt, r.manifest)
	if err != nil {
		return response, -1, r.fail(source, err)
	}
	return response, index, nil
}

func evidenceHTTPError(source string, response api.RawWorkItemEvidence) error {
	var body api.ErrorResponse
	_ = json.Unmarshal(response.Body, &body)
	message := strings.TrimSpace(body.Error)
	if message == "" {
		message = strings.TrimSpace(string(response.Body))
	}
	if len(message) > 512 {
		message = message[:512]
	}
	return fmt.Errorf("%s evidence HTTP %d: %s", source, response.StatusCode, message)
}

func (r *workItemEvidenceReader) readCurrent(ctx context.Context, final bool) error {
	entry := r.manifest.Sources["current"]
	if !final && r.manifest.Snapshot.ItemRevision > 0 {
		return nil
	}
	phase := "start"
	if final {
		phase = "end"
	}
	read := api.WorkItemEvidenceRead{Operation: api.WorkItemEvidenceCurrent, TaskID: r.manifest.TaskID, ItemID: r.manifest.ItemID}
	response, index, err := r.fetch(ctx, "current", phase, read, map[string]string{"phase": phase})
	if err != nil {
		return err
	}
	page := &entry.Pages[index]
	if response.StatusCode == http.StatusNotFound {
		if final || r.manifest.Snapshot.ItemRevision > 0 {
			return r.fail("current", errors.New("current item disappeared during evidence read"))
		}
		page.Valid = true
		entry.State, entry.LastError = evidenceStateAbsent, ""
		return r.persist()
	}
	if response.StatusCode != http.StatusOK {
		return r.fail("current", evidenceHTTPError("current", response))
	}
	var item api.WorkItem
	if err = json.Unmarshal(response.Body, &item); err != nil {
		return r.fail("current", fmt.Errorf("decode current evidence: %w", err))
	}
	if item.ID != r.manifest.ItemID || item.TaskID != r.manifest.TaskID || item.Revision < 1 || item.ScopeRevision < 1 {
		return r.fail("current", errors.New("current evidence returned mismatched item coordinates"))
	}
	page.Valid = true
	entry.Count = 1
	entry.LastError = ""
	if !final {
		r.manifest.Snapshot.ItemRevision = item.Revision
		r.manifest.Snapshot.ScopeRevision = item.ScopeRevision
		r.manifest.Snapshot.StartedAt = page.CompletedAt
		entry.State = evidenceStatePartial
		return r.persist()
	}
	if item.Revision != r.manifest.Snapshot.ItemRevision || item.ScopeRevision != r.manifest.Snapshot.ScopeRevision {
		return r.fail("current", fmt.Errorf("item snapshot changed during evidence read: revision/scope %d/%d -> %d/%d", r.manifest.Snapshot.ItemRevision, r.manifest.Snapshot.ScopeRevision, item.Revision, item.ScopeRevision))
	}
	r.manifest.Snapshot.CompletedAt = page.CompletedAt
	entry.State = evidenceStateVerified
	return r.persist()
}

func historyCoverageCompatible(snapshot workItemEvidenceSnapshot, got api.HistoryCoverage, previous *api.HistoryCoverage) error {
	if snapshot.ItemRevision > 0 && got.ObservedCurrentRevision != snapshot.ItemRevision {
		return fmt.Errorf("history snapshot revision %d does not match anchored current revision %d", got.ObservedCurrentRevision, snapshot.ItemRevision)
	}
	if got.Complete && (got.LatestMaterialized != got.ObservedCurrentRevision || got.GapCount != 0) {
		return errors.New("history terminal metadata is internally inconsistent")
	}
	if previous != nil && !reflect.DeepEqual(*previous, got) {
		return errors.New("history coverage changed between pages")
	}
	return nil
}

func lastValidNext(entry *workItemEvidenceSource) int64 {
	var next int64
	for _, page := range entry.Pages {
		if page.Valid && page.StatusCode == http.StatusOK {
			next = page.NextAfter
		}
	}
	return next
}

func (r *workItemEvidenceReader) readRevisionPages(ctx context.Context) error {
	entry := r.manifest.Sources["revisions"]
	if entry.State == evidenceStateVerified || entry.State == evidenceStateAbsent {
		return nil
	}
	after := lastValidNext(entry)
	for {
		params := evidenceSourceParameters("revisions", r.manifest.Selection)
		params["after"] = strconv.FormatInt(after, 10)
		read := api.WorkItemEvidenceRead{Operation: api.WorkItemEvidenceRevisions, TaskID: r.manifest.TaskID, ItemID: r.manifest.ItemID, After: after, Limit: r.manifest.Selection.PageLimit}
		response, index, err := r.fetch(ctx, "revisions", "", read, params)
		if err != nil {
			return err
		}
		page := &entry.Pages[index]
		if response.StatusCode == http.StatusNotFound {
			if r.manifest.Snapshot.ItemRevision > 0 {
				return r.fail("revisions", errors.New("revision history disappeared after the current-item snapshot was anchored"))
			}
			page.Valid = true
			entry.State, entry.LastError = evidenceStateAbsent, ""
			return r.persist()
		}
		if response.StatusCode != http.StatusOK {
			return r.fail("revisions", evidenceHTTPError("revisions", response))
		}
		if r.manifest.Snapshot.ItemRevision == 0 {
			return r.fail("revisions", errors.New("revision evidence exists without a current-item snapshot anchor"))
		}
		var list api.WorkItemRevisionList
		if err = json.Unmarshal(response.Body, &list); err != nil {
			return r.fail("revisions", fmt.Errorf("decode revisions evidence: %w", err))
		}
		if err = historyCoverageCompatible(r.manifest.Snapshot, list.Coverage, entry.Coverage); err != nil {
			return r.fail("revisions", err)
		}
		for _, revision := range list.Revisions {
			if revision.TaskID != r.manifest.TaskID || revision.ItemID != r.manifest.ItemID || revision.Revision <= after {
				return r.fail("revisions", errors.New("revision page contains mismatched or nonadvancing coordinates"))
			}
			after = revision.Revision
		}
		if list.NextAfter != 0 && (list.NextAfter <= pageParameterInt(page.Parameters, "after") || list.NextAfter != after) {
			return r.fail("revisions", fmt.Errorf("revision cursor did not advance exactly: nextAfter=%d", list.NextAfter))
		}
		projectedCount := entry.Count + int64(len(list.Revisions))
		coverage := list.Coverage
		entry.Coverage = &coverage
		entry.State, entry.LastError = evidenceStatePartial, ""
		if list.NextAfter == 0 {
			if !list.Coverage.Complete || projectedCount != list.Coverage.SnapshotCount {
				return r.fail("revisions", fmt.Errorf("native revision history is incomplete: complete=%t pages=%d snapshots=%d", list.Coverage.Complete, projectedCount, list.Coverage.SnapshotCount))
			}
		}
		page.NextAfter, page.Valid = list.NextAfter, true
		entry.Count = projectedCount
		if list.NextAfter == 0 {
			entry.State = evidenceStateVerified
			return r.persist()
		}
		if err = r.persist(); err != nil {
			return err
		}
		after = list.NextAfter
	}
}

func pageParameterInt(parameters map[string]string, key string) int64 {
	n, _ := strconv.ParseInt(parameters[key], 10, 64)
	return n
}

func (r *workItemEvidenceReader) readMessagePages(ctx context.Context) error {
	entry := r.manifest.Sources["messages"]
	if entry.State == evidenceStateVerified || entry.State == evidenceStateAbsent {
		return nil
	}
	after := lastValidNext(entry)
	for {
		params := evidenceSourceParameters("messages", r.manifest.Selection)
		params["after"] = strconv.FormatInt(after, 10)
		read := api.WorkItemEvidenceRead{Operation: api.WorkItemEvidenceMessages, TaskID: r.manifest.TaskID, ItemID: r.manifest.ItemID, After: after, Limit: r.manifest.Selection.PageLimit, Revision: r.manifest.Selection.MessageRevision}
		response, index, err := r.fetch(ctx, "messages", "", read, params)
		if err != nil {
			return err
		}
		page := &entry.Pages[index]
		if response.StatusCode == http.StatusNotFound {
			if r.manifest.Snapshot.ItemRevision > 0 {
				return r.fail("messages", errors.New("linked-message history disappeared after the current-item snapshot was anchored"))
			}
			page.Valid = true
			entry.State, entry.LastError = evidenceStateAbsent, ""
			return r.persist()
		}
		if response.StatusCode != http.StatusOK {
			return r.fail("messages", evidenceHTTPError("messages", response))
		}
		if r.manifest.Snapshot.ItemRevision == 0 {
			return r.fail("messages", errors.New("message evidence exists without a current-item snapshot anchor"))
		}
		var list api.WorkItemMessageList
		if err = json.Unmarshal(response.Body, &list); err != nil {
			return r.fail("messages", fmt.Errorf("decode messages evidence: %w", err))
		}
		if err = historyCoverageCompatible(r.manifest.Snapshot, list.Coverage, entry.Coverage); err != nil {
			return r.fail("messages", err)
		}
		cursor := after
		coordinates := map[string]bool{}
		for _, link := range list.Links {
			if err = validFullMessage(link, r.manifest.TaskID, r.manifest.ItemID, r.manifest.Snapshot.ItemRevision, r.manifest.Selection.MessageRevision, after, coordinates); err != nil || link.Message.Seq < cursor {
				if err == nil {
					err = errors.New("message page contains nonmonotonic source coordinates")
				}
				return r.fail("messages", err)
			}
			cursor = link.Message.Seq
		}
		if list.NextAfter != 0 && (list.NextAfter <= after || list.NextAfter != cursor) {
			return r.fail("messages", fmt.Errorf("message cursor did not advance exactly: nextAfter=%d", list.NextAfter))
		}
		coverage := list.Coverage
		entry.Coverage = &coverage
		entry.State, entry.LastError = evidenceStatePartial, ""
		if list.NextAfter == 0 {
			if !list.Coverage.Complete {
				return r.fail("messages", errors.New("native linked-message history reports incomplete revision coverage"))
			}
			minimumCount := list.Coverage.ExplicitMessageCount
			if list.Coverage.SourceMessageCount > minimumCount {
				minimumCount = list.Coverage.SourceMessageCount
			}
			if list.Coverage.DispatchCount > minimumCount {
				minimumCount = list.Coverage.DispatchCount
			}
			if entry.Count+int64(len(list.Links)) < minimumCount {
				return r.fail("messages", fmt.Errorf("terminal linked-message pages contain %d records but native coverage requires at least %d", entry.Count+int64(len(list.Links)), minimumCount))
			}
			page.NextAfter, page.Valid = list.NextAfter, true
			entry.Count += int64(len(list.Links))
			entry.State = evidenceStateVerified
			return r.persist()
		}
		page.NextAfter, page.Valid = list.NextAfter, true
		entry.Count += int64(len(list.Links))
		if err = r.persist(); err != nil {
			return err
		}
		after = list.NextAfter
	}
}

func (r *workItemEvidenceReader) readReceipt(ctx context.Context) error {
	entry := r.manifest.Sources["receipt"]
	if entry.State == evidenceStateVerified || entry.State == evidenceStateAbsent {
		return nil
	}
	params := evidenceSourceParameters("receipt", r.manifest.Selection)
	read := api.WorkItemEvidenceRead{Operation: api.WorkItemEvidenceReceipt, TaskID: r.manifest.TaskID, ItemID: r.manifest.ItemID, RequestID: r.manifest.Selection.ReceiptRequestID, AgentID: r.manifest.Selection.ReceiptAgentID}
	response, index, err := r.fetch(ctx, "receipt", "", read, params)
	if err != nil {
		return err
	}
	page := &entry.Pages[index]
	if response.StatusCode == http.StatusNotFound {
		page.Valid = true
		entry.State, entry.LastError = evidenceStateAbsent, ""
		return r.persist()
	}
	if response.StatusCode != http.StatusOK {
		return r.fail("receipt", evidenceHTTPError("receipt", response))
	}
	if r.manifest.Snapshot.ItemRevision == 0 {
		return r.fail("receipt", errors.New("receipt evidence exists without a current-item snapshot anchor"))
	}
	var result api.WorkItemUpdateResult
	if err = json.Unmarshal(response.Body, &result); err != nil {
		return r.fail("receipt", fmt.Errorf("decode receipt evidence: %w", err))
	}
	if result.Receipt.TaskID != r.manifest.TaskID || result.Receipt.ItemID != r.manifest.ItemID || result.Receipt.RequestID != r.manifest.Selection.ReceiptRequestID || result.Receipt.ResultRevision != result.Revision.Revision || result.Revision.ItemID != r.manifest.ItemID || result.Revision.TaskID != r.manifest.TaskID {
		return r.fail("receipt", errors.New("receipt evidence returned mismatched request/item/revision coordinates"))
	}
	if r.manifest.Snapshot.ItemRevision > 0 && result.Revision.Revision > r.manifest.Snapshot.ItemRevision {
		return r.fail("receipt", errors.New("receipt result is newer than the anchored item snapshot"))
	}
	page.Valid = true
	entry.Count, entry.State, entry.LastError = 1, evidenceStateVerified, ""
	return r.persist()
}

func (r *workItemEvidenceReader) run(ctx context.Context) error {
	current := r.manifest.Sources["current"]
	if current.State != evidenceStateAbsent && current.State != evidenceStateVerified {
		if err := r.readCurrent(ctx, false); err != nil {
			return err
		}
	}
	for _, source := range r.manifest.Selection.Sources {
		if source == "current" {
			continue
		}
		var err error
		switch source {
		case "revisions":
			err = r.readRevisionPages(ctx)
		case "messages":
			err = r.readMessagePages(ctx)
		case "receipt":
			err = r.readReceipt(ctx)
		}
		if err != nil {
			return err
		}
	}
	if current.State != evidenceStateAbsent && current.State != evidenceStateVerified {
		if err := r.readCurrent(ctx, true); err != nil {
			return err
		}
	}
	return r.persist()
}

func cmdWorkItemEvidence(e env, args []string) error {
	fs := flag.NewFlagSet("work-items evidence", flag.ContinueOnError)
	projectFlag := fs.String("project", e.task, "owning project id")
	manifestFlag := fs.String("manifest", "", "private resumable manifest path (required)")
	sourcesFlag := fs.String("sources", "", "comma-separated current,revisions,messages,receipt (current required)")
	messageRevision := fs.Int64("message-revision", 0, "limit linked messages to one exact item revision")
	receiptRequestID := fs.String("receipt-request-id", "", "exact work-item mutation request key")
	receiptAgentID := fs.String("receipt-agent", e.agent, "exact mutation author agent id")
	pageLimit := fs.Int("limit", api.DefaultWorkItemHistoryPage, "native revision/message page size")
	asJSON := fs.Bool("json", false, "print final manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !api.ValidID(fs.Arg(0), "wi") || *manifestFlag == "" || *messageRevision < 0 || *pageLimit < 1 || *pageLimit > api.MaxWorkItemHistoryPage {
		return errors.New("usage: tt work-items evidence --manifest PATH --sources current[,revisions,messages,receipt] [options] WI_ID")
	}
	project, err := workItemProject(e, *projectFlag)
	if err != nil {
		return err
	}
	sources, err := parseEvidenceSources(*sourcesFlag)
	if err != nil {
		return err
	}
	hasReceipt := false
	hasMessages := false
	for _, source := range sources {
		hasReceipt = hasReceipt || source == "receipt"
		hasMessages = hasMessages || source == "messages"
	}
	if hasReceipt != (*receiptRequestID != "") {
		return errors.New("receipt source and --receipt-request-id must be supplied together")
	}
	if hasReceipt && *receiptAgentID != "" && !api.ValidID(*receiptAgentID, "agt") {
		return errors.New("--receipt-agent must be a valid agt_ id")
	}
	if !hasMessages && *messageRevision != 0 {
		return errors.New("--message-revision requires the messages source")
	}
	if !hasReceipt {
		*receiptAgentID = ""
	}
	manifestPath, err := filepath.Abs(*manifestFlag)
	if err != nil || manifestPath == "-" {
		return errors.New("--manifest must name a local file")
	}
	c, err := e.client(30 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	if e.agent == "" {
		return errors.New("work-item evidence reader requires an exact database handler agent")
	}
	actor, err := c.GetAgent(ctx, project, e.agent)
	if err != nil {
		return err
	}
	if actor.Role != api.AgentRoleDatabaseHandler {
		return errors.New("work-item evidence reader is restricted to the project's database handler role")
	}
	selection := workItemEvidenceSelection{Sources: sources, MessageRevision: *messageRevision, ReceiptRequestID: *receiptRequestID, ReceiptAgentID: *receiptAgentID, PageLimit: *pageLimit}
	var manifest workItemEvidenceManifest
	if _, statErr := os.Lstat(manifestPath); statErr == nil {
		manifest, err = loadWorkItemEvidenceManifest(manifestPath)
		if err != nil {
			return err
		}
		if manifest.TaskID != project || manifest.ItemID != fs.Arg(0) || !reflect.DeepEqual(manifest.Selection, selection) {
			return errors.New("existing evidence manifest selection does not match this request")
		}
		if err = verifyEvidencePages(manifestPath, &manifest); err != nil {
			return err
		}
		if err = validateRetainedEvidenceManifest(manifestPath, &manifest); err != nil {
			return fmt.Errorf("invalid evidence manifest: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else {
		manifest = newWorkItemEvidenceManifest(project, fs.Arg(0), selection, time.Now())
		if err = saveWorkItemEvidenceManifest(manifestPath, &manifest); err != nil {
			return err
		}
	}
	reader := workItemEvidenceReader{client: c, manifestPath: manifestPath, manifest: &manifest}
	if err = reader.run(ctx); err != nil {
		return fmt.Errorf("evidence manifest retained at %s: %w", manifestPath, err)
	}
	if *asJSON {
		printJSON(manifest)
	} else {
		fmt.Printf("%s complete=%t verified=%t revision=%d scopeRevision=%d\n", manifestPath, manifest.Complete, manifest.Verified, manifest.Snapshot.ItemRevision, manifest.Snapshot.ScopeRevision)
	}
	return nil
}
