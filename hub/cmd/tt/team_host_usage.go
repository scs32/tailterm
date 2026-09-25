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

func saveHostRelayCensus(ctx context.Context, c *api.Client, task, host, domain string, policyVersion int64, prior *api.TeamHostUsage, now time.Time) error {
	usage := hostRelayCensus(relayDir(), host, domain, now)
	usage.PolicyVersion = policyVersion
	if prior != nil && prior.PolicyVersion == usage.PolicyVersion && prior.Complete == usage.Complete && prior.SourceDigest == usage.SourceDigest && prior.LimiterDomain == usage.LimiterDomain {
		observed, err := time.Parse(time.RFC3339Nano, prior.ObservedAt)
		if err == nil && now.Sub(observed) < 20*time.Second {
			return nil
		}
	}
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d:%s:%s", usage.Host, usage.LimiterDomain, usage.PolicyVersion, usage.ObservedAt, usage.SourceDigest)))
	_, err := c.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: "host-usage-" + hex.EncodeToString(seed[:12]), Operation: "observe_host", Host: host, HostUsage: &usage})
	return err
}
