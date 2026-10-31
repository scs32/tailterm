package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func canonicalLimiterDomain(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("limiter domain must be a canonical hub origin")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

// hostRelayCensus reads every binding file, including stale runs, so a bad or
// unreadable file cannot silently lower projected load. The digest describes
// the observed file set without disclosing thread IDs in policy records.
func hostRelayCensus(dir, host, domain string, now time.Time) api.TeamHostUsage {
	out := api.TeamHostUsage{Host: host, LimiterDomain: domain, ObservedAt: now.UTC().Format(time.RFC3339Nano), Complete: true}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		out.Complete = false
	}
	var records []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".binding.json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		var binding runtimeBinding
		if readErr != nil || json.Unmarshal(data, &binding) != nil || !validBinding(binding) {
			out.Complete = false
			records = append(records, entry.Name()+":unreadable")
			continue
		}
		bindingDomain, domainErr := canonicalLimiterDomain(binding.Hub)
		if domainErr != nil {
			out.Complete = false
		}
		if bindingDomain == domain {
			out.RelayBindings++
		} else {
			// One host policy describes one limiter domain. Another live
			// domain needs its own explicit budget before parallel admission.
			out.Complete = false
		}
		records = append(records, entry.Name()+":"+bindingDomain+":"+binding.Agent+":"+binding.Run)
	}
	sort.Strings(records)
	digest := sha256.Sum256([]byte(strings.Join(records, "\n")))
	out.SourceDigest = hex.EncodeToString(digest[:])
	return out
}

// statfsFreeMiB reports the space available to this user on a path's
// filesystem. Tests replace it.
var statfsFreeMiB = func(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize) / (1 << 20), nil
}

// hostFreeDiskMiB is the least free space across the host's queue worktrees.
// A worktree that no longer exists is skipped rather than failing the census;
// with none observable it reports the invoking checkout's filesystem.
func hostFreeDiskMiB(cwds []string, statfs func(string) (int64, error)) *int64 {
	var least *int64
	seen := map[string]bool{}
	for _, dir := range cwds {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		free, err := statfs(dir)
		if err != nil {
			continue
		}
		if least == nil || free < *least {
			least = &free
		}
	}
	if least == nil {
		if dir, err := os.Getwd(); err == nil {
			if free, err := statfs(dir); err == nil {
				least = &free
			}
		}
	}
	return least
}

// diskReserveState is -1 when free disk is unobserved, 1 when it is below the
// reserve and 0 otherwise. A change forces a census write.
func diskReserveState(free *int64, reserve int64) int {
	switch {
	case free == nil:
		return -1
	case *free < reserve:
		return 1
	}
	return 0
}

// hostCensusReuse is how long an unchanged census is reused rather than
// posted again.
const hostCensusReuse = 20 * time.Second

// hostCensusRefreshAfter is the census age at which the queue runner posts a
// fresh one just before a parallel claim or member attempt. The hub refuses
// either once the census is more than 30 seconds old
// (internal/store/team_host_capacity.go), so half that window leaves the call
// 15 seconds to reach the hub.
const hostCensusRefreshAfter = 15 * time.Second

// hostCensusKnownAt is when the census on the hub was observed after a
// saveHostRelayCensus call at now: prior's time when that call reused it, and
// otherwise now, since a posted census is observed no earlier than now.
func hostCensusKnownAt(prior *api.TeamHostUsage, now time.Time) time.Time {
	if prior != nil {
		if observed, err := time.Parse(time.RFC3339Nano, prior.ObservedAt); err == nil && now.Sub(observed) < hostCensusReuse {
			return observed
		}
	}
	return now
}

// hostMemoryPressure is the host's memory pressure, 0 to 100, higher is
// worse: 100 - kern.memorystatus_level, as scripts/verify-matrix-host-lock.mjs
// reads it. It is nil off macOS or when sysctl fails. Tests replace it.
var hostMemoryPressure = func() *int {
	if runtime.GOOS != "darwin" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sysctl", "-n", "kern.memorystatus_level").Output()
	if err != nil {
		return nil
	}
	level, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || level < 0 || level > 100 {
		return nil
	}
	pressure := 100 - level
	return &pressure
}

// hostMatrixWaiters counts the live matrix runs waiting in this host's lock
// (matrix_wait_activity.go). A lock file that does not exist is a confirmed
// 0: no run has used the host. It is nil when the lock path is unknown or the
// file is not a confirmed read, so a lane policy that holds while a matrix
// waits holds then too.
func hostMatrixWaiters() *int {
	path := os.Getenv("TAILTERM_MATRIX_HOST_LOCK")
	if path == "" {
		path = matrixWaitDefaultPath()
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	waiting := 0
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &waiting
	}
	lock := readMatrixLock()
	if lock == nil {
		return nil
	}
	for _, w := range lock.Waiters {
		if matrixWaitPIDAlive(w.PID) {
			waiting++
		}
	}
	return &waiting
}

// memoryStrainState is -1 when memory pressure is unobserved, 1 when it is
// over the default lane strain threshold and 0 otherwise. Like the disk
// reserve state, only a change forces a census write: the pressure moves on
// every read, and a census posted each tick would spend the hub's request
// budget. The census is shared by every project on the host, so a project's
// own threshold reads a reading at most hostCensusReuse old.
func memoryStrainState(pressure *int) int {
	switch {
	case pressure == nil:
		return -1
	case *pressure > api.DefaultLaneStrainPressure:
		return 1
	}
	return 0
}

func sameCount(a, b *int) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func saveHostRelayCensus(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, prior *api.TeamHostUsage, cwds []string, now time.Time) error {
	usage := hostRelayCensus(relayDir(), host, policy.LimiterDomain, now)
	usage.PolicyVersion = policy.Version
	usage.FreeDiskMiB = hostFreeDiskMiB(cwds, statfsFreeMiB)
	usage.MemoryPressure = hostMemoryPressure()
	usage.MatrixWaiters = hostMatrixWaiters()
	reserve := policy.DiskReserveMiB()
	if prior != nil && prior.PolicyVersion == usage.PolicyVersion && prior.Complete == usage.Complete && prior.SourceDigest == usage.SourceDigest && prior.LimiterDomain == usage.LimiterDomain && diskReserveState(prior.FreeDiskMiB, reserve) == diskReserveState(usage.FreeDiskMiB, reserve) && memoryStrainState(prior.MemoryPressure) == memoryStrainState(usage.MemoryPressure) && sameCount(prior.MatrixWaiters, usage.MatrixWaiters) {
		observed, err := time.Parse(time.RFC3339Nano, prior.ObservedAt)
		if err == nil && now.Sub(observed) < hostCensusReuse {
			return nil
		}
	}
	free := int64(-1)
	if usage.FreeDiskMiB != nil {
		free = *usage.FreeDiskMiB
	}
	pressure, waiters := -1, -1
	if usage.MemoryPressure != nil {
		pressure = *usage.MemoryPressure
	}
	if usage.MatrixWaiters != nil {
		waiters = *usage.MatrixWaiters
	}
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s:%s:%d:%d:%d", usage.Host, usage.LimiterDomain, usage.PolicyVersion, usage.ObservedAt, usage.SourceDigest, free, pressure, waiters)))
	_, err := c.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: "host-usage-" + hex.EncodeToString(seed[:12]), Operation: "observe_host", Host: host, HostUsage: &usage})
	return err
}
