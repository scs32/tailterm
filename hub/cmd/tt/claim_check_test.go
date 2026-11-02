package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
	// Outcome wording (review finding f1): zero counts of failures or errors
	// are passes, "not ok" is a failure, and an outcome read neither way is
	// never matched by a failed run.
	for _, c := range []struct {
		outcome string
		exit    int
		want    claimState
	}{
		{"no errors", 0, claimMatched},
		{"no errors", 1, claimContradicted},
		{"62 passed, 0 failed", 1, claimContradicted},
		{"62 passed, 0 failed", 0, claimMatched},
		{"pass, no errors", 0, claimMatched},
		{"4 pass 0 fail", 0, claimMatched},
		{"3 failed", 0, claimContradicted},
		{"3 failed", 1, claimMatched},
		{"not ok 3 - TestX", 0, claimContradicted},
		{"1 passed, 3 failed", 1, claimUnproven},
		{"62/62", 1, claimUnproven},
		{"62/62", 0, claimMatched},
	} {
		f := state(cited(c.outcome), claimEvidenceRow(cmd, claimExit(c.exit), "", at(1)))
		if f.state != c.want {
			t.Errorf("%q with exit %d: got %s %q, want %s", c.outcome, c.exit, claimStateNames[f.state], f.detail, claimStateNames[c.want])
		}
	}
	for text, want := range map[string]int{"no errors": 1, "0 failures": 1, "NO FAILED": 1, "not ok": -1, "ok": 1, "FAIL": -1, "62/62": 0, "": 0} {
		if got := claimOutcome(text); got != want {
			t.Errorf("claimOutcome(%q) = %d, want %d", text, got, want)
		}
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

// A check that runs out of budget is reported as skipped, never as clean or
// as "no ledger", even when the deadline has passed before the ledger is
// opened. The ledger's rows would match, so a clean report here would be a
// false negative.
func TestClaimCheckExpiredIsSkipped(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	cmd := "go test ./cmd/tt -count=1"
	claimLedgerDir(t, claimEvidenceRow(cmd, claimExit(0), claimCommitA, at))
	m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: "ok"}}, map[string]string{"commit": claimCommitA[:8]})
	if r, ok := claimRun(claimTestAgent, m, time.Now().Add(time.Minute)); !ok || r.skipped || len(r.findings) != 2 || r.failed() {
		t.Fatalf("live deadline: ok %v %+v", ok, r)
	}
	for i := 0; i < 100; i++ {
		r, ok := claimRun(claimTestAgent, m, time.Now().Add(-time.Second))
		if !ok || !r.skipped || !r.failed() || len(r.findings) != 0 {
			t.Fatalf("expired deadline, run %d: ok %v %+v", i, ok, r)
		}
	}
	// Through the budget: whichever of the run and the timer claimCheck
	// selects, a 1 ns budget is reported as skipped.
	old := claimCheckBudget
	claimCheckBudget = time.Nanosecond
	t.Cleanup(func() { claimCheckBudget = old })
	for i := 0; i < 100; i++ {
		if r, ok := claimCheck(claimTestAgent, m); !ok || !r.skipped {
			t.Fatalf("1 ns budget, run %d: ok %v %+v", i, ok, r)
		}
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

// A cited "exit N" is the claimed outcome and is compared exactly with the
// ledger's exit; the prose after it is not read. Entries without an exit keep
// the word rules. A run at another commit than the single cited one is never
// credited, even when its exit matches (lead decision L1).
func TestClaimCheckExitPrecedence(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	cmd := "go test ./cmd/tt"
	exitRow := func(n int) toolLedgerRow { return claimEvidenceRow(cmd, claimExit(n), "", at) }
	for _, c := range []struct {
		outcome string
		row     toolLedgerRow
		want    claimState
		detail  string
	}{
		{"exit 1; 2 passed", exitRow(0), claimContradicted, "you cited exit 1 but your latest run exited 0"},
		{"exit 0; 3 failed", exitRow(1), claimContradicted, "you cited exit 0 but your latest run exited 1"},
		{"exit 1", exitRow(1), claimMatched, "exit 1 at "},
		{"exit 0; ok", exitRow(0), claimMatched, "exit 0 at "},
		{"exit 2", exitRow(1), claimContradicted, "you cited exit 2 but your latest run exited 1"},
		{"exit 1", claimEvidenceRow(cmd, nil, "", at), claimUnproven, "exit not recorded"},
		{"ok, 0 failed", exitRow(0), claimMatched, "exit 0 at "},
		{"fail", exitRow(0), claimContradicted, "your latest run exited 0"},
	} {
		m := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: c.outcome}}, nil)
		f := claimFindingFor(t, claimRunFor(t, m, c.row), cmd)
		if f.state != c.want || !strings.Contains(f.detail, c.detail) {
			t.Errorf("%q: got %s %q, want %s containing %q", c.outcome, claimStateNames[f.state], f.detail, claimStateNames[c.want], c.detail)
		}
	}
	wrong := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: "exit 0"}}, map[string]string{"commit": "0a1b2c3"})
	f := claimFindingFor(t, claimRunFor(t, wrong, claimEvidenceRow(cmd, claimExit(0), claimCommitA, at)), cmd)
	if f.state != claimContradicted || !strings.Contains(f.detail, "ran at abcdef012345, not 0a1b2c3") {
		t.Errorf("explicit exit at another commit: %s %q", claimStateNames[f.state], f.detail)
	}
}

// claimCountRow is an evidence row whose run recorded counts from source, or
// none when counts is nil.
func claimCountRow(cmd string, counts *toolEvidenceCounts, source string, at time.Time) toolLedgerRow {
	row := claimEvidenceRow(cmd, claimExit(0), "", at)
	row.Evidence.Counts, row.Evidence.CountSource = counts, source
	if counts == nil {
		row.Evidence.CountSkip = "truncated"
	}
	return row
}

// claimCountFinding returns the report's count finding, if any.
func claimCountFinding(r claimReport) (claimFinding, bool) {
	for _, f := range r.findings {
		if f.claim.kind == claimCount {
			return f, true
		}
	}
	return claimFinding{}, false
}

// Counts (feature wi_512ca524b2432974): the cited-count grammar, then the
// comparison with the latest identical run's recorded counts.
func TestClaimCheckCounts(t *testing.T) {
	read := func(outcome string) string {
		var parts []string
		for _, n := range claimCounts(outcome) {
			parts = append(parts, fmt.Sprintf("%s=%d", n.field, n.n))
		}
		return strings.Join(parts, " ")
	}
	for outcome, want := range map[string]string{
		"62 tests":                     "total=62",
		"4 pass 0 skipped":             "pass=4 skip=0",
		"62 passed, 0 failed":          "pass=62 fail=0",
		"exit 0; 12 tests":             "total=12",
		"(3 skipped)":                  "skip=3",
		"1 pass,2 fail":                "pass=1 fail=2",
		"007 Tests":                    "total=7",
		"ok: 5 passing\t1 failure":     "pass=5 fail=1",
		"1234567890 tests, 0 failed":   "fail=0",
		"62/62 tests":                  "",
		"1.5 tests":                    "",
		"-3 tests":                     "",
		"x62 tests":                    "",
		"62tests":                      "",
		"62 testsuites":                "",
		"62 passedx":                   "",
		"1234567890 tests":             "",
		"62/62":                        "",
		"no failures":                  "",
		"ok":                           "",
		"exit 1":                       "",
		"3 packages ok, 2 tests later": "total=2",
	} {
		if got := read(outcome); got != want {
			t.Errorf("claimCounts(%q) = %q; want %q", outcome, got, want)
		}
	}

	t0 := time.Now().Add(-time.Hour)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	cmd := "go test ./cmd/tt -v -count=1"
	cited := func(outcome string) api.Envelope {
		return claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: outcome}}, nil)
	}
	v := func(total, pass, fail, skip int) *toolEvidenceCounts {
		return &toolEvidenceCounts{Total: total, Pass: pass, Fail: fail, Skip: skip}
	}
	for _, c := range []struct {
		name    string
		outcome string
		rows    []toolLedgerRow
		state   string // the count finding's state, or "" for none
		detail  string
		reasons []string
	}{
		{"total matched", "62 tests", []toolLedgerRow{claimCountRow(cmd, v(62, 60, 0, 2), "go-test-v", at(1))}, "matched", "total 62, pass 60, fail 0, skip 2 at ", nil},
		{"pass and skip matched", "4 pass 0 skipped", []toolLedgerRow{claimCountRow(cmd, v(6, 4, 1, 0), "node-summary", at(1))}, "matched", "", nil},
		{"fail mismatched", "62 passed, 0 failed", []toolLedgerRow{claimCountRow(cmd, v(63, 62, 1, 0), "go-test-v", at(1))}, "mismatched",
			"you cited 62 passed, 0 failed; your latest run recorded total 63, pass 62, fail 1, skip 0 at ", nil},
		{"latest run decides", "62 tests", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1)), claimCountRow(cmd, v(60, 60, 0, 0), "go-test-v", at(2))}, "mismatched",
			"you cited 62 tests; your latest run recorded total 60", nil},
		{"latest run decides, matched", "60 tests", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1)), claimCountRow(cmd, v(60, 60, 0, 0), "go-test-v", at(2))}, "matched", "", nil},
		{"run without counts", "62 tests", []toolLedgerRow{claimCountRow(cmd, nil, "", at(1))}, "", "", []string{claimReasonCountNone}},
		{"per-package counts", "3 passed", []toolLedgerRow{claimCountRow(cmd, v(3, 3, 0, 0), "go-test-packages", at(1))}, "", "", []string{claimReasonCountPackage}},
		{"no recorded run", "62 tests", []toolLedgerRow{claimCountRow("go test ./other -v", v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", []string{claimReasonCountRun}},
		{"no count cited", "62/62", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"no failures is not a count", "no failures", []toolLedgerRow{claimCountRow(cmd, v(1, 0, 1, 0), "go-test-v", at(1))}, "", "", nil},
		{"62/62 tests", "62/62 tests", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"1.5 tests", "1.5 tests", []toolLedgerRow{claimCountRow(cmd, v(5, 5, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"-3 tests", "-3 tests", []toolLedgerRow{claimCountRow(cmd, v(3, 3, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"x62 tests", "x62 tests", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"62tests", "62tests", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"62 testsuites", "62 testsuites", []toolLedgerRow{claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"ten digits", "1234567890 tests", []toolLedgerRow{claimCountRow(cmd, v(234567890, 0, 0, 0), "go-test-v", at(1))}, "", "", nil},
		{"ten digits beside a valid count", "1234567890 tests, 0 failed", []toolLedgerRow{claimCountRow(cmd, v(234567890, 1, 0, 0), "go-test-v", at(1))}, "matched", "", nil},
		{"exit then a count", "exit 0; 12 tests", []toolLedgerRow{claimCountRow(cmd, v(12, 12, 0, 0), "go-test-v", at(1))}, "matched", "", nil},
		{"parenthesised", "(3 skipped)", []toolLedgerRow{claimCountRow(cmd, v(3, 0, 0, 3), "node-summary", at(1))}, "matched", "", nil},
		{"comma without space", "1 pass,2 fail", []toolLedgerRow{claimCountRow(cmd, v(3, 1, 2, 0), "node-summary", at(1))}, "matched", "", nil},
	} {
		r := claimRunFor(t, cited(c.outcome), c.rows...)
		f, ok := claimCountFinding(r)
		switch {
		case c.state == "" && ok:
			t.Errorf("%s: unexpected count finding %+v", c.name, f)
		case c.state != "" && (!ok || claimStateNames[f.state] != c.state || !strings.Contains(f.detail, c.detail)):
			t.Errorf("%s: count finding %+v (found %v); want %s with %q", c.name, f, ok, c.state, c.detail)
		}
		if fmt.Sprint(r.reasons) != fmt.Sprint(c.reasons) || r.notChecked != len(c.reasons) {
			t.Errorf("%s: not checked %d %v; want %v", c.name, r.notChecked, r.reasons, c.reasons)
		}
		// The command's own finding is unchanged by the counts.
		if cmdF := claimFindingFor(t, r, cmd); cmdF.claim.kind != claimCommand {
			t.Errorf("%s: first finding %+v", c.name, cmdF)
		}
	}

	// A cited command that is not one isolated command adds only the command
	// reason, counts or not.
	pipe := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd + " | tee out", Outcome: "62 tests"}}, nil)
	if r := claimRunFor(t, pipe, claimCountRow(cmd, v(62, 62, 0, 0), "go-test-v", at(1))); len(r.findings) != 0 || fmt.Sprint(r.reasons) != fmt.Sprint([]string{claimReasonCommand}) || r.notChecked != 1 {
		t.Errorf("piped command: %+v", r)
	}

	// A lead's accept disposition is checked the same way, and the check
	// never changes the envelope.
	accept := api.Envelope{Kind: "notice", Subject: "Accept the candidate", Body: api.EnvelopeBody{Text: "Accepted"},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: "62 passed, 0 failed"}},
		Review:   &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: claimCommitA}}
	if !claimCheckable(accept) {
		t.Fatal("an accept disposition is not checkable")
	}
	for _, m := range []api.Envelope{accept, cited("62 passed, 0 failed")} {
		before, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		r := claimRunFor(t, m, claimCountRow(cmd, v(63, 62, 1, 0), "go-test-v", at(1)))
		if f, ok := claimCountFinding(r); !ok || f.state != claimMismatched {
			t.Errorf("%s: count finding %+v, %v", m.Kind, f, ok)
		}
		if after, _ := json.Marshal(m); !bytes.Equal(before, after) {
			t.Errorf("%s: the check changed the envelope:\n%s\n%s", m.Kind, before, after)
		}
	}
}

// Counts in the report: mismatched sorts after contradicted, a matched count
// counts in the one-line summary, and --claims-only exits 1 on a mismatch.
func TestClaimCheckCountsReport(t *testing.T) {
	cmd := "go test ./cmd/tt -v"
	command := citedClaim{kind: claimCommand, source: "e1", text: cmd, outcome: "62 tests"}
	count := command
	count.kind, count.counts = claimCount, claimCounts("62 tests")
	r := claimReport{findings: []claimFinding{
		{claim: command, state: claimMatched, detail: "exit 0 at T"},
		{claim: count, state: claimMismatched, detail: "you cited 62 tests; your latest run recorded total 60, pass 60, fail 0, skip 0 at T"},
		{claim: citedClaim{kind: claimCommand, source: "e2", text: "go vet ./cmd/tt", outcome: "ok"}, state: claimContradicted, detail: "your latest run exited 1 at T"},
	}}
	var out bytes.Buffer
	writeClaimReport(&out, r, false)
	want := "claim check (your local tool ledger; warning only, the message is sent unchanged):\n" +
		"  contradicted  e2 go vet ./cmd/tt -> ok: your latest run exited 1 at T\n" +
		"  mismatched    e1 counts 62 tests (go test ./cmd/tt -v): you cited 62 tests; your latest run recorded total 60, pass 60, fail 0, skip 0 at T\n" +
		"  matched       e1 go test ./cmd/tt -v -> 62 tests: exit 0 at T\n"
	if out.String() != want {
		t.Fatalf("report:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	matched := claimReport{findings: []claimFinding{r.findings[0], {claim: count, state: claimMatched}}}
	matched.skip(claimReasonCountNone)
	writeClaimReport(&out, matched, false)
	if out.String() != "claim check: 2 cited claims match your tool ledger; 1 not checked ("+claimReasonCountNone+")\n" {
		t.Fatalf("summary %q", out.String())
	}

	// --claims-only through its own path: exit 1 on a mismatch, nil when the
	// counts are only not checked.
	env := claimResult(map[string]api.Evidence{"e1": {Type: "command", Value: cmd, Outcome: "62 tests"}}, nil)
	claimLedgerDir(t, claimCountRow(cmd, &toolEvidenceCounts{Total: 60, Pass: 60}, "go-test-v", time.Now().Add(-time.Minute)))
	var exit *exitError
	if err := claimsOnlyReport(claimTestAgent, env); !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("claims-only on a mismatch = %v", err)
	}
	claimLedgerDir(t, claimCountRow(cmd, nil, "", time.Now().Add(-time.Minute)))
	if err := claimsOnlyReport(claimTestAgent, env); err != nil {
		t.Fatalf("claims-only with counts not checked = %v", err)
	}
}
