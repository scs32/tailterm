package main

import (
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// wi_5b03fe47520b7c4f p1: setup provisions the matrix prerequisites in the
// deployer's checkout before it spawns the role, and spawns nothing otherwise.
func TestDeploymentSetupProvisionsPrerequisitesBeforeSpawn(t *testing.T) {
	var provisioned [][2]string
	var spawned [][]string
	provisionErr := error(nil)
	restoreProvision, restoreSpawn := provisionDeploymentPrerequisites, spawnDeploymentAgent
	t.Cleanup(func() { provisionDeploymentPrerequisites, spawnDeploymentAgent = restoreProvision, restoreSpawn })
	provisionDeploymentPrerequisites = func(checkout, source string) error {
		provisioned = append(provisioned, [2]string{checkout, source})
		return provisionErr
	}
	spawnDeploymentAgent = func(_ env, args []string) error {
		spawned = append(spawned, args)
		return nil
	}
	for _, args := range [][]string{
		{"--name", "deployer", "--run", "tt deployment serve --config /private/c.json"},
		{"--cwd", "/deployer", "--name", "deployer"},
		{"--prerequisites-from", "/root", "--name", "deployer"},
		{"--cwd", "/deployer", "--prerequisites-from"},
	} {
		if err := cmdDeployment(env{}, append([]string{"setup"}, args...)); err == nil {
			t.Fatalf("%q: setup accepted", args)
		}
	}
	if len(provisioned) != 0 || len(spawned) != 0 {
		t.Fatal("refused setup provisioned or spawned", provisioned, spawned)
	}
	provisionErr = errors.New("deployment prerequisites: Missing matrix prerequisites: .build/test.wasm")
	err := cmdDeployment(env{}, []string{"setup", "--cwd", "/deployer", "--prerequisites-from", "/root", "--name", "deployer"})
	if err == nil || !strings.Contains(err.Error(), "Missing matrix prerequisites: .build/test.wasm") || len(spawned) != 0 {
		t.Fatal("failed provisioning spawned or lost its reason", err, spawned)
	}
	provisionErr = nil
	provisioned = nil
	if err := cmdDeployment(env{}, []string{"setup", "--name", "deployer", "--cwd=/deployer", "-prerequisites-from=/root", "--run", "tt deployment serve --config /private/c.json"}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{"/deployer", "/root"}}; !reflect.DeepEqual(provisioned, want) {
		t.Fatal(provisioned)
	}
	want := []string{"--role", api.AgentRoleDeployment, "--name", "deployer", "--cwd", "/deployer", "--run", "tt deployment serve --config /private/c.json"}
	if len(spawned) != 1 || !reflect.DeepEqual(spawned[0], want) {
		t.Fatal(spawned)
	}
}

// The real seam runs the checkout's runner and returns only its named reason.
func TestDeploymentProvisioningReturnsTheRunnerReason(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := "if(process.argv.includes('--from')&&process.argv.at(-1)==='/good'){console.log('{\"version\":1}')}else{console.error('noise');console.error('Missing matrix prerequisites: .build/test.wasm');process.exitCode=1}"
	if err := os.WriteFile(filepath.Join(checkout, "scripts", "release-runner.mjs"), []byte(runner), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := provisionPrerequisites(checkout, "/good"); err != nil {
		t.Fatal(err)
	}
	err := provisionPrerequisites(checkout, "/bad")
	if err == nil || err.Error() != "deployment prerequisites: Missing matrix prerequisites: .build/test.wasm" {
		t.Fatal(err)
	}
}
