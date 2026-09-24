package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/scs32/tailterm/hub/internal/api"
)

// linkFlags are the work-item context flags shared by tt post and tt send.
// A bound worker's messages must carry its item link or bound recipients
// never see them (store.ListMessages filters bound inboxes by item).
type linkFlags struct {
	workItemTask, workItemID, workOrderTask *string
	workItemRevision, workOrderMessage      *int64
	related                                 *stringListFlag
}

func registerLinkFlags(fs *flag.FlagSet) *linkFlags {
	var related stringListFlag
	fs.Var(&related, "related", "related exact item as TASK/ITEM@REVISION (repeatable)")
	return &linkFlags{
		workItemTask:     fs.String("work-item-task", "", "project owning the linked item (default: --task)"),
		workItemID:       fs.String("work-item", "", "bug or feature linked to this message"),
		workItemRevision: fs.Int64("work-item-revision", 0, "exact linked item revision"),
		workOrderTask:    fs.String("work-order-task", "", "project containing the work-order message"),
		workOrderMessage: fs.Int64("work-order-message", 0, "recorded work-order message sequence"),
		related:          &related,
	}
}

// apply links req to the sender's bound item (or explicit flags), sets its
// audit classification, and derives a request ID for bound senders.
func (l *linkFlags) apply(ctx context.Context, c *api.Client, e env, task, target string, reply int64, text, requestID string, intake bool, req *api.PostMessageRequest) error {
	workItemTask, workItemID, workItemRevision := l.workItemTask, l.workItemID, l.workItemRevision
	workOrderTask, workOrderMessage, related := l.workOrderTask, l.workOrderMessage, *l.related
	if e.agent != "" && os.Getenv("TAILTERM_WORK_ITEM") != "" {
		self, selfErr := c.GetAgent(ctx, task, e.agent)
		if selfErr != nil {
			return selfErr
		}
		if self.WorkItem != nil && *workItemID == "" {
			*workItemTask, *workItemID, *workItemRevision = self.WorkItem.ItemTaskID, self.WorkItem.ItemID, self.WorkItem.ItemRevision
			*workOrderTask, *workOrderMessage = self.WorkItem.WorkOrderMessage.TaskID, self.WorkItem.WorkOrderMessage.Seq
			if requestID == "" {
				digest := sha256.Sum256([]byte(e.runID + "\x00" + target + "\x00" + fmt.Sprint(reply) + "\x00" + text))
				requestID = fmt.Sprintf("item-post-%x", digest[:12])
			}
		}
	}
	linked := *workItemID != "" || *workItemTask != "" || *workItemRevision != 0 || *workOrderTask != "" || *workOrderMessage != 0 || len(related) > 0
	if intake {
		if linked || requestID == "" {
			return errors.New("--intake requires --request-id and forbids work-item, related, and work-order flags")
		}
		req.AuditKind = api.MessageAuditIntake
		req.RequestID = requestID
	}
	if linked {
		if intake {
			return errors.New("--intake cannot be combined with work context")
		}
		if *workItemTask == "" {
			*workItemTask = task
		}
		if *workOrderTask == "" {
			*workOrderTask = *workItemTask
		}
		if !api.ValidID(*workItemTask, "tsk") || !api.ValidID(*workItemID, "wi") || *workItemRevision < 1 || !api.ValidID(*workOrderTask, "tsk") || *workOrderMessage < 1 || requestID == "" {
			return errors.New("linked post requires --request-id, --work-item, --work-item-revision and --work-order-message")
		}
		req.AuditKind = api.MessageAuditWork
		req.RequestID = requestID
		req.WorkItems = []api.MessageWorkItem{{ItemTaskID: *workItemTask, ItemID: *workItemID, ItemRevision: *workItemRevision, Relationship: "primary"}}
		if len(related) > api.MaxMessageAuditRelated {
			return fmt.Errorf("at most %d related items are allowed", api.MaxMessageAuditRelated)
		}
		seen := map[string]bool{fmt.Sprintf("%s/%s", *workItemTask, *workItemID): true}
		for _, raw := range related {
			item, parseErr := parseRelatedItem(raw)
			if parseErr != nil {
				return parseErr
			}
			identity := item.ItemTaskID + "/" + item.ItemID
			if seen[identity] {
				return fmt.Errorf("duplicate primary/related item %s", identity)
			}
			seen[identity] = true
			req.WorkItems = append(req.WorkItems, item)
		}
		req.WorkOrderMessage = &api.MessageReference{TaskID: *workOrderTask, Seq: *workOrderMessage}
	} else if requestID != "" && !intake {
		// A retry key alone is a keyed legacy/unclassified post, not evidence of
		// linked work context.
		req.RequestID = requestID
	}
	return nil
}
