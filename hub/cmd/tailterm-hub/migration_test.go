package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestMigrationOnlyStartsNoServices(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "hub")
	cmd := exec.Command("go", "build", "-o", binary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	cmd = exec.Command(binary, "--migrate-only", filepath.Join(dir, "backup-copy.sqlite"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TAILTERM_STATE=" + filepath.Join(dir, "forbidden-state"), "TAILTERM_DEV_LISTEN=not-a-listener"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "listening") {
		t.Fatal(string(out), err)
	}
	if _, err = os.Stat(filepath.Join(dir, "forbidden-state")); !os.IsNotExist(err) {
		t.Fatal("state/service startup occurred", err)
	}
}

// The usage digest mode opens only the supplied file, read-only: it starts no
// service, creates and changes no file, refuses a database that is open
// elsewhere, and neither it nor the cleanup that migrate-only runs prints
// anything a message, a turn, an upload batch or a launch plan holds.
func TestUsageDigestStartsNoServicesChangesNoFileAndPrintsNoContent(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "hub")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	const markerBody, markerTurn, markerBatch, markerLaunch = "digest-marker-body", "digest-marker-turn", "digest-marker-batch", "digest-marker-launch"
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	data := filepath.Join(dir, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(data, "backup-copy.sqlite")
	st, err := store.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Digest fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "metered", AgentID: api.NewID("agt"), Runtime: "codex", Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: markerBody, RequestID: "marker-body"}, by); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	batch := api.UsageBatch{Version: 1, RequestID: "batch-one", RunID: agent.RunID, Session: "synthetic-session", StartedAt: at.Add(-time.Hour), Coverage: markerBatch, Turns: []api.UsageTurn{{
		ID: "turn-one", Revision: 1, Runtime: "codex", Session: "synthetic-session", Model: markerTurn, At: at,
		Tokens:       map[string]int64{"input": 21, "cached": 80, "cacheWrite": 0, "output": 7, "reasoning": 2},
		Raw:          map[string]int64{"input_tokens": 101, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 9, "reasoning_output_tokens": 2},
		SourceDigest: strings.Repeat("a", 64), Activation: "activation-one", Complete: true,
	}}}
	if _, err = st.ReportUsage(ctx, task.ID, agent.ID, batch); err != nil {
		t.Fatal(err)
	}

	run := func(mode string) (string, string, error) {
		t.Helper()
		cmd := exec.Command(binary, mode, file)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TAILTERM_STATE=" + filepath.Join(dir, "forbidden-state"), "TAILTERM_DEV_LISTEN=not-a-listener"}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	clean := func(what, output string) {
		t.Helper()
		for i, secret := range []string{markerBody, markerTurn, markerBatch, markerLaunch, task.ID, agent.ID, agent.RunID, file, "listening"} {
			if strings.Contains(output, secret) {
				t.Fatalf("%s carries content %d (%d bytes of output)", what, i, len(output))
			}
		}
	}

	// The store is still open and has written: the digest refuses the file.
	stdout, stderr, err := run("--usage-digest")
	if err == nil || stdout != "" || !strings.Contains(stderr, "usage digest refused") {
		t.Fatalf("a database open elsewhere was not refused: %d bytes printed, %v", len(stdout), err)
	}
	clean("the refusal", stderr)
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	// Put the rows back into the form an earlier hub stored: the turn twice
	// more, the whole upload batch, a queue result holding its launch plan.
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	var projection []byte
	if err = db.QueryRow(`SELECT projection FROM usage_turns`).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Turn json.RawMessage `json:"turn"`
	}
	if err = json.Unmarshal(projection, &stored); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE usage_turns SET payload=?`, []any{string(stored.Turn)}},
		{`INSERT INTO usage_turn_revisions SELECT task_id,agent_id,run_id,request_id,revision,payload,projection FROM usage_turns`, nil},
		{`UPDATE usage_receipts SET payload=?`, []any{string(raw)}},
		{`INSERT INTO team_queue_requests(task_id,request_id,payload_hash,result_json,launch_digest) VALUES(?,'old-result','hash',?,'')`, []any{task.ID, []byte(`{"id":"tqe_fixture", "launch":{"name":"` + markerLaunch + `"}}`)}},
		{`DELETE FROM storage_migrations`, nil},
	} {
		if _, err = db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	listing := func() string {
		t.Helper()
		entries, err := os.ReadDir(data)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(data, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s %d %x %d", e.Name(), len(b), sha256.Sum256(b), info.ModTime().UnixNano()))
		}
		return strings.Join(out, "\n")
	}
	files := listing()
	before, stderr, err := run("--usage-digest")
	if err != nil || stderr != "" {
		t.Fatalf("usage digest: %v, %d bytes on stderr", err, len(stderr))
	}
	again, _, err := run("--usage-digest")
	if err != nil || again != before {
		t.Fatal("two digests of one file differ", err)
	}
	if listing() != files || !strings.HasPrefix(files, "backup-copy.sqlite ") || strings.Contains(files, "\n") {
		t.Fatal("the digest created or changed a file beside the database")
	}
	clean("the digest", before)
	for _, want := range []string{"file size=", "\nform usage_turns new=0 old=1\n", "\nform usage_receipts new=0 old=1\n", "\nexport omits=work_item_history_state.checked_at\n", "\nproject "} {
		if !strings.Contains(before, want) {
			t.Fatalf("the digest lacks %q", want)
		}
	}
	if strings.Contains(before, "\nstep ") {
		t.Fatal("step lines for a database no cleanup has opened")
	}

	// Migrate-only runs the cleanup; its log lines are counts.
	stdout, stderr, err = run("--migrate-only")
	if err != nil {
		t.Fatalf("migrate-only: %v, %d bytes on stderr", err, len(stderr))
	}
	clean("migrate-only", stdout+stderr)
	for _, want := range []string{"storage cleanup revisions: examined 1, converted 1, left 0", "storage cleanup turn-payload: examined 1, converted 1, left 0", "storage cleanup receipt-batch: examined 1, converted 1, left 0", "storage cleanup queue-launch: examined 1, converted 0, left 1"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("migrate-only did not log %q", want)
		}
	}
	after, stderr, err := run("--usage-digest")
	if err != nil || stderr != "" {
		t.Fatalf("usage digest after the cleanup: %v, %d bytes on stderr", err, len(stderr))
	}
	clean("the digest after the cleanup", after)
	for _, want := range []string{"\nform usage_turns new=1 old=0\n", "\nform usage_receipts new=1 old=0\n", "\nstep revisions state=done ", "\nstep queue-launch state=done "} {
		if !strings.Contains(after, want) {
			t.Fatalf("the digest after the cleanup lacks %q", want)
		}
	}
	if _, err = os.Stat(filepath.Join(dir, "forbidden-state")); !os.IsNotExist(err) {
		t.Fatal("state/service startup occurred", err)
	}
}
