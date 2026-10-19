package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
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

// matrixLockDefault is the lock path when TAILTERM_MATRIX_HOST_LOCK is not
// set, resolved as scripts/verify-matrix-host-lock.mjs lockPath resolves it:
// the account home's shared file, taken from the user database, not HOME
// (wi_ee29358d0c7791ab). A HOME that is not the account's home (an isolated
// test or matrix check home), or an account home that cannot be read, gets no
// path and the reason instead. Such a process would otherwise read a private,
// usually absent file, which the sweep takes for a free host, or the shared
// lock its own matrix run may hold; it names the lock it means with the
// override.
func matrixLockDefault() (path, refused string) {
	account, err := matrixAccountHome()
	if err == nil && !filepath.IsAbs(account) {
		err = fmt.Errorf("%q is not an absolute path", account)
	}
	if err != nil {
		return "", fmt.Sprintf("cannot read this account's home (%v); set TAILTERM_MATRIX_HOST_LOCK to an absolute lock path", err)
	}
	shared := filepath.Join(account, ".local/state/tailterm-matrix/host.json")
	if home := os.Getenv("HOME"); home != "" && !sameMatrixHome(home, account) {
		return "", fmt.Sprintf("HOME (%s) is not this account's home (%s), so the default lock would be private; set TAILTERM_MATRIX_HOST_LOCK to an absolute lock path (the shared one is %s)", home, account, shared)
	}
	return shared, ""
}

// sameMatrixHome compares two homes after resolving symbolic links, or, for
// a path that cannot be resolved, as an absolute clean path.
func sameMatrixHome(a, b string) bool {
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
		return filepath.Clean(path)
	}
	return resolve(a) == resolve(b)
}

// Tests replace these, so none reads the host's live lock file or signals a
// real process.
var (
	// matrixAccountHome is this account's home from the user database:
	// os/user does not read HOME for the current user on macOS.
	matrixAccountHome = func() (string, error) {
		u, err := user.Current()
		if err != nil {
			return "", err
		}
		return u.HomeDir, nil
	}
	// matrixWaitDefaultPath is matrixLockDefault's path, empty when it
	// refuses one: the readers then take the lock as unusable.
	matrixWaitDefaultPath = func() string {
		path, _ := matrixLockDefault()
		return path
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

// readMatrixLock returns this host's lock file as a confirmed read: a file
// that parsed as version 1 or 2 and names this host. It returns nil for an
// unknown read: the file is absent, unreadable, malformed, of another version
// or written by another host, its path is not absolute, or this host's name
// could not be read. The lock script replaces the file by rename and never
// removes it, so a run that ended is a confirmed read without its entry.
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
// nil. An entry is the agent's when it names the agent (by name or ID), its
// process is alive and its item, when both are known, is the agent's bound
// item. An entry that names no agent (empty or "unknown", as a run started
// without TAILTERM_AGENT_NAME writes it) is the wait of every agent bound to
// its item, so the team that owns the item is not taken for idle; without a
// valid item it is nobody's (wi_6128966a8025aaad).
//
// Only a confirmed read ends a wait. On an unknown read the result is the
// last confirmed wait the hub holds for the agent's current run, so the relay
// repeats its last accepted report and sends nothing new.
func matrixWaitFor(a api.Agent, now time.Time) *api.MatrixWait {
	if a.Name == "" {
		return nil
	}
	lock := readMatrixLock()
	if lock == nil {
		return lastConfirmedMatrixWait(a, now)
	}
	match := func(e matrixLockEntry, at string) *api.MatrixWait {
		agentless := e.Agent == "" || e.Agent == "unknown"
		named := e.Agent == a.Name || (a.ID != "" && e.Agent == a.ID)
		if (!named && !agentless) || !matrixWaitPIDAlive(e.PID) {
			return nil
		}
		item := ""
		if api.ValidID(e.Item, "wi") {
			item = e.Item
		}
		if item != "" && a.WorkItem != nil && a.WorkItem.ItemID != item {
			return nil
		}
		if !named && (item == "" || a.WorkItem == nil) {
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

// lastConfirmedMatrixWait is the wait in the activity the hub holds for the
// agent's current run, which the relay's last accepted report put there. A
// wait that is not valid, or that starts after now, is none.
func lastConfirmedMatrixWait(a api.Agent, now time.Time) *api.MatrixWait {
	if a.Activity == nil || !a.Activity.MatrixWait.Valid() || a.Activity.MatrixWait.Since.After(now) {
		return nil
	}
	w := *a.Activity.MatrixWait
	w.Since = w.Since.UTC()
	return &w
}

// matrixWaitKey is the part of a matrix wait that makes a new activity
// report. The position is left out, so a moving waitlist sends none.
func matrixWaitKey(w *api.MatrixWait) string {
	if w == nil {
		return ""
	}
	return "\x00matrix=" + w.Role + "\x00" + w.Item + "\x00" + w.Since.UTC().Format(time.RFC3339Nano)
}
