package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

func scopedLeadItem(ctx context.Context, tx *sql.Tx, task, agent, run string) (string, error) {
	if run == "" {
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&run); err != nil {
			return "", err
		}
	}
	var item string
	err := tx.QueryRowContext(ctx, `SELECT item_id FROM item_team_leads WHERE task_id=? AND agent_id=? AND run_id=? AND state<>'closed'`, task, agent, run).Scan(&item)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return item, err
}

func ensureLeadItemLinks(ctx context.Context, tx *sql.Tx, task, agent, run string, links []api.MessageWorkItem, recipient string) error {
	if run == "" {
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&run); err != nil {
			return err
		}
	}
	item, err := scopedLeadItem(ctx, tx, task, agent, run)
	if err != nil {
		return err
	}
	if item == "" {
		var limit, bound int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT concurrency_limit FROM team_queue_settings WHERE task_id=?),1)`, task).Scan(&limit); err != nil {
			return err
		}
		if limit > 1 {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=? AND item_task_id=?`, agent, run, task).Scan(&bound); err != nil {
				return err
			}
			if bound != 0 {
				return fmt.Errorf("%w: only the exact current item lead may assign or request a decision", api.ErrConflict)
			}
		}
		return nil
	}
	if len(links) == 0 {
		return fmt.Errorf("%w: item lead message needs its own item link", api.ErrConflict)
	}
	for _, link := range links {
		if link.ItemTaskID != task || link.ItemID != item {
			return fmt.Errorf("%w: item lead cannot act for another item", api.ErrConflict)
		}
	}
	if recipient != "" {
		var targetItem string
		err := tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND item_task_id=? ORDER BY created_at DESC LIMIT 1`, recipient, task).Scan(&targetItem)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && targetItem != item {
			return fmt.Errorf("%w: item lead cannot assign another item's worker", api.ErrConflict)
		}
	}
	return nil
}
