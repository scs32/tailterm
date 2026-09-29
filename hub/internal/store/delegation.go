package store

// Owner delegation windows (docs/owner-delegation-windows.md). A window routes
// the owner's covered decision requests in one project to a delegate agent
// until it expires or the owner closes it; unanswered routed requests then
// return to the owner. Matrix approvals are never routed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

func migrateDelegationWindows(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS owner_delegation_windows (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, delegate_agent_id TEXT NOT NULL, delegate_name TEXT NOT NULL,
 scope TEXT NOT NULL, ends_at TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
 ended_at TEXT NOT NULL DEFAULT '', end_reason TEXT NOT NULL DEFAULT '',
 opened_by_node TEXT NOT NULL, opened_by_user TEXT NOT NULL,
 opened_source TEXT NOT NULL DEFAULT '', closed_source TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS owner_delegation_windows_one_open ON owner_delegation_windows(task_id) WHERE state='open';
CREATE TABLE IF NOT EXISTS owner_delegation_routes (
 window_id TEXT NOT NULL REFERENCES owner_delegation_windows(id), task_id TEXT NOT NULL,
 request_kind TEXT NOT NULL, request_seq INTEGER NOT NULL, obligation_id TEXT NOT NULL DEFAULT '',
 category TEXT NOT NULL, notice_seq INTEGER NOT NULL DEFAULT 0, routed_at TEXT NOT NULL,
 returned_at TEXT NOT NULL DEFAULT '', answer_seq INTEGER NOT NULL DEFAULT 0,
 answered_by_agent_id TEXT NOT NULL DEFAULT '', answered_by_run_id TEXT NOT NULL DEFAULT '',
 rationale TEXT NOT NULL DEFAULT '', followed_recommendation INTEGER, answered_at TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(window_id,request_kind,request_seq));
CREATE INDEX IF NOT EXISTS owner_delegation_routes_request ON owner_delegation_routes(task_id,request_kind,request_seq);
CREATE TABLE IF NOT EXISTS decision_request_categories (
 task_id TEXT NOT NULL, message_seq INTEGER NOT NULL, category TEXT NOT NULL,
 PRIMARY KEY(task_id,message_seq));`)
	return err
}

var windowIDRE = regexp.MustCompile(`^dlw_[0-9a-f]{16}$`)

const windowCols = `id,task_id,delegate_agent_id,delegate_name,scope,ends_at,reason,state,ended_at,end_reason,opened_by_node,opened_by_user,opened_source,closed_source,created_at`

func scanWindow(row interface{ Scan(...any) error }) (api.DelegationWindow, error) {
	var w api.DelegationWindow
	var ends, ended, opened, closed, created string
	err := row.Scan(&w.ID, &w.TaskID, &w.DelegateAgentID, &w.DelegateName, &w.Scope, &ends, &w.Reason, &w.State, &ended, &w.EndReason, &w.OpenedBy.Node, &w.OpenedBy.User, &opened, &closed, &created)
	if err != nil {
		return w, err
	}
	w.EndsAt, w.CreatedAt = parseTS(ends), parseTS(created)
	if ended != "" {
		t := parseTS(ended)
		w.EndedAt = &t
	}
	w.OpenedSource, w.ClosedSource = decodeSource(opened), decodeSource(closed)
	w.Routes = []api.DelegationRoute{}
	return w, nil
}

func encodeSource(src *api.DelegationSource) string {
	if src == nil {
		return ""
	}
	b, _ := json.Marshal(src)
	return string(b)
}

func decodeSource(raw string) *api.DelegationSource {
	if raw == "" {
		return nil
	}
	var src api.DelegationSource
	if json.Unmarshal([]byte(raw), &src) != nil {
		return nil
	}
	return &src
}

type windowRoute struct {
	api.DelegationRoute
	returned, answered bool
}

func scanRoutes(rows *sql.Rows) ([]windowRoute, error) {
	defer rows.Close()
	var out []windowRoute
	for rows.Next() {
		var r windowRoute
		var routed, returned, answered string
		var followed sql.NullInt64
		if err := rows.Scan(&r.WindowID, &r.RequestKind, &r.RequestSeq, &r.ObligationID, &r.Category, &r.NoticeSeq, &routed, &returned, &r.AnswerSeq, &r.AnsweredByAgentID, &r.AnsweredByRunID, &r.Rationale, &followed, &answered); err != nil {
			return nil, err
		}
		r.RoutedAt = parseTS(routed)
		if returned != "" {
			t := parseTS(returned)
			r.ReturnedAt, r.returned = &t, true
		}
		if answered != "" {
			t := parseTS(answered)
			r.AnsweredAt, r.answered = &t, true
		}
		if followed.Valid {
			v := followed.Int64 == 1
			r.FollowedRecommendation = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const routeCols = `window_id,request_kind,request_seq,obligation_id,category,notice_seq,routed_at,returned_at,answer_seq,answered_by_agent_id,answered_by_run_id,rationale,followed_recommendation,answered_at`

// openWindow is the project's open window, if any (it may be past its end
// until the broker or the next open expires it).
func openWindow(ctx context.Context, q queryRower, taskID string) (api.DelegationWindow, bool, error) {
	w, err := scanWindow(q.QueryRowContext(ctx, `SELECT `+windowCols+` FROM owner_delegation_windows WHERE task_id=? AND state=?`, taskID, api.DelegationOpen))
	if errors.Is(err, sql.ErrNoRows) {
		return w, false, nil
	}
	return w, err == nil, err
}

// resolveDelegate finds a same-project, live agent by ID or name.
func resolveDelegate(ctx context.Context, tx *sql.Tx, taskID, delegate string) (api.Agent, error) {
	delegate = strings.TrimSpace(delegate)
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND (id=? OR name=?) AND status NOT IN (?,?) ORDER BY created_at DESC LIMIT 1`, taskID, delegate, delegate, api.AgentClosed, api.AgentExited))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: the delegate must be a live agent in this project", api.ErrInvalid)
	}
	return a, err
}

// delegateLinks are the item links that make a notice visible in the
// delegate's inbox: its own bound item when it has one, else the request's.
func delegateLinks(ctx context.Context, tx *sql.Tx, delegate api.Agent, source api.Message) ([]api.MessageWorkItem, *api.MessageReference, error) {
	binding, err := loadAgentWorkItemBinding(tx, ctx, delegate.ID, delegate.RunID)
	if err != nil && !errors.Is(err, api.ErrNotFound) {
		return nil, nil, err
	}
	if binding != nil && binding.ItemTaskID == delegate.TaskID {
		order := binding.WorkOrderMessage
		return []api.MessageWorkItem{{ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision, Relationship: "primary"}}, &order, nil
	}
	return source.WorkItems, source.WorkOrderMessage, nil
}

// postDelegationNotice inserts a hub-authored notice. A directed notice gets a
// delivery obligation, which wakes the delegate like any directed message.
func (s *Store) postDelegationNotice(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, text string, refs map[string]string, links []api.MessageWorkItem, order *api.MessageReference, requestID string) (api.Message, error) {
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	if to.ID != "" {
		env.To = to.Name
	}
	req := api.PostMessageRequest{Envelope: env, To: to.ID, WorkItems: links, WorkOrderMessage: order}
	if len(links) > 0 {
		req.RequestID = requestID
	}
	m, err := s.insertBrokerMessage(ctx, tx, task, req, to, to.Name)
	if err != nil {
		return m, err
	}
	return m, s.createObligations(ctx, tx, m, req, false)
}

func clip(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func windowEnd(w api.DelegationWindow) string {
	return w.EndsAt.UTC().Format("Jan 2 15:04 UTC")
}

func scopeLabel(scope string) string {
	if scope == api.DelegationScopeDecisionsMergesDeploys {
		return "decisions, merges and deploys"
	}
	return "decisions"
}

// routable is one open owner decision request a window may take.
type routable struct {
	kind         string
	seq          int64
	obligationID string
	subject      string
	category     string
	author       string
	source       api.Message
}

// covers reports whether w routes r now: never matrix, never the delegate's
// own request, never after the window's end or to a gone delegate.
func covers(w api.DelegationWindow, delegate api.Agent, r routable, now time.Time) bool {
	return w.Active(now) && api.DelegationScopeCovers(w.Scope, r.category) && r.author != w.DelegateAgentID &&
		delegate.ID == w.DelegateAgentID && delegate.Status != api.AgentClosed && delegate.Status != api.AgentExited
}

// routeRequest records r under w and tells the delegate how to answer.
func (s *Store) routeRequest(ctx context.Context, tx *sql.Tx, task api.Task, w api.DelegationWindow, delegate api.Agent, r routable, now time.Time) error {
	command := fmt.Sprintf("tt owner answer %s --text \"...\" --rationale \"...\"", r.obligationID)
	if r.source.Envelope != nil && r.source.Envelope.ExpectedAnswer != "" {
		command += " (or --approve --rationale \"...\" for the exact expected answer)"
	}
	if r.kind == api.DelegationRouteDecision {
		command = fmt.Sprintf("tt ask answer %d (--option ID | --text \"...\") --rationale \"...\"", r.seq)
	}
	text := fmt.Sprintf("The owner delegated %s to you until %s. Answer message #%d (%s) with:\n%s\nA rationale is required; your answer is marked as delegated and listed for the owner. The owner may still answer first. When the window ends, unanswered requests return to the owner.\n\nRequest:\n%s",
		scopeLabel(w.Scope), windowEnd(w), r.seq, r.subject, command, clip(r.source.Text, 2400))
	refs := map[string]string{"delegation": w.ID, "message": fmt.Sprint(r.seq), "category": r.category, "onBehalfOf": "owner"}
	if r.obligationID != "" {
		refs["obligation"] = r.obligationID
	}
	links, order, err := delegateLinks(ctx, tx, delegate, r.source)
	if err != nil {
		return err
	}
	m, err := s.postDelegationNotice(ctx, tx, task, delegate, "An owner request is delegated to you to answer", text, refs, links, order, fmt.Sprintf("delegation-route-%s-%s-%d", w.ID, r.kind, r.seq))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO owner_delegation_routes (window_id,task_id,request_kind,request_seq,obligation_id,category,notice_seq,routed_at) VALUES (?,?,?,?,?,?,?,?)`,
		w.ID, task.ID, r.kind, r.seq, r.obligationID, r.category, m.Seq, ts(now))
	return err
}

func (s *Store) delegateAgent(ctx context.Context, tx *sql.Tx, w api.DelegationWindow) (api.Agent, error) {
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, w.DelegateAgentID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Agent{}, nil
	}
	return a, err
}

// routeIfDelegated routes a request just created in tx when an open window
// covers it. Uncovered requests stay with the owner.
func (s *Store) routeIfDelegated(ctx context.Context, tx *sql.Tx, taskID string, r routable) error {
	w, ok, err := openWindow(ctx, tx, taskID)
	if err != nil || !ok {
		return err
	}
	delegate, err := s.delegateAgent(ctx, tx, w)
	if err != nil {
		return err
	}
	now := s.now()
	if !covers(w, delegate, r, now) {
		return nil
	}
	task, err := decisionTask(tx, ctx, taskID)
	if err != nil {
		return err
	}
	return s.routeRequest(ctx, tx, task, w, delegate, r, now)
}

// openRequests lists the project's unanswered owner requests and decisions.
func openRequests(ctx context.Context, tx *sql.Tx, taskID string) ([]routable, error) {
	var out []routable
	rows, err := tx.QueryContext(ctx, `SELECT id,message_seq,subject FROM obligations WHERE task_id=? AND recipient_kind=? AND state<>? ORDER BY message_seq`, taskID, api.ObligationRecipientOwner, api.ObligationClosed)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r routable
		if err := rows.Scan(&r.obligationID, &r.seq, &r.subject); err != nil {
			rows.Close()
			return nil, err
		}
		r.kind = api.DelegationRouteObligation
		out = append(out, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.message_seq FROM decision_requests r LEFT JOIN decision_answers a ON a.task_id=r.task_id AND a.request_seq=r.message_seq WHERE r.task_id=? AND a.message_seq IS NULL ORDER BY r.message_seq`, taskID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r := routable{kind: api.DelegationRouteDecision}
		if err := rows.Scan(&r.seq); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := fillRoutable(ctx, tx, taskID, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillRoutable loads the request message, author and category.
func fillRoutable(ctx context.Context, tx *sql.Tx, taskID string, r *routable) error {
	var err error
	if r.kind == api.DelegationRouteObligation {
		if r.source, err = originalMessage(ctx, tx, taskID, r.seq); err != nil {
			return err
		}
		r.category = api.OwnerRequestCategory(r.source.Envelope)
	} else {
		if r.source, err = loadMessage(tx, ctx, taskID, r.seq); err != nil {
			return err
		}
		request, err := loadDecisionRequest(tx, ctx, taskID, r.seq)
		if err != nil {
			return err
		}
		r.category = api.DecisionCategory(request)
		r.subject = clip(request.Question, 100)
	}
	r.author = r.source.From.AgentID
	return nil
}

// OpenDelegationWindow opens the project's one window and routes the covered
// requests already waiting on the owner.
func (s *Store) OpenDelegationWindow(ctx context.Context, taskID string, req api.OpenDelegationWindowRequest, by api.Caller) (api.OwnerActionResult, error) {
	return s.ownerAction(ctx, taskID, "delegation-open", taskID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		if err := api.ValidateOpenDelegationWindow(req, now); err != nil {
			return api.OwnerActionResult{}, err
		}
		delegate, err := resolveDelegate(ctx, tx, taskID, req.Delegate)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if prior, ok, err := openWindow(ctx, tx, taskID); err != nil {
			return api.OwnerActionResult{}, err
		} else if ok && !prior.Active(now) {
			if err := s.endWindow(ctx, tx, task, prior, api.DelegationExpired, "the window reached its end time", nil, now); err != nil {
				return api.OwnerActionResult{}, err
			}
		} else if ok {
			return api.OwnerActionResult{}, fmt.Errorf("%w: a delegation window to %s is already open until %s; close it first", api.ErrConflict, prior.DelegateName, windowEnd(prior))
		}
		w := api.DelegationWindow{ID: newObligationID("dlw"), TaskID: taskID, DelegateAgentID: delegate.ID, DelegateName: delegate.Name, Scope: req.Scope,
			EndsAt: req.EndsAt.UTC(), Reason: req.Reason, State: api.DelegationOpen, OpenedBy: api.Sender{Node: by.Node, User: by.User}, OpenedSource: req.Source, CreatedAt: now, Routes: []api.DelegationRoute{}}
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner_delegation_windows (`+windowCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, taskID, w.DelegateAgentID, w.DelegateName, w.Scope, ts(w.EndsAt), w.Reason, w.State, "", "", by.Node, by.User, encodeSource(req.Source), "", ts(now)); err != nil {
			return api.OwnerActionResult{}, err
		}
		links, order, err := delegateLinks(ctx, tx, delegate, api.Message{})
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		text := fmt.Sprintf("The owner delegated %s in this project to you until %s. Each request routed to you arrives as its own notice with the exact answer command. Every answer needs a rationale and is listed for the owner. Matrix approvals stay with the owner.", scopeLabel(w.Scope), windowEnd(w))
		if req.Reason != "" {
			text += " Reason: " + req.Reason
		}
		if _, err := s.postDelegationNotice(ctx, tx, task, delegate, "The owner delegated decisions to you for a set time", text, map[string]string{"delegation": w.ID, "onBehalfOf": "owner"}, links, order, "delegation-open-"+w.ID); err != nil {
			return api.OwnerActionResult{}, err
		}
		waiting, err := openRequests(ctx, tx, taskID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		for _, r := range waiting {
			if covers(w, delegate, r, now) {
				if err := s.routeRequest(ctx, tx, task, w, delegate, r, now); err != nil {
					return api.OwnerActionResult{}, err
				}
			}
		}
		loaded, err := loadWindow(ctx, tx, taskID, w.ID)
		return api.OwnerActionResult{Window: &loaded}, err
	})
}

// CloseDelegationWindow ends the open window now and returns its unanswered
// requests to the owner.
func (s *Store) CloseDelegationWindow(ctx context.Context, taskID, windowID string, req api.CloseDelegationWindowRequest, by api.Caller) (api.OwnerActionResult, error) {
	if !windowIDRE.MatchString(windowID) || !api.ValidText(req.Reason, 500) {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	if err := api.ValidateDelegationSource(req.Source); err != nil {
		return api.OwnerActionResult{}, err
	}
	return s.ownerAction(ctx, taskID, "delegation-close", windowID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		w, err := loadWindow(ctx, tx, taskID, windowID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if w.State != api.DelegationOpen {
			return api.OwnerActionResult{}, fmt.Errorf("%w: the delegation window already ended (%s)", api.ErrConflict, w.State)
		}
		reason := "the owner ended the window"
		if strings.TrimSpace(req.Reason) != "" {
			reason += ": " + strings.TrimSpace(req.Reason)
		}
		if err := s.endWindow(ctx, tx, task, w, api.DelegationClosed, reason, req.Source, now); err != nil {
			return api.OwnerActionResult{}, err
		}
		loaded, err := loadWindow(ctx, tx, taskID, windowID)
		return api.OwnerActionResult{Window: &loaded}, err
	})
}

// requestStillOpen reports whether a routed request still waits for an answer.
func requestStillOpen(ctx context.Context, tx *sql.Tx, taskID string, r api.DelegationRoute) (bool, error) {
	var n int
	var err error
	if r.RequestKind == api.DelegationRouteObligation {
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM obligations WHERE id=? AND task_id=? AND state<>?`, r.ObligationID, taskID, api.ObligationClosed).Scan(&n)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM decision_answers WHERE task_id=? AND request_seq=?`, taskID, r.RequestSeq).Scan(&n)
		n = 1 - n
	}
	return n > 0, err
}

// endWindow marks w ended and hands back each unanswered routed request: the
// delegate is told to stop and one board notice lists them for the owner.
// Only an open window ends, so a retried expiry posts nothing more.
func (s *Store) endWindow(ctx context.Context, tx *sql.Tx, task api.Task, w api.DelegationWindow, state, reason string, source *api.DelegationSource, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE owner_delegation_windows SET state=?,ended_at=?,end_reason=?,closed_source=? WHERE id=? AND state=?`,
		state, ts(now), reason, encodeSource(source), w.ID, api.DelegationOpen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrBrokerStale
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+routeCols+` FROM owner_delegation_routes WHERE window_id=? AND answer_seq=0 AND returned_at='' ORDER BY request_seq`, w.ID)
	if err != nil {
		return err
	}
	routes, err := scanRoutes(rows)
	if err != nil {
		return err
	}
	var returned []string
	for _, r := range routes {
		open, err := requestStillOpen(ctx, tx, task.ID, r.DelegationRoute)
		if err != nil {
			return err
		}
		if !open {
			continue // the owner answered it during the window
		}
		if _, err := tx.ExecContext(ctx, `UPDATE owner_delegation_routes SET returned_at=? WHERE window_id=? AND request_kind=? AND request_seq=?`, ts(now), w.ID, r.RequestKind, r.RequestSeq); err != nil {
			return err
		}
		line := fmt.Sprintf("#%d", r.RequestSeq)
		if r.ObligationID != "" {
			line += " (" + r.ObligationID + ")"
		}
		returned = append(returned, line)
	}
	ended := "expired"
	if state == api.DelegationClosed {
		ended = "was ended by the owner"
	}
	list := "No unanswered requests were routed to the delegate."
	if len(returned) > 0 {
		list = "Returned to the owner: " + clip(strings.Join(returned, ", "), 2400) + "."
	}
	delegate, err := s.delegateAgent(ctx, tx, w)
	if err != nil {
		return err
	}
	if delegate.ID != "" && delegate.Status != api.AgentClosed && delegate.Status != api.AgentExited {
		links, order, err := delegateLinks(ctx, tx, delegate, api.Message{})
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Your owner delegation window %s. Do not answer any request routed to you under it; the owner answers them now. %s", ended, list)
		if _, err := s.postDelegationNotice(ctx, tx, task, delegate, "Your owner delegation window ended; requests returned", text, map[string]string{"delegation": w.ID, "state": state}, links, order, "delegation-end-"+w.ID); err != nil {
			return err
		}
	}
	text := fmt.Sprintf("The owner delegation window to %s (%s) %s. %s Delegated answers stay listed under the window.", w.DelegateName, scopeLabel(w.Scope), ended, list)
	_, err = s.postDelegationNotice(ctx, tx, task, api.Agent{}, "Owner delegation window ended; requests are with the owner again", text, map[string]string{"delegation": w.ID, "state": state, "recipientKind": "owner"}, nil, nil, "")
	return err
}

// BrokerExpireDelegationWindows ends each open window at or past its end
// time, including in paused projects. It returns the expired window IDs.
func (s *Store) BrokerExpireDelegationWindows(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.task_id,w.ends_at FROM owner_delegation_windows w JOIN tasks t ON t.id=w.task_id WHERE w.state=? AND t.status=?`, api.DelegationOpen, api.TaskOpen)
	if err != nil {
		return nil, err
	}
	type due struct{ id, task string }
	var candidates []due
	for rows.Next() {
		var d due
		var ends string
		if err := rows.Scan(&d.id, &d.task, &ends); err != nil {
			rows.Close()
			return nil, err
		}
		if !now.Before(parseTS(ends)) {
			candidates = append(candidates, d)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var expired []string
	var firstErr error
	for _, d := range candidates {
		err := s.expireWindow(ctx, d.task, d.id, now)
		switch {
		case errors.Is(err, ErrBrokerStale):
		case err != nil:
			if firstErr == nil {
				firstErr = fmt.Errorf("expire delegation window %s: %w", d.id, err)
			}
		default:
			expired = append(expired, d.id)
		}
	}
	return expired, firstErr
}

func (s *Store) expireWindow(ctx context.Context, taskID, windowID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	w, err := loadWindow(ctx, tx, taskID, windowID)
	if err != nil {
		return err
	}
	if w.State != api.DelegationOpen || now.Before(w.EndsAt) {
		return ErrBrokerStale
	}
	task, err := decisionTask(tx, ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status != api.TaskOpen {
		return ErrBrokerStale
	}
	if err := s.endWindow(ctx, tx, task, w, api.DelegationExpired, "the window reached its end time", nil, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(taskID)
	return nil
}

func loadWindow(ctx context.Context, q queryRower, taskID, windowID string) (api.DelegationWindow, error) {
	w, err := scanWindow(q.QueryRowContext(ctx, `SELECT `+windowCols+` FROM owner_delegation_windows WHERE id=? AND task_id=?`, windowID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return w, api.ErrNotFound
	}
	if err != nil {
		return w, err
	}
	return w, fillWindowRoutes(ctx, q, &w)
}

func fillWindowRoutes(ctx context.Context, q queryRower, w *api.DelegationWindow) error {
	rows, err := q.QueryContext(ctx, `SELECT `+routeCols+` FROM owner_delegation_routes WHERE window_id=? ORDER BY request_seq,request_kind`, w.ID)
	if err != nil {
		return err
	}
	routes, err := scanRoutes(rows)
	if err != nil {
		return err
	}
	for _, r := range routes {
		route := r.DelegationRoute
		if m, err := loadMessage(q, ctx, w.TaskID, route.RequestSeq); err == nil {
			route.Subject = routeSubject(ctx, q, w.TaskID, route, m)
		}
		if route.AnswerSeq > 0 {
			if m, err := loadMessage(q, ctx, w.TaskID, route.AnswerSeq); err == nil {
				route.Answer = answerText(m)
			}
			if route.AnsweredByAgentID == w.DelegateAgentID {
				route.AnsweredByName = w.DelegateName
			}
		}
		w.Routes = append(w.Routes, route)
	}
	return nil
}

func routeSubject(ctx context.Context, q queryRower, taskID string, r api.DelegationRoute, m api.Message) string {
	if r.RequestKind == api.DelegationRouteDecision {
		if request, err := loadDecisionRequest(q, ctx, taskID, r.RequestSeq); err == nil {
			return clip(request.Question, 200)
		}
	}
	if m.Envelope != nil {
		return m.Envelope.Subject
	}
	return clip(m.Text, 200)
}

func answerText(m api.Message) string {
	if m.Envelope != nil && m.Envelope.Body.Answer != "" {
		return m.Envelope.Body.Answer
	}
	return m.Text
}

// ListDelegationWindows returns the project's windows, newest first, with
// every routed request and delegated answer, including after they end.
func (s *Store) ListDelegationWindows(ctx context.Context, taskID string) (api.DelegationWindowList, error) {
	if !api.ValidID(taskID, "tsk") {
		return api.DelegationWindowList{}, api.ErrInvalid
	}
	if _, err := decisionTask(s.db, ctx, taskID); err != nil {
		return api.DelegationWindowList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+windowCols+` FROM owner_delegation_windows WHERE task_id=? ORDER BY created_at DESC,rowid DESC`, taskID)
	if err != nil {
		return api.DelegationWindowList{}, err
	}
	out := api.DelegationWindowList{Windows: []api.DelegationWindow{}}
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Windows = append(out.Windows, w)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for i := range out.Windows {
		if err := fillWindowRoutes(ctx, s.db, &out.Windows[i]); err != nil {
			return out, err
		}
	}
	return out, nil
}

var errDelegateMatrix = fmt.Errorf("%w: matrix approvals stay with the owner; a delegate cannot give one", api.ErrConflict)

var errDelegationRationale = fmt.Errorf("%w: a delegated answer needs a nonblank rationale of at most %d bytes", api.ErrInvalid, api.MaxDelegationRationale)

// delegatedRoute is the verified window and route for a delegate's answer.
type delegatedRoute struct {
	window   api.DelegationWindow
	delegate api.Agent
	route    windowRoute
}

// verifyWindowDelegate checks, at answer time and independent of the broker,
// that agentID's current run holds an active window that routed this request
// within its scope. 403: not the delegate, a stale run or no window; 409: the
// window ended, the request was returned or is outside the scope.
func (s *Store) verifyWindowDelegate(ctx context.Context, tx *sql.Tx, taskID, kind string, seq int64, agentID, runID string, now time.Time) (delegatedRoute, error) {
	var out delegatedRoute
	rows, err := tx.QueryContext(ctx, `SELECT r.window_id,r.request_kind,r.request_seq,r.obligation_id,r.category,r.notice_seq,r.routed_at,r.returned_at,r.answer_seq,r.answered_by_agent_id,r.answered_by_run_id,r.rationale,r.followed_recommendation,r.answered_at
FROM owner_delegation_routes r JOIN owner_delegation_windows w ON w.id=r.window_id
WHERE r.task_id=? AND r.request_kind=? AND r.request_seq=? AND w.delegate_agent_id=? ORDER BY r.routed_at DESC,r.rowid DESC`, taskID, kind, seq, agentID)
	if err != nil {
		return out, err
	}
	routes, err := scanRoutes(rows)
	if err != nil {
		return out, err
	}
	if len(routes) == 0 {
		if w, ok, err := openWindow(ctx, tx, taskID); err != nil {
			return out, err
		} else if ok && w.DelegateAgentID == agentID {
			return out, fmt.Errorf("%w: this request is outside the delegation window's scope or was not routed to you; it stays with the owner", api.ErrConflict)
		}
		return out, fmt.Errorf("%w: no owner delegation window routes this request to you", api.ErrDelegationForbidden)
	}
	out.route = routes[0]
	if out.window, err = loadWindow(ctx, tx, taskID, out.route.WindowID); err != nil {
		return out, err
	}
	if err := s.requireCurrentRun(ctx, tx, taskID, agentID, runID); err != nil {
		if errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrNotFound) {
			return out, fmt.Errorf("%w: only the delegate's current run may answer", api.ErrDelegationForbidden)
		}
		return out, err
	}
	if !out.window.Active(now) {
		return out, fmt.Errorf("%w: the delegation window ended; the owner answers this request now", api.ErrConflict)
	}
	if out.route.returned {
		return out, fmt.Errorf("%w: this request was returned to the owner", api.ErrConflict)
	}
	if out.route.AnswerSeq > 0 {
		return out, fmt.Errorf("%w: this request was already answered", api.ErrConflict)
	}
	if !api.DelegationScopeCovers(out.window.Scope, out.route.Category) {
		return out, fmt.Errorf("%w: this request is outside the delegation window's scope", api.ErrConflict)
	}
	if out.delegate, err = s.delegateAgent(ctx, tx, out.window); err != nil {
		return out, err
	}
	return out, nil
}

// recordDelegatedAnswer saves the delegate's answer on its route.
func recordDelegatedAnswer(ctx context.Context, tx *sql.Tx, d delegatedRoute, answerSeq int64, runID, rationale string, followed *bool, now time.Time) error {
	var follow any
	if followed != nil {
		follow = 0
		if *followed {
			follow = 1
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE owner_delegation_routes SET answer_seq=?,answered_by_agent_id=?,answered_by_run_id=?,rationale=?,followed_recommendation=?,answered_at=? WHERE window_id=? AND request_kind=? AND request_seq=? AND answer_seq=0`,
		answerSeq, d.window.DelegateAgentID, runID, rationale, follow, ts(now), d.window.ID, d.route.RequestKind, d.route.RequestSeq)
	return err
}

func delegatedRefs(w api.DelegationWindow) map[string]string {
	return map[string]string{"delegated": "true", "delegation": w.ID, "onBehalfOf": "owner"}
}
