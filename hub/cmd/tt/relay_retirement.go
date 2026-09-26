package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Archives are siblings of active state, so both polling and the host census
// stop seeing a binding as soon as its archival move commits.
func relayArchiveDir(dir string) string { return filepath.Clean(dir) + "-archive" }

func withRelayBindingLock(dir, key string, fn func() error) error {
	locks := filepath.Join(dir, "binding-locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(locks, key+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

type relayRetirement struct {
	Binding        runtimeBinding `json:"binding"`
	BindingDigest  string         `json:"bindingDigest"`
	ProgressDigest string         `json:"progressDigest,omitempty"`
	Reason         string         `json:"reason"`
}

func relayFileDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// A durable decision precedes either move. Recovery compares exact bytes and
// never overwrites a destination or touches a replacement run's state.
var relayArchiveRename = os.Rename

func syncRelayDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func moveRelayArchiveFile(source, destination, digest string) error {
	archived, err := os.ReadFile(destination)
	if err == nil {
		if relayFileDigest(archived) != digest {
			return fmt.Errorf("archive destination collision: %s", destination)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if relayFileDigest(data) != digest {
		return fmt.Errorf("relay state changed before archival: %s", source)
	}
	if err := relayArchiveRename(source, destination); err != nil {
		return err
	}
	if err := syncRelayDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	return syncRelayDirectory(filepath.Dir(source))
}

func finishRelayRetirement(dir, archive string, r relayRetirement) error {
	key := bindingKey(r.Binding)
	source := filepath.Join(dir, key+".binding.json")
	target := filepath.Join(archive, "binding.json")
	// Until the binding has moved, it must still be byte-identical. In particular,
	// do not move progress from under an independently replaced binding.
	if _, err := os.Stat(target); os.IsNotExist(err) {
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if relayFileDigest(data) != r.BindingDigest {
			return fmt.Errorf("binding replaced before retirement")
		}
	}
	if r.ProgressDigest != "" {
		if err := moveRelayArchiveFile(filepath.Join(dir, key+".progress.json"), filepath.Join(archive, "progress.json"), r.ProgressDigest); err != nil {
			return err
		}
	}
	_, before := os.Stat(target)
	if err := moveRelayArchiveFile(source, target, r.BindingDigest); err != nil {
		return err
	}
	if os.IsNotExist(before) {
		fmt.Fprintf(os.Stderr, "[tt relay] retired %s run=%s: %s archive=%s\n", r.Binding.Agent, r.Binding.Run, r.Reason, archive)
	}
	return nil
}

func recoverRelayRetirementsLocked(dir, key string) error {
	paths, err := filepath.Glob(filepath.Join(relayArchiveDir(dir), key+"-*", "retirement.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r relayRetirement
		if err = json.Unmarshal(data, &r); err != nil {
			return err
		}
		if !validBinding(r.Binding) || bindingKey(r.Binding) != key || r.BindingDigest == "" {
			return fmt.Errorf("invalid relay retirement record")
		}
		archive := filepath.Dir(path)
		if err := finishRelayRetirement(dir, archive, r); err != nil {
			return err
		}
	}
	return nil
}

func recoverRelayRetirements(dir string) error {
	paths, err := filepath.Glob(filepath.Join(relayArchiveDir(dir), "*", "retirement.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r relayRetirement
		if err = json.Unmarshal(data, &r); err != nil {
			return err
		}
		if !validBinding(r.Binding) {
			return fmt.Errorf("invalid relay retirement binding")
		}
		key := bindingKey(r.Binding)
		if err = withRelayBindingLock(dir, key, func() error { return recoverRelayRetirementsLocked(dir, key) }); err != nil {
			return err
		}
	}
	return nil
}

func relayRetirementReason(b runtimeBinding, a api.Agent) string {
	if a.ID != b.Agent || a.TaskID != b.Task || !runIDPattern.MatchString(a.RunID) {
		return ""
	}
	switch a.Status {
	case api.AgentRunning, api.AgentDone, api.AgentNeedsInput, api.AgentRetired, api.AgentClosed, api.AgentExited:
	default:
		return ""
	}
	if a.RunID != b.Run {
		return "verified run mismatch"
	}
	if (a.Status == api.AgentClosed || a.Status == api.AgentExited) && a.CleanupDone {
		return "terminal run cleanup complete"
	}
	return ""
}

// A failed/unknown hub read retains the binding and suppresses this pass's
// other paths, so an uncertain status cannot trigger activity or wake writes.
func retireRelayBinding(ctx context.Context, dir, path string, b runtimeBinding, c *api.Client) (bool, error) {
	a, err := c.GetAgent(ctx, b.Task, b.Agent)
	if err != nil {
		return false, err
	}
	reason := relayRetirementReason(b, a)
	if reason == "" {
		return false, nil
	}
	retired := false
	err = withRelayBindingLock(dir, bindingKey(b), func() error {
		if err := recoverRelayRetirementsLocked(dir, bindingKey(b)); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			retired = true
			return nil
		}
		if err != nil {
			return err
		}
		var current runtimeBinding
		if json.Unmarshal(data, &current) != nil || current != b {
			retired = true // loaded snapshot was superseded; do not act on it
			return nil
		}
		r := relayRetirement{Binding: b, BindingDigest: relayFileDigest(data), Reason: reason}
		progress, err := os.ReadFile(filepath.Join(dir, bindingKey(b)+".progress.json"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		var p relayProgress
		if err == nil && json.Unmarshal(progress, &p) == nil && p.Run == b.Run && p.Thread == b.Thread {
			r.ProgressDigest = relayFileDigest(progress)
		}
		root := relayArchiveDir(dir)
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		archive, err := os.MkdirTemp(root, bindingKey(b)+"-")
		if err != nil {
			return err
		}
		if err := writePrivateJSON(filepath.Join(archive, "retirement.json"), r); err != nil {
			return err
		}
		if err := syncRelayDirectory(archive); err != nil {
			return err
		}
		if err := syncRelayDirectory(root); err != nil {
			return err
		}
		// Once the decision is durable, do not poll or rewrite progress, even if a
		// move fails. Recovery completes this transaction before a writer can bind.
		retired = true
		return finishRelayRetirement(dir, archive, r)
	})
	return retired, err
}

func writeRelayBinding(b runtimeBinding) error {
	dir := relayDir()
	key := bindingKey(b)
	return withRelayBindingLock(dir, key, func() error {
		if err := recoverRelayRetirementsLocked(dir, key); err != nil {
			return err
		}
		path := filepath.Join(dir, key+".binding.json")
		var old runtimeBinding
		data, _ := os.ReadFile(path)
		if json.Unmarshal(data, &old) == nil {
			same := old
			same.CreatedAt = b.CreatedAt
			if same == b && !old.CreatedAt.IsZero() {
				return nil
			}
		}
		return writePrivateJSON(path, b)
	})
}
func saveRelayProgress(dir, path string, b runtimeBinding, p relayProgress) error {
	return withRelayBindingLock(dir, bindingKey(b), func() error {
		if err := recoverRelayRetirementsLocked(dir, bindingKey(b)); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(dir, bindingKey(b)+".binding.json"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var current runtimeBinding
		if json.Unmarshal(data, &current) != nil || current != b {
			return nil
		}
		return writePrivateJSON(path, p)
	})
}
func relayArchiveCount(dir string) int {
	paths, _ := filepath.Glob(filepath.Join(relayArchiveDir(dir), "*", "retirement.json"))
	count := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		var r relayRetirement
		if err != nil || json.Unmarshal(data, &r) != nil || !validBinding(r.Binding) {
			continue
		}
		data, err = os.ReadFile(filepath.Join(filepath.Dir(path), "binding.json"))
		if err != nil || relayFileDigest(data) != r.BindingDigest {
			continue
		}
		if r.ProgressDigest != "" {
			data, err = os.ReadFile(filepath.Join(filepath.Dir(path), "progress.json"))
			if err != nil || relayFileDigest(data) != r.ProgressDigest {
				continue
			}
		}
		count++
	}
	return count
}

// A new binding is examined immediately, including the first --once migration.
// Keep the next probe in exact-run progress so loop passes and relay restarts
// add at most one retirement read per minute. Existing delivery revalidation
// remains independent and fresh. Failures also consume the probe interval.
func probeRelayRetirement(ctx context.Context, dir, path string, b runtimeBinding, p *relayProgress, c *api.Client, now time.Time) (bool, error) {
	if p.Run != b.Run || p.Thread != b.Thread {
		*p = relayProgress{Run: b.Run, Thread: b.Thread}
	}
	if now.Before(p.NextRetirementCheck) {
		return false, nil
	}
	p.NextRetirementCheck = now.Add(time.Minute)
	return retireRelayBinding(ctx, dir, path, b, c)
}
