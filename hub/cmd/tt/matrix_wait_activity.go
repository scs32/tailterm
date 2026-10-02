package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The host's matrix lock (scripts/verify-matrix-host-lock.mjs) as the relay
// reads it: who holds the verification host and who waits, in order.
type matrixLockEntry struct {
	PID         int    `json:"pid"`
	Item        string `json:"item"`
	Agent       string `json:"agent"`
	RequestedAt string `json:"requestedAt"`
	StartedAt   string `json:"startedAt"`
}

type matrixLockFile struct {
	Version int               `json:"version"`
	Host    string            `json:"host"`
	Holder  *matrixLockEntry  `json:"holder"`
	Waiters []matrixLockEntry `json:"waiters"`
}

// Tests replace these, so none reads the host's live lock file or signals a
// real process.
var (
	matrixWaitDefaultPath = func() string {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".local/state/tailterm-matrix/host.json")
	}
	matrixWaitHostname = os.Hostname
	// Only "no such process" is gone; a pid we may not signal is alive.
	matrixWaitPIDAlive = func(pid int) bool {
		return pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
)

// matrixWaitHost is the first DNS label in lower case, as the lock's readers
// compare hosts.
func matrixWaitHost(host string) string {
	return strings.ToLower(strings.SplitN(strings.TrimSpace(host), ".", 2)[0])
}

// readMatrixLock returns this host's lock file, or nil when it is absent,
// unreadable, malformed, of another version or written by another host.
func readMatrixLock() *matrixLockFile {
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if path == "" {
		path = matrixWaitDefaultPath()
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lock matrixLockFile
	if json.Unmarshal(raw, &lock) != nil || lock.Version != 1 {
		return nil
	}
	local, err := matrixWaitHostname()
	if err != nil || matrixWaitHost(lock.Host) == "" || matrixWaitHost(lock.Host) != matrixWaitHost(local) {
		return nil
	}
	return &lock
}

// matrixWaitFor is the agent's live verification run in this host's matrix
// lock: holding the host, else its first place in the waitlist, else nil. An
// entry is the agent's when it names the agent, its process is alive and its
// item, when both are known, is the agent's bound item.
func matrixWaitFor(a api.Agent, now time.Time) *api.MatrixWait {
	if a.Name == "" {
		return nil
	}
	lock := readMatrixLock()
	if lock == nil {
		return nil
	}
	match := func(e matrixLockEntry, at string) *api.MatrixWait {
		if e.Agent != a.Name || !matrixWaitPIDAlive(e.PID) {
			return nil
		}
		item := ""
		if api.ValidID(e.Item, "wi") {
			item = e.Item
		}
		if item != "" && a.WorkItem != nil && a.WorkItem.ItemID != item {
			return nil
		}
		since, err := time.Parse(time.RFC3339Nano, at)
		if err != nil || since.IsZero() {
			return nil
		}
		return &api.MatrixWait{Item: item, Since: since.UTC()}
	}
	if h := lock.Holder; h != nil {
		if w := match(*h, h.StartedAt); w != nil {
			w.Role = api.MatrixWaitRunning
			return w
		}
	}
	if len(lock.Waiters) > 1000 {
		return nil
	}
	for i, e := range lock.Waiters {
		if w := match(e, e.RequestedAt); w != nil {
			w.Role, w.Position, w.Length = api.MatrixWaitWaiting, i+1, len(lock.Waiters)
			return w
		}
	}
	return nil
}

// matrixWaitKey is the part of a matrix wait that makes a new activity
// report. The position is left out, so a moving waitlist sends none.
func matrixWaitKey(w *api.MatrixWait) string {
	if w == nil {
		return ""
	}
	return "\x00matrix=" + w.Role + "\x00" + w.Item + "\x00" + w.Since.UTC().Format(time.RFC3339Nano)
}
