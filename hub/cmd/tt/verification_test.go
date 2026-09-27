package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestVerificationLogEvidenceRejectsTamperingAndMissingFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "check.log")
	content := []byte("isolated check passes\n")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	r := api.VerificationReceipt{Checks: []api.VerificationResult{{LogURI: p, LogDigest: hex.EncodeToString(sum[:])}}}
	if err := verifyReceiptLogs(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceiptLogs(r); err == nil {
		t.Fatal("altered log accepted")
	}
	os.Remove(p)
	if err := verifyReceiptLogs(r); err == nil {
		t.Fatal("missing log accepted")
	}
}
func TestVerificationCLIRequiresExactHandlerInput(t *testing.T) {
	if err := cmdVerification(env{}, []string{"history", "--item", "wi_aaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("unbound caller accepted")
	}
}

func TestVerificationTimeoutReceiptRoundTrip(t *testing.T) {
	raw := []byte(`{"checks":[{"id":"fixture","argv":["node","fixture.mjs"],"cwd":".","environment":{"VERIFICATION_TIMEOUT_MS":"1500"},"exitCode":124,"failureReason":"timeout"}]}`)
	var receipt api.VerificationReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Checks[0].FailureReason != "timeout" || receipt.Checks[0].ExitCode != 124 || receipt.Checks[0].Environment["VERIFICATION_TIMEOUT_MS"] != "1500" {
		t.Fatalf("timeout contract lost: %+v", receipt.Checks[0])
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var restored api.VerificationReceipt
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Checks[0].FailureReason != "timeout" {
		t.Fatal("native round trip lost timeout failure")
	}
	success, err := json.Marshal(api.VerificationResult{})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(success, &result); err != nil {
		t.Fatal(err)
	}
	if _, exists := result["failureReason"]; exists {
		t.Fatal("successful result includes failure reason")
	}
}

func TestVerificationRetryChecksEveryAttemptLog(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "first.log"), filepath.Join(dir, "final.log")}
	attempts := []api.VerificationAttempt{}
	for i, p := range paths {
		data := []byte(string(rune('a' + i)))
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		attempts = append(attempts, api.VerificationAttempt{Attempt: i + 1, ExitCode: 1 - i, LogURI: p, LogDigest: hex.EncodeToString(hash[:])})
	}
	r := api.VerificationReceipt{Checks: []api.VerificationResult{{Attempts: attempts, Status: "flaky", LogURI: attempts[1].LogURI, LogDigest: attempts[1].LogDigest}}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var restored api.VerificationReceipt
	if err = json.Unmarshal(raw, &restored); err != nil || len(restored.Checks[0].Attempts) != 2 || restored.Checks[0].Status != "flaky" {
		t.Fatal("retry roundtrip", err)
	}
	if err = verifyReceiptLogs(restored); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths[0], []byte("tampered first failure"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyReceiptLogs(restored); err == nil {
		t.Fatal("first failed attempt log tampering accepted")
	}
}
