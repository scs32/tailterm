package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ErrDecisionAnswerForbidden distinguishes an agent-authored answer from a
// malformed human answer so the HTTP boundary can return the promised 403.
var ErrDecisionAnswerForbidden = errors.New("only a human can answer a decision request")

type operationHashInput struct {
	Operation string `json:"operation"`
	Payload   any    `json:"payload"`
}

func operationRequestHash(operation string, payload any) string {
	return requestHash(operationHashInput{Operation: operation, Payload: payload})
}

func replayMessageRequest(q queryRower, ctx context.Context, taskID, requestID, agentID, payload string, by api.Caller) (api.Message, bool, error) {
	receipt, priorHash, err := findMessagePostReceipt(q, ctx, taskID, requestID, agentID, by)
	if errors.Is(err, api.ErrNotFound) {
		return api.Message{}, false, nil
	}
	if err != nil {
		return api.Message{}, false, err
	}
	if priorHash != payload {
		return api.Message{}, false, workItemConflict("request ID was already used with different message data")
	}
	message, err := loadMessage(q, ctx, taskID, receipt.MessageSeq)
	if err != nil {
		return message, false, err
	}
	message.PostReceipt = &receipt
	return message, true, nil
}

func cloneDecisionRequest(req api.DecisionRequest) api.DecisionRequest {
	clone := req
	clone.Options = append([]api.DecisionOption(nil), req.Options...)
	return clone
}

func insertDecisionRequest(ctx context.Context, tx *sql.Tx, message *api.Message, request api.DecisionRequest) error {
	options, err := json.Marshal(request.Options)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO decision_requests
(message_seq,task_id,question,options,recommended_option_id,recommendation_reason,created_at)
VALUES(?,?,?,?,?,?,?)`, message.Seq, message.TaskID, request.Question, string(options),
		request.RecommendedOptionID, request.RecommendationReason, ts(message.CreatedAt)); err != nil {
		return err
	}
	copy := cloneDecisionRequest(request)
	message.DecisionRequest = &copy
	return nil
}

func loadDecisionRequest(q queryRower, ctx context.Context, taskID string, requestSeq int64) (api.DecisionRequest, error) {
	var request api.DecisionRequest
	var options string
	err := q.QueryRowContext(ctx, `SELECT question,options,recommended_option_id,recommendation_reason
FROM decision_requests WHERE task_id=? AND message_seq=?`, taskID, requestSeq).Scan(
		&request.Question, &options, &request.RecommendedOptionID, &request.RecommendationReason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return request, api.ErrNotFound
	}
	if err != nil {
		return request, err
	}
	if err = json.Unmarshal([]byte(options), &request.Options); err != nil {
		return request, err
	}
	return request, nil
}

func insertDecisionAnswer(ctx context.Context, tx *sql.Tx, message *api.Message, answer api.DecisionAnswer) error {
	var optionID any
	var text any
	if answer.OptionID != "" {
		optionID = answer.OptionID
	}
	if answer.Text != "" {
		text = answer.Text
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO decision_answers
(message_seq,task_id,request_seq,option_id,text,created_at) VALUES(?,?,?,?,?,?)`,
		message.Seq, message.TaskID, answer.RequestSeq, optionID, text, ts(message.CreatedAt)); err != nil {
		return err
	}
	copy := answer
	message.DecisionAnswer = &copy
	return nil
}

func decisionTask(q queryRower, ctx context.Context, taskID string) (api.Task, error) {
	task, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return task, api.ErrNotFound
	}
	return task, err
}

// CreateDecision atomically stores an agent-authored question, immutable
// decision metadata, the message event, and its recoverable post receipt.
func (s *Store) CreateDecision(ctx context.Context, taskID string, req api.CreateDecisionRequest, by api.Caller) (api.Message, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(req.AgentID, "agt") || !validRequestID(req.RequestID) {
		return api.Message{}, api.ErrInvalid
	}
	if err := api.ValidateDecisionRequest(req.DecisionRequest); err != nil {
		return api.Message{}, err
	}
	messageReq := api.PostMessageRequest{
		WorkItems: req.WorkItems, WorkOrderMessage: req.WorkOrderMessage,
		RequestID: req.RequestID, AgentID: req.AgentID,
		Text: api.FormatDecisionRequest(req.DecisionRequest),
	}
	if err := validateMessageRequestShape(messageReq); err != nil {
		return api.Message{}, err
	}
	payload := operationRequestHash("decision_request", req)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replayMessageRequest(tx, ctx, taskID, req.RequestID, req.AgentID, payload, by); found || err != nil {
		return replay, err
	}
	// Broker phase 3.1 (round one B2): a decision request is an agent board
	// post, so unacknowledged work gates it too.
	if err := ackGate(ctx, tx, req.AgentID, "", 0, s.now()); err != nil {
		return api.Message{}, err
	}
	task, err := decisionTask(tx, ctx, taskID)
	if err != nil {
		return api.Message{}, err
	}
	if task.Status != api.TaskOpen {
		return api.Message{}, api.ErrClosed
	}
	author, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, req.AgentID))
	if err != nil || author.TaskID != taskID {
		return api.Message{}, api.ErrInvalid
	}
	if author.Status == api.AgentClosed || author.Status == api.AgentExited {
		return api.Message{}, api.ErrConflict
	}
	message, err := s.insertMessage(ctx, tx, task, messageReq, api.Agent{}, by, false, false)
	if err != nil {
		return message, err
	}
	if err = insertDecisionRequest(ctx, tx, &message, req.DecisionRequest); err != nil {
		return message, err
	}
	if err = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, payload, by); err != nil {
		return message, err
	}
	if err = tx.Commit(); err != nil {
		return message, err
	}
	s.notify(taskID)
	return message, nil
}

type answerHashPayload struct {
	RequestSeq int64                     `json:"requestSeq"`
	Answer     api.AnswerDecisionRequest `json:"answer"`
}

// AnswerDecision atomically stores the first human answer as a directed reply.
// An exact retry is recovered before closure, resolution, or agent state checks.
func (s *Store) AnswerDecision(ctx context.Context, taskID string, requestSeq int64, req api.AnswerDecisionRequest, by api.Caller) (api.Message, error) {
	if req.AgentID != "" {
		return api.Message{}, ErrDecisionAnswerForbidden
	}
	if !api.ValidID(taskID, "tsk") || requestSeq < 1 || !validRequestID(req.RequestID) {
		return api.Message{}, api.ErrInvalid
	}
	payload := operationRequestHash("decision_answer", answerHashPayload{RequestSeq: requestSeq, Answer: req})

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replayMessageRequest(tx, ctx, taskID, req.RequestID, "", payload, by); found || err != nil {
		return replay, err
	}
	task, err := decisionTask(tx, ctx, taskID)
	if err != nil {
		return api.Message{}, err
	}
	if task.Status != api.TaskOpen {
		return api.Message{}, api.ErrClosed
	}
	request, err := loadDecisionRequest(tx, ctx, taskID, requestSeq)
	if err != nil {
		return api.Message{}, err
	}
	if err = api.ValidateDecisionAnswer(req, request); err != nil {
		return api.Message{}, err
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT message_seq FROM decision_answers WHERE task_id=? AND request_seq=?`, taskID, requestSeq).Scan(&existing)
	if err == nil {
		return api.Message{}, workItemConflict("decision request was already answered")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, err
	}
	requestMessage, err := loadMessage(tx, ctx, taskID, requestSeq)
	if err != nil {
		return api.Message{}, err
	}
	if requestMessage.From.AgentID == "" {
		return api.Message{}, fmt.Errorf("decision request %d has no requesting agent", requestSeq)
	}
	target, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, requestMessage.From.AgentID))
	if err != nil || target.TaskID != taskID {
		return api.Message{}, api.ErrInvalid
	}
	messageReq := api.PostMessageRequest{
		RequestID: req.RequestID, ReplyTo: requestSeq, To: target.ID,
		Text:             api.FormatDecisionAnswer(requestSeq, req, request),
		WorkItems:        requestMessage.WorkItems,
		WorkOrderMessage: requestMessage.WorkOrderMessage,
	}
	// This typed answer copies the immutable request's exact item coordinates.
	// The item may have advanced since the owner was asked, so the preserved
	// considered revision is valid even when it is no longer current.
	message, err := s.insertMessage(ctx, tx, task, messageReq, target, by, false, true)
	if err != nil {
		return message, err
	}
	answer := api.DecisionAnswer{RequestSeq: requestSeq, OptionID: req.OptionID, Text: req.Text}
	if err = insertDecisionAnswer(ctx, tx, &message, answer); err != nil {
		return message, err
	}
	if err = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, payload, by); err != nil {
		return message, err
	}
	if err = tx.Commit(); err != nil {
		return message, err
	}
	s.notify(taskID)
	return message, nil
}

// ListDecisions returns the mutable request/answer projection in request order.
func (s *Store) ListDecisions(ctx context.Context, taskID string, after int64, limit int) (api.DecisionList, error) {
	if !api.ValidID(taskID, "tsk") || after < 0 {
		return api.DecisionList{}, api.ErrInvalid
	}
	if limit <= 0 {
		limit = api.MaxDecisionPage
	}
	if limit > api.MaxDecisionPage {
		return api.DecisionList{}, api.ErrInvalid
	}
	if _, err := decisionTask(s.db, ctx, taskID); err != nil {
		return api.DecisionList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.message_seq,COALESCE(a.message_seq,0)
FROM decision_requests r LEFT JOIN decision_answers a ON a.task_id=r.task_id AND a.request_seq=r.message_seq
WHERE r.task_id=? AND r.message_seq>? ORDER BY r.message_seq LIMIT ?`, taskID, after, limit+1)
	if err != nil {
		return api.DecisionList{}, err
	}
	type pair struct{ request, answer int64 }
	pairs := make([]pair, 0, limit+1)
	for rows.Next() {
		var value pair
		if err = rows.Scan(&value.request, &value.answer); err != nil {
			rows.Close()
			return api.DecisionList{}, err
		}
		pairs = append(pairs, value)
	}
	if err = rows.Close(); err != nil {
		return api.DecisionList{}, err
	}
	if err = rows.Err(); err != nil {
		return api.DecisionList{}, err
	}
	more := len(pairs) > limit
	if more {
		pairs = pairs[:limit]
	}
	out := api.DecisionList{Decisions: make([]api.DecisionRecord, 0, len(pairs))}
	for _, value := range pairs {
		request, err := loadMessage(s.db, ctx, taskID, value.request)
		if err != nil {
			return api.DecisionList{}, err
		}
		record := api.DecisionRecord{Request: request}
		if value.answer > 0 {
			answer, err := loadMessage(s.db, ctx, taskID, value.answer)
			if err != nil {
				return api.DecisionList{}, err
			}
			record.Answer = &answer
		}
		out.Decisions = append(out.Decisions, record)
	}
	if more && len(pairs) > 0 {
		out.NextAfter = pairs[len(pairs)-1].request
	}
	return out, nil
}
