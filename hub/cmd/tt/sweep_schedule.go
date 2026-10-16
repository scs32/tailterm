package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The scheduled sweep. On every host the relay runs the same pass as
// `tt team queue sweep-worktrees --apply` on a schedule, for each repository
// this host's queue entries name: the same keep rules, receipts and cache
// trim. Each attempt adds one line to a host journal, and the owner helper is
// told once per low-space episode. Settings come from
// ~/.config/tailterm/relay.json and are read at every due check, so a change
// needs no relay restart.

const (
	defaultSweepInterval    = time.Hour
	minSweepInterval        = 5 * time.Minute
	defaultSweepMinIdle     = 6 * time.Hour
	defaultSweepLowSpaceGiB = 8
	// sweepRetryAfter is the wait after a skipped or failed attempt.
	sweepRetryAfter = 5 * time.Minute
	// maxPendingSweepNotices bounds the notices waiting for a receipt.
	maxPendingSweepNotices = 8
	sweepNoticeSubject     = "Disk space is low after the scheduled worktree sweep"
)

// Whole-run skip reasons, as the journal names them.
const (
	sweepSkipOff      = "off"
	sweepSkipSettings = "settings"
	sweepSkipCleanup  = "cleanup-active"
	sweepSkipHub      = "hub"
	sweepSkipMatrix   = "matrix-lock"
)

type sweepSettings struct {
	Off           bool
	Interval      time.Duration
	MinIdle       time.Duration
	LowSpaceBytes int64
}

func defaultSweepSettings() sweepSettings {
	return sweepSettings{Interval: defaultSweepInterval, MinIdle: defaultSweepMinIdle, LowSpaceBytes: defaultSweepLowSpaceGiB << 30}
}

// sweepSettingsWarned limits each bad-value notice to one line per process.
var sweepSettingsWarned sync.Map

func sweepSettingsWarn(key, raw, used string) {
	if _, seen := sweepSettingsWarned.LoadOrStore(key+"\x00"+raw, true); !seen {
		fmt.Fprintf(os.Stderr, "[tt relay] relay.json %s=%s is not usable; using %s\n", key, raw, used)
	}
}

// readRelayConfig reads ~/.config/tailterm/relay.json as its top-level keys.
// A missing file is no keys and no fault; a file that exists but cannot be
// read, is over 64 KiB or is not a JSON object is a fault.
func readRelayConfig() (map[string]json.RawMessage, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "no home directory"
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "tailterm", "relay.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, "relay.json cannot be read"
	}
	if len(data) > 64<<10 {
		return nil, "relay.json is larger than 64 KiB"
	}
	var config map[string]json.RawMessage
	if json.Unmarshal(data, &config) != nil || config == nil {
		return nil, "relay.json is not a JSON object"
	}
	return config, ""
}

// readSweepSettings reads the four worktreeSweep keys from
// ~/.config/tailterm/relay.json, each by its exact name (see
// relayStallAction). A missing file means the defaults. A file that exists but
// cannot be read, is over 64 KiB or is not a JSON object, or a switch that is
// neither "on" nor "off", returns a fault: the run is skipped, since the
// owner's switch cannot be read. A bad interval, min-idle or threshold uses
// its default and says so once on stderr.
func readSweepSettings() (sweepSettings, string) {
	settings := defaultSweepSettings()
	config, fault := readRelayConfig()
	if fault != "" {
		return settings, fault
	}
	if raw, ok := config["worktreeSweep"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "on" && value != "off") {
			return settings, `worktreeSweep is neither "on" nor "off"`
		}
		settings.Off = value == "off"
	}
	duration := func(key string, into *time.Duration, least time.Duration) {
		raw, ok := config[key]
		if !ok {
			return
		}
		var value string
		d, err := time.Duration(0), error(nil)
		if json.Unmarshal(raw, &value) == nil {
			d, err = time.ParseDuration(value)
		}
		if value == "" || err != nil || d <= 0 {
			sweepSettingsWarn(key, string(raw), into.String())
			return
		}
		if d < least {
			sweepSettingsWarn(key, string(raw), least.String())
			d = least
		}
		*into = d
	}
	duration("worktreeSweepInterval", &settings.Interval, minSweepInterval)
	duration("worktreeSweepMinIdle", &settings.MinIdle, 0)
	if raw, ok := config["worktreeSweepLowSpaceGiB"]; ok {
		var value float64
		if json.Unmarshal(raw, &value) != nil || !(value > 0) || value > 1<<20 {
			sweepSettingsWarn("worktreeSweepLowSpaceGiB", string(raw), strconv.Itoa(defaultSweepLowSpaceGiB))
		} else {
			settings.LowSpaceBytes = int64(value * (1 << 30))
		}
	}
	return settings, ""
}

// State.

type sweepEpisode struct {
	StartedAt time.Time `json:"startedAt"`
}

// sweepNotice is one low-space notice, frozen when its episode began and
// replayed unchanged until the hub returns its receipt.
type sweepNotice struct {
	RequestID string       `json:"requestId"`
	TaskID    string       `json:"taskId"`
	To        string       `json:"to,omitempty"`
	ToName    string       `json:"toName,omitempty"`
	Envelope  api.Envelope `json:"envelope"`
	Text      string       `json:"text"`
	FrozenAt  time.Time    `json:"frozenAt"`
}

// sweepState is worktree-sweep-state.json in the relay's state directory.
type sweepState struct {
	// LastRunAt is the last run that counts for the interval: a swept run, or
	// one in which every repository failed. LastAttemptAt also counts skipped
	// and other failed attempts.
	LastRunAt     time.Time `json:"lastRunAt"`
	LastAttemptAt time.Time `json:"lastAttemptAt"`
	// OffSince is set while the owner's switch is off, so the switch is
	// journalled once.
	OffSince       *time.Time     `json:"offSince,omitempty"`
	Episode        *sweepEpisode  `json:"episode,omitempty"`
	PendingNotices []sweepNotice  `json:"pendingNotices,omitempty"`
	Intents        []detachIntent `json:"intents,omitempty"`
	// GoCacheTrim is the Go build cache trim's schedule and last receipt.
	GoCacheTrim *goCacheTrimState `json:"goCacheTrim,omitempty"`
}

func sweepStatePath() string { return filepath.Join(relayDir(), "worktree-sweep-state.json") }

func loadSweepState() (sweepState, error) {
	var state sweepState
	data, err := os.ReadFile(sweepStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return sweepState{}, fmt.Errorf("%s is not readable JSON: %w", sweepStatePath(), err)
	}
	return state, nil
}

// updateSweepState is one read-modify-write of the state file under its own
// lock: the schedule and a manual sweep's trim intents both write it. The
// file is written and synced before this returns.
func updateSweepState(change func(*sweepState) error) error {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(relayDir(), "worktree-sweep-state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	state, err := loadSweepState()
	if err != nil {
		return err
	}
	if err := change(&state); err != nil {
		return err
	}
	return writePrivateJSON(sweepStatePath(), state)
}

func saveDetachIntent(intent detachIntent) error {
	return updateSweepState(func(s *sweepState) error {
		s.Intents = append(s.Intents, intent)
		return nil
	})
}

func clearDetachIntent(tombstone string) error {
	return updateSweepState(func(s *sweepState) error {
		kept := s.Intents[:0]
		for _, intent := range s.Intents {
			if intent.Tombstone != tombstone {
				kept = append(kept, intent)
			}
		}
		s.Intents = kept
		return nil
	})
}

func loadDetachIntents() ([]detachIntent, error) {
	state, err := loadSweepState()
	return state.Intents, err
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sweepDue reports whether a run is due: an interval after the last swept
// run, and sweepRetryAfter after a later skipped or failed attempt.
func sweepDue(state sweepState, interval time.Duration, now time.Time) bool {
	if !state.LastRunAt.IsZero() && !now.Before(state.LastRunAt) && now.Sub(state.LastRunAt) < interval {
		return false
	}
	if state.LastAttemptAt.After(state.LastRunAt) && !now.Before(state.LastAttemptAt) && now.Sub(state.LastAttemptAt) < sweepRetryAfter {
		return false
	}
	return true
}

// stateFaultGate spaces and journals a state file that cannot be read or
// saved: one journal line per episode, and one try per sweepRetryAfter while
// it lasts. It lives in the relay's memory, since the fault is the state
// directory itself; a relay restart during a fault writes one more line.
type stateFaultGate struct {
	mu sync.Mutex
	// kind is "read" or "save" while a fault is open.
	kind    string
	lastTry time.Time
}

// The scheduled sweep and the Go build cache trim each keep their own.
var sweepStateFaults, goCacheTrimStateFaults stateFaultGate

const (
	stateFaultRead = "read"
	stateFaultSave = "save"
)

// blocked reports whether an open fault was tried less than sweepRetryAfter
// ago, so this check does nothing.
func (g *stateFaultGate) blocked(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.kind != "" && !now.Before(g.lastTry) && now.Sub(g.lastTry) < sweepRetryAfter
}

// check notes the result of one state read or save. A failure opens or
// extends the episode, and journal is true only for the episode's first. A
// good save ends any episode; a good read ends only a read fault, since a
// state file that reads may still not save.
func (g *stateFaultGate) check(kind string, err error, now time.Time) (ok, journal bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		if kind == stateFaultSave || g.kind == kind {
			g.kind = ""
		}
		return true, false
	}
	journal = g.kind == ""
	if journal {
		g.kind = kind
	}
	g.lastTry = now
	return false, journal
}

// The matrix lock, as the sweep reads it.
//
// scripts/verify-matrix-host-lock.mjs serialises every admission with a mutex
// file, host.json.lock, taken by linking a private owner record into place.
// The sweep takes the same mutex the same way for each delete, so a matrix
// run cannot register on a path between the sweep's final check and its
// removal. A mutex left by a process that is gone is reclaimed under the
// script's own protocol (reclaimMatrixMutex). Go never writes host.json.

// Tests shorten these.
var (
	// matrixMutexWait is how long one delete waits for the mutex.
	matrixMutexWait = 2 * time.Second
	// matrixHoldLimit is the longest the sweep may hold the mutex: a holder
	// that cannot update the lock file for 30 s aborts its own run. The
	// mutex is released at matrixHoldRelease, so no hold reaches the limit.
	matrixHoldLimit   = 20 * time.Second
	matrixHoldRelease = 18 * time.Second
	// matrixHoldGap is the pause between holds, so a waiting matrix process
	// gets in.
	matrixHoldGap = 250 * time.Millisecond
	// exclusiveAfterCheck runs under the mutex after the final check and
	// before the removal or detach.
	exclusiveAfterCheck func()
	// matrixOwnerProbe reports whether a mutex owner's pid is alive and, when
	// known, when that process started. Tests replace it, so none signals a
	// real process.
	matrixOwnerProbe = nativeMatrixOwnerProbe
)

// matrixOwnerStartMargin is how much later than its own record a live process
// must have started before the record is taken to be another, dead process's.
const matrixOwnerStartMargin = 5 * time.Second

// parsePsLstart reads `ps -o lstart=` as printed in the C locale under
// TZ=UTC, so no local time is read twice when the clocks go back.
func parsePsLstart(text string) (time.Time, bool) {
	started, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(text), " "), time.UTC)
	return started, err == nil
}

// nativeMatrixOwnerProbe: only "no such process" is gone, as the script's
// pidGone; a start time that cannot be read is unknown.
func nativeMatrixOwnerProbe(pid int) (alive bool, started time.Time, known bool) {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false, time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return true, time.Time{}, false
	}
	started, known = parsePsLstart(string(out))
	return true, started, known
}

// matrixSweepEntry is one holder or waiter's paths.
type matrixSweepEntry struct {
	ID              string
	Worktree        string
	Output          string
	RecordDirectory string
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' || json.Unmarshal(raw, &object) != nil {
		return nil, false
	}
	return object, true
}

func jsonArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	array := []json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &array) != nil {
		return nil, false
	}
	return array, true
}

// jsonInteger accepts only a JSON number written as a whole number in the
// range JavaScript holds exactly.
func jsonInteger(raw json.RawMessage) (int64, bool) {
	n, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	return n, err == nil && n >= -(1<<53-1) && n <= 1<<53-1
}

func jsonText(raw json.RawMessage) (string, bool) {
	var value string
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func jsonTime(raw json.RawMessage) bool {
	value, ok := jsonText(raw)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

// validMatrixState checks the lock file's structure as the script's
// parseState does, and more strictly where the sweep depends on it: every
// holder and waiter, of either version, must be complete and must name an
// absolute worktree, and the file must be this host's. It returns the
// entries, or the reason the file cannot be trusted.
func validMatrixState(raw []byte, host string) ([]matrixSweepEntry, string) {
	state, ok := jsonObject(raw)
	if !ok {
		return nil, "not a JSON object"
	}
	version, ok := jsonInteger(state["version"])
	if !ok || (version != 1 && version != 2) {
		return nil, "unknown version"
	}
	for _, key := range []string{"requestSeq", "grantSeq"} {
		if n, ok := jsonInteger(state[key]); !ok || n < 0 {
			return nil, "invalid " + key
		}
	}
	waiters, ok := jsonArray(state["waiters"])
	if !ok {
		return nil, "no waiters list"
	}
	var holders []json.RawMessage
	if version == 2 {
		if holders, ok = jsonArray(state["holders"]); !ok {
			return nil, "no holders list"
		}
		limit, ok := jsonInteger(state["holderLimit"])
		if !ok || limit < 1 || limit > 16 {
			return nil, "invalid holderLimit"
		}
		if _, has := state["holder"]; has {
			return nil, "a version 2 file with a holder key"
		}
		if int64(len(holders)) > limit {
			return nil, "more holders than the limit"
		}
	} else if holder := bytes.TrimSpace(state["holder"]); string(holder) != "null" {
		if _, ok := jsonObject(holder); !ok {
			return nil, "invalid holder"
		}
		holders = []json.RawMessage{json.RawMessage(holder)}
	}
	fileHost, _ := jsonText(state["host"])
	if matrixWaitHost(fileHost) == "" || matrixWaitHost(fileHost) != matrixWaitHost(host) {
		return nil, "written by another host"
	}
	seen := map[string]bool{}
	var entries []matrixSweepEntry
	for i, raw := range append(append([]json.RawMessage(nil), holders...), waiters...) {
		entry, why := validMatrixEntry(raw, i < len(holders), version == 2)
		if why != "" {
			return nil, "invalid entry: " + why
		}
		if seen[entry.ID] {
			return nil, "duplicate entry id"
		}
		seen[entry.ID] = true
		entries = append(entries, entry)
	}
	return entries, ""
}

func validMatrixEntry(raw json.RawMessage, holder, counters bool) (matrixSweepEntry, string) {
	var entry matrixSweepEntry
	e, ok := jsonObject(raw)
	if !ok {
		return entry, "not an object"
	}
	if entry.ID, ok = jsonText(e["id"]); !ok || entry.ID == "" {
		return entry, "id"
	}
	for _, key := range []string{"pid", "seq", "runTimeoutMs"} {
		if n, ok := jsonInteger(e[key]); !ok || n <= 0 {
			return entry, key
		}
	}
	if !jsonTime(e["requestedAt"]) {
		return entry, "requestedAt"
	}
	if holder && !jsonTime(e["startedAt"]) {
		return entry, "startedAt"
	}
	if kind, ok := jsonText(e["kind"]); !ok || (kind != "run" && kind != "targeted" && kind != "exec") {
		return entry, "kind"
	}
	for _, key := range []string{"item", "agent"} {
		if _, ok := jsonText(e[key]); !ok {
			return entry, key
		}
	}
	if priority, ok := jsonText(e["priority"]); !ok || (priority != "urgent" && priority != "high" && priority != "normal") {
		return entry, "priority"
	}
	if counters && !holder {
		for _, key := range []string{"grantSeqAtRequest", "overtakenBy"} {
			if n, ok := jsonInteger(e[key]); !ok || n < 0 {
				return entry, key
			}
		}
	}
	for key, into := range map[string]*string{"worktree": &entry.Worktree, "output": &entry.Output, "recordDirectory": &entry.RecordDirectory} {
		field, has := e[key]
		if !has {
			if key == "worktree" {
				return entry, "no worktree"
			}
			continue
		}
		value, ok := jsonText(field)
		if !ok || !filepath.IsAbs(value) {
			return entry, key
		}
		*into = value
	}
	return entry, ""
}

// matrixExclusion is the host's matrix lock for one sweep.
type matrixExclusion struct {
	file string
	host string

	mu          sync.Mutex
	entries     []matrixSweepEntry
	broken      string
	exceeded    int
	longestHold time.Duration
	lastRelease time.Time
	// busy counts the paths kept only because the mutex could not be taken,
	// busyOwner names who held it, and reclaimed names each mutex this pass
	// reclaimed.
	busy      int
	busyOwner string
	reclaimed []string
}

// loadMatrixExclusion reads and validates this host's matrix lock file. An
// absent file is a free host. Any other failure returns nil and the reason:
// the caller skips its run. A refused default lock gives matrixLockDefault's
// reason, not the empty path's shape.
func loadMatrixExclusion() (*matrixExclusion, string) {
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if path == "" {
		if path = matrixWaitDefaultPath(); path == "" {
			if _, refused := matrixLockDefault(); refused != "" {
				return nil, refused
			}
		}
	}
	if !filepath.IsAbs(path) {
		return nil, "the lock path is not absolute"
	}
	host, err := matrixWaitHostname()
	if err != nil {
		return nil, "this host's name cannot be read"
	}
	m := &matrixExclusion{file: path, host: host}
	entries, why := m.read()
	if why != "" {
		return nil, why
	}
	m.entries = entries
	return m, ""
}

func (m *matrixExclusion) read() ([]matrixSweepEntry, string) {
	raw, err := os.ReadFile(m.file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ""
	}
	if err != nil {
		return nil, "unreadable"
	}
	return validMatrixState(raw, m.host)
}

func matrixEntryPaths(entries []matrixSweepEntry) []string {
	var out []string
	for _, e := range entries {
		for _, p := range []string{e.Worktree, e.Output, e.RecordDirectory} {
			if p != "" {
				out = append(out, canonicalPath(p))
			}
		}
	}
	return out
}

// paths are the checkout, output and record folders the matrix entries read
// at the start of the sweep hold.
func (m *matrixExclusion) paths() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return matrixEntryPaths(m.entries)
}

// linkMatrixOwner is one attempt at the mutex or the reclaim guard, as the
// script's linkOwner: the owner record is written to a private file and
// linked into place. It returns the token, or "" when target exists.
func linkMatrixOwner(target string) (string, error) {
	token, err := randomHex(16)
	if err != nil {
		return "", err
	}
	owner, err := json.Marshal(map[string]any{"pid": os.Getpid(), "token": token, "at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z")})
	if err != nil {
		return "", err
	}
	temp := fmt.Sprintf("%s.%d.%s.tmp", target, os.Getpid(), token)
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer os.Remove(temp)
	_, err = file.Write(append(owner, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Link(temp, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", nil
		}
		return "", err
	}
	return token, nil
}

// takeMatrixMutex is one attempt. It returns the token, or "" when another
// owner holds the mutex; it never reclaims.
func takeMatrixMutex(mutex string) (string, error) { return linkMatrixOwner(mutex) }

// matrixOwner is a mutex or guard owner record. The script identifies an
// owner by a whole-number pid and a text token; any other record is never
// reclaimed.
type matrixOwner struct {
	PID   int
	Token string
	At    time.Time
	// HasAt is false when the record's time does not parse.
	HasAt bool
}

// readMatrixOwner returns the owner of target, or false when the file is
// absent, unreadable or does not identify one.
func readMatrixOwner(target string) (matrixOwner, bool) {
	var owner matrixOwner
	data, err := os.ReadFile(target)
	if err != nil {
		return owner, false
	}
	record, ok := jsonObject(data)
	if !ok {
		return owner, false
	}
	pid, ok := jsonInteger(record["pid"])
	if !ok || pid <= 0 || pid > 1<<31-1 {
		return owner, false
	}
	if owner.Token, ok = jsonText(record["token"]); !ok {
		return owner, false
	}
	owner.PID = int(pid)
	if at, ok := jsonText(record["at"]); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, at); err == nil {
			owner.At, owner.HasAt = parsed, true
		}
	}
	return owner, true
}

// matrixOwnerDead reports why an owner cannot be the process that wrote its
// record, or "" when it may still be: its pid is gone (the script's rule), or
// the process now holding that pid started more than matrixOwnerStartMargin
// after the record was written. An unknown start time is alive.
func matrixOwnerDead(owner matrixOwner) string {
	alive, started, known := matrixOwnerProbe(owner.PID)
	if !alive {
		return fmt.Sprintf("pid %d is gone", owner.PID)
	}
	if known && owner.HasAt && started.After(owner.At.Add(matrixOwnerStartMargin)) {
		return fmt.Sprintf("pid %d now belongs to a process started after the record was written", owner.PID)
	}
	return ""
}

// reclaimMatrixMutex removes a mutex whose owner is dead, as the script's
// reclaimMutex: only the holder of the guard file removes it, and only after
// re-reading the same token under the guard, so two reclaimers cannot both
// win and a mutex taken meanwhile is never removed. A guard whose own owner
// is gone is left for the operator. It returns a note on a reclaim, or who
// holds the mutex.
func reclaimMatrixMutex(mutex string) (reclaimed, holder string) {
	owner, ok := readMatrixOwner(mutex)
	if !ok {
		if _, err := os.Lstat(mutex); errors.Is(err, fs.ErrNotExist) {
			// Released meanwhile: the next take decides.
			return "", ""
		}
		return "", "an owner record that names no owner"
	}
	holder = fmt.Sprintf("owner pid %d", owner.PID)
	why := matrixOwnerDead(owner)
	if why == "" {
		return "", holder
	}
	guard := mutex + ".reclaim"
	token, err := linkMatrixOwner(guard)
	if err != nil {
		return "", holder + ", reclaim guard: " + err.Error()
	}
	if token == "" {
		if left, ok := readMatrixOwner(guard); ok {
			if alive, _, _ := matrixOwnerProbe(left.PID); !alive {
				return "", fmt.Sprintf("reclaim guard left by pid %d", left.PID)
			}
		}
		return "", holder + ", reclaim in progress"
	}
	defer releaseMatrixMutex(guard, token)
	if again, ok := readMatrixOwner(mutex); !ok || again.Token != owner.Token {
		return "", holder
	}
	if err := os.Remove(mutex); err != nil {
		return "", holder + ": " + err.Error()
	}
	return fmt.Sprintf("reclaimed the matrix mutex left by pid %d (%s)", owner.PID, why), ""
}

// releaseMatrixMutex unlinks the mutex, or the guard, only while it still
// holds this token.
func releaseMatrixMutex(mutex, token string) {
	data, err := os.ReadFile(mutex)
	if err != nil {
		return
	}
	var owner struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(data, &owner) == nil && owner.Token == token {
		_ = os.Remove(mutex)
	}
}

// run takes the matrix mutex, re-reads the lock file and calls act with the
// paths its entries name now; act makes its final check and removes or
// detaches. It returns a keep reason, without calling act, when the mutex
// stays busy or the file can no longer be trusted, which also ends the pass;
// otherwise "" and a note when act outran the hold. A busy mutex whose owner
// is dead is reclaimed, at most once a call.
// The mutex is released on every path, and at matrixHoldRelease even when act
// is still running: that removal then finishes outside the mutex and is
// counted in exceeded.
func (m *matrixExclusion) run(act func(live []string)) (string, string) {
	m.mu.Lock()
	broken, last := m.broken, m.lastRelease
	m.mu.Unlock()
	if broken != "" {
		return keepInUse, "the matrix lock file became unusable (" + broken + "); the pass ended"
	}
	if wait := matrixHoldGap - time.Since(last); !last.IsZero() && wait > 0 {
		time.Sleep(wait)
	}
	mutex := m.file + ".lock"
	busy := func(holder string) (string, string) {
		m.mu.Lock()
		m.busy++
		if holder != "" {
			m.busyOwner = holder
		}
		m.mu.Unlock()
		if holder == "" {
			return keepInUse, "matrix lock busy"
		}
		return keepInUse, "matrix lock busy (" + holder + ")"
	}
	if err := os.MkdirAll(filepath.Dir(m.file), 0700); err != nil {
		return busy(err.Error())
	}
	token, holder, tried := "", "", false
	for deadline := time.Now().Add(matrixMutexWait); ; {
		var err error
		if token, err = takeMatrixMutex(mutex); err != nil {
			return busy(err.Error())
		}
		if token != "" {
			break
		}
		if !tried {
			tried = true
			var reclaimed string
			if reclaimed, holder = reclaimMatrixMutex(mutex); reclaimed != "" {
				m.mu.Lock()
				m.reclaimed = append(m.reclaimed, reclaimed)
				m.mu.Unlock()
				continue
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if token == "" {
		return busy(holder)
	}
	start := time.Now()
	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseMatrixMutex(mutex, token)
			held := time.Since(start)
			m.mu.Lock()
			if held > m.longestHold {
				m.longestHold = held
			}
			m.lastRelease = time.Now()
			m.mu.Unlock()
		})
	}
	defer release()
	entries, why := m.read()
	if why != "" {
		m.mu.Lock()
		m.broken = why
		m.mu.Unlock()
		return keepInUse, "the matrix lock file became unusable (" + why + "); the pass ended"
	}
	timer := time.AfterFunc(matrixHoldRelease-time.Since(start), release)
	act(matrixEntryPaths(entries))
	if !timer.Stop() {
		m.mu.Lock()
		m.exceeded++
		m.mu.Unlock()
		return "", "outran the matrix hold limit and finished outside the matrix lock"
	}
	return "", ""
}

// status reports why the pass ended early, how many removals outran the
// hold limit, and the longest hold.
func (m *matrixExclusion) status() (string, int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.broken, m.exceeded, m.longestHold
}

// mutexStatus reports how many paths were kept only because the mutex was
// busy, who held it, and the mutexes this pass reclaimed.
func (m *matrixExclusion) mutexStatus() (int, string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.busy, m.busyOwner, append([]string(nil), m.reclaimed...)
}

// The journal.

// sweepJournalLine is one attempt in worktree-sweep.jsonl.
type sweepJournalLine struct {
	At      string `json:"at"`
	Source  string `json:"source"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	// Detail explains a skip or failure.
	Detail       string `json:"detail,omitempty"`
	Repositories int    `json:"repositories"`
	// RepositoriesFailed counts the repositories whose sweep returned an
	// error; Detail names them.
	RepositoriesFailed int   `json:"repositoriesFailed,omitempty"`
	TreesRemoved       int   `json:"treesRemoved"`
	FoldersRemoved     int   `json:"foldersRemoved"`
	CachesTrimmed      int   `json:"cachesTrimmed"`
	Pruned             int   `json:"pruned"`
	BytesFreed         int64 `json:"bytesFreed"`
	FreeBytesAfter     int64 `json:"freeBytesAfter"`
	// LowestVolume is the path whose volume gave FreeBytesAfter.
	LowestVolume      string         `json:"lowestVolume,omitempty"`
	Kept              map[string]int `json:"kept"`
	MinIdle           string         `json:"minIdle"`
	DurationMs        int64          `json:"durationMs"`
	ExclusionExceeded int            `json:"exclusionExceeded"`
	LowSpace          bool           `json:"lowSpace"`
	NoticeSeq         int64          `json:"noticeSeq"`
	NoticePending     string         `json:"noticePending"`
	// NoticesDropped counts pending notices dropped because the list was full.
	NoticesDropped int `json:"noticesDropped,omitempty"`
	// Set on every Go build cache trim line (source go-cache-trim), so a zero
	// is written; absent from sweep lines.
	FilesRemoved    *int64 `json:"filesRemoved,omitempty"`
	CacheBytesAfter *int64 `json:"cacheBytesAfter,omitempty"`
	MaxAge          string `json:"maxAge,omitempty"`
}

func sweepJournalPath() string { return filepath.Join(relayDir(), "worktree-sweep.jsonl") }

// The journal is bounded by size: an append that leaves it over
// sweepJournalMaxBytes rewrites it to the whole lines of its newest
// sweepJournalKeepBytes.
const (
	sweepJournalMaxBytes  = 4 << 20
	sweepJournalKeepBytes = 1 << 20
	sweepJournalChunk     = 64 << 10
)

// appendSweepJournal adds one line and keeps the file within its bound. The
// append and the rewrite run under one lock file, so no writer's line is lost
// to another's rewrite. When the lock cannot be opened the line is still
// appended, and the file is left untrimmed.
func appendSweepJournal(line sweepJournalLine) error {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return err
	}
	if line.Kept == nil {
		line.Kept = map[string]int{}
	}
	data, err := json.Marshal(line)
	if err != nil {
		return err
	}
	locked := false
	if lock, err := os.OpenFile(sweepJournalPath()+".lock", os.O_CREATE|os.O_RDWR, 0600); err == nil {
		defer lock.Close()
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) == nil {
			locked = true
			defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		}
	}
	file, err := os.OpenFile(sweepJournalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil && locked {
		if trimErr := trimSweepJournal(); trimErr != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] worktree sweep journal trim: %v\n", trimErr)
		}
	}
	return err
}

// trimSweepJournal rewrites a journal over its bound to its newest whole
// lines, through a private file renamed into place. Its caller holds the
// journal lock.
func trimSweepJournal() error {
	path := sweepJournalPath()
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= sweepJournalMaxBytes {
		return err
	}
	tail := make([]byte, sweepJournalKeepBytes)
	if _, err := file.ReadAt(tail, info.Size()-sweepJournalKeepBytes); err != nil {
		return err
	}
	// The first line is cut short. A single line over the whole keep size
	// is dropped.
	cut := bytes.IndexByte(tail, '\n')
	if cut < 0 {
		cut = len(tail) - 1
	}
	tail = tail[cut+1:]
	temp, err := os.CreateTemp(filepath.Dir(path), ".worktree-sweep-journal-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(tail)
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// readSweepJournal returns the last limit lines, oldest first.
func readSweepJournal(limit int) ([]sweepJournalLine, error) {
	file, err := os.Open(sweepJournalPath())
	if errors.Is(err, fs.ErrNotExist) {
		return []sweepJournalLine{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return readSweepJournalTail(file, info.Size(), limit)
}

// readSweepJournalTail reads backwards from the end in chunks until it holds
// limit readable lines or reaches the start. It reads at most the journal's
// bound and one chunk, whatever the file's size.
func readSweepJournalTail(file io.ReaderAt, size int64, limit int) ([]sweepJournalLine, error) {
	parse := func(data []byte) []sweepJournalLine {
		lines := []sweepJournalLine{}
		for _, raw := range bytes.Split(data, []byte("\n")) {
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			var line sweepJournalLine
			if json.Unmarshal(raw, &line) != nil {
				continue
			}
			lines = append(lines, line)
		}
		return lines
	}
	var tail []byte
	// whole is the tail from its first whole line.
	whole := func(offset int64) []byte {
		if offset > 0 {
			return tail[bytes.IndexByte(tail, '\n')+1:]
		}
		return tail
	}
	offset := size
	// want is the line ends to hold before the tail is parsed again.
	for want := limit; offset > 0 && len(tail) < sweepJournalMaxBytes; {
		n := int64(sweepJournalChunk)
		if n > offset {
			n = offset
		}
		offset -= n
		chunk := make([]byte, n)
		if _, err := file.ReadAt(chunk, offset); err != nil && !(errors.Is(err, io.EOF) && offset+n >= size) {
			return nil, err
		}
		tail = append(chunk, tail...)
		ends := bytes.Count(whole(offset), []byte("\n"))
		if ends < want {
			continue
		}
		readable := len(parse(whole(offset)))
		if readable >= limit {
			break
		}
		want = ends + limit - readable
	}
	lines := parse(whole(offset))
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

// printSweepJournal is `tt team queue sweep-worktrees --journal`: this host's
// journal file, newest last. It reads no hub.
func printSweepJournal(limit int, jsonOut bool) error {
	lines, err := readSweepJournal(limit)
	if err != nil {
		return err
	}
	if jsonOut {
		printJSON(lines)
		return nil
	}
	if len(lines) == 0 {
		fmt.Println("no scheduled sweep has run on this host")
		return nil
	}
	for _, l := range lines {
		if l.Source == goCacheTrimSource {
			fmt.Println(goCacheTrimJournalText(l))
			continue
		}
		if l.Outcome != "swept" {
			text := fmt.Sprintf("%s %s %s", l.At, l.Outcome, l.Reason)
			if l.Detail != "" {
				text += ": " + l.Detail
			}
			if l.TreesRemoved+l.FoldersRemoved+l.CachesTrimmed+l.Pruned > 0 {
				text += fmt.Sprintf("; trees=%d folders=%d caches=%d pruned=%d freed=%s", l.TreesRemoved, l.FoldersRemoved, l.CachesTrimmed, l.Pruned, humanBytes(l.BytesFreed))
			}
			fmt.Println(text)
			continue
		}
		reasons := make([]string, 0, len(l.Kept))
		for reason, n := range l.Kept {
			reasons = append(reasons, fmt.Sprintf("%s=%d", reason, n))
		}
		sort.Strings(reasons)
		free := "unknown"
		if l.FreeBytesAfter >= 0 {
			free = humanBytes(l.FreeBytesAfter)
		}
		text := fmt.Sprintf("%s swept repositories=%d trees=%d folders=%d caches=%d pruned=%d freed=%s free=%s min-idle=%s", l.At, l.Repositories, l.TreesRemoved, l.FoldersRemoved, l.CachesTrimmed, l.Pruned, humanBytes(l.BytesFreed), free, l.MinIdle)
		if l.Detail != "" {
			text += "; " + l.Detail
		}
		if len(reasons) > 0 {
			text += "; kept " + strings.Join(reasons, " ")
		}
		if l.ExclusionExceeded > 0 {
			text += fmt.Sprintf("; %d removal(s) outran the matrix hold limit", l.ExclusionExceeded)
		}
		if l.LowSpace {
			text += "; low space"
		}
		if l.NoticeSeq > 0 {
			text += fmt.Sprintf("; notice #%d", l.NoticeSeq)
		}
		if l.NoticePending != "" {
			text += "; notice pending " + l.NoticePending
		}
		if l.NoticesDropped > 0 {
			text += fmt.Sprintf("; %d pending notice(s) dropped", l.NoticesDropped)
		}
		fmt.Println(text)
	}
	return nil
}

// The schedule.

// sweepScheduleDeps are the schedule's inputs; tests inject them.
type sweepScheduleDeps struct {
	now func() time.Time
	// freeBytes is the free space of the volume holding path.
	freeBytes func(path string) (int64, error)
	settings  func() (sweepSettings, string)
	// hub is the relay's client and this host's name, or false on a host
	// with no hub configured.
	hub   func() (*api.Client, string, bool)
	sweep func(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error)
}

func volumeFreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func nativeSweepScheduleDeps() sweepScheduleDeps {
	return sweepScheduleDeps{
		now:       time.Now,
		freeBytes: volumeFreeBytes,
		settings:  readSweepSettings,
		hub: func() (*api.Client, string, bool) {
			e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
			e.loadConfig()
			if e.hub == "" {
				return nil, "", false
			}
			c, err := e.client(20 * time.Second)
			if err != nil {
				return nil, "", false
			}
			// A notice the sweep posts is the relay's own, not the owner's.
			c.HTTP.Transport = relayAuthorTransport{base: c.HTTP.Transport}
			attachRelayBudget(c, activeRelayBudget)
			return c, spawn.Host(), true
		},
		sweep: runWorktreeSweep,
	}
}

// sweepRepositories are the distinct repositories of this host's queue
// entries that still exist, by main checkout.
func sweepRepositories(host string, entries []api.TeamQueueEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, q := range entries {
		if q.Host != host {
			continue
		}
		repo := q.Repository
		if repo == "" && q.Acceptance != nil {
			repo = q.Acceptance.Repository
		}
		if repo == "" || !filepath.IsAbs(repo) {
			continue
		}
		if main := mainCheckoutGuess(repo); main != "" {
			repo = main
		} else {
			repo = canonicalPath(repo)
		}
		if info, err := os.Stat(repo); err != nil || !info.IsDir() || seen[repo] {
			continue
		}
		seen[repo] = true
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// sweepKept is one kept decision with its size, for the notice.
type sweepKept struct {
	Path   string
	Reason string
	Bytes  int64
}

// largestKept measures the kept worktrees and folders and returns the five
// largest. It walks every kept tree, so it runs only when a notice is frozen.
func largestKept(decisions []worktreeDecision) []sweepKept {
	var kept []sweepKept
	for _, d := range decisions {
		if d.Action != "kept" || d.Reason == keepMissing || d.Kind == kindCache {
			continue
		}
		if bytes := worktreeBytes(d.Path, nil); bytes > 0 {
			kept = append(kept, sweepKept{Path: d.Path, Reason: d.Reason, Bytes: bytes})
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Bytes > kept[j].Bytes })
	if len(kept) > 5 {
		kept = kept[:5]
	}
	return kept
}

// sweepNoticeRequestID identifies one episode's notice on this host.
func sweepNoticeRequestID(host string, start time.Time) string {
	clean := []byte(host)
	for i, c := range clean {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			clean[i] = '-'
		}
	}
	if len(clean) > 64 {
		clean = clean[:64]
	}
	return fmt.Sprintf("worktree-sweep-low-%s-%d", clean, start.Unix())
}

// freezeSweepNotice builds an episode's notice: to the newest live owner
// helper across the hub's projects, in that helper's project, or with no
// recipient to the Board of the project of this host's newest queue entry.
// It returns false when there is nowhere to post.
func freezeSweepNotice(host string, hub sweepHubState, line sweepJournalLine, threshold int64, kept []sweepKept, trim *goCacheTrimState, now time.Time) (sweepNotice, bool) {
	var helper api.Agent
	for _, a := range hub.agents {
		if a.Role != api.AgentRoleOwnerHelper || a.Status == api.AgentClosed || a.Status == api.AgentExited || a.Status == api.AgentRetired {
			continue
		}
		if helper.ID == "" || a.CreatedAt.After(helper.CreatedAt) {
			helper = a
		}
	}
	task := helper.TaskID
	if task == "" {
		newest := ""
		for _, q := range hub.entries {
			if q.Host == host && q.TaskID != "" && (task == "" || q.UpdatedAt > newest) {
				task, newest = q.TaskID, q.UpdatedAt
			}
		}
	}
	if task == "" {
		return sweepNotice{}, false
	}
	volume := ""
	if line.LowestVolume != "" {
		volume = " on the volume holding " + line.LowestVolume
	}
	text := fmt.Sprintf("Host %s has %s free%s after the scheduled worktree sweep, below the %s threshold. This run freed %s: %d worktrees, %d session temp folders, %d caches trimmed.",
		host, humanBytes(line.FreeBytesAfter), volume, humanBytes(threshold), humanBytes(line.BytesFreed), line.TreesRemoved, line.FoldersRemoved, line.CachesTrimmed)
	if line.RepositoriesFailed > 0 {
		text += fmt.Sprintf(" %d of %d repositories could not be swept and were skipped.", line.RepositoriesFailed, line.Repositories)
	}
	if line.Outcome == "skipped" || line.Reason == sweepSkipMatrix {
		text += " The run was cut short by the matrix lock."
	}
	if len(kept) > 0 {
		parts := make([]string, 0, len(kept))
		for _, k := range kept {
			parts = append(parts, fmt.Sprintf("%s (%s, %s)", k.Path, humanBytes(k.Bytes), k.Reason))
		}
		text += " Largest kept: " + strings.Join(parts, "; ") + "."
	} else {
		text += " Nothing it kept holds any data."
	}
	text += " The relay sends this once per low-space episode. Read the runs with tt team queue sweep-worktrees --journal."
	text += " " + goCacheTrimNoticeSentence(trim)
	envelope := api.Envelope{Kind: api.EnvelopeKindNotice, To: helper.Name, Subject: sweepNoticeSubject, Refs: map[string]string{"host": host}, Body: api.EnvelopeBody{Text: text}}
	return sweepNotice{RequestID: sweepNoticeRequestID(host, now), TaskID: task, To: helper.ID, ToName: helper.Name, Envelope: envelope, Text: api.RenderText(envelope), FrozenAt: now.UTC()}, true
}

// deliverSweepNotice posts a frozen notice unchanged. Delivered means a post
// receipt carrying the notice's own request id and project: from the post,
// or, after any error, from the hub's receipt lookup. Anything else leaves
// the notice pending.
func deliverSweepNotice(ctx context.Context, c *api.Client, n sweepNotice) (int64, bool) {
	matches := func(m api.Message) (int64, bool) {
		if m.PostReceipt == nil || m.PostReceipt.RequestID != n.RequestID || m.PostReceipt.TaskID != n.TaskID || m.PostReceipt.MessageSeq <= 0 {
			return 0, false
		}
		return m.PostReceipt.MessageSeq, true
	}
	envelope := n.Envelope
	posted, err := c.PostMessage(ctx, n.TaskID, api.PostMessageRequest{Envelope: &envelope, Text: n.Text, To: n.To, RequestID: n.RequestID})
	if err == nil {
		if seq, ok := matches(posted); ok {
			return seq, true
		}
	}
	found, err := c.GetMessagePostReceipt(ctx, n.TaskID, n.RequestID, "")
	if err != nil {
		return 0, false
	}
	return matches(found)
}

// sweepFailedRepositories is the journal's account of the repositories whose
// sweep failed: the first three with their reasons, then a count.
func sweepFailedRepositories(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	shown := failed
	if len(shown) > 3 {
		shown = shown[:3]
	}
	text := fmt.Sprintf("%d repositories failed and were skipped: %s", len(failed), strings.Join(shown, "; "))
	if len(failed) == 1 {
		text = "1 repository failed and was skipped: " + shown[0]
	}
	if more := len(failed) - len(shown); more > 0 {
		text += fmt.Sprintf("; and %d more", more)
	}
	return text
}

// sweepScheduleAttempt runs one scheduled sweep when it is due. It reports
// whether it made an attempt, which is exactly when it wrote a journal line.
func sweepScheduleAttempt(ctx context.Context, d sweepScheduleDeps) bool {
	now := d.now()
	started := time.Now()
	if sweepStateFaults.blocked(now) {
		return false
	}
	settings, fault := d.settings()
	line := sweepJournalLine{At: now.UTC().Format(time.RFC3339Nano), Source: "schedule", Kept: map[string]int{}, MinIdle: settings.MinIdle.String(), FreeBytesAfter: -1}
	write := func(outcome, reason, detail string) {
		line.Outcome, line.Reason, line.Detail = outcome, reason, detail
		line.DurationMs = time.Since(started).Milliseconds()
		if err := appendSweepJournal(line); err != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] worktree sweep journal: %v\n", err)
		}
	}
	// stateFault journals a state read or save that failed, once per episode.
	// It reports whether the state is good, and whether a line was written.
	stateFault := func(kind string, err error, detail string) (ok, wrote bool) {
		ok, journal := sweepStateFaults.check(kind, err, now)
		if journal {
			if detail != "" {
				detail += "; "
			}
			write("failed", "state", detail+err.Error())
		}
		return ok, journal
	}
	state, err := loadSweepState()
	if ok, wrote := stateFault(stateFaultRead, err, ""); !ok {
		return wrote
	}
	if fault == "" && settings.Off {
		if state.OffSince != nil {
			return false
		}
		// The switch is journalled once, not at every check.
		err := updateSweepState(func(s *sweepState) error {
			at := now.UTC()
			s.OffSince = &at
			return nil
		})
		if ok, wrote := stateFault(stateFaultSave, err, "the off switch was not recorded"); !ok {
			return wrote
		}
		write("skipped", sweepSkipOff, "")
		return true
	}
	if fault == "" && state.OffSince != nil {
		// On again: the next off is a new transition, due or not.
		err := updateSweepState(func(s *sweepState) error {
			s.OffSince = nil
			return nil
		})
		if ok, wrote := stateFault(stateFaultSave, err, "the on switch was not recorded"); !ok {
			return wrote
		}
		state.OffSince = nil
	}
	if !sweepDue(state, settings.Interval, now) {
		return false
	}
	// attempted saves the attempt time and writes the line; a save that fails
	// is journalled in its place, once per episode.
	attempted := func(outcome, reason, detail string) bool {
		err := updateSweepState(func(s *sweepState) error {
			s.LastAttemptAt, s.OffSince = now.UTC(), nil
			return nil
		})
		if ok, wrote := stateFault(stateFaultSave, err, strings.TrimSuffix(outcome+" "+reason+": "+detail, ": ")); !ok {
			return wrote
		}
		write(outcome, reason, detail)
		return true
	}
	if fault != "" {
		return attempted("skipped", sweepSkipSettings, fault)
	}
	c, host, ok := d.hub()
	if !ok {
		return false
	}
	lock, err := worktreeCleanupLock()
	if err != nil {
		if errors.Is(err, errWorktreeCleanupActive) {
			return attempted("skipped", sweepSkipCleanup, "")
		}
		return attempted("failed", "lock", err.Error())
	}
	defer unlockQueueLaunch(lock)
	matrix, why := loadMatrixExclusion()
	if matrix == nil {
		return attempted("skipped", sweepSkipMatrix, why)
	}
	hub, err := readSweepHub(ctx, c)
	if err != nil {
		return attempted("skipped", sweepSkipHub, err.Error())
	}
	repos := sweepRepositories(host, hub.entries)
	line.Repositories = len(repos)
	var all []worktreeDecision
	var failed []string
	for _, repo := range repos {
		decisions, err := d.sweep(ctx, hub, host, sweepOptions{Repo: repo, Apply: true, MinIdle: settings.MinIdle, AcceptedAfter: artifactAcceptedAfter(), Now: now, Matrix: matrix})
		if err != nil {
			// One repository's failure is journalled and skipped; the others
			// still count.
			reason := err.Error()
			if len(reason) > 200 {
				reason = strings.ToValidUTF8(reason[:200], "") + "..."
			}
			failed = append(failed, repo+": "+reason)
		}
		all = append(all, decisions...)
	}
	line.RepositoriesFailed = len(failed)
	// The volumes read for low space: each repository and its artifacts root,
	// every cache the sweep decided on, and the Go build cache the trim last
	// validated. The lowest reading is the run's.
	var volumes []string
	for _, repo := range repos {
		volumes = append(volumes, repo, artifactsRootFor(repo))
	}
	for _, dec := range all {
		switch dec.Action {
		case "removed":
			if dec.Kind == kindSessionTemp {
				line.FoldersRemoved++
			} else {
				line.TreesRemoved++
			}
			line.BytesFreed += dec.Bytes
		case "trimmed":
			line.CachesTrimmed++
			line.BytesFreed += dec.Bytes
		case "pruned":
			line.Pruned++
		case "kept":
			line.Kept[dec.Reason]++
		}
		if dec.Kind == kindCache {
			volumes = append(volumes, dec.Path)
		}
	}
	if trim := state.GoCacheTrim; trim != nil && filepath.IsAbs(trim.CacheDir) {
		if info, err := os.Stat(trim.CacheDir); err == nil && info.IsDir() {
			volumes = append(volumes, trim.CacheDir)
		}
	}
	for _, path := range volumes {
		if path == "" {
			continue
		}
		if free, err := d.freeBytes(path); err == nil && (line.FreeBytesAfter < 0 || free < line.FreeBytesAfter) {
			line.FreeBytesAfter, line.LowestVolume = free, path
		}
	}
	broken, exceeded, _ := matrix.status()
	busy, busyOwner, reclaimed := matrix.mutexStatus()
	line.ExclusionExceeded = exceeded
	// The outcome. Only a run that swept, or in which every repository
	// failed, counts as a run for the interval; a run cut short by the matrix
	// lock is tried again after sweepRetryAfter.
	outcome, reason, countsAsRun := "swept", "", true
	var notes []string
	switch {
	case broken != "":
		outcome, reason, countsAsRun = "failed", sweepSkipMatrix, false
		notes = append(notes, broken)
	case busy > 0:
		outcome, reason, countsAsRun = "skipped", sweepSkipMatrix, false
		if busyOwner == "" {
			busyOwner = "owner unknown"
		}
		notes = append(notes, fmt.Sprintf("%d path(s) kept: matrix mutex busy (%s)", busy, busyOwner))
	case len(failed) > 0 && len(failed) == len(repos):
		outcome, reason = "failed", "sweep"
	}
	if text := sweepFailedRepositories(failed); text != "" {
		notes = append(notes, text)
	}
	notes = append(notes, reclaimed...)
	detail := strings.Join(notes, "; ")
	line.Outcome, line.Reason = outcome, reason
	// Every attempt that read the free space takes part in the low-space
	// episode: it begins at the first one that ends below the threshold and
	// ends at the first one at or above it. Its notice is frozen and saved
	// before anything is posted.
	line.LowSpace = line.FreeBytesAfter >= 0 && line.FreeBytesAfter < settings.LowSpaceBytes
	var notice *sweepNotice
	if line.LowSpace && state.Episode == nil {
		if frozen, ok := freezeSweepNotice(host, hub, line, settings.LowSpaceBytes, largestKept(all), state.GoCacheTrim, now); ok {
			notice = &frozen
		}
	}
	var pending []sweepNotice
	err = updateSweepState(func(s *sweepState) error {
		s.LastAttemptAt, s.OffSince = now.UTC(), nil
		if countsAsRun {
			s.LastRunAt = now.UTC()
		}
		switch {
		case line.LowSpace && s.Episode == nil:
			s.Episode = &sweepEpisode{StartedAt: now.UTC()}
			if notice != nil {
				s.PendingNotices = append(s.PendingNotices, *notice)
			}
		case !line.LowSpace && line.FreeBytesAfter >= 0:
			s.Episode = nil
		}
		for len(s.PendingNotices) > maxPendingSweepNotices {
			s.PendingNotices = s.PendingNotices[1:]
			line.NoticesDropped++
		}
		pending = append([]sweepNotice(nil), s.PendingNotices...)
		return nil
	})
	// Nothing is posted from a state that was not saved.
	if ok, wrote := stateFault(stateFaultSave, err, strings.TrimSuffix(strings.TrimSpace(outcome+" "+reason)+": "+detail, ": ")); !ok {
		return wrote
	}
	var waiting []string
	for _, n := range pending {
		seq, delivered := deliverSweepNotice(ctx, c, n)
		if !delivered {
			waiting = append(waiting, n.RequestID)
			continue
		}
		line.NoticeSeq = seq
		if err := updateSweepState(func(s *sweepState) error {
			kept := s.PendingNotices[:0]
			for _, p := range s.PendingNotices {
				if p.RequestID != n.RequestID {
					kept = append(kept, p)
				}
			}
			s.PendingNotices = kept
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] worktree sweep state: %v\n", err)
		}
	}
	line.NoticePending = strings.Join(waiting, ",")
	write(outcome, reason, detail)
	return true
}

// relaySweepSchedule is the relay's scheduled sweep, with the Go build cache
// trim before it; tests replace it.
var relaySweepSchedule = func(ctx context.Context) {
	goCacheTrimAttempt(ctx, nativeGoCacheTrimDeps())
	sweepScheduleAttempt(ctx, nativeSweepScheduleDeps())
}

// sweepCheckInterval spaces the relay's due checks: each reads two small
// local files.
const sweepCheckInterval = 30 * time.Second

// relaySweepPass runs the scheduled sweep off the wake path, like
// relayRotationPass: a sweep can take minutes, and one is in flight at most.
// Only the relay loop calls its methods.
type relaySweepPass struct {
	done      chan struct{} // nil while no pass is in flight
	lastCheck time.Time
}

func (r *relaySweepPass) start() {
	if r.done != nil || time.Since(r.lastCheck) < sweepCheckInterval {
		return
	}
	r.lastCheck = time.Now()
	done := make(chan struct{})
	r.done = done
	run := relaySweepSchedule
	go func() {
		defer close(done)
		// A fault in one sweep must not take the relay's wakes down with it.
		defer func() {
			if fault := recover(); fault != nil {
				fmt.Fprintf(os.Stderr, "[tt relay] worktree sweep: %v\n", fault)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		run(ctx)
	}()
}

// collect notes a finished pass.
func (r *relaySweepPass) collect() {
	if r.done == nil {
		return
	}
	select {
	case <-r.done:
		r.done = nil
	default:
	}
}
