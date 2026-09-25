package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Capacity is owner supplied and expires. Conservative reservations include
// uncertain launches and every non-cleaned exact agent run on the host.
func checkTeamHostCapacity(ctx context.Context, tx *sql.Tx, host string, now time.Time, additionalReservations int) error {
	var version int64
	var expires string
	var sessionsLimit, pollsLimit int
	if err := tx.QueryRowContext(ctx, `SELECT version,expires_at,max_sessions,max_polling FROM team_host_policies WHERE host=?`, host).Scan(&version, &expires, &sessionsLimit, &pollsLimit); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: host capacity policy is missing", api.ErrConflict)
		}
		return err
	}
	until, err := time.Parse(time.RFC3339, expires)
	if err != nil || version < 1 || !until.After(now) {
		return fmt.Errorf("%w: host capacity policy is stale", api.ErrConflict)
	}
	var agents, pendingSlots, polls int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE host=? AND (status<>'closed' OR cleanup_done=0)`, host).Scan(&agents); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT q.launch_json FROM team_launch_reservations r JOIN team_queue_entries q ON q.id=r.entry_id WHERE q.host=? AND r.state IN ('reserved','launching')`, host)
	if err != nil {
		return err
	}
	for rows.Next() {
		var frozen string
		if err := rows.Scan(&frozen); err != nil {
			rows.Close()
			return err
		}
		if frozen == "" {
			pendingSlots += 4
			continue
		}
		var plan struct {
			Members []struct {
				State string `json:"state"`
			} `json:"members"`
		}
		if json.Unmarshal([]byte(frozen), &plan) != nil || len(plan.Members) == 0 {
			rows.Close()
			return fmt.Errorf("%w: frozen reservation is invalid", api.ErrConflict)
		}
		for _, member := range plan.Members {
			if member.State != "started" {
				pendingSlots++
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT task_id FROM agents WHERE host=? AND (status<>'closed' OR cleanup_done=0)
		UNION SELECT task_id FROM team_queue_entries WHERE host=? AND state IN ('queued','launching','running'))`, host, host).Scan(&polls); err != nil {
		return err
	}
	if agents+pendingSlots+4*additionalReservations > sessionsLimit || polls > pollsLimit {
		return fmt.Errorf("%w: host session or polling budget exhausted", api.ErrConflict)
	}
	return nil
}
