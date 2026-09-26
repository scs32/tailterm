package main

import (
	"crypto/sha256"
	"encoding/hex"
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
