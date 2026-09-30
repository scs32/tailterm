package main

import (
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentSetupUsesExactRetryJournal(t *testing.T) {
	f := newHandlerFixture(t)
	f.req.Role = api.AgentRoleDeployment
	f.req.Name = "deployer"
	a, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.launch()
	if err != nil || again.RunID != a.RunID {
		t.Fatal(again, err)
	}
	f.executions(t, 1)
	if !strings.HasPrefix(a.Session, "tt-deployer-") {
		t.Fatal(a.Session)
	}
}
func TestDeploymentBriefingPreservesHandlerBoundary(t *testing.T) {
	b := deploymentBriefing()
	for _, text := range []string{"Never read or mutate work-item APIs", "exact-SHA", "private host credential", "persistent", "handler"} {
		if !strings.Contains(strings.ToLower(b), strings.ToLower(text)) {
			t.Fatal(text)
		}
	}
}

// Q2: supersede proves the job's commit shipped on the local tasks-hub.
func TestSupersedeAncestryRequiresHandReleaseOnTasksHub(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(args, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "tasks-hub")
	run("-c", "user.email=f@example.invalid", "-c", "user.name=F", "commit", "--allow-empty", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("-c", "user.email=f@example.invalid", "-c", "user.name=F", "commit", "--allow-empty", "-m", "candidate")
	candidate := run("rev-parse", "HEAD")
	run("checkout", "-b", "side", base)
	run("-c", "user.email=f@example.invalid", "-c", "user.name=F", "commit", "--allow-empty", "-m", "unreleased")
	unreleased := run("rev-parse", "HEAD")
	jobs := []api.ReleaseJob{{ID: "rel_hand", Commit: candidate}, {ID: "rel_side", Commit: unreleased}}
	if err := supersedeAncestry(dir, jobs, "rel_hand", candidate); err != nil {
		t.Fatal(err)
	}
	if err := supersedeAncestry(dir, jobs, "rel_side", unreleased); err == nil {
		t.Fatal("released commit not on tasks-hub was accepted")
	}
	if err := supersedeAncestry(dir, jobs, "rel_side", candidate); err == nil {
		t.Fatal("job commit not contained in the hand release was accepted")
	}
	if err := supersedeAncestry(dir, jobs, "rel_missing", candidate); err == nil {
		t.Fatal("missing job accepted")
	}
	// f5: a malformed released commit is refused before git runs, so an
	// option-shaped value cannot write a file.
	marker := filepath.Join(t.TempDir(), "written")
	for _, bad := range []string{"--output=" + marker, candidate[:12], strings.ToUpper(candidate), ""} {
		if err := supersedeAncestry(filepath.Join(t.TempDir(), "no-repo"), jobs, "rel_hand", bad); err == nil || !strings.Contains(err.Error(), "40-hex") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git ran with an option-shaped commit", err)
	}
}
