package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
