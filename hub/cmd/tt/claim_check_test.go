package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Every claim checker test reads a ledger it wrote under t.TempDir() through
// TAILTERM_TOOL_LEDGER_DIR; none reads a live agent's ledger.

const claimTestAgent = "agt_0000000000000c1a"

func claimExit(n int) *int { return &n }

// claimEvidenceRow is one evidence row of the test agent's ledger.
func claimEvidenceRow(cmd string, exit *int, commit string, at time.Time) toolLedgerRow {
	return toolLedgerRow{V: 1, Time: at.UTC().Format(time.RFC3339Nano), Task: "tsk_0000000000000001", Agent: claimTestAgent,
		Run: "run_0000000000000001", Session: "s", Tool: "Bash", ToolUseID: "toolu_1", Outcome: "ok",
		Evidence: &toolEvidence{Kinds: toolEvidenceKinds(cmd, toolEvidenceCommandBytes), Command: cmd, ExitCode: exit, Commit: commit}}
}

// claimLedgerDir points the ledger root at a new temp directory holding the
// rows as the agent's ledger.jsonl, and returns the root.
func claimLedgerDir(t *testing.T, rows ...toolLedgerRow) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tool-ledger")
	if err := os.MkdirAll(filepath.Join(root, claimTestAgent), 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, claimTestAgent, "ledger.jsonl"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAILTERM_TOOL_LEDGER_DIR", root)
	return root
}

func claimResult(evidence map[string]api.Evidence, refs map[string]string) api.Envelope {
	return api.Envelope{Kind: "result", Subject: "Claims to check", Refs: refs, Evidence: evidence,
		Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}}
}

func claimRunFor(t *testing.T, m api.Envelope, rows ...toolLedgerRow) claimReport {
	t.Helper()
	claimLedgerDir(t, rows...)
	r, ok := claimRun(claimTestAgent, m, time.Now().Add(time.Minute))
	if !ok {
		t.Fatal("the test ledger was not read")
	}
	return r
}

// claimFindingFor returns the finding whose claim text is text.
func claimFindingFor(t *testing.T, r claimReport, text string) claimFinding {
	t.Helper()
	for _, f := range r.findings {
		if f.claim.text == text {
			return f
		}
	}
	t.Fatalf("no finding for %q in %+v", text, r.findings)
	return claimFinding{}
}

const (
	claimCommitA = "abcdef0123456789abcdef0123456789abcdef01"
	claimCommitB = "0123456789abcdef0123456789abcdef01234567"
)

// Criterion a2.
func TestClaimCheckCommit(t *testing.T) {
	now := time.Now()
	moved := claimEvidenceRow("cd hub && go test ./...", claimExit(0), "", now)
	moved.Evidence.CommitSkip = "changed-directory"
	rows := []toolLedgerRow{
		claimEvidenceRow("go test ./cmd/tt", claimExit(0), claimCommitA, now),
		moved,
		claimEvidenceRow("go vet ./cmd/tt", claimExit(0), "", now),
	}
	m := claimResult(map[string]api.Evidence{"e9": {Type: "commit", Value: "ABCDEF0123"}},
		map[string]string{"commit": "abcdef01", "other": "fedcba9876", "entry": "1234567", "base": "short"})
	r := claimRunFor(t, m, rows...)
	if f := claimFindingFor(t, r, "abcdef01"); f.state != claimMatched {
		t.Errorf("cited prefix: %+v", f)
	}
	if f := claimFindingFor(t, r, "abcdef0123"); f.state != claimMatched || f.claim.source != "e9" {
		t.Errorf("commit evidence, upper case: %+v", f)
	}
	f := claimFindingFor(t, r, "fedcba9876")
	if f.state != claimUnmatched || !strings.Contains(f.detail, "2 of your runs recorded no commit") {
		t.Errorf("absent commit: %+v", f)
	}
	if len(r.findings) != 3 {
		t.Errorf("a run of digits or a short value is not a commit: %+v", r.findings)
	}
	// A candidate is a commit claim too, and a duplicate is checked once.
	m = claimResult(nil, map[string]string{"commit": claimCommitB})
	m.Body.Candidate = strings.ToUpper(claimCommitB)
	r = claimRunFor(t, m, rows...)
	if len(r.findings) != 1 || r.findings[0].state != claimUnmatched {
		t.Errorf("candidate: %+v", r.findings)
	}
}

// Criterion a3, the isolation amendment included.
func TestClaimCheckCommand(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	cmd := "go test ./cmd/tt"
	cited := func(outcome string) api.Envelope {
		return claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: outcome}}, nil)
	}
	state := func(m api.Envelope, rows ...toolLedgerRow) claimFinding {
		t.Helper()
		r := claimRunFor(t, m, rows...)
		if len(r.findings) != 1 {
			t.Fatalf("findings %+v", r.findings)
		}
		return r.findings[0]
	}
	if f := state(cited("ok"), claimEvidenceRow(cmd, claimExit(0), claimCommitA, at(1))); f.state != claimMatched {
		t.Errorf("identical pass: %+v", f)
	}
	f := state(cited("ok"), claimEvidenceRow(cmd, claimExit(0), "", at(1)), claimEvidenceRow(cmd, claimExit(1), "", at(2)))
	if f.state != claimContradicted || !strings.Contains(f.detail, "your latest run exited 1") {
		t.Errorf("pass claim, latest failed: %+v", f)
	}
	// The latest by time decides, not the position in the file.
	if f := state(cited("ok"), claimEvidenceRow(cmd, claimExit(0), "", at(3)), claimEvidenceRow(cmd, claimExit(1), "", at(2))); f.state != claimMatched {
		t.Errorf("latest by time: %+v", f)
	}
	if f := state(cited("FAIL: TestX"), claimEvidenceRow(cmd, claimExit(0), "", at(1))); f.state != claimContradicted {
		t.Errorf("failure claim, exit 0: %+v", f)
	}
	if f := state(cited("ok"), claimEvidenceRow(cmd, nil, "", at(1))); f.state != claimUnproven || f.detail != "exit not recorded" {
		t.Errorf("no exit: %+v", f)
	}
	withCommit := cited("ok")
	withCommit.Refs = map[string]string{"commit": "0a1b2c3"}
	if f := claimFindingFor(t, claimRunFor(t, withCommit, claimEvidenceRow(cmd, claimExit(0), claimCommitA, at(1))), cmd); f.state != claimContradicted ||
		!strings.Contains(f.detail, "ran at abcdef012345, not 0a1b2c3") {
		t.Errorf("single cited commit differs: %+v", f)
	}
	// Not checked: a command that is not a verification command, or that is
	// not one isolated simple command.
	for _, text := range []string{"ls -la", "cd hub && go test ./...", "go test ./... | tail -3", "go test \"./...$X\""} {
		m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: text, Outcome: "ok"}}, nil)
		r := claimRunFor(t, m, claimEvidenceRow(text, claimExit(0), "", at(1)))
		if len(r.findings) != 0 || r.notChecked != 1 {
			t.Errorf("%q should not be checked: %+v", text, r)
		}
	}
	// Negative cases: none of these rows, all exit 0, credits go test ./...
	plain := "go test ./..."
	for _, row := range []string{
		"go test ./... -run TestOne",
		"false && go test ./...; true",
		"cd hub && go test ./...",
		"go test ./... | tail -3",
		"out=$(go test ./...)",
		"go test ./... &",
		"go test ./...; ((1))",
		"go test ./... & # note",
		"go test ./... # x",
		"go test ./... > out.txt",
		"go test ./... $(true)",
		"go test \"./...$X\"",
		"go test './...",
	} {
		m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: plain, Outcome: "ok"}}, nil)
		if f := state(m, claimEvidenceRow(row, claimExit(0), "", at(1))); f.state != claimUnmatched {
			t.Errorf("row %q credited %q: %+v", row, plain, f)
		}
		if row != "go test ./... -run TestOne" && claimIsolated(row) {
			t.Errorf("row %q passed the isolation guard", row)
		}
	}
	reverse := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: "go test ./... -run TestOne", Outcome: "ok"}}, nil)
	if f := state(reverse, claimEvidenceRow(plain, claimExit(0), "", at(1))); f.state != claimUnmatched {
		t.Errorf("reverse: %+v", f)
	}
	// Positive: quoted metacharacters stay allowed.
	filter := "go test ./cmd/tt -run 'ClaimCheck|Send|Ledger' -count=1"
	m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: filter, Outcome: "ok"}}, nil)
	if f := state(m, claimEvidenceRow(filter, claimExit(0), "", at(1))); f.state != claimMatched {
		t.Errorf("quoted filter: %+v", f)
	}
}

func claimWriteReceipt(t *testing.T, dir, name, commit string, exit int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	body := fmt.Sprintf(`{"commit":%q,"environment":{},"checks":[{"id":"go","exitCode":0},{"id":"unit","exitCode":%d}]}`, commit, exit)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Criterion a4.
func TestClaimCheckReceipt(t *testing.T) {
	base := t.TempDir()
	at := time.Now()
	run := filepath.Join(base, "out", "run")
	good := claimWriteReceipt(t, run, "receipt.json", claimCommitA, 0)
	bad := claimWriteReceipt(t, filepath.Join(base, "bad"), "receipt.json", claimCommitA, 1)
	targetedDir := filepath.Join(base, "targeted")
	targeted := claimWriteReceipt(t, targetedDir, "targeted-receipt.json", claimCommitA, 0)
	other := claimWriteReceipt(t, filepath.Join(base, "out", "run-other"), "receipt.json", claimCommitA, 0)
	matrix := func(mode, output string) string {
		return "node scripts/verify-matrix.mjs " + mode + " /plans/plan.json " + output
	}
	cite := func(path string) api.Envelope {
		return claimResult(nil, map[string]string{"receipt": path})
	}
	state := func(m api.Envelope, rows ...toolLedgerRow) claimFinding {
		t.Helper()
		r := claimRunFor(t, m, rows...)
		if len(r.findings) != 1 {
			t.Fatalf("findings %+v (not checked %d)", r.findings, r.notChecked)
		}
		return r.findings[0]
	}
	for name, c := range map[string]struct {
		receipt string
		row     toolLedgerRow
		want    claimState
	}{
		"run":                   {good, claimEvidenceRow(matrix("run", run), claimExit(0), claimCommitA, at), claimMatched},
		"run wrapped":           {good, claimEvidenceRow("node scripts/verify-matrix-host-lock.mjs exec --item wi_1 -- node scripts/verify-matrix.mjs run /p.json "+run, claimExit(0), "", at), claimMatched},
		"targeted":              {targeted, claimEvidenceRow(matrix("targeted", targetedDir), claimExit(0), claimCommitA, at), claimMatched},
		"exit 0, not eligible":  {bad, claimEvidenceRow(matrix("run", filepath.Dir(bad)), claimExit(0), "", at), claimContradicted},
		"exit 1, eligible":      {good, claimEvidenceRow(matrix("run", run), claimExit(1), "", at), claimContradicted},
		"commit differs":        {good, claimEvidenceRow(matrix("run", run), claimExit(0), claimCommitB, at), claimContradicted},
		"no exit":               {good, claimEvidenceRow(matrix("run", run), nil, "", at), claimUnproven},
		"plan row":              {good, claimEvidenceRow(matrix("plan", run), claimExit(0), "", at), claimUnmatched},
		"run vs run-other":      {other, claimEvidenceRow(matrix("run", run), claimExit(0), "", at), claimUnmatched},
		"run-other vs run":      {good, claimEvidenceRow(matrix("run", filepath.Dir(other)), claimExit(0), "", at), claimUnmatched},
		"relative output":       {good, claimEvidenceRow(matrix("run", "out/run"), claimExit(0), "", at), claimUnmatched},
		"run row for targeted":  {targeted, claimEvidenceRow(matrix("run", targetedDir), claimExit(0), "", at), claimUnmatched},
		"compound run row":      {good, claimEvidenceRow("cd / && "+matrix("run", run), claimExit(0), "", at), claimUnmatched},
		"backgrounded run row":  {good, claimEvidenceRow(matrix("run", run)+" &", claimExit(0), "", at), claimUnmatched},
		"isolation: redirected": {good, claimEvidenceRow(matrix("run", run)+" > log.txt", claimExit(0), "", at), claimUnmatched},
	} {
		if f := state(cite(c.receipt), c.row); f.state != c.want {
			t.Errorf("%s: got %s %q, want %s", name, claimStateNames[f.state], f.detail, claimStateNames[c.want])
		}
	}
	// A single cited commit the receipt does not name.
	m := cite(good)
	m.Refs["commit"] = "0a1b2c3"
	r := claimRunFor(t, m, claimEvidenceRow(matrix("run", run), claimExit(0), "", at))
	if f := claimFindingFor(t, r, good); f.state != claimContradicted {
		t.Errorf("cited commit: %+v", f)
	}
	// Not checked: no path, a missing file, an undecodable file, another
	// file name, and a file over the 1 MiB limit, which is not read past it.
	undecodable := filepath.Join(base, "junk", "receipt.json")
	_ = os.MkdirAll(filepath.Dir(undecodable), 0o700)
	_ = os.WriteFile(undecodable, []byte("{not json"), 0o600)
	huge := filepath.Join(base, "huge", "receipt.json")
	_ = os.MkdirAll(filepath.Dir(huge), 0o700)
	_ = os.WriteFile(huge, []byte(`{"commit":"`+claimCommitA+`","pad":"`+strings.Repeat("x", api.MaxVerificationBody)+`","checks":[]}`), 0o600)
	for _, v := range []string{"generation 3", filepath.Join(base, "missing", "receipt.json"), undecodable, huge, filepath.Join(base, "out", "run", "summary.json")} {
		r := claimRunFor(t, cite(v), claimEvidenceRow(matrix("run", filepath.Dir(v)), claimExit(0), "", at))
		if len(r.findings) != 0 || r.notChecked != 1 {
			t.Errorf("%s should not be checked: %+v", v, r)
		}
	}
	// An attachment named like a receipt is a receipt citation.
	att := claimResult(nil, nil)
	att.Attachments = []string{good, "/some/notes.md"}
	if f := state(att, claimEvidenceRow(matrix("run", run), claimExit(0), "", at)); f.state != claimMatched || f.claim.source != "attachment" {
		t.Errorf("attachment: %+v", f)
	}
}

// Criterion a6, the check's half: a large ledger inside the budget, the
// budget's timeout, and malformed lines.
func TestClaimCheckBounded(t *testing.T) {
	root := claimLedgerDir(t)
	dir := filepath.Join(root, claimTestAgent)
	at := time.Now().Add(-time.Hour)
	fill := func(name string, size int, last toolLedgerRow) {
		var buf bytes.Buffer
		plain, _ := json.Marshal(toolLedgerRow{V: 1, Time: at.Format(time.RFC3339Nano), Agent: claimTestAgent, Tool: "Read", Outcome: "ok", ArgsDigest: strings.Repeat("a", 64)})
		ev, _ := json.Marshal(claimEvidenceRow("go test ./internal/store -run TestSomething -count=1", claimExit(0), claimCommitB, at))
		// One line in eight is a verification run, already far more than a
		// working session's ledger holds, and one in eight is malformed.
		for i := 0; buf.Len() < size; i++ {
			switch i % 8 {
			case 0:
				buf.Write(ev)
			case 1:
				buf.WriteString(`{"v":1,"evidence":{"command":`) // malformed
			default:
				buf.Write(plain)
			}
			buf.WriteByte('\n')
		}
		line, _ := json.Marshal(last)
		buf.Write(line)
		buf.WriteByte('\n')
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := "go test ./cmd/tt -count=1"
	fill("ledger.jsonl.1", 2<<20, claimEvidenceRow(cmd, claimExit(1), "", at))
	fill("ledger.jsonl", 8<<20, claimEvidenceRow(cmd, claimExit(0), claimCommitA, at.Add(time.Minute)))
	m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: "ok"}}, map[string]string{"commit": claimCommitA[:8]})
	start := time.Now()
	r, ok := claimCheck(claimTestAgent, m)
	if !ok || r.skipped || len(r.findings) != 2 || r.failed() {
		t.Fatalf("8 MiB ledger: ok %v %+v after %s", ok, r, time.Since(start))
	}
	t.Logf("8 MiB plus a 2 MiB rotated ledger checked in %s", time.Since(start))
	old := claimCheckBudget
	claimCheckBudget = time.Nanosecond
	t.Cleanup(func() { claimCheckBudget = old })
	r, ok = claimCheck(claimTestAgent, m)
	var out bytes.Buffer
	writeClaimReport(&out, r, false)
	if !ok || !r.skipped || !r.failed() || out.String() != "claim check skipped: over 1ns\n" {
		t.Fatalf("1 ns budget: %v %+v %q", ok, r, out.String())
	}
	// The send half: with the budget at 1 ns the send prints the skip and posts.
	hub := newClaimHub(t, false)
	e := env{hub: hub.url, task: "tsk_0000000000000001", agent: claimTestAgent}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	_, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "result", "--subject", "Claims to check under a tiny budget", "--outcome", "done",
			"--status", "a1=pass", "--evidence", "e1: " + cmd + " -> ok"})
	})
	restore()
	if err != nil || !strings.Contains(stderr.String(), "claim check skipped") || len(hub.posts()) != 1 {
		t.Fatalf("send under a 1 ns budget: %v %q posts %d", err, stderr.String(), len(hub.posts()))
	}
}

func TestClaimCheckReportFormat(t *testing.T) {
	c := func(kind claimKind, text string) citedClaim {
		return citedClaim{kind: kind, source: "e1", text: text, outcome: "ok"}
	}
	r := claimReport{findings: []claimFinding{
		{claim: c(claimReceipt, "/abs/out/receipt.json"), state: claimMatched},
		{claim: c(claimCommand, "go vet ./cmd/tt"), state: claimUnproven, detail: "exit not recorded"},
		{claim: c(claimCommand, "go test ./cmd/tt"), state: claimContradicted, detail: "your latest run exited 1 at T"},
	}}
	r.skip(claimReasonCommand)
	var out bytes.Buffer
	writeClaimReport(&out, r, false)
	want := "claim check (your local tool ledger; warning only, the message is sent unchanged):\n" +
		"  contradicted  e1 go test ./cmd/tt -> ok: your latest run exited 1 at T\n" +
		"  unproven      e1 go vet ./cmd/tt -> ok: exit not recorded\n" +
		"  matched       receipt /abs/out/receipt.json (e1)\n" +
		"  not checked   1 (" + claimReasonCommand + ")\n"
	if out.String() != want {
		t.Fatalf("report:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	writeClaimReport(&out, claimReport{findings: r.findings[:1]}, false)
	if out.String() != "claim check: 1 cited claim matches your tool ledger\n" {
		t.Fatalf("summary %q", out.String())
	}
	out.Reset()
	writeClaimReport(&out, claimReport{notChecked: 2, reasons: []string{claimReasonCommand}}, false)
	if out.Len() != 0 {
		t.Fatalf("a send with nothing checkable is silent, got %q", out.String())
	}
}
