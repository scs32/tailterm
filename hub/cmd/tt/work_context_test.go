package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestFormatWorkItemContextIsExplicitlyScoped(t *testing.T) {
	context := api.AgentWorkItemContext{
		Version: 1,
		Binding: api.AgentWorkItemBinding{
			AgentID: "agt_0000000000000001", RunID: "run_0000000000000002",
			ItemTaskID: "tsk_0000000000000003", ItemID: "wi_0000000000000004", ItemRevision: 7,
			ContextThroughMessageSeq: 816,
		},
		Bundle: []byte(`{"version":1,"history":{"description":"Only relevant content"}}`),
	}
	digest := sha256.Sum256(context.Bundle)
	context.Binding.ContextDigest = hex.EncodeToString(digest[:])
	formatted, err := formatWorkItemContext(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"exact agent run", "intentionally excluded", "contextThroughMessageSeq", "816", "Only relevant content"} {
		if !strings.Contains(formatted, required) {
			t.Fatalf("formatted context missing %q: %s", required, formatted)
		}
	}
}

func TestPreparedWorkContextByteBoundariesAndRestoredDigest(t *testing.T) {
	for _, size := range []int{152973, api.MaxAgentWorkItemContextBytes, api.MaxAgentWorkItemContextBytes + 1} {
		overhead := len(`{"source":""}`)
		count := size - overhead
		data := []byte(`{"source":"` + strings.Repeat("界", count/3) + strings.Repeat("x", count%3) + `"}`)
		path := filepath.Join(t.TempDir(), "synthetic.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		for _, file := range []bool{false, true} {
			inline, filename := string(data), ""
			if file {
				inline, filename = "", path
			}
			got, err := readPreparedWorkContext(inline, filename)
			if size > api.MaxAgentWorkItemContextBytes {
				if !errors.Is(err, api.ErrContextLimit) {
					t.Fatalf("over-limit = %v", err)
				}
				continue
			}
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("complete source changed: %v", err)
			}
		}
	}
	bundle := []byte(`{"source": "<>&界", "n":9007199254740993}`)
	hash := sha256.Sum256(bundle)
	ctx := api.AgentWorkItemContext{Version: 1, Bundle: bundle, Binding: api.AgentWorkItemBinding{ContextDigest: hex.EncodeToString(hash[:])}}
	formatted, err := formatWorkItemContext(ctx)
	if err != nil || !strings.Contains(formatted, string(bundle)) {
		t.Fatalf("format lost raw source: %v", err)
	}
	ctx.Bundle = json.RawMessage(`{"source":"changed"}`)
	if _, err := formatWorkItemContext(ctx); err == nil {
		t.Fatal("changed immutable digest accepted")
	}
	for _, invalid := range []string{"{broken", string([]byte{'"', 0xff, '"'})} {
		if _, err := readPreparedWorkContext(invalid, ""); err == nil {
			t.Fatal("invalid UTF-8 JSON accepted")
		}
	}
}

func TestWrapCompleteQuotedContextFileBound(t *testing.T) {
	const limit = 4*api.MaxAgentWorkItemContextBytes + 64*1024
	for _, size := range []int{limit, limit + 1} {
		path := filepath.Join(t.TempDir(), "synthetic-command")
		data := strings.Repeat("'", size)
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := wrapCommand([]string{"--shell-file", path})
		if size > limit {
			if err == nil {
				t.Fatal("oversized command accepted")
			}
			continue
		}
		if err != nil || got != data {
			t.Fatalf("complete bounded command changed: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("consumed command retained")
		}
	}
}
