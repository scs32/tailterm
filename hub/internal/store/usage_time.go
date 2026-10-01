package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Time accounting (docs/usage-accounting.md, Time). The relay uploads each
// completed turn as immutable chunks; the hub projects every segment piece
// with the token ledger's own rule at ingest and assembles the report, with
// what each wait was for, at read time.
const usageSpansSchema = `CREATE TABLE IF NOT EXISTS usage_spans(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,span_id TEXT NOT NULL,
 start_at TEXT NOT NULL,end_at TEXT NOT NULL,payload TEXT NOT NULL,projection TEXT NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id,span_id));
 CREATE INDEX IF NOT EXISTS usage_spans_time ON usage_spans(task_id,start_at);`

const usageSpanMaxEntries = 32

// usageSpanPiece is a stretch of one chunk with a single attribution.
type usageSpanPiece struct {
	From        time.Time              `json:"from"`
	To          time.Time              `json:"to"`
	Shares      []api.UsageAttribution `json:"shares"`
	Phase       string                 `json:"phase"`
	ReviewRound int                    `json:"reviewRound,omitempty"`
	Role        string                 `json:"role"`
}
type usageSpanProjection struct {
	AgentID string           `json:"agentId"`
	RunID   string           `json:"runId"`
	Pieces  []usageSpanPiece `json:"pieces"`
}

func validUsageSpan(s api.UsageSpan) bool {
	if s.Turn == "" || len(s.Turn) > 160 || s.Chunk < 0 || s.Chunk > 64 || s.ID != fmt.Sprintf("%s#%d", s.Turn, s.Chunk) {
		return false
	}
	for _, r := range s.Turn {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	if s.Start.IsZero() || s.End.Before(s.Start) || len(s.Gap) > 240 || len(s.SourceDigest) != 64 || len(s.Handled) > 64 {
		return false
	}
	if _, err := hex.DecodeString(s.SourceDigest); err != nil {
		return false
	}
	if len(s.Segments) == 0 || len(s.Tool)+len(s.Wait)+len(s.Mixed)+len(s.Segments) > usageSpanMaxEntries {
		return false
	}
	if s.Unavailable && len(s.Tool)+len(s.Wait)+len(s.Mixed) > 0 {
		return false
	}
	// Segments cover the chunk exactly.
	at := s.Start
	for _, piece := range s.Segments {
		if piece.At.IsZero() || !piece.From.Equal(at) || piece.To.Before(piece.From) {
			return false
		}
		at = piece.To
	}
	if !at.Equal(s.End) {
		return false
	}
	// Tool, wait and mixed intervals lie inside the chunk and never overlap.
	var all [][2]time.Time
	for _, list := range [][][2]time.Time{s.Tool, s.Wait, s.Mixed} {
		for _, x := range list {
			if x[0].Before(s.Start) || x[1].After(s.End) || !x[1].After(x[0]) {
				return false
			}
			all = append(all, x)
		}
	}
	if spanLength(spanUnion(all)) != spanLength(all) {
		return false
	}
	for _, e := range s.Handled {
		if !api.ValidID(e.TaskID, "tsk") || e.Seq < 1 || e.At.IsZero() || (e.Operation != "ack" && e.Operation != "progress" && e.Operation != "reply" && e.Operation != "post") {
			return false
		}
	}
	return true
}

// storeUsageSpans keeps each new chunk with its projection. A chunk is
// immutable: an identical replay is a no-op and a different payload under
// the same id is a conflict.
func storeUsageSpans(ctx context.Context, tx *sql.Tx, task, agent, run string, spans []api.UsageSpan, provenance usageRunProvenance) error {
	for _, span := range spans {
		payload, _ := json.Marshal(span)
		var old string
		err := tx.QueryRowContext(ctx, `SELECT payload FROM usage_spans WHERE task_id=? AND agent_id=? AND run_id=? AND span_id=?`, task, agent, run, span.ID).Scan(&old)
		if err == nil {
			if old != string(payload) {
				return api.ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		projection, err := projectUsageSpan(ctx, tx, task, agent, run, span, provenance)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(projection)
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_spans VALUES(?,?,?,?,?,?,?,?)`, task, agent, run, span.ID, ts(span.Start), ts(span.End), string(payload), string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// projectUsageSpan resolves every segment piece at the timestamp of the
// request that owns it, exactly as that request's tokens are resolved, so a
// request and the time in its segment always get the same item, phase and
// role. Each piece names its own request, so the result does not depend on
// where the turn was cut into chunks.
func projectUsageSpan(ctx context.Context, tx *sql.Tx, task, agent, run string, span api.UsageSpan, provenance usageRunProvenance) (usageSpanProjection, error) {
	out := usageSpanProjection{AgentID: agent, RunID: run, Pieces: []usageSpanPiece{}}
	resolved := map[time.Time]api.UsageProjection{}
	for _, segment := range span.Segments {
		key := segment.At.UTC()
		p, ok := resolved[key]
		if !ok {
			var err error
			p, err = resolveUsage(ctx, tx, task, agent, run, api.UsageTurn{At: segment.At, Handled: span.Handled}, provenance)
			if err != nil {
				return out, err
			}
			resolved[key] = p
		}
		piece := usageSpanPiece{From: segment.From, To: segment.To, Shares: p.Shares, Phase: p.Phase, ReviewRound: p.ReviewRound, Role: p.Role}
		if n := len(out.Pieces); n > 0 {
			last := &out.Pieces[n-1]
			if last.To.Equal(piece.From) && last.Phase == piece.Phase && last.ReviewRound == piece.ReviewRound && last.Role == piece.Role && reflect.DeepEqual(last.Shares, piece.Shares) {
				last.To = piece.To
				continue
			}
		}
		out.Pieces = append(out.Pieces, piece)
	}
	return out, nil
}

// Interval arithmetic over half-open [from,to) stretches.

// spanUnion returns the exact union, sorted and disjoint: overlapping or
// touching stretches merge and a gap is never bridged.
func spanUnion(in [][2]time.Time) [][2]time.Time {
	sorted := append([][2]time.Time(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0].Before(sorted[j][0]) })
	out := [][2]time.Time{}
	for _, x := range sorted {
		if !x[1].After(x[0]) {
			continue
		}
		if n := len(out); n > 0 && !x[0].After(out[n-1][1]) {
			if x[1].After(out[n-1][1]) {
				out[n-1][1] = x[1]
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

// spanSubtract removes cut from in. Both are sorted and disjoint.
func spanSubtract(in, cut [][2]time.Time) [][2]time.Time {
	out := [][2]time.Time{}
	for _, x := range in {
		from := x[0]
		for _, c := range cut {
			if !c[1].After(from) {
				continue
			}
			if !c[0].Before(x[1]) {
				break
			}
			if c[0].After(from) {
				out = append(out, [2]time.Time{from, c[0]})
			}
			from = c[1]
		}
		if from.Before(x[1]) {
			out = append(out, [2]time.Time{from, x[1]})
		}
	}
	return out
}

// spanClip keeps the parts of in inside [from,to).
func spanClip(in [][2]time.Time, from, to time.Time) [][2]time.Time {
	out := [][2]time.Time{}
	for _, x := range in {
		if x[0].Before(from) {
			x[0] = from
		}
		if x[1].After(to) {
			x[1] = to
		}
		if x[1].After(x[0]) {
			out = append(out, x)
		}
	}
	return out
}
func spanLength(in [][2]time.Time) (total time.Duration) {
	for _, x := range in {
		total += x[1].Sub(x[0])
	}
	return total
}

// usagePhaseKey is the phase label the token ledger groups by.
func usagePhaseKey(phase string, round int) string {
	if phase != "review" {
		return phase
	}
	if round > 0 {
		return phase + " round " + strconv.Itoa(round)
	}
	return phase + " round unavailable"
}

// timePiece is one attributed stretch of an agent's turn on one item.
type timePiece struct {
	from, to               time.Time
	den                    int64
	phase, role            string
	tool, wait, unmeasured [][2]time.Time
}
type timeAgent struct {
	pieces []timePiece
	polls  map[string]bool
	pollNs *big.Rat
}

// waitStretch is waiting with a weight: 1 outside the agent's item work and
// inside a blocking wait, (d-1)/d over model and tool time shared by d items.
type waitStretch struct {
	from, to    time.Time
	num, den    int64
	phase, role string
}

// waitCandidate is something the Board shows an agent could be waiting for.
// It is open over its recorded interval, whatever its state is now.
type waitCandidate struct {
	seq      int64
	subject  string
	author   string
	owner    bool // an owner obligation or an owner decision request
	handler  bool // the recipient is a database handler
	from, to time.Time
	items    map[string]bool
}

func (c waitCandidate) openOver(from, to time.Time) bool {
	return !c.from.After(from) && (c.to.IsZero() || !c.to.Before(to))
}

func loadWaitCandidates(ctx context.Context, tx *sql.Tx, task string) ([]waitCandidate, error) {
	links := map[int64]map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT message_seq,item_id FROM message_work_item_links WHERE message_task_id=? AND item_task_id=?`, task, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var seq int64
		var item string
		if err = rows.Scan(&seq, &item); err != nil {
			rows.Close()
			return nil, err
		}
		if links[seq] == nil {
			links[seq] = map[string]bool{}
		}
		links[seq][item] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []waitCandidate
	rows, err = tx.QueryContext(ctx, `SELECT o.message_seq,o.subject,m.from_agent,o.recipient_kind,COALESCE(a.role,''),o.created_at,o.closed_at
FROM obligations o JOIN messages m ON m.seq=o.message_seq LEFT JOIN agents a ON a.id=o.agent_id WHERE o.task_id=?`, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c waitCandidate
		var kind, role, created, closed string
		if err = rows.Scan(&c.seq, &c.subject, &c.author, &kind, &role, &created, &closed); err != nil {
			rows.Close()
			return nil, err
		}
		c.owner, c.handler = kind == api.ObligationRecipientOwner, role == api.AgentRoleDatabaseHandler
		c.from, c.to, c.items = parseTS(created), parseTS(closed), links[c.seq]
		out = append(out, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.message_seq,r.question,m.from_agent,m.created_at,COALESCE(am.created_at,'')
FROM decision_requests r JOIN messages m ON m.seq=r.message_seq
LEFT JOIN decision_answers a ON a.task_id=r.task_id AND a.request_seq=r.message_seq LEFT JOIN messages am ON am.seq=a.message_seq WHERE r.task_id=?`, task)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		c := waitCandidate{owner: true}
		var created, answered string
		if err = rows.Scan(&c.seq, &c.subject, &c.author, &created, &answered); err != nil {
			rows.Close()
			return nil, err
		}
		if len(c.subject) > 120 {
			c.subject = strings.ToValidUTF8(c.subject[:120], "")
		}
		c.from, c.to, c.items = parseTS(created), parseTS(answered), links[c.seq]
		out = append(out, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].from.Equal(out[j].from) {
			return out[i].from.Before(out[j].from)
		}
		return out[i].seq < out[j].seq
	})
	return out, nil
}

// waitCause labels one stretch during which nothing relevant opens or closes
// by the first rule that matches. Candidates are in creation order, so the
// earliest created wins inside a rule.
func waitCause(candidates []waitCandidate, agent, item string, from, to time.Time) (string, *waitCandidate) {
	rules := []struct {
		cause string
		match func(c *waitCandidate) bool
	}{
		{"owner", func(c *waitCandidate) bool { return c.owner && c.author == agent }},
		{"handler", func(c *waitCandidate) bool { return !c.owner && c.handler && c.author == agent }},
		{"teammate", func(c *waitCandidate) bool { return !c.owner && c.author == agent }},
		{"owner", func(c *waitCandidate) bool { return c.owner && c.items[item] }},
	}
	for _, rule := range rules {
		for i := range candidates {
			if c := &candidates[i]; rule.match(c) && c.openOver(from, to) {
				return rule.cause, c
			}
		}
	}
	return "unknown", nil
}

// usageTimeReports assembles, per item id ("" is project overhead), where
// its wall time went. Everything is read from durable tables, so the report
// is the same after close and restart and costs no agent turn.
func usageTimeReports(ctx context.Context, tx *sql.Tx, task string, q api.UsageQuery) (map[string]*api.UsageTime, error) {
	clip := func(from, to time.Time) (time.Time, time.Time, bool) {
		if !q.From.IsZero() && from.Before(q.From) {
			from = q.From
		}
		if !q.To.IsZero() && to.After(q.To) {
			to = q.To
		}
		return from, to, to.After(from)
	}
	items := map[string]map[string]*timeAgent{}
	agentOf := func(item, agent string) *timeAgent {
		if items[item] == nil {
			items[item] = map[string]*timeAgent{}
		}
		if items[item][agent] == nil {
			items[item][agent] = &timeAgent{polls: map[string]bool{}, pollNs: new(big.Rat)}
		}
		return items[item][agent]
	}
	rows, err := tx.QueryContext(ctx, `SELECT agent_id,run_id,payload,projection FROM usage_spans ORDER BY agent_id,start_at,span_id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var agent, run, payload, encoded string
		if err = rows.Scan(&agent, &run, &payload, &encoded); err != nil {
			break
		}
		var span api.UsageSpan
		var projection usageSpanProjection
		if err = json.Unmarshal([]byte(payload), &span); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(encoded), &projection); err != nil {
			break
		}
		busy := spanUnion(append(append(append([][2]time.Time(nil), span.Tool...), span.Wait...), span.Mixed...))
		for _, p := range projection.Pieces {
			from, to, ok := clip(p.From, p.To)
			for _, share := range p.Shares {
				if share.TaskID != task || (q.Item != "" && share.ItemID != q.Item) || share.Denominator < 1 {
					continue
				}
				a := agentOf(share.ItemID, agent)
				if ok {
					piece := timePiece{from: from, to: to, den: share.Denominator, phase: usagePhaseKey(p.Phase, p.ReviewRound), role: p.Role}
					if span.Unavailable {
						piece.unmeasured = [][2]time.Time{{from, to}}
					} else {
						piece.tool, piece.wait, piece.unmeasured = spanClip(span.Tool, from, to), spanClip(span.Wait, from, to), spanClip(span.Mixed, from, to)
					}
					a.pieces = append(a.pieces, piece)
				}
				// A poll is counted once per request, at the request's time.
				for _, segment := range span.Segments {
					if !segment.Poll || segment.From.Before(p.From) || segment.To.After(p.To) {
						continue
					}
					if (!q.From.IsZero() && segment.At.Before(q.From)) || (!q.To.IsZero() && !segment.At.Before(q.To)) {
						continue
					}
					a.polls[run+"/"+span.Turn+"/"+ts(segment.At)] = true
					if sf, st, some := clip(segment.From, segment.To); some {
						model := spanSubtract([][2]time.Time{{sf, st}}, busy)
						a.pollNs.Add(a.pollNs, big.NewRat(int64(spanLength(model)), share.Denominator))
					}
				}
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := map[string]*api.UsageTime{}
	if len(items) == 0 {
		return out, nil
	}
	candidates, err := loadWaitCandidates(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	rows, err = tx.QueryContext(ctx, `SELECT id,name FROM agents`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			break
		}
		names[id] = name
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	for item, agents := range items {
		if report := assembleUsageTime(item, agents, candidates, names); report != nil {
			out[item] = report
		}
	}
	return out, nil
}

type timeSplit struct{ model, tool, waiting *big.Rat }

func newTimeSplit() *timeSplit { return &timeSplit{new(big.Rat), new(big.Rat), new(big.Rat)} }

// usageMs renders exact nanoseconds as milliseconds.
func usageMs(ns *big.Rat) string {
	return new(big.Rat).Quo(ns, big.NewRat(int64(time.Millisecond), 1)).RatString()
}
func durationRat(d time.Duration, den int64) *big.Rat { return big.NewRat(int64(d), den) }

func assembleUsageTime(item string, agents map[string]*timeAgent, candidates []waitCandidate, names map[string]string) *api.UsageTime {
	var from, to time.Time
	ids := make([]string, 0, len(agents))
	for id, a := range agents {
		// An agent has one turn at a time; keep its stretches ordered and disjoint.
		sort.SliceStable(a.pieces, func(i, j int) bool { return a.pieces[i].from.Before(a.pieces[j].from) })
		kept := a.pieces[:0]
		var end time.Time
		for _, p := range a.pieces {
			if p.from.Before(end) {
				p.from = end
			}
			if !p.to.After(p.from) {
				continue
			}
			p.tool, p.wait, p.unmeasured = spanClip(p.tool, p.from, p.to), spanClip(p.wait, p.from, p.to), spanClip(p.unmeasured, p.from, p.to)
			kept = append(kept, p)
			end = p.to
		}
		a.pieces = kept
		if len(kept) == 0 {
			continue
		}
		ids = append(ids, id)
		if from.IsZero() || kept[0].from.Before(from) {
			from = kept[0].from
		}
		if kept[len(kept)-1].to.After(to) {
			to = kept[len(kept)-1].to
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool {
		if names[ids[i]] != names[ids[j]] {
			return names[ids[i]] < names[ids[j]]
		}
		return ids[i] < ids[j]
	})
	wall := to.Sub(from)
	report := &api.UsageTime{WallMs: usageMs(durationRat(wall, 1)), From: from, To: to, Agents: []api.UsageTimeAgent{}, Waits: []api.UsageTimeWait{}}
	total, unmeasuredTotal, pollTotal := newTimeSplit(), new(big.Rat), new(big.Rat)
	phases, roles, phaseRoles := map[string]*timeSplit{}, map[string]*timeSplit{}, map[string]*timeSplit{}
	split := func(m map[string]*timeSplit, key string) *timeSplit {
		if m[key] == nil {
			m[key] = newTimeSplit()
		}
		return m[key]
	}
	type waitKey struct {
		cause string
		seq   int64
	}
	type waitTotal struct {
		ns      *big.Rat
		subject string
		agents  map[string]bool
	}
	waits := map[waitKey]*waitTotal{}
	causes := map[string]*big.Rat{"owner": new(big.Rat), "handler": new(big.Rat), "teammate": new(big.Rat), "unknown": new(big.Rat)}
	var modelAll, toolAll, unmeasuredAll [][2]time.Time
	for _, id := range ids {
		a := agents[id]
		mine, unmeasured := newTimeSplit(), new(big.Rat)
		var stretches []waitStretch
		// Model and tool time go to the phase and role of the segment they
		// fall in; so does a blocking wait inside it. Waiting outside the
		// agent's item work takes the phase and role of its latest item
		// segment that ended before it, or of its first one.
		add := func(kind int, phase, role string, d time.Duration, num, den int64) {
			if d <= 0 || num == 0 {
				return
			}
			amount := big.NewRat(int64(d)*num, den)
			for _, s := range []*timeSplit{mine, total, split(phases, phase), split(roles, role), split(phaseRoles, phase+" / "+role)} {
				switch kind {
				case 0:
					s.model.Add(s.model, amount)
				case 1:
					s.tool.Add(s.tool, amount)
				default:
					s.waiting.Add(s.waiting, amount)
				}
			}
		}
		waiting := func(from, to time.Time, num, den int64, phase, role string) {
			if to.After(from) && num > 0 {
				stretches = append(stretches, waitStretch{from, to, num, den, phase, role})
				add(2, phase, role, to.Sub(from), num, den)
			}
		}
		at := from
		for i, p := range a.pieces {
			reference := p
			if i > 0 {
				reference = a.pieces[i-1]
			}
			waiting(at, p.from, 1, 1, reference.phase, reference.role)
			tool := spanSubtract(spanSubtract(spanUnion(p.tool), spanUnion(p.wait)), spanUnion(p.unmeasured))
			wait := spanSubtract(spanUnion(p.wait), spanUnion(p.unmeasured))
			busy := spanUnion(append(append(append([][2]time.Time(nil), tool...), wait...), p.unmeasured...))
			model := spanSubtract([][2]time.Time{{p.from, p.to}}, busy)
			add(0, p.phase, p.role, spanLength(model), 1, p.den)
			add(1, p.phase, p.role, spanLength(tool), 1, p.den)
			unmeasured.Add(unmeasured, durationRat(spanLength(p.unmeasured), 1))
			for _, x := range wait {
				waiting(x[0], x[1], 1, 1, p.phase, p.role)
			}
			// The rest of a request shared by several items is waiting here.
			for _, x := range spanUnion(append(append([][2]time.Time(nil), model...), tool...)) {
				waiting(x[0], x[1], p.den-1, p.den, p.phase, p.role)
			}
			modelAll, toolAll, unmeasuredAll = append(modelAll, model...), append(toolAll, tool...), append(unmeasuredAll, p.unmeasured...)
			at = p.to
		}
		last := a.pieces[len(a.pieces)-1]
		waiting(at, to, 1, 1, last.phase, last.role)

		// What each wait was for: split at every open and close, then label.
		for _, w := range stretches {
			cuts := []time.Time{w.from, w.to}
			for _, c := range candidates {
				if c.author != id && !(c.owner && c.items[item]) {
					continue
				}
				for _, edge := range []time.Time{c.from, c.to} {
					if edge.After(w.from) && edge.Before(w.to) {
						cuts = append(cuts, edge)
					}
				}
			}
			sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
			for i := 1; i < len(cuts); i++ {
				if !cuts[i].After(cuts[i-1]) {
					continue
				}
				cause, c := waitCause(candidates, id, item, cuts[i-1], cuts[i])
				amount := big.NewRat(int64(cuts[i].Sub(cuts[i-1]))*w.num, w.den)
				causes[cause].Add(causes[cause], amount)
				key := waitKey{cause: cause}
				subject := ""
				if c != nil {
					key.seq, subject = c.seq, c.subject
				}
				if waits[key] == nil {
					waits[key] = &waitTotal{ns: new(big.Rat), subject: subject, agents: map[string]bool{}}
				}
				waits[key].ns.Add(waits[key].ns, amount)
				waits[key].agents[id] = true
			}
		}
		unmeasuredTotal.Add(unmeasuredTotal, unmeasured)
		pollTotal.Add(pollTotal, a.pollNs)
		report.Polls += len(a.polls)
		name := names[id]
		if name == "" {
			name = id
		}
		report.Agents = append(report.Agents, api.UsageTimeAgent{AgentID: id, Name: name, Role: last.role, ModelMs: usageMs(mine.model), ToolMs: usageMs(mine.tool), WaitingMs: usageMs(mine.waiting), UnmeasuredMs: usageMs(unmeasured), Polls: len(a.polls), PollMs: usageMs(a.pollNs)})
	}
	report.ModelMs, report.ToolMs, report.WaitingMs = usageMs(total.model), usageMs(total.tool), usageMs(total.waiting)
	report.UnmeasuredMs, report.PollMs = usageMs(unmeasuredTotal), usageMs(pollTotal)
	rowsOf := func(m map[string]*timeSplit) []api.UsageTimeSplit {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []api.UsageTimeSplit{}
		for _, k := range keys {
			out = append(out, api.UsageTimeSplit{Key: k, ModelMs: usageMs(m[k].model), ToolMs: usageMs(m[k].tool), WaitingMs: usageMs(m[k].waiting)})
		}
		return out
	}
	report.Phases, report.Roles, report.PhaseRoles = rowsOf(phases), rowsOf(roles), rowsOf(phaseRoles)

	// Team timeline over the window, from unsplit intervals: some model
	// working, then only tools running, then nobody active. A stretch any
	// agent could not split is left out of all three.
	unmeasured := spanUnion(unmeasuredAll)
	model := spanSubtract(spanUnion(modelAll), unmeasured)
	toolsOnly := spanSubtract(spanSubtract(spanUnion(toolAll), model), unmeasured)
	idle := wall - spanLength(unmeasured) - spanLength(model) - spanLength(toolsOnly)
	report.Timeline = api.UsageTimeline{ModelMs: usageMs(durationRat(spanLength(model), 1)), ToolsOnlyMs: usageMs(durationRat(spanLength(toolsOnly), 1)), IdleMs: usageMs(durationRat(idle, 1)), UnmeasuredMs: usageMs(durationRat(spanLength(unmeasured), 1))}

	report.Causes = api.UsageTimeCauses{Owner: usageMs(causes["owner"]), Handler: usageMs(causes["handler"]), Teammate: usageMs(causes["teammate"]), Unknown: usageMs(causes["unknown"])}
	keys := make([]waitKey, 0, len(waits))
	for k := range waits {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if c := waits[keys[i]].ns.Cmp(waits[keys[j]].ns); c != 0 {
			return c > 0
		}
		if keys[i].seq != keys[j].seq {
			return keys[i].seq < keys[j].seq
		}
		return keys[i].cause < keys[j].cause
	})
	for _, k := range keys {
		if len(report.Waits) == 5 {
			break
		}
		w := waits[k]
		waiters := make([]string, 0, len(w.agents))
		for id := range w.agents {
			if names[id] != "" {
				id = names[id]
			}
			waiters = append(waiters, id)
		}
		sort.Strings(waiters)
		report.Waits = append(report.Waits, api.UsageTimeWait{Cause: k.cause, MessageSeq: k.seq, Subject: w.subject, AwaitedBy: strings.Join(waiters, ", "), Ms: usageMs(w.ns)})
	}
	return report
}
