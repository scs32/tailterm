package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const (
	evidenceTask    = "tsk_0000000000000001"
	evidenceItem    = "wi_0000000000000002"
	evidenceHandler = "agt_0000000000000003"
)

func evidenceCLIEnv(t *testing.T, handle func(http.ResponseWriter, *http.Request)) env {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			t.Fatalf("evidence reader mutated through %s %s", req.Method, req.URL.String())
		}
		if req.URL.Path == "/v1/tasks/"+evidenceTask+"/agents/"+evidenceHandler {
			_ = json.NewEncoder(w).Encode(api.Agent{ID: evidenceHandler, TaskID: evidenceTask, Role: api.AgentRoleDatabaseHandler})
			return
		}
		handle(w, req)
	}))
	t.Cleanup(srv.Close)
	return env{hub: srv.URL, task: evidenceTask, agent: evidenceHandler, agentName: "database"}
}

func evidenceCurrent(revision, scope int64) api.WorkItem {
	return api.WorkItem{ID: evidenceItem, TaskID: evidenceTask, Kind: "feature", Title: "Synthetic", Status: "open", Priority: "normal", Revision: revision, ScopeRevision: scope}
}

func evidenceCoverage(revision, snapshots, messages int64) api.HistoryCoverage {
	return api.HistoryCoverage{Complete: true, ObservedCurrentRevision: revision, LatestMaterialized: revision, SnapshotCount: snapshots, ConversationLinks: "explicit_only", ExplicitMessageCount: messages}
}

func evidenceMessageLink(itemRevision, seq int64, text string) api.WorkItemMessageLink {
	return api.WorkItemMessageLink{
		ItemRevision: itemRevision, RevisionCoverage: "verified",
		Message: api.Message{TaskID: evidenceTask, Seq: seq, Text: text, From: api.Sender{Node: "fixture", User: "owner"}, CreatedAt: time.Unix(seq, 0).UTC()},
	}
}

func writeEvidenceJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func readEvidenceManifestForTest(t *testing.T, path string) workItemEvidenceManifest {
	t.Helper()
	manifest, err := loadWorkItemEvidenceManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyEvidencePages(path, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestWorkItemEvidenceReaderTraversesNativePagesAndExactReceipt(t *testing.T) {
	var currentCalls atomic.Int32
	var paths []string
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		paths = append(paths, req.URL.RequestURI())
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		switch {
		case req.URL.Path == base:
			currentCalls.Add(1)
			writeEvidenceJSON(w, evidenceCurrent(2, 2))
		case req.URL.Path == base+"/revisions":
			after, _ := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
			list := api.WorkItemRevisionList{Coverage: evidenceCoverage(2, 2, 2)}
			if after == 0 {
				list.Revisions = []api.WorkItemRevision{{TaskID: evidenceTask, ItemID: evidenceItem, Revision: 1}}
				list.NextAfter = 1
			} else if after == 1 {
				list.Revisions = []api.WorkItemRevision{{TaskID: evidenceTask, ItemID: evidenceItem, Revision: 2}}
			} else {
				t.Fatalf("unexpected revision cursor %d", after)
			}
			writeEvidenceJSON(w, list)
		case req.URL.Path == base+"/messages":
			after, _ := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
			list := api.WorkItemMessageList{Coverage: evidenceCoverage(2, 2, 2)}
			if after == 0 {
				list.Links = []api.WorkItemMessageLink{evidenceMessageLink(1, 11, "first")}
				list.NextAfter = 11
			} else if after == 11 {
				list.Links = []api.WorkItemMessageLink{evidenceMessageLink(2, 12, "second")}
			} else {
				t.Fatalf("unexpected message cursor %d", after)
			}
			writeEvidenceJSON(w, list)
		case req.URL.Path == base+"/updates/receipts/update%2Fone" || req.URL.EscapedPath() == base+"/updates/receipts/update%2Fone":
			if got := req.URL.Query().Get("agentId"); got != evidenceHandler {
				t.Fatalf("receipt agent=%q", got)
			}
			writeEvidenceJSON(w, api.WorkItemUpdateResult{
				Revision: api.WorkItemRevision{TaskID: evidenceTask, ItemID: evidenceItem, Revision: 2},
				Receipt:  api.WorkItemUpdateReceipt{ID: "wir_0000000000000004", RequestID: "update/one", TaskID: evidenceTask, ItemID: evidenceItem, ResultRevision: 2},
			})
		default:
			t.Fatalf("wrong native getter %s", req.URL.RequestURI())
		}
	})
	manifestPath := filepath.Join(t.TempDir(), "evidence.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "messages,current,receipt,revisions", "--receipt-request-id", "update/one", "--limit", "1", evidenceItem})
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if !manifest.Complete || !manifest.Verified || manifest.Snapshot.ItemRevision != 2 || manifest.Snapshot.ScopeRevision != 2 || currentCalls.Load() != 2 {
		t.Fatalf("manifest=%+v currentCalls=%d", manifest, currentCalls.Load())
	}
	if len(manifest.Sources["revisions"].Pages) != 2 || len(manifest.Sources["messages"].Pages) != 2 || manifest.Sources["receipt"].Count != 1 {
		t.Fatalf("sources=%+v", manifest.Sources)
	}
	joined := strings.Join(paths, "\n")
	for _, required := range []string{"/work-items/" + evidenceItem + "\n", "/revisions?after=0&limit=1", "/revisions?after=1&limit=1", "/messages?after=11&limit=1", "/updates/receipts/update%2Fone?agentId=" + evidenceHandler} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing native route %q in\n%s", required, joined)
		}
	}
	for _, source := range manifest.Sources {
		for _, page := range source.Pages {
			body, readErr := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), page.Path))
			if readErr != nil {
				t.Fatal(readErr)
			}
			digest := sha256.Sum256(body)
			if page.Bytes != int64(len(body)) || page.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("page integrity=%+v", page)
			}
		}
	}
	beforeReplay := len(paths)
	if _, err = captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "messages,current,receipt,revisions", "--receipt-request-id", "update/one", "--limit", "1", evidenceItem})
	}); err != nil {
		t.Fatalf("verified manifest replay: %v", err)
	}
	if len(paths) != beforeReplay || currentCalls.Load() != 2 {
		t.Fatalf("verified manifest replay repeated native evidence reads: paths=%d->%d current=%d", beforeReplay, len(paths), currentCalls.Load())
	}
}

func TestWorkItemEvidenceReaderRetainsTransientFailureAndResumes(t *testing.T) {
	var messages atomic.Int32
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		switch req.URL.Path {
		case base:
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
		case base + "/messages":
			if messages.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				writeEvidenceJSON(w, api.ErrorResponse{Error: "synthetic transient"})
				return
			}
			writeEvidenceJSON(w, api.WorkItemMessageList{Links: []api.WorkItemMessageLink{evidenceMessageLink(1, 8, "resumed")}, Coverage: evidenceCoverage(1, 1, 1)})
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
	})
	manifestPath := filepath.Join(t.TempDir(), "resume.json")
	args := []string{"evidence", "--manifest", manifestPath, "--sources", "current,messages", evidenceItem}
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, args) }); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("first read error=%v", err)
	}
	failed := readEvidenceManifestForTest(t, manifestPath)
	if failed.Complete || failed.Sources["messages"].State != evidenceStateError || len(failed.Sources["messages"].Pages) != 1 {
		t.Fatalf("failed manifest=%+v", failed)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, args) }); err != nil {
		t.Fatal(err)
	}
	resumed := readEvidenceManifestForTest(t, manifestPath)
	if !resumed.Complete || !resumed.Verified || len(resumed.Sources["messages"].Pages) != 2 || !resumed.Sources["messages"].Pages[1].Valid {
		t.Fatalf("resumed manifest=%+v", resumed)
	}
}

func TestWorkItemEvidenceReaderRejectsRepeatedCursor(t *testing.T) {
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		if req.URL.Path == base {
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
			return
		}
		if req.URL.Path != base+"/messages" {
			t.Fatalf("path=%s", req.URL.Path)
		}
		after := req.URL.Query().Get("after")
		list := api.WorkItemMessageList{Coverage: evidenceCoverage(1, 1, 2)}
		if after == "0" {
			list.Links = []api.WorkItemMessageLink{evidenceMessageLink(1, 10, "first")}
			list.NextAfter = 10
		} else {
			list.Links = []api.WorkItemMessageLink{evidenceMessageLink(1, 11, "second")}
			list.NextAfter = 10
		}
		writeEvidenceJSON(w, list)
	})
	manifestPath := filepath.Join(t.TempDir(), "cursor.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current,messages", "--limit", "1", evidenceItem})
	})
	if err == nil || !strings.Contains(err.Error(), "cursor did not advance") {
		t.Fatalf("error=%v", err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if manifest.Complete || manifest.Sources["messages"].State != evidenceStateError || len(manifest.Sources["messages"].Pages) != 2 {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestWorkItemEvidenceReaderRejectsScopeChangeAtBookend(t *testing.T) {
	var calls atomic.Int32
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		if req.URL.Path != base {
			t.Fatalf("wrong getter %s", req.URL.Path)
		}
		if calls.Add(1) == 1 {
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
		} else {
			writeEvidenceJSON(w, evidenceCurrent(2, 2))
		}
	})
	manifestPath := filepath.Join(t.TempDir(), "scope.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current", evidenceItem})
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("error=%v", err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if manifest.Complete || manifest.Sources["current"].State != evidenceStateError || len(manifest.Sources["current"].Pages) != 2 {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestWorkItemEvidenceReaderDoesNotRelabelBookendDisappearanceAsAbsence(t *testing.T) {
	var calls atomic.Int32
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeEvidenceJSON(w, api.ErrorResponse{Error: "not found"})
	})
	manifestPath := filepath.Join(t.TempDir(), "disappeared.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current", evidenceItem})
	})
	if err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("error=%v", err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if manifest.Complete || manifest.Sources["current"].State != evidenceStateError {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestWorkItemEvidenceReaderRecordsAbsentReceiptWithoutClaimingVerification(t *testing.T) {
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		if req.URL.Path == base {
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
			return
		}
		if strings.Contains(req.URL.Path, "/updates/receipts/") {
			w.WriteHeader(http.StatusNotFound)
			writeEvidenceJSON(w, api.ErrorResponse{Error: "not found"})
			return
		}
		t.Fatalf("path=%s query=%s", req.URL.Path, req.URL.RawQuery)
	})
	manifestPath := filepath.Join(t.TempDir(), "absent.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "receipt,current", "--receipt-request-id", "missing", evidenceItem})
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if !manifest.Complete || manifest.Verified || manifest.Sources["receipt"].State != evidenceStateAbsent {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestWorkItemEvidenceReaderRequiresHandlerAndExactSelection(t *testing.T) {
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		t.Fatalf("unexpected evidence request %s", req.URL.String())
	})
	if err := cmdWorkItems(e, []string{"evidence", "--manifest", filepath.Join(t.TempDir(), "bad.json"), "--sources", "messages", evidenceItem}); err == nil || !strings.Contains(err.Error(), "current is required") {
		t.Fatalf("missing anchor error=%v", err)
	}
	if err := cmdWorkItems(e, []string{"evidence", "--manifest", filepath.Join(t.TempDir(), "bad2.json"), "--sources", "current,receipt", evidenceItem}); err == nil || !strings.Contains(err.Error(), "supplied together") {
		t.Fatalf("missing receipt key error=%v", err)
	}
}

func TestWorkItemEvidenceReaderRejectsNonHandlerRoleBeforeEvidenceRead(t *testing.T) {
	var evidenceCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "/agents/") {
			writeEvidenceJSON(w, api.Agent{ID: evidenceHandler, TaskID: evidenceTask, Role: ""})
			return
		}
		evidenceCalls.Add(1)
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: evidenceTask, agent: evidenceHandler, agentName: "worker"}
	err := cmdWorkItems(e, []string{"evidence", "--manifest", filepath.Join(t.TempDir(), "role.json"), "--sources", "current", evidenceItem})
	if err == nil || !strings.Contains(err.Error(), "database handler role") || evidenceCalls.Load() != 0 {
		t.Fatalf("error=%v evidenceCalls=%d", err, evidenceCalls.Load())
	}
}

func TestWorkItemEvidenceReaderRejectsMissingRetainedPageBeforeResume(t *testing.T) {
	var currentCalls atomic.Int32
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		currentCalls.Add(1)
		writeEvidenceJSON(w, evidenceCurrent(1, 1))
	})
	manifestPath := filepath.Join(t.TempDir(), "missing-page.json")
	args := []string{"evidence", "--manifest", manifestPath, "--sources", "current", evidenceItem}
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, args) }); err != nil {
		t.Fatal(err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	pagePath := filepath.Join(filepath.Dir(manifestPath), manifest.Sources["current"].Pages[0].Path)
	if err := os.Rename(pagePath, pagePath+".held-out"); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, args) }); err == nil || !strings.Contains(err.Error(), "missing or not a mode 0600 regular file") {
		t.Fatalf("missing page error=%v", err)
	}
	if currentCalls.Load() != 2 {
		t.Fatalf("resume made a new evidence request after retained-page failure: calls=%d", currentCalls.Load())
	}
}

func TestWorkItemEvidenceReaderRejectsManifestStateWithoutRetainedPages(t *testing.T) {
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		t.Fatalf("forged manifest bypass attempted native request %s", req.URL.String())
	})
	manifestPath := filepath.Join(t.TempDir(), "forged-state.json")
	selection := workItemEvidenceSelection{Sources: []string{"current"}, PageLimit: api.DefaultWorkItemHistoryPage}
	manifest := newWorkItemEvidenceManifest(evidenceTask, evidenceItem, selection, time.Now())
	manifest.Snapshot.ItemRevision, manifest.Snapshot.ScopeRevision = 1, 1
	manifest.Sources["current"].State = evidenceStateVerified
	manifest.Complete, manifest.Verified = true, true
	if err := saveWorkItemEvidenceManifest(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current", evidenceItem})
	})
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("forged verified manifest accepted: %v", err)
	}
}

func TestWorkItemEvidenceReaderRejectsMetadataOnlyLinkedMessage(t *testing.T) {
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		switch req.URL.Path {
		case base:
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
		case base + "/messages":
			writeEvidenceJSON(w, api.WorkItemMessageList{
				Links:    []api.WorkItemMessageLink{{ItemRevision: 1, RevisionCoverage: "verified", Message: api.Message{TaskID: evidenceTask, Seq: 1}}},
				Coverage: evidenceCoverage(1, 1, 1),
			})
		default:
			t.Fatalf("unexpected path %s", req.URL.String())
		}
	})
	manifestPath := filepath.Join(t.TempDir(), "metadata-only.json")
	_, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current,messages", evidenceItem})
	})
	if err == nil || !strings.Contains(err.Error(), "message") {
		t.Fatalf("metadata-only message accepted: %v", err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if manifest.Complete || manifest.Sources["messages"].State != evidenceStateError {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestWorkItemEvidenceReaderRejectsUnexplainedForeignTaskAndFutureRevision(t *testing.T) {
	for name, link := range map[string]api.WorkItemMessageLink{
		"foreign-without-dispatch": evidenceMessageLink(1, 1, "foreign"),
		"future-revision":          evidenceMessageLink(2, 1, "future"),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "foreign-without-dispatch" {
				link.Message.TaskID = "tsk_0000000000000009"
			}
			e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
				base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
				switch req.URL.Path {
				case base:
					writeEvidenceJSON(w, evidenceCurrent(1, 1))
				case base + "/messages":
					writeEvidenceJSON(w, api.WorkItemMessageList{Links: []api.WorkItemMessageLink{link}, Coverage: evidenceCoverage(1, 1, 1)})
				default:
					t.Fatalf("unexpected path %s", req.URL.String())
				}
			})
			_, err := captureCLIOutput(t, func() error {
				return cmdWorkItems(e, []string{"evidence", "--manifest", filepath.Join(t.TempDir(), "coordinates.json"), "--sources", "current,messages", evidenceItem})
			})
			if err == nil || !strings.Contains(err.Error(), "message") {
				t.Fatalf("mismatched linked-message coordinates accepted: %v", err)
			}
		})
	}
}

func TestWorkItemEvidenceReaderPreservesPrimaryCrossProjectDispatchMessage(t *testing.T) {
	link := evidenceMessageLink(1, 1, "validated dispatch")
	link.Message.TaskID = "tsk_0000000000000009"
	link.Relationship = "primary"
	link.Message.WorkItems = []api.MessageWorkItem{{ItemTaskID: evidenceTask, ItemID: evidenceItem, ItemRevision: 1, Relationship: "primary"}}
	e := evidenceCLIEnv(t, func(w http.ResponseWriter, req *http.Request) {
		base := "/v1/tasks/" + evidenceTask + "/work-items/" + evidenceItem
		switch req.URL.Path {
		case base:
			writeEvidenceJSON(w, evidenceCurrent(1, 1))
		case base + "/messages":
			writeEvidenceJSON(w, api.WorkItemMessageList{Links: []api.WorkItemMessageLink{link}, Coverage: evidenceCoverage(1, 1, 1)})
		default:
			t.Fatalf("unexpected path %s", req.URL.String())
		}
	})
	manifestPath := filepath.Join(t.TempDir(), "cross-project.json")
	if _, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"evidence", "--manifest", manifestPath, "--sources", "current,messages", evidenceItem})
	}); err != nil {
		t.Fatal(err)
	}
	manifest := readEvidenceManifestForTest(t, manifestPath)
	if !manifest.Complete || !manifest.Verified || manifest.Sources["messages"].Count != 1 {
		t.Fatalf("manifest=%+v", manifest)
	}
}
