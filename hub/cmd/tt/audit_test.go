package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestParseRelatedItem(t *testing.T) {
	item, err := parseRelatedItem("tsk_0000000000000001/wi_0000000000000002@7")
	if err != nil {
		t.Fatal(err)
	}
	want := api.MessageWorkItem{ItemTaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", ItemRevision: 7, Relationship: "related"}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("got %+v", item)
	}
	for _, bad := range []string{"bad", "tsk_0000000000000001/wi_0000000000000002@0", "tsk_0000000000000001/wi_bad@1"} {
		if _, err := parseRelatedItem(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestBindAuditActorUsesExactCurrentRun(t *testing.T) {
	e := env{agent: "agt_0000000000000001", runID: "run_0000000000000002"}
	agent, run := "", ""
	if err := bindAuditActor(e, &agent, &run); err != nil {
		t.Fatal(err)
	}
	if agent != e.agent || run != e.runID {
		t.Fatalf("identity %q/%q", agent, run)
	}
	agent, run = e.agent, "run_0000000000000003"
	if err := bindAuditActor(e, &agent, &run); err == nil || !strings.Contains(err.Error(), "exact run") {
		t.Fatalf("stale run err=%v", err)
	}
}

func TestDownloadAuditExportVerifiesDigestAndRefusesOverwrite(t *testing.T) {
	content := []byte("immutable synthetic project audit export\n")
	h := sha256.Sum256(content)
	digest := hex.EncodeToString(h[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		end := offset + 7
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		write := api.AuditExportChunk{ExportID: "aex_0000000000000001", Offset: offset, NextOffset: end, Total: int64(len(content)), SHA256: digest, Data: content[offset:end], Complete: end == int64(len(content))}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(write)
	}))
	defer server.Close()
	client, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "audit.json")
	if err = downloadAuditExport(context.Background(), client, "tsk_0000000000000001", "aex_0000000000000001", output); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !reflect.DeepEqual(got, content) {
		t.Fatalf("content=%q err=%v", got, err)
	}
	if err = downloadAuditExport(context.Background(), client, "tsk_0000000000000001", "aex_0000000000000001", output); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite err=%v", err)
	}
}

func TestDownloadAuditExportRejectsIdentityProgressAndNoClobberRaces(t *testing.T) {
	content := []byte("immutable synthetic project audit export\n")
	h := sha256.Sum256(content)
	digest := hex.EncodeToString(h[:])
	tests := []struct {
		name   string
		mutate func(api.AuditExportChunk, int, string) api.AuditExportChunk
		decoy  bool
	}{
		{"wrong id", func(chunk api.AuditExportChunk, _ int, _ string) api.AuditExportChunk {
			chunk.ExportID = "aex_wrong"
			return chunk
		}, false},
		{"changing total", func(chunk api.AuditExportChunk, call int, _ string) api.AuditExportChunk {
			if call > 1 {
				chunk.Total++
			}
			return chunk
		}, false},
		{"zero progress", func(chunk api.AuditExportChunk, _ int, _ string) api.AuditExportChunk {
			chunk.Data, chunk.NextOffset, chunk.Complete = nil, chunk.Offset, false
			return chunk
		}, false},
		{"oversize", func(chunk api.AuditExportChunk, _ int, _ string) api.AuditExportChunk {
			chunk.Data = make([]byte, api.MaxAuditExportChunkBytes+1)
			chunk.NextOffset, chunk.Total = int64(len(chunk.Data)), int64(len(chunk.Data))
			return chunk
		}, false},
		{"destination race", func(chunk api.AuditExportChunk, call int, output string) api.AuditExportChunk {
			if call == 1 {
				_ = os.WriteFile(output, []byte("do not replace"), 0600)
			}
			return chunk
		}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "audit.json")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
				end := offset + 7
				if end > int64(len(content)) {
					end = int64(len(content))
				}
				chunk := api.AuditExportChunk{ExportID: "aex_0000000000000001", Offset: offset, NextOffset: end, Total: int64(len(content)), SHA256: digest, Data: content[offset:end], Complete: end == int64(len(content))}
				chunk = test.mutate(chunk, calls, output)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(chunk)
			}))
			defer server.Close()
			client, err := api.NewClient(server.URL, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err = downloadAuditExport(context.Background(), client, "tsk_0000000000000001", "aex_0000000000000001", output); err == nil {
				t.Fatal("invalid download succeeded")
			}
			if test.decoy {
				got, readErr := os.ReadFile(output)
				if readErr != nil || string(got) != "do not replace" {
					t.Fatalf("destination replaced: %q err=%v", got, readErr)
				}
			}
			partials, globErr := filepath.Glob(filepath.Join(filepath.Dir(output), ".audit.json.*.partial"))
			if globErr != nil || len(partials) != 0 {
				t.Fatalf("partials=%v err=%v", partials, globErr)
			}
		})
	}
}

func TestPostAuditFlagsAndRequestOnlyClassification(t *testing.T) {
	const task = "tsk_0000000000000001"
	var requests []api.PostMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/tasks/"+task+"/messages" {
			http.Error(w, "unexpected", 500)
			return
		}
		var req api.PostMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(api.Message{Seq: int64(len(requests)), TaskID: task, Text: req.Text})
	}))
	defer server.Close()
	e := env{hub: server.URL, task: task}
	if err := cmdPost(e, []string{"--request-id", "plain-key", "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdPost(e, []string{"--intake", "--request-id", "intake-key", "intake"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdPost(e, []string{"--request-id", "work-key", "--work-item", "wi_0000000000000002", "--work-item-revision", "3", "--work-order-message", "9", "--related", "tsk_0000000000000001/wi_0000000000000003@4", "work"}); err != nil {
		t.Fatal(err)
	}
	if requests[0].AuditKind != "" || len(requests[0].WorkItems) != 0 || requests[0].RequestID != "plain-key" {
		t.Fatalf("request-only became linked: %+v", requests[0])
	}
	if requests[1].AuditKind != api.MessageAuditIntake || len(requests[1].WorkItems) != 0 {
		t.Fatalf("intake: %+v", requests[1])
	}
	if requests[2].AuditKind != api.MessageAuditWork || len(requests[2].WorkItems) != 2 || requests[2].WorkItems[1].Relationship != "related" {
		t.Fatalf("work: %+v", requests[2])
	}
}
