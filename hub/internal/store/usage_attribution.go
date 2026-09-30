package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

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
	out := api.UsageProjection{Turn: t, AgentID: agent, RunID: run, Role: p.Role, RoleSource: p.RoleSource, Phase: "hand-offs", PhaseReason: "no applicable handled order"}
	items := map[string]api.UsageAttribution{}
	seen := map[string]bool{}
	var messages []api.Message
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
		messages = append(messages, m)
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
	if p.Role == "database_handler" {
		out.Phase = "handler bookkeeping"
		out.PhaseReason = "authoritative handler role"
		return out, nil
	}
	// Results do not replace the phase of the applicable active request. Opening
	// request inherits the first handled request; later requests use latest <= At.
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].CreatedAt.Equal(messages[j].CreatedAt) {
			return messages[i].Seq < messages[j].Seq
		}
		return messages[i].CreatedAt.Before(messages[j].CreatedAt)
	})
	var selected *api.Message
	for i := range messages {
		m := &messages[i]
		if m.Envelope == nil {
			continue
		}
		k := m.Envelope.Kind
		if k != "assign" && k != "request" && k != "review" {
			continue
		}
		if selected == nil || !m.CreatedAt.After(t.At) {
			selected = m
		}
	}
	if selected == nil {
		return out, nil
	}
	m := *selected
	e := m.Envelope
	out.PhaseReason = "handled request " + stringID(m.Seq)
	if e.Review != nil && e.Review.Mode == "focused" {
		out.Phase = "verification"
		return out, nil
	}
	if e.Kind == "review" || (e.Review != nil && e.Review.Mode == "general") {
		out.Phase = "review"
		for _, share := range out.Shares {
			if share.ItemID == "" {
				continue
			}
			state, err := reviewState(ctx, tx, share.TaskID, share.ItemID)
			if err != nil {
				return out, err
			}
			for _, round := range state.Rounds {
				if round.RequestSeq == m.Seq || round.ActiveRequestSeq == m.Seq {
					out.ReviewRound = round.Number
				}
			}
		}
		if out.ReviewRound == 0 {
			out.PhaseReason += "; review round unavailable"
		}
		return out, nil
	}
	if (e.Review != nil && e.Review.Mode == "focused") || p.Role == "verifier" {
		out.Phase = "verification"
		return out, nil
	}
	if e.Kind == "assign" {
		for _, share := range out.Shares {
			if share.ItemID == "" {
				continue
			}
			state, err := reviewState(ctx, tx, share.TaskID, share.ItemID)
			if err != nil {
				return out, err
			}
			for _, r := range state.Rounds {
				if r.ResultSeq > 0 && r.ResultSeq < m.Seq && len(r.Blockers) > 0 {
					out.Phase = "corrections"
					return out, nil
				}
			}
		}
		if p.Role == "planner" || e.Refs["phase"] == "planning" {
			out.Phase = "intake and planning"
		} else {
			out.Phase = "build"
		}
		return out, nil
	}
	if p.Role == "planner" || e.Refs["phase"] == "planning" || e.Refs["phase"] == "intake" {
		out.Phase = "intake and planning"
	}
	return out, nil
}
