package main

import (
	"encoding/json"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// No transcript text, message body, credentials or tool arguments are journaled.
// Inbox entries are candidates only; only successful handling operations meter.
func usageContextPath(e env) string {
	return filepath.Join(relayDir(), fmt.Sprintf("%s-%s.usage-context.json", e.agent, e.runID))
}
func recordUsageContext(e env, task, operation string, seq int64) {
	if e.agent == "" || e.runID == "" || seq < 1 {
		return
	}
	path := usageContextPath(e)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintln(os.Stderr, "usage context unavailable")
		return
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var entries []api.UsageEvidence
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &entries)
	entries = append(entries, api.UsageEvidence{TaskID: task, Seq: seq, Operation: operation, At: time.Now().UTC()})
	if len(entries) > 512 {
		// Keep the high-water timestamp of discarded evidence so attribution
		// never silently presents a truncated activation as complete.
		_ = writePrivateJSON(path+".gap.json", entries[len(entries)-513].At)
		entries = entries[len(entries)-512:]
	}
	if err = writePrivateJSON(path, entries); err != nil {
		fmt.Fprintln(os.Stderr, "usage context unavailable")
	}
}
func usageHandled(b runtimeBinding, from, to time.Time) ([]api.UsageEvidence, bool) {
	e := env{agent: b.Agent, runID: b.Run}
	var entries []api.UsageEvidence
	data, _ := os.ReadFile(usageContextPath(e))
	_ = json.Unmarshal(data, &entries)
	out := []api.UsageEvidence{}
	for _, x := range entries {
		if x.Operation != "inbox" && !x.At.Before(from) && !x.At.After(to) {
			out = append(out, x)
		}
	}
	var discardedThrough time.Time
	marker, _ := os.ReadFile(usageContextPath(e) + ".gap.json")
	_ = json.Unmarshal(marker, &discardedThrough)
	partial := !discardedThrough.IsZero() && !discardedThrough.Before(from)
	if len(out) > 64 {
		partial = true
		out = out[len(out)-64:]
	}
	return out, partial
}
