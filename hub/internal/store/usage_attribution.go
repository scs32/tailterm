package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type usageRunProvenance struct {
	Agent      api.Agent                 `json:"agent"`
	Binding    *api.AgentWorkItemBinding `json:"binding,omitempty"`
	Role       string                    `json:"role"`
	RoleSource string                    `json:"roleSource"`
}

func normalizeUsageRole(role string) string {
	switch strings.TrimSpace(role) {
	case "Planning and acceptance criteria":
		return "planner"
	case "Implementation":
		return "builder"
	case "Independent code review":
		return "reviewer"
	case "Independent matrix verification":
		return "verifier"
	default:
		return role
	}
}
func stringID(n int64) string { return strconv.FormatInt(n, 10) }
func freezeUsageRole(ctx context.Context, tx *sql.Tx, p *usageRunProvenance) error {
	a := p.Agent
	p.Role, p.RoleSource = "unknown", "no authoritative role"
	if a.Role != "" {
		p.Role, p.RoleSource = a.Role, "agent role"
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM item_team_leads WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		p.Role, p.RoleSource = "lead", "exact item lead"
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id=? AND orchestrator=?`, a.TaskID, a.Name).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		p.Role, p.RoleSource = "project_lead", "exact project lead"
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT launch_json FROM team_queue_entries WHERE task_id=? AND length(launch_json)>0`, a.TaskID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var plan struct {
			Members []struct {
				Fields struct {
					AgentID string `json:"agentId"`
					Role    string `json:"role"`
				} `json:"fields"`
				RunID string `json:"runId"`
			} `json:"members"`
		}
		if json.Unmarshal(raw, &plan) != nil {
			continue
		}
		for _, m := range plan.Members {
			if m.Fields.AgentID == a.ID && m.RunID == a.RunID && m.Fields.Role != "" {
				p.Role, p.RoleSource = normalizeUsageRole(m.Fields.Role), "frozen team slot role: "+m.Fields.Role
				return nil
			}
		}
	}
	return rows.Err()
}
func resolveUsage(ctx context.Context, tx *sql.Tx, task, agent, run string, t api.UsageTurn, p usageRunProvenance) (api.UsageProjection, error) {
	out := api.UsageProjection{Turn: t, AgentID: agent, RunID: run, Role: p.Role, RoleSource: p.RoleSource, Phase: "hand-offs", PhaseReason: usageNoOpenOrder}
	items := map[string]api.UsageAttribution{}
	seen := map[string]bool{}
	invalid := false
	var visit func(api.MessageReference, int) error
	visit = func(ref api.MessageReference, depth int) error {
		key := ref.TaskID + "/" + stringID(ref.Seq)
		if seen[key] {
			return nil
		}
		seen[key] = true
		if depth > 8 {
			return nil // Bound ancestry without discarding the head's valid links.
		}
		m, err := loadMessage(tx, ctx, ref.TaskID, ref.Seq)
		if errors.Is(err, api.ErrNotFound) {
			invalid = true
			return nil
		}
		if err != nil {
			return err
		}
		out.Evidence = append(out.Evidence, ref)
		add := func(project, item string) {
			if project == "" {
				project = m.TaskID
			}
			var n int
			if !api.ValidID(project, "tsk") || !api.ValidID(item, "wi") {
				invalid = true
				return
			}
			if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_items WHERE task_id=? AND id=?`, project, item).Scan(&n); e != nil || n != 1 {
				invalid = true
				return
			}
			items[project+"/"+item] = api.UsageAttribution{TaskID: project, ItemID: item, Reason: "handled Board evidence"}
		}
		for _, link := range m.WorkItems {
			add(link.ItemTaskID, link.ItemID)
		}
		if m.Envelope != nil {
			refs := m.Envelope.Refs
			project := refs["itemTaskId"]
			if project == "" {
				project = refs["project"]
			}
			for _, k := range []string{"item", "workItem", "work-item"} {
				if refs[k] != "" {
					add(project, refs[k])
				}
			}
		}
		if m.ReplyTo > 0 {
			if err = visit(api.MessageReference{TaskID: m.TaskID, Seq: m.ReplyTo}, depth+1); err != nil {
				return err
			}
		}
		if m.WorkOrderMessage != nil {
			if err = visit(*m.WorkOrderMessage, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	evidence := append([]api.UsageEvidence(nil), t.Handled...)
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].At.Equal(evidence[j].At) {
			return evidence[i].Seq < evidence[j].Seq
		}
		return evidence[i].At.Before(evidence[j].At)
	})
	for _, e := range evidence {
		m, err := loadMessage(tx, ctx, e.TaskID, e.Seq)
		if errors.Is(err, api.ErrNotFound) {
			invalid = true
			continue
		}
		if err != nil {
			return out, err
		}
		// Retrieved/broadcast messages cannot acquire credit. Ack/progress must be
		// addressed to this exact agent; post/reply must have this author.
		if e.Operation == "post" || e.Operation == "reply" {
			if m.From.AgentID != agent {
				invalid = true
				continue
			}
		} else if m.To != agent {
			invalid = true
			continue
		}
		if err = visit(api.MessageReference{TaskID: e.TaskID, Seq: e.Seq}, 0); err != nil {
			return out, err
		}
	}
	persistent := p.Role == "database_handler" || p.Role == "project_lead" || p.Role == "deployment_agent" || p.Role == "backlog_steward"
	if len(items) == 0 && !persistent && p.Binding != nil {
		b := p.Binding
		items[b.ItemTaskID+"/"+b.ItemID] = api.UsageAttribution{TaskID: b.ItemTaskID, ItemID: b.ItemID, Reason: "exact admitted binding " + b.ContextDigest}
		if err := visit(b.WorkOrderMessage, 0); err != nil {
			return out, err
		}
	}
	if invalid {
		out.PhaseReason = "some handled evidence was unresolved; valid links retained"
	}
	if len(items) == 0 {
		reason := "unattributable persistent or unbound request"
		if invalid {
			reason = "conflicting or unresolved handled references"
		}
		out.Shares = []api.UsageAttribution{{TaskID: task, Denominator: 1, Reason: reason}}
	} else {
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			x := items[k]
			x.Denominator = int64(len(items))
			out.Shares = append(out.Shares, x)
		}
	}
	phase, reason, round, err := classifyUsagePhase(ctx, tx, out, newUsagePhaseCache())
	if err != nil {
		return out, err
	}
	out.Phase, out.ReviewRound = phase, round
	if !invalid || reason != usageNoOpenOrder {
		out.PhaseReason = reason
	}
	return out, nil
}

const usageNoOpenOrder = "no open order at request time"

// usageOrder is an assign, review or request an agent owed an outcome for, and
// the window it stayed open. A zero closed time means it is still open.
type usageOrder struct {
	ref            api.MessageReference
	kind           string
	opened, closed time.Time
}

// usagePhaseCache holds the lookups of one report or one ingest, so that
// classifying a request costs no query once its agent's orders are loaded.
type usagePhaseCache struct {
	orders       map[string][]usageOrder
	messages     map[string]*api.Message
	reviews      map[string]api.ReviewConvergence
	orderQueries int
}

func newUsagePhaseCache() *usagePhaseCache {
	return &usagePhaseCache{orders: map[string][]usageOrder{}, messages: map[string]*api.Message{}, reviews: map[string]api.ReviewConvergence{}}
}
func (c *usagePhaseCache) agentOrders(ctx context.Context, q queryRower, agent string) ([]usageOrder, error) {
	if orders, ok := c.orders[agent]; ok {
		return orders, nil
	}
	c.orderQueries++
	rows, err := q.QueryContext(ctx, `SELECT task_id,message_seq,source_kind,created_at,closed_at FROM obligations WHERE agent_id=? AND needs=? AND source_kind IN ('assign','review','request')`, agent, api.ObligationNeedsOutcome)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	orders := []usageOrder{}
	for rows.Next() {
		var o usageOrder
		var opened, closed string
		if err = rows.Scan(&o.ref.TaskID, &o.ref.Seq, &o.kind, &opened, &closed); err != nil {
			return nil, err
		}
		o.opened, o.closed = parseTS(opened), parseTS(closed)
		orders = append(orders, o)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(orders, func(i, j int) bool {
		if orders[i].opened.Equal(orders[j].opened) {
			return orders[i].ref.Seq < orders[j].ref.Seq
		}
		return orders[i].opened.Before(orders[j].opened)
	})
	c.orders[agent] = orders
	return orders, nil
}

// message returns nil for a reference that no longer resolves.
func (c *usagePhaseCache) message(ctx context.Context, q queryRower, ref api.MessageReference) (*api.Message, error) {
	key := ref.TaskID + "/" + stringID(ref.Seq)
	if m, ok := c.messages[key]; ok {
		return m, nil
	}
	m, err := loadMessage(q, ctx, ref.TaskID, ref.Seq)
	if errors.Is(err, api.ErrNotFound) {
		c.messages[key] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.messages[key] = &m
	return &m, nil
}
func (c *usagePhaseCache) review(ctx context.Context, q queryRower, task, item string) (api.ReviewConvergence, error) {
	key := task + "/" + item
	if state, ok := c.reviews[key]; ok {
		return state, nil
	}
	state, err := reviewState(ctx, q, task, item)
	if err == nil {
		c.reviews[key] = state
	}
	return state, err
}
func usageOrderKind(m *api.Message) bool {
	if m == nil || m.Envelope == nil {
		return false
	}
	k := m.Envelope.Kind
	return k == "assign" || k == "request" || k == "review"
}

// classifyUsagePhase names what the agent was doing when it made the request:
// the order addressed to it that was open at the request time governs, from
// the moment it was posted until the agent's outcome closed it. Messages the
// agent wrote never govern. It reads only fields a stored projection keeps, so
// ingest and every report apply the same rule.
func classifyUsagePhase(ctx context.Context, q queryRower, p api.UsageProjection, c *usagePhaseCache) (string, string, int, error) {
	if p.Role == "database_handler" {
		return "handler bookkeeping", "authoritative handler role", 0, nil
	}
	orders, err := c.agentOrders(ctx, q, p.AgentID)
	if err != nil {
		return "", "", 0, err
	}
	at := p.Turn.At
	strong := func(o *usageOrder) bool { return o.kind == "assign" || o.kind == "review" }
	var open *usageOrder
	// An order posted by the request time is settled by its own window; handled
	// evidence can no longer vouch for it.
	windowed := map[string]bool{}
	for i := range orders {
		o := &orders[i]
		if o.opened.After(at) {
			continue
		}
		windowed[o.ref.TaskID+"/"+stringID(o.ref.Seq)] = true
		if !o.closed.IsZero() && !o.closed.After(at) {
			continue
		}
		// Orders arrive oldest first: a later one wins unless it is a plain
		// request and an assign or review is already open.
		if open == nil || strong(o) || !strong(open) {
			open = o
		}
	}
	var selected *api.Message
	reason := usageNoOpenOrder
	if open != nil {
		if selected, err = c.message(ctx, q, open.ref); err != nil {
			return "", "", 0, err
		}
		if !usageOrderKind(selected) {
			selected = nil
		} else {
			reason = "open order " + stringID(selected.Seq) + " at request time"
		}
	}
	if selected == nil {
		// No obligation window covers this request: the order predates
		// obligations, or a skewed host clock put the request before the order
		// it acknowledged. Use the handled order addressed to this agent; the
		// opening request inherits the first one, later requests the latest.
		handled := []*api.Message{}
		for _, ref := range p.Evidence {
			m, err := c.message(ctx, q, ref)
			if err != nil {
				return "", "", 0, err
			}
			if !usageOrderKind(m) || m.To != p.AgentID || windowed[ref.TaskID+"/"+stringID(ref.Seq)] {
				continue
			}
			handled = append(handled, m)
		}
		sort.Slice(handled, func(i, j int) bool {
			if handled[i].CreatedAt.Equal(handled[j].CreatedAt) {
				return handled[i].Seq < handled[j].Seq
			}
			return handled[i].CreatedAt.Before(handled[j].CreatedAt)
		})
		for _, m := range handled {
			if selected == nil || !m.CreatedAt.After(at) {
				selected = m
			}
		}
		if selected == nil {
			return "hand-offs", reason, 0, nil
		}
		reason = "handled request " + stringID(selected.Seq)
	}
	e := selected.Envelope
	focused := e.Review != nil && e.Review.Mode == "focused"
	general := e.Review != nil && e.Review.Mode == "general"
	switch {
	case focused || p.Role == "verifier":
		return "verification", reason, 0, nil
	case e.Kind == "review" || general || p.Role == "reviewer":
		round := 0
		for _, share := range p.Shares {
			if share.ItemID == "" {
				continue
			}
			state, err := c.review(ctx, q, share.TaskID, share.ItemID)
			if err != nil {
				return "", "", 0, err
			}
			for _, r := range state.Rounds {
				if r.RequestSeq == selected.Seq || r.ActiveRequestSeq == selected.Seq {
					round = r.Number
				}
			}
		}
		if round == 0 {
			reason += "; review round unavailable"
		}
		return "review", reason, round, nil
	case p.Role == "planner" || e.Refs["phase"] == "planning" || e.Refs["phase"] == "intake":
		return "intake and planning", reason, 0, nil
	case e.Kind == "assign":
		for _, share := range p.Shares {
			if share.ItemID == "" {
				continue
			}
			state, err := c.review(ctx, q, share.TaskID, share.ItemID)
			if err != nil {
				return "", "", 0, err
			}
			for _, r := range state.Rounds {
				if r.ResultSeq > 0 && r.ResultSeq < selected.Seq && len(r.Blockers) > 0 {
					return "corrections", reason, 0, nil
				}
			}
		}
		return "build", reason, 0, nil
	}
	return "hand-offs", reason, 0, nil
}
