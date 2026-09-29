package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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

func saveHostRelayCensus(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, prior *api.TeamHostUsage, cwds []string, now time.Time) error {
	usage := hostRelayCensus(relayDir(), host, policy.LimiterDomain, now)
	usage.PolicyVersion = policy.Version
	usage.FreeDiskMiB = hostFreeDiskMiB(cwds, statfsFreeMiB)
	reserve := policy.DiskReserveMiB()
	if prior != nil && prior.PolicyVersion == usage.PolicyVersion && prior.Complete == usage.Complete && prior.SourceDigest == usage.SourceDigest && prior.LimiterDomain == usage.LimiterDomain && diskReserveState(prior.FreeDiskMiB, reserve) == diskReserveState(usage.FreeDiskMiB, reserve) {
		observed, err := time.Parse(time.RFC3339Nano, prior.ObservedAt)
		if err == nil && now.Sub(observed) < 20*time.Second {
			return nil
		}
	}
	free := int64(-1)
	if usage.FreeDiskMiB != nil {
		free = *usage.FreeDiskMiB
	}
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s:%s:%d", usage.Host, usage.LimiterDomain, usage.PolicyVersion, usage.ObservedAt, usage.SourceDigest, free)))
	_, err := c.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: "host-usage-" + hex.EncodeToString(seed[:12]), Operation: "observe_host", Host: host, HostUsage: &usage})
	return err
}
