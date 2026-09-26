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
