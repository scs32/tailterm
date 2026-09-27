package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Shares the activity append reader. Only metadata and numeric classes persist.
// Request rows remain provisional until the activation's handled set is known.
type usageCursor struct {
	UploadedCoverage  string                   `json:"uploadedCoverage"`
	Enrolled          bool                     `json:"enrolled"`
	LastUploadAttempt time.Time                `json:"lastUploadAttempt"`
	WrongEpoch        bool                     `json:"wrongEpoch"`
	Binding           runtimeBinding           `json:"binding"`
	Model             string                   `json:"model"`
	Activation        string                   `json:"activation"`
	Start             time.Time                `json:"start"`
	Checkpoint        map[string]int64         `json:"checkpoint,omitempty"`
	Turns             map[string]api.UsageTurn `json:"turns"`
	Dirty             map[string]bool          `json:"dirty"`
	Coverage          string                   `json:"coverage"`
	StartedAt         time.Time                `json:"startedAt"`
	Pending           *api.UsageBatch          `json:"pending,omitempty"`
	Uploaded          map[string]int64         `json:"uploaded,omitempty"`
	Finished          map[string]bool          `json:"finished,omitempty"`
	RejectedCoverage  string                   `json:"rejectedCoverage,omitempty"`
	Frozen            bool                     `json:"frozen"`
}

func usageStatePath(b runtimeBinding) string {
	return filepath.Join(relayDir(), b.Agent+"-"+b.Run+".usage-state.json")
}
func loadUsageCursor(b runtimeBinding) (*usageCursor, error) {
	path := usageStatePath(b)
	var u usageCursor
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &u); err != nil {
			return nil, err
		}
		if u.Binding.Run != b.Run || u.Binding.Thread != b.Thread {
			return nil, fmt.Errorf("usage run identity mismatch")
		}
	}
	if u.Binding.Run == "" {
		u = usageCursor{Binding: b, StartedAt: time.Now().UTC(), Coverage: "partial: awaiting first reconciled request", Turns: map[string]api.UsageTurn{}, Dirty: map[string]bool{}}
	}
	if u.Uploaded == nil {
		u.Uploaded = map[string]int64{}
	}
	if u.Finished == nil {
		u.Finished = map[string]bool{}
	}
	return &u, nil
}
func saveUsageCursor(u *usageCursor) error { return writePrivateJSON(usageStatePath(u.Binding), u) }
func usageFields(raw json.RawMessage) map[string]int64 {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	out := map[string]int64{}
	for k, v := range fields {
		if strings.TrimSpace(string(v)) == "null" {
			continue
		}
		var n int64
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			out[k] = n
		}
	}
	var details map[string]json.RawMessage
	if json.Unmarshal(fields["output_tokens_details"], &details) == nil {
		var thinking int64
		if strings.TrimSpace(string(details["thinking_tokens"])) != "null" && json.Unmarshal(details["thinking_tokens"], &thinking) == nil && thinking >= 0 {
			out["output_tokens_details.thinking_tokens"] = thinking
		}
	}
	return out
}
func normalizeUsage(runtime string, raw map[string]int64) (map[string]int64, string) {
	return api.NormalizeUsageTokens(runtime, raw)
}
func (u *usageCursor) put(id string, at time.Time, model string, raw map[string]int64, digest string) {
	runtime := u.Binding.Runtime
	if runtime == "" {
		runtime = "codex"
	}
	tokens, gap := normalizeUsage(runtime, raw)
	if len(tokens) == 0 {
		u.Coverage = "partial: token normalization unavailable"
		return
	}
	if u.Finished[id] {
		return
	}
	old, exists := u.Turns[id]
	t := api.UsageTurn{ID: id, Revision: 1, Runtime: runtime, Session: u.Binding.Thread, Model: model, At: at, Tokens: tokens, Raw: raw, SourceDigest: digest, Activation: u.Activation, Gap: gap}
	if exists {
		if old.Model != model || (!old.Complete && old.Activation != u.Activation) {
			u.Coverage = "partial: conflicting request identity"
			return
		}
		t = old
		t.Tokens = map[string]int64{}
		t.Raw = map[string]int64{}
		for k, v := range old.Tokens {
			t.Tokens[k] = v
		}
		for k, v := range old.Raw {
			t.Raw[k] = v
		}
		for k, v := range tokens {
			if v > t.Tokens[k] {
				t.Tokens[k] = v
			} else if _, ok := t.Tokens[k]; !ok {
				t.Tokens[k] = v
			}
		}
		for k, v := range raw {
			if v > t.Raw[k] {
				t.Raw[k] = v
			} else if _, ok := t.Raw[k]; !ok {
				t.Raw[k] = v
			}
		}
		t.Tokens, t.Gap = normalizeUsage(runtime, t.Raw)
		if reflect.DeepEqual(old.Tokens, t.Tokens) && reflect.DeepEqual(old.Raw, t.Raw) {
			return
		}

		t.Revision = u.nextRevision(id)
		t.SourceDigest = digest
	}
	if u.Coverage == "partial: awaiting first reconciled request" && !u.Start.IsZero() && !strings.HasPrefix(u.Activation, "unbounded-") && !strings.HasPrefix(u.Activation, "claude-") {
		u.Coverage = "measured from first reconciled request"
	}
	u.Turns[id] = t
	u.Dirty[id] = true
}
func (u *usageCursor) finish(at time.Time) {
	if u.Start.IsZero() {
		return
	}
	handled, partial := usageHandled(u.Binding, u.Start, at)
	if partial {
		u.Coverage = "partial: handled context retention exceeded"
	}
	for id, t := range u.Turns {
		if t.Activation != u.Activation || t.Complete {
			continue
		}
		t.Handled = handled
		t.Complete = true
		t.Revision = u.nextRevision(id)
		u.Turns[id] = t
		u.Dirty[id] = true
	}
	u.Start = time.Time{}
	u.Activation = ""
}
func (u *usageCursor) parse(line []byte) error {
	var rec activityRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		u.Coverage = "partial: malformed transcript record"
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		u.Coverage = "partial: request timestamp unavailable"
		return nil
	}
	var p struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Model     string `json:"model"`
		TurnID    string `json:"turn_id"`
		RequestID string `json:"request_id"`
		Info      struct {
			Last  json.RawMessage `json:"last_token_usage"`
			Total json.RawMessage `json:"total_token_usage"`
		} `json:"info"`
	}
	_ = json.Unmarshal(rec.Payload, &p)
	digest := fmt.Sprintf("%x", sha256.Sum256(line))
	begin := func() {
		if !u.Start.IsZero() {
			u.finish(at)
		}
		u.Start = at
		u.Activation = "activation-" + fmt.Sprintf("%x", sha256.Sum256([]byte(u.Binding.Thread+at.String())))[:24]
		if p.TurnID != "" {
			u.Activation = p.TurnID
		}
	}
	if rec.Type == "session_meta" {
		if p.ID != "" && p.ID != u.Binding.Thread {
			u.WrongEpoch = true
			u.Coverage = "partial: transcript session identity differs from binding"
		}
		return nil
	}
	if u.WrongEpoch {
		return nil
	}
	if rec.Type == "turn_context" {
		if p.Model != "" {
			u.Model = p.Model
		}
		return nil
	}
	if (rec.Type == "event_msg" && p.Type == "task_started") || rec.Type == "task_started" {
		begin()
		return nil
	}
	if (rec.Type == "event_msg" && p.Type == "task_complete") || rec.Type == "task_complete" || rec.Type == "result" {
		u.finish(at)
		return nil
	}
	runtime := u.Binding.Runtime
	if runtime == "" {
		runtime = "codex"
	}
	if runtime == "claude" {
		if rec.Type == "user" {
			var msg struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(rec.Message, &msg)
			var content string
			if json.Unmarshal(msg.Content, &content) == nil {
				begin()
			}
			return nil
		}
		if rec.Type != "assistant" {
			return nil
		}
		var msg struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
			Stop  string          `json:"stop_reason"`
		}
		if json.Unmarshal(rec.Message, &msg) != nil {
			return nil
		}
		if msg.ID == "" || len(msg.Usage) == 0 {
			u.Coverage = "partial: Claude request identity or usage unavailable"
			return nil
		}
		if u.Start.IsZero() {
			u.Start = at
			u.Activation = "claude-" + msg.ID
		}
		u.put("claude-"+msg.ID, at, msg.Model, usageFields(msg.Usage), digest)
		if msg.Stop == "end_turn" {
			u.finish(at)
		}
		return nil
	}
	if rec.Type != "event_msg" || p.Type != "token_count" {
		return nil
	}
	total := usageFields(p.Info.Total)
	last := usageFields(p.Info.Last)
	if len(total) == 0 {
		u.Coverage = "partial: cumulative checkpoint unavailable"
		return nil
	}
	if reflect.DeepEqual(total, u.Checkpoint) {
		return nil
	}
	prior := u.Checkpoint
	if len(prior) > 0 {
		for k, n := range prior {
			v, ok := total[k]
			if !ok || v < n {
				u.Coverage = "partial: cumulative reset"
				return nil
			}
		}
	}
	u.Checkpoint = total
	// Use last usage only if reconciled with the checkpoint. An unexplained
	// lifetime first snapshot is never emitted as one fabricated large request.
	delta := map[string]int64{}
	for k, v := range total {
		delta[k] = v - prior[k]
	}
	if len(prior) == 0 {
		if len(last) == 0 || !reflect.DeepEqual(last, total) {
			u.Coverage = "partial: first checkpoint has unmeasured history"
			return nil
		}
	}
	if len(last) == 0 {
		last = delta
	} else {
		for k, v := range last {
			if delta[k] != v {
				u.Coverage = "partial: checkpoint gap"
				return nil
			}
		}
	}
	if u.Start.IsZero() {
		u.Start = at
		u.Activation = "unbounded-" + digest[:16]
		u.Coverage = "partial: activation start unavailable"
	}
	rawIdentity, _ := json.Marshal(total)
	id := "codex-" + fmt.Sprintf("%x", sha256.Sum256(append([]byte(u.Binding.Thread), rawIdentity...)))[:32]
	if p.RequestID != "" {
		id = "codex-" + p.RequestID
	}
	u.put(id, at, u.Model, last, digest)
	return nil
}
func freezeUsageBatch(u *usageCursor) error {
	if u.Pending != nil {
		return saveUsageCursor(u)
	}
	keys := []string{}
	for k, v := range u.Dirty {
		if v {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	batch := api.UsageBatch{Version: api.UsageVersion, RunID: u.Binding.Run, Session: u.Binding.Thread, StartedAt: u.StartedAt, Coverage: u.Coverage, Turns: []api.UsageTurn{}}
	for _, k := range keys {
		if len(batch.Turns) >= 32 {
			break
		}
		candidate := append(batch.Turns, u.Turns[k])
		batch.Turns = candidate
		raw, _ := json.Marshal(batch)
		if len(raw) > 48<<10 {
			batch.Turns = batch.Turns[:len(batch.Turns)-1]
			break
		}
	}
	if len(batch.Turns) == 0 && ((u.Enrolled && u.UploadedCoverage == u.Coverage) || u.RejectedCoverage == u.Coverage) {
		return saveUsageCursor(u)
	}
	raw, _ := json.Marshal(batch)
	batch.RequestID = "usage-" + fmt.Sprintf("%x", sha256.Sum256(raw))[:32]
	u.Pending = &batch
	return saveUsageCursor(u)
}
func uploadUsage(ctx context.Context, u *usageCursor, c *api.Client) error {
	if u.Uploaded == nil {
		u.Uploaded = map[string]int64{}
	}
	if u.Finished == nil {
		u.Finished = map[string]bool{}
	}
	if err := freezeUsageBatch(u); err != nil {
		return err
	}
	if u.Pending == nil {
		return nil
	}
	if !u.LastUploadAttempt.IsZero() && time.Since(u.LastUploadAttempt) < 15*time.Second {
		return nil
	}
	u.LastUploadAttempt = time.Now().UTC()
	if err := saveUsageCursor(u); err != nil {
		return err
	}
	receipt, err := c.ReportUsage(ctx, u.Binding.Task, u.Binding.Agent, *u.Pending)
	if err != nil {
		var rejected *api.HTTPError
		if errors.As(err, &rejected) && (rejected.Status == 400 || rejected.Status == 404 || rejected.Status == 409) {
			// Persist the immutable rejected batch before advancing. Never label
			// it uploaded or discard the only numeric/provenance evidence.
			coverage := fmt.Sprintf("partial: upload rejected %d; batch quarantined locally", rejected.Status)
			quarantine := struct {
				Binding  runtimeBinding `json:"binding"`
				Batch    api.UsageBatch `json:"batch"`
				Status   int            `json:"status"`
				Coverage string         `json:"coverage"`
				At       time.Time      `json:"at"`
			}{u.Binding, *u.Pending, rejected.Status, coverage, time.Now().UTC()}
			path := usageStatePath(u.Binding) + ".rejected-" + u.Pending.RequestID + ".json"
			if saveErr := writePrivateJSON(path, quarantine); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			for _, turn := range u.Pending.Turns {
				delete(u.Dirty, turn.ID)
				delete(u.Turns, turn.ID)
				delete(u.Uploaded, turn.ID)
				u.Finished[turn.ID] = true
			}
			u.Pending = nil
			u.Coverage = coverage
			u.RejectedCoverage = coverage
			if saveErr := saveUsageCursor(u); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	if receipt.RequestID != u.Pending.RequestID || receipt.Turns != len(u.Pending.Turns) {
		return fmt.Errorf("usage receipt mismatch")
	}
	for _, t := range u.Pending.Turns {
		u.Uploaded[t.ID] = t.Revision
		current := u.Turns[t.ID]
		if reflect.DeepEqual(current, t) {
			delete(u.Dirty, t.ID)
			if t.Complete {
				// Keep a bounded recent projection for late Claude usage updates.
				// Exact request identity still counts once in the hub.
			}
		}
	}
	u.Enrolled = true
	u.UploadedCoverage = u.Pending.Coverage
	u.Pending = nil
	u.prune()
	return saveUsageCursor(u)
}

// Frozen outboxes are retried without loading agents or scanning transcripts.
// The caller's budgeted Client accounts for every upload; no agent turn is added.
func flushFrozenUsage(ctx context.Context, dir string, clientFor func(runtimeBinding) (*api.Client, error)) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.usage-state.json"))
	if err != nil {
		return err
	}
	// Rotate from the last attempted path, so transient failures and a small host
	// budget cannot repeatedly consume the slot ahead of later frozen runs.
	marker := filepath.Join(dir, "usage-flush-position.json")
	var last string
	if raw, e := os.ReadFile(marker); e == nil {
		_ = json.Unmarshal(raw, &last)
	}
	split := sort.SearchStrings(paths, last)
	if split < len(paths) && paths[split] == last {
		split++
	}
	paths = append(paths[split:], paths[:split]...)
	var failures []error
	attempted := 0
	for _, path := range paths {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		var u usageCursor
		raw, e := os.ReadFile(path)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		if e = json.Unmarshal(raw, &u); e != nil {
			failures = append(failures, e)
			continue
		}
		if !u.Frozen {
			continue
		}
		drained := func() bool {
			return len(u.Dirty) == 0 && u.Pending == nil && (u.Coverage == u.UploadedCoverage || u.Coverage == u.RejectedCoverage)
		}
		if drained() {
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				failures = append(failures, e)
			}
			continue
		}
		if !u.LastUploadAttempt.IsZero() && time.Since(u.LastUploadAttempt) < 15*time.Second {
			continue
		}
		if attempted >= 4 {
			break
		}
		if e = writePrivateJSON(marker, path); e != nil {
			failures = append(failures, e)
			break
		}
		attempted++
		c, e := clientFor(u.Binding)
		if e != nil {
			failures = append(failures, e)
			continue
		}
		if e = uploadUsage(ctx, &u, c); e != nil {
			failures = append(failures, e)
		}
		if drained() {
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				failures = append(failures, e)
			}
		}
	}
	return errors.Join(failures...)
}
func freezeRetiredUsage(b runtimeBinding) error {
	if _, err := os.Stat(usageStatePath(b)); os.IsNotExist(err) {
		return nil
	}
	u, err := loadUsageCursor(b)
	if err != nil {
		return err
	}
	u.Frozen = true
	u.Coverage = "partial: terminal boundary may omit final transcript tail"
	for id, t := range u.Turns {
		if !t.Complete {
			t.Gap = "terminal boundary before activation completion"
			t.Revision = u.nextRevision(id)
			u.Turns[id] = t
			u.Dirty[id] = true
		}
	}
	return freezeUsageBatch(u)
}

func (u *usageCursor) nextRevision(id string) int64 {
	n := u.Uploaded[id]
	if u.Pending != nil {
		for _, t := range u.Pending.Turns {
			if t.ID == id && t.Revision > n {
				n = t.Revision
			}
		}
	}
	return n + 1
}

func (u *usageCursor) prune() {
	if len(u.Turns) <= 512 {
		return
	}
	keys := []string{}
	for id, t := range u.Turns {
		if t.Complete && !u.Dirty[id] {
			keys = append(keys, id)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return u.Turns[keys[i]].At.Before(u.Turns[keys[j]].At) })
	for _, id := range keys {
		if len(u.Turns) <= 512 {
			break
		}
		u.Finished[id] = true
		delete(u.Turns, id)
		delete(u.Uploaded, id)
	}
	if len(u.Finished) > 4096 {
		u.Coverage = "partial: old request deduplication retention exhausted"
		u.Finished = map[string]bool{}
	}
}

// Host capability negotiation is cached privately for five minutes. It never
// changes the legacy activity observer when used against an older hub.
type usageEnabledContextKey struct{}

func prepareUsageContext(ctx context.Context, b runtimeBinding, c *api.Client, now time.Time) context.Context {
	path := filepath.Join(relayDir(), fmt.Sprintf("%x.usage-capability.json", sha256.Sum256([]byte(b.Hub))))
	var cached struct {
		At        time.Time `json:"at"`
		Supported bool      `json:"supported"`
	}
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &cached)
	if cached.At.IsZero() || now.Sub(cached.At) > 5*time.Minute {
		caps, err := c.Capabilities(ctx)
		if err != nil {
			var httpErr *api.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
				return ctx
			}
		}
		cached.At = now
		cached.Supported = caps.Usage.Supported
		_ = writePrivateJSON(path, cached)
	}
	return context.WithValue(ctx, usageEnabledContextKey{}, cached.Supported)
}
