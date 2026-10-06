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
	Kind        string `json:"kind"`
	Item        string `json:"item"`
	Agent       string `json:"agent"`
	Priority    string `json:"priority"`
	RequestedAt string `json:"requestedAt"`
	StartedAt   string `json:"startedAt"`
}

// matrixLockFile is either lock version in one shape: version 1 keeps a
// single holder and version 2 a list, both read here as Holders.
type matrixLockFile struct {
	Version int
	Host    string
	Holders []matrixLockEntry
	Waiters []matrixLockEntry
}

// parseMatrixLock is the one parse of the lock file's bytes, for the relay and
// the queue view: the holders and the ordered waiters of a version 1 or 2
// file, as holdersOf in the script. Otherwise it returns nil and the reason,
// "not JSON" or "unknown version". It does not check the host.
func parseMatrixLock(raw []byte) (*matrixLockFile, string) {
	var file struct {
		Version int               `json:"version"`
		Host    string            `json:"host"`
		Holder  *matrixLockEntry  `json:"holder"`
		Holders []matrixLockEntry `json:"holders"`
		Waiters []matrixLockEntry `json:"waiters"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return nil, "not JSON"
	}
	lock := &matrixLockFile{Version: file.Version, Host: file.Host, Waiters: file.Waiters}
	switch file.Version {
	case 1:
		if file.Holder != nil {
			lock.Holders = []matrixLockEntry{*file.Holder}
		}
	case 2:
		lock.Holders = file.Holders
	default:
		return nil, "unknown version"
	}
	return lock, ""
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
// unreadable, malformed, of a version other than 1 or 2, or written by
// another host.
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
	lock, _ := parseMatrixLock(raw)
	if lock == nil {
		return nil
	}
	local, err := matrixWaitHostname()
	if err != nil || matrixWaitHost(lock.Host) == "" || matrixWaitHost(lock.Host) != matrixWaitHost(local) {
		return nil
	}
	return lock
}

// matrixWaitFor is the agent's live verification run in this host's matrix
// lock: one of the host's holders, else its first place in the waitlist, else
// nil. An entry is the agent's when it names the agent, its process is alive
// and its item, when both are known, is the agent's bound item.
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
	for _, h := range lock.Holders {
		if w := match(h, h.StartedAt); w != nil {
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
