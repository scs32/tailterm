package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/scs32/tailterm/hub/internal/api"
)

const messageSelectCols = `m.seq,m.task_id,m.from_agent,m.from_node,m.from_user,m.to_agent,m.text,m.created_at,m.reply_to,m.broadcast,
COALESCE(l.item_task_id,''),COALESCE(l.item_id,''),COALESCE(l.item_revision,0),COALESCE(l.relationship,''),
COALESCE(l.work_order_task_id,''),COALESCE(l.work_order_message_seq,0),
COALESCE(r.receipt_id,''),COALESCE(r.request_id,''),COALESCE(r.task_id,''),COALESCE(r.message_seq,0),COALESCE(r.created_at,'')`

type rowScanner interface {
	Scan(...any) error
}

func scanMessage(row rowScanner) (api.Message, error) {
	var message api.Message
	var created, itemTaskID, itemID, relationship, orderTaskID string
	var receipt api.MessagePostReceipt
	var receiptCreated string
	var itemRevision, orderSeq int64
	err := row.Scan(
		&message.Seq, &message.TaskID,
		&message.From.AgentID, &message.From.Node, &message.From.User,
		&message.To, &message.Text, &created, &message.ReplyTo, &message.Broadcast,
		&itemTaskID, &itemID, &itemRevision, &relationship,
		&orderTaskID, &orderSeq,
		&receipt.ID, &receipt.RequestID, &receipt.TaskID, &receipt.MessageSeq, &receiptCreated,
	)
	if err != nil {
		return message, err
	}
	message.CreatedAt = parseTS(created)
	if itemID != "" {
		message.WorkItems = []api.MessageWorkItem{{
			ItemTaskID:   itemTaskID,
			ItemID:       itemID,
			ItemRevision: itemRevision,
			Relationship: relationship,
		}}
	}
	if orderTaskID != "" {
		message.WorkOrderMessage = &api.MessageReference{TaskID: orderTaskID, Seq: orderSeq}
	}
	if receipt.ID != "" {
		receipt.CreatedAt = parseTS(receiptCreated)
		message.PostReceipt = &receipt
	}
	return message, nil
}

func loadMessage(q queryRower, ctx context.Context, taskID string, seq int64) (api.Message, error) {
	message, err := scanMessage(q.QueryRowContext(ctx, `SELECT `+messageSelectCols+`
FROM messages m
LEFT JOIN message_work_item_links l ON l.message_seq=m.seq
LEFT JOIN message_post_requests r ON r.message_seq=m.seq
WHERE m.task_id=? AND m.seq=?`, taskID, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return message, api.ErrNotFound
	}
	return message, err
}

func validateMessageRequestShape(req api.PostMessageRequest) error {
	if len(req.WorkItems) > 1 || (req.RequestID != "" && !validRequestID(req.RequestID)) {
		return api.ErrInvalid
	}
	if len(req.WorkItems) == 0 {
		if req.WorkOrderMessage != nil {
			return api.ErrInvalid
		}
		return nil
	}
	link := req.WorkItems[0]
	if req.RequestID == "" || link.Relationship != "primary" || !api.ValidID(link.ItemTaskID, "tsk") || !api.ValidID(link.ItemID, "wi") || link.ItemRevision < 1 {
		return api.ErrInvalid
	}
	if order := req.WorkOrderMessage; order != nil {
		if !api.ValidID(order.TaskID, "tsk") || order.Seq < 1 {
			return api.ErrInvalid
		}
	}
	return nil
}

func validateMessageContext(q queryRower, ctx context.Context, messageTaskID string, req api.PostMessageRequest, allowCrossProject bool) error {
	if allowCrossProject {
		if len(req.WorkItems) != 1 {
			return api.ErrInvalid
		}
		link := req.WorkItems[0]
		if link.Relationship != "primary" || !api.ValidID(link.ItemTaskID, "tsk") || !api.ValidID(link.ItemID, "wi") || link.ItemRevision < 1 || req.WorkOrderMessage != nil {
			return api.ErrInvalid
		}
	} else if err := validateMessageRequestShape(req); err != nil {
		return err
	}
	if len(req.WorkItems) == 0 {
		return nil
	}
	link := req.WorkItems[0]
	if !allowCrossProject && link.ItemTaskID != messageTaskID {
		return api.ErrInvalid
	}
	item, err := getWorkItem(q, ctx, link.ItemTaskID, link.ItemID)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return api.ErrInvalid
		}
		return err
	}
	if item.Revision != link.ItemRevision {
		return workItemConflict("work item revision changed; refresh it before posting")
	}
	if order := req.WorkOrderMessage; order != nil {
		if order.TaskID != link.ItemTaskID {
			return api.ErrInvalid
		}
		var found int64
		if err := q.QueryRowContext(ctx, `SELECT seq FROM messages WHERE task_id=? AND seq=?`, order.TaskID, order.Seq).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return api.ErrInvalid
			}
			return err
		}
	}
	return nil
}

func insertMessageContext(ctx context.Context, tx *sql.Tx, message *api.Message, req api.PostMessageRequest) error {
	if len(req.WorkItems) == 0 {
		return nil
	}
	link := req.WorkItems[0]
	var orderTask any
	var orderSeq any
	if req.WorkOrderMessage != nil {
		orderTask = req.WorkOrderMessage.TaskID
		orderSeq = req.WorkOrderMessage.Seq
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_work_item_links
(message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,work_order_task_id,work_order_message_seq,created_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		message.Seq, message.TaskID, link.ItemTaskID, link.ItemID, link.ItemRevision, link.Relationship,
		orderTask, orderSeq, ts(message.CreatedAt)); err != nil {
		return err
	}
	message.WorkItems = append([]api.MessageWorkItem(nil), req.WorkItems...)
	if req.WorkOrderMessage != nil {
		order := *req.WorkOrderMessage
		message.WorkOrderMessage = &order
	}
	return nil
}

func insertMessagePostReceipt(ctx context.Context, tx *sql.Tx, message *api.Message, requestID, payload string, by api.Caller) error {
	receipt := api.MessagePostReceipt{
		ID:         api.NewID("mpr"),
		RequestID:  requestID,
		TaskID:     message.TaskID,
		MessageSeq: message.Seq,
		CreatedAt:  message.CreatedAt,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_post_requests
(receipt_id,task_id,agent_id,by_node,by_user,request_id,payload_hash,message_seq,created_at)
VALUES(?,?,?,?,?,?,?,?,?)`, receipt.ID, receipt.TaskID, message.From.AgentID, by.Node, by.User,
		receipt.RequestID, payload, receipt.MessageSeq, ts(receipt.CreatedAt)); err != nil {
		return err
	}
	message.PostReceipt = &receipt
	return nil
}

func findMessagePostReceipt(q queryRower, ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.MessagePostReceipt, string, error) {
	var receipt api.MessagePostReceipt
	var created, payload string
	err := q.QueryRowContext(ctx, `SELECT receipt_id,request_id,task_id,message_seq,created_at,payload_hash
FROM message_post_requests WHERE task_id=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, agentID, by.Node, by.User, requestID).Scan(
		&receipt.ID, &receipt.RequestID, &receipt.TaskID, &receipt.MessageSeq, &created, &payload,
	)
	receipt.CreatedAt = parseTS(created)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, payload, api.ErrNotFound
	}
	return receipt, payload, err
}

func (s *Store) GetMessagePostReceipt(ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.Message, error) {
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.Message{}, api.ErrInvalid
	}
	receipt, _, err := findMessagePostReceipt(s.db, ctx, taskID, requestID, agentID, by)
	if err != nil {
		return api.Message{}, err
	}
	message, err := loadMessage(s.db, ctx, taskID, receipt.MessageSeq)
	if err != nil {
		return message, err
	}
	message.PostReceipt = &receipt
	return message, nil
}
